package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/intentplan"
	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/pricing"
	"github.com/infercrane/infercrane/internal/secrets"
)

const recordedToken = "hf_recorded_test_token"

// recordedHub serves the Hugging Face responses recorded in
// internal/modelcheck/testdata. The gated config.json answers only to the
// test token.
func recordedHub(t *testing.T) (*modelcheck.Resolver, *atomic.Int64) {
	t.Helper()
	fixtures := map[string]string{"microsoft/phi-4": "phi-4", "RWKV/rwkv-4-169m-pile": "rwkv-4-169m", "meta-llama/Llama-3.2-1B-Instruct": "llama-3.2-1b-gated"}
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("..", "modelcheck", "testdata", "huggingface", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if rest, ok := strings.CutPrefix(r.URL.Path, "/api/models/"); ok {
			repository, _, _ := strings.Cut(rest, "/revision/")
			name, known := fixtures[repository]
			if !known {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write(read("missing.info.json"))
				return
			}
			_, _ = w.Write(read(name + ".info.json"))
			return
		}
		owner, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		name, _, _ := strings.Cut(rest, "/raw/")
		fixture := fixtures[owner+"/"+name]
		if fixture == "llama-3.2-1b-gated" {
			if r.Header.Get("Authorization") != "Bearer "+recordedToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"architectures":["LlamaForCausalLM"],"model_type":"llama","num_hidden_layers":16,"hidden_size":2048,"num_attention_heads":32,"num_key_value_heads":8,"head_dim":64,"max_position_embeddings":131072}`))
			return
		}
		_, _ = w.Write(read(fixture + ".config.json"))
	}))
	t.Cleanup(server.Close)
	resolver, err := modelcheck.New(modelcheck.ResolverOptions{Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return resolver, &calls
}

func modelCheckAPI(t *testing.T, resolver *modelcheck.Resolver) http.Handler {
	t.Helper()
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	catalog := pricing.NewDynamicCatalog(map[pricing.Request]pricing.Estimate{
		{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}: {Currency: "USD", Hourly: .74, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: "https://api.runpod.io/graphql", ObservedAt: now, StaleAfter: time.Hour},
	})
	return (API{
		APIKey: "secret", Store: &fakeStore{}, Integrations: registry.Snapshot(), GPUPriceCatalog: catalog, ModelCheck: resolver,
		ComputeProviders: []ComputeProvider{{ID: "runpod", Label: "RunPod", State: "ready"}},
		Secrets:          secrets.Environment{Tenants: secrets.NewTenantPolicy("*"), Lookup: func(name string) (string, bool) { return recordedToken, name == "OPENROUTER_API_KEY" }},
	}).Handler()
}

func call(handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestHuggingFaceModelCheckRoute(t *testing.T) {
	resolver, calls := recordedHub(t)
	handler := modelCheckAPI(t, resolver)
	response := call(handler, http.MethodGet, "/api/v1/catalog/hugging-face/check?repository=microsoft/phi-4", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Check             modelcheck.Report `json:"check"`
		ReviewedModel     *string           `json:"reviewed_model"`
		PerformanceClaims bool              `json:"performance_claims"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Check.Verdict != modelcheck.Supported || *body.Check.Memory.GPU != "L40S" || body.ReviewedModel != nil || body.PerformanceClaims || body.Check.Reviewed {
		t.Fatalf("unexpected check: %+v", body)
	}
	for target, want := range map[string]int{
		"/api/v1/catalog/hugging-face/check?repository=phi-4":                                          http.StatusBadRequest,
		"/api/v1/catalog/hugging-face/check?repository=microsoft/phi-4&concurrency=0":                  http.StatusBadRequest,
		"/api/v1/catalog/hugging-face/check?repository=microsoft/phi-4&other=1":                        http.StatusBadRequest,
		"/api/v1/catalog/hugging-face/check?repository=acme/missing":                                   http.StatusNotFound,
		"/api/v1/catalog/hugging-face/check?repository=microsoft/phi-4&model_secret_reference_id=nope": http.StatusUnprocessableEntity,
	} {
		if got := call(handler, http.MethodGet, target, ""); got.Code != want {
			t.Fatalf("%s: status=%d body=%s", target, got.Code, got.Body.String())
		}
	}
	before := calls.Load()
	if got := call(modelCheckAPI(t, nil), http.MethodGet, "/api/v1/catalog/hugging-face/check?repository=microsoft/phi-4", ""); got.Code != http.StatusNotImplemented || !strings.Contains(got.Body.String(), codeMetadataAccessDisabled) {
		t.Fatalf("disabled check: status=%d body=%s", got.Code, got.Body.String())
	}
	if calls.Load() != before {
		t.Fatal("a disabled control plane reached Hugging Face")
	}
}

func TestIntentPlanningChecksUnreviewedModels(t *testing.T) {
	resolver, _ := recordedHub(t)
	handler := modelCheckAPI(t, resolver)
	response := call(handler, http.MethodPost, "/api/v1/planning/intents", `{"intent":"chat workload","model":"microsoft/phi-4"}`)
	var envelope intentplan.Envelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
	if envelope.Plan.Status != "ready" || envelope.Plan.Model.Review != intentplan.ReviewCheckedAutomatically || envelope.DeploymentDraft == nil || envelope.DeploymentDraft.ModelRevision != "2db69c1c3e91a05d2c64a3185acfbaf36f744e25" || envelope.Plan.ModelCheck == nil {
		t.Fatalf("unexpected plan: %s", response.Body.String())
	}
	if err := envelope.DeploymentDraft.Validate(); err != nil {
		t.Fatalf("draft does not validate: %v", err)
	}

	unsupported := call(handler, http.MethodPost, "/api/v1/planning/intents", `{"intent":"chat","model":"RWKV/rwkv-4-169m-pile"}`)
	if unsupported.Code != http.StatusOK || !strings.Contains(unsupported.Body.String(), `"status":"unsupported"`) || strings.Contains(unsupported.Body.String(), `"deployment_draft"`) {
		t.Fatalf("unsupported: status=%d body=%s", unsupported.Code, unsupported.Body.String())
	}
	gated := call(handler, http.MethodPost, "/api/v1/planning/intents", `{"intent":"chat","model":"meta-llama/Llama-3.2-1B-Instruct"}`)
	if !strings.Contains(gated.Body.String(), `"field":"model_secret_reference_id"`) || strings.Contains(gated.Body.String(), recordedToken) {
		t.Fatalf("gated without token: %s", gated.Body.String())
	}
	withToken := call(handler, http.MethodPost, "/api/v1/planning/intents", `{"intent":"chat","model":"meta-llama/Llama-3.2-1B-Instruct","model_secret_reference_id":"secret"}`)
	if !strings.Contains(withToken.Body.String(), `"status":"ready"`) || !strings.Contains(withToken.Body.String(), `"model_secret_reference_id":"secret"`) || strings.Contains(withToken.Body.String(), recordedToken) {
		t.Fatalf("gated with token: %s", withToken.Body.String())
	}
	if unknown := call(handler, http.MethodPost, "/api/v1/planning/intents", `{"intent":"chat","model":"meta-llama/Llama-3.2-1B-Instruct","model_secret_reference_id":"other"}`); unknown.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown credential: status=%d body=%s", unknown.Code, unknown.Body.String())
	}
	missing := call(handler, http.MethodPost, "/api/v1/planning/intents", `{"intent":"chat","model":"acme/missing"}`)
	if missing.Code != http.StatusOK || !strings.Contains(missing.Body.String(), "or it is private") {
		t.Fatalf("missing model: status=%d body=%s", missing.Code, missing.Body.String())
	}
	disabled := call(modelCheckAPI(t, nil), http.MethodPost, "/api/v1/planning/intents", `{"intent":"chat","model":"microsoft/phi-4"}`)
	if disabled.Code != http.StatusOK || !strings.Contains(disabled.Body.String(), "metadata access is turned off") || !strings.Contains(disabled.Body.String(), `"status":"needs_input"`) {
		t.Fatalf("disabled: status=%d body=%s", disabled.Code, disabled.Body.String())
	}
	// A reviewed model never reaches Hugging Face.
	reviewed := call(modelCheckAPI(t, nil), http.MethodPost, "/api/v1/planning/intents", `{"intent":"chat","model":"Qwen/Qwen3-8B"}`)
	if !strings.Contains(reviewed.Body.String(), `"review":"reviewed"`) || strings.Contains(reviewed.Body.String(), "model_check") {
		t.Fatalf("reviewed: %s", reviewed.Body.String())
	}
}

