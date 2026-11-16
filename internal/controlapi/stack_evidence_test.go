package controlapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
	"github.com/infercrane/infercrane/internal/stackevidence"
)

const measuredQwenStack = "qwen3.8-27b-fp8-h200x1-sglang-0.5.20-nextn"

func stacksAPI(t *testing.T, prices map[pricing.Request]pricing.Estimate) http.Handler {
	t.Helper()
	// Store is nil on purpose: ranking must read only the embedded dataset and
	// the price snapshot, so any store call would panic and fail the test.
	return (API{APIKey: "secret", GPUPriceCatalog: pricing.NewDynamicCatalog(prices)}).Handler()
}

func decodeStacks(t *testing.T, response interface{ Bytes() []byte }) stackevidence.Result {
	t.Helper()
	var result stackevidence.Result
	if err := json.Unmarshal(response.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestBestKnownStacksRanksCitedEvidenceWithACurrentPrice(t *testing.T) {
	now := time.Now().UTC()
	handler := stacksAPI(t, map[pricing.Request]pricing.Estimate{
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA H200", GPUCount: 1, Replicas: 1}: {Currency: "USD", Hourly: 3.99, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: "https://api.runpod.io/graphql", ObservedAt: now.Add(-time.Minute), StaleAfter: time.Hour},
		{Cloud: "vast", Region: "global", GPU: "H200", GPUCount: 1, Replicas: 1}:          {Currency: "USD", Hourly: 1.10, CostScope: pricing.CostScopeAcceleratorOnly, Authority: pricing.PriceAuthorityProviderAPI, Source: "https://console.vast.ai/api", ObservedAt: now.Add(-time.Minute), StaleAfter: time.Hour},
		{Cloud: "aws", Region: "us-east-1", GPU: "NVIDIA H200", GPUCount: 1, Replicas: 1}: {Currency: "USD", Hourly: 0.50, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: "https://pricing.us-east-1.amazonaws.com", ObservedAt: now.Add(-2 * time.Hour), StaleAfter: time.Hour},
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA H200", GPUCount: 1, Replicas: 2}: {Currency: "USD", Hourly: 0.10, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: "https://api.runpod.io/graphql", ObservedAt: now.Add(-time.Minute), StaleAfter: time.Hour},
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA H200", GPUCount: 2, Replicas: 1}: {Currency: "USD", Hourly: 0.20, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: "https://api.runpod.io/graphql", ObservedAt: now.Add(-time.Minute), StaleAfter: time.Hour},
	})
	response := searchRequest(handler, "/api/v1/catalog/stacks?model=qwen3.8-27b-fp8&goal=lowest_cost&input_tokens=3919&output_tokens=1377&max_ttft_p95_ms=1000", true)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	result := decodeStacks(t, response.Body)
	if result.SchemaVersion != stackevidence.ResultSchemaVersion || result.Model.Requested != "Qwen/Qwen3.8-27B-FP8" || result.Model.Match != stackevidence.MatchExact {
		t.Fatalf("result = %+v", result.Model)
	}
	if result.DefaultStack != measuredQwenStack || len(result.Stacks) == 0 || result.Stacks[0].ID != measuredQwenStack {
		t.Fatalf("default %s stacks %+v", result.DefaultStack, result.Stacks)
	}
	top := result.Stacks[0]
	// Only the fresh, complete, one-replica, one-GPU row may price it: the
	// cheaper accelerator-only, stale, two-replica and two-GPU rows never count.
	if top.Cost.State != stackevidence.CostEstimated || top.Cost.Provider != "runpod" || *top.Cost.HourlyUSD != 3.99 {
		t.Fatalf("cost = %+v", top.Cost)
	}
	want := 3.99 / (*cited(t, top.OperatingPoint.Concurrency) * 3600) * 1_000_000
	if math.Abs(*top.Cost.PerMillionOutputTokensUSD-want) > 0.0001 {
		t.Fatalf("cost per 1M = %v, want %v", *top.Cost.PerMillionOutputTokensUSD, want)
	}
	if top.Target != stackevidence.TargetMet || *top.Metrics.TTFTP95Ms > 1000 || top.Source.Display != "Measured by InferCrane · 2026-09-23" {
		t.Fatalf("top = %+v", top)
	}
}

// cited returns the published aggregate throughput of the measured stack at a concurrency.
func cited(t *testing.T, concurrency int) *float64 {
	t.Helper()
	stack, ok := stackevidence.MustDefault().Stack(measuredQwenStack)
	if !ok {
		t.Fatal("measured stack is missing")
	}
	for _, point := range stack.Points {
		if point.Concurrency == concurrency {
			return point.OutputTokensPerSecond
		}
	}
	t.Fatalf("no point at concurrency %d", concurrency)
	return nil
}

func TestBestKnownStacksLeavesCostUnknownWithoutAPrice(t *testing.T) {
	now := time.Now().UTC()
	onlyL40S := map[pricing.Request]pricing.Estimate{
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}: {Currency: "USD", Hourly: 0.86, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: "https://api.runpod.io/graphql", ObservedAt: now, StaleAfter: time.Hour},
	}
	response := searchRequest(stacksAPI(t, onlyL40S), "/api/v1/catalog/stacks?model=Qwen/Qwen3.8-27B-FP8", true)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	stack := raw["stacks"].([]any)[0].(map[string]any)
	cost := stack["cost"].(map[string]any)
	if cost["state"] != "unknown" || cost["per_million_output_tokens_usd"] != nil || !strings.Contains(cost["basis"].(string), "1 × H200") {
		t.Fatalf("cost = %+v", cost)
	}
	if _, present := stack["metrics"].(map[string]any)["ttft_p99_ms"]; !present {
		t.Fatal("unpublished metrics must be present as null, not omitted")
	}
}

func TestBestKnownStacksForAModelWithoutEvidence(t *testing.T) {
	response := searchRequest(stacksAPI(t, nil), "/api/v1/catalog/stacks?model=mistral-7b-instruct&goal=lowest_latency", true)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	result := decodeStacks(t, response.Body)
	if result.Model.Requested != "mistralai/Mistral-7B-Instruct-v0.3" || result.Model.Match != stackevidence.MatchNone || len(result.Stacks) != 0 || result.DefaultStack != "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestBestKnownStacksValidatesItsQuery(t *testing.T) {
	handler := stacksAPI(t, nil)
	if response := searchRequest(handler, "/api/v1/catalog/stacks?model=Qwen/Qwen3.8-27B-FP8", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.Code)
	}
	for _, target := range []string{
		"/api/v1/catalog/stacks",
		"/api/v1/catalog/stacks?model=llama",
		"/api/v1/catalog/stacks?model=Qwen/Qwen3.8-27B-FP8&goal=fastest",
		"/api/v1/catalog/stacks?model=Qwen/Qwen3.8-27B-FP8&limit=x",
		"/api/v1/catalog/stacks?model=Qwen/Qwen3.8-27B-FP8&limit=51",
		"/api/v1/catalog/stacks?model=Qwen/Qwen3.8-27B-FP8&max_ttft_p95_ms=NaN",
		"/api/v1/catalog/stacks?model=Qwen/Qwen3.8-27B-FP8&input_tokens=0.5",
		"/api/v1/catalog/stacks?model=Qwen/Qwen3.8-27B-FP8&gpus=H100,,H200",
		"/api/v1/catalog/stacks?model=Qwen/Qwen3.8-27B-FP8&provider=runpod",
		"/api/v1/catalog/stacks?model=Qwen/Qwen3.8-27B-FP8&goal=lowest_cost&goal=lowest_latency",
	} {
		response := searchRequest(handler, target, true)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_query") {
			t.Fatalf("%s: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

func TestPlanIntentStartsFromAStack(t *testing.T) {
	handler := (API{APIKey: "secret", ComputeProviders: []ComputeProvider{{ID: "runpod", Label: "RunPod", State: "ready"}}}).Handler()
	request := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/planning/intents", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	response := request(`{"stack":"` + measuredQwenStack + `","provider":"runpod"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Plan struct {
			Stack *struct {
				ID, Source string
			} `json:"stack"`
			Configuration struct {
				Profile string `json:"profile"`
			} `json:"configuration"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Plan.Stack == nil || envelope.Plan.Stack.ID != measuredQwenStack || envelope.Plan.Configuration.Profile != "sglang-h200-nextn-k4-provisional" {
		t.Fatalf("plan = %+v", envelope.Plan)
	}
	if response := request(`{"stack":"qwen3.8-27b-fp8-h200x1-sglang-0.5.20"}`); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "no reviewed launch configuration") {
		t.Fatalf("unlaunchable stack: status=%d body=%s", response.Code, response.Body.String())
	}
}