func TestOptimizationProposalPinsUnreviewedModels(t *testing.T) {
	resolver, _ := recordedHub(t)
	handler := modelCheckAPI(t, resolver)
	response := call(handler, http.MethodPost, "/api/v1/optimization/proposals", `{"model_identity":"microsoft/phi-4","provider":"runpod","gpu":"L40S","objective":"interactive","max_candidates":3}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Proposal struct {
			Input struct {
				ModelRevision string `json:"model_revision"`
				MaxModelLen   int    `json:"max_model_len"`
			} `json:"input"`
			Candidates []struct {
				Deployment struct {
					Runtime struct {
						Args []string `json:"args"`
					} `json:"runtime"`
				} `json:"deployment"`
			} `json:"candidates"`
		} `json:"proposal"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Proposal.Input.ModelRevision != "2db69c1c3e91a05d2c64a3185acfbaf36f744e25" || body.Proposal.Input.MaxModelLen != modelcheck.DefaultContextTokens || len(body.Proposal.Candidates) == 0 {
		t.Fatalf("unexpected proposal: %s", response.Body.String())
	}
	for _, candidate := range body.Proposal.Candidates {
		if strings.Join(candidate.Deployment.Runtime.Args, " ") != "--max-model-len 8192" && strings.Join(candidate.Deployment.Runtime.Args, " ") != "--context-length 8192" {
			t.Fatalf("baseline did not bound its context: %+v", candidate.Deployment.Runtime.Args)
		}
	}
	unsupported := call(handler, http.MethodPost, "/api/v1/optimization/proposals", `{"model_identity":"RWKV/rwkv-4-169m-pile","provider":"runpod","gpu":"L40S","objective":"interactive","max_candidates":3}`)
	if unsupported.Code != http.StatusUnprocessableEntity || !strings.Contains(unsupported.Body.String(), "model_unsupported") {
		t.Fatalf("unsupported: status=%d body=%s", unsupported.Code, unsupported.Body.String())
	}
	// Without metadata access a pinned commit proceeds exactly as before.
	pinned := call(modelCheckAPI(t, nil), http.MethodPost, "/api/v1/optimization/proposals", `{"model_identity":"microsoft/phi-4","model_revision":"2db69c1c3e91a05d2c64a3185acfbaf36f744e25","provider":"runpod","gpu":"L40S","objective":"interactive","max_candidates":3}`)
	if pinned.Code != http.StatusOK || strings.Contains(pinned.Body.String(), "max_model_len") {
		t.Fatalf("pinned without access: status=%d body=%s", pinned.Code, pinned.Body.String())
	}
}
