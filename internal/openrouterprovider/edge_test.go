package openrouterprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryReceiptRecorder struct {
	mu       sync.Mutex
	receipts []RequestReceipt
}

func (r *memoryReceiptRecorder) Record(receipt RequestReceipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipts = append(r.receipts, receipt)
	return nil
}

func (r *memoryReceiptRecorder) Lookup(channel string, requestIDs []string) ([]RequestCost, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	requested := make(map[string]struct{}, len(requestIDs))
	for _, requestID := range requestIDs {
		requested[requestID] = struct{}{}
	}
	result := make([]RequestCost, 0, len(requestIDs))
	for _, receipt := range r.receipts {
		if receipt.Channel != channel || receipt.StatusCode < 200 || receipt.StatusCode >= 400 || receipt.Outcome != "completed" {
			continue
		}
		if _, found := requested[receipt.RequestID]; found {
			result = append(result, RequestCost{RequestID: receipt.RequestID, CostNanoUSD: receipt.CostNanoUSD})
		}
	}
	return result, nil
}

func (r *memoryReceiptRecorder) last(t *testing.T) RequestReceipt {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.receipts) == 0 {
		t.Fatal("expected a request receipt")
	}
	return r.receipts[len(r.receipts)-1]
}

func (r *memoryReceiptRecorder) findOutcome(t *testing.T, outcome string) RequestReceipt {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, receipt := range r.receipts {
		if receipt.Outcome == outcome {
			return receipt
		}
	}
	t.Fatalf("expected a request receipt with outcome %q", outcome)
	return RequestReceipt{}
}

func testCatalog() *Catalog {
	return &Catalog{Data: []Model{{
		SchemaVersion: "2.5", ID: "qwen/qwen3.8-27b", Name: "Qwen3.8", HuggingFaceID: "Qwen/Qwen3.8-27B-FP8", Created: 1,
		InputModalities: []InputModality{{Type: "text"}}, OutputModalities: []OutputModality{{Type: "text", SupportedParameters: map[string]any{}}},
		OpenRouter: OpenRouterIdentity{Slug: "qwen/qwen3.8-27b"},
	}}}
}

func TestEdgeAuthenticatesRewritesAndStreams(t *testing.T) {
	var seenModel string
	var seenThinking bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		seenModel, _ = payload["model"].(string)
		kwargs, _ := payload["chat_template_kwargs"].(map[string]any)
		seenThinking, _ = kwargs["enable_thinking"].(bool)
		if _, present := payload["reasoning"]; present {
			t.Error("OpenRouter reasoning control reached strict upstream")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"secret response\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7,\"total_tokens\":18}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	receipts := &memoryReceiptRecorder{}
	edge := &Edge{Catalog: testCatalog(), PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "Qwen/Qwen3.8-27B-FP8", APIKey: "provider-secret", UpstreamURL: upstream.URL, MaxInFlight: 1, ReceiptRecorder: receipts}
	handler, err := edge.Handler()
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/models", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized, got %d", unauthorized.Code)
	}
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRequest.Header.Set("Authorization", "Bearer provider-secret")
	models := httptest.NewRecorder()
	handler.ServeHTTP(models, modelsRequest)
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), "qwen/qwen3.8-27b") {
		t.Fatalf("unexpected models response code=%d body=%q", models.Code, models.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3.8-27b","stream":true,"messages":[],"reasoning":{"effort":"high"},"include_reasoning":true}`))
	request.Header.Set("Authorization", "Bearer provider-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || seenModel != "Qwen/Qwen3.8-27B-FP8" || !seenThinking || !strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("unexpected proxy response code=%d model=%q thinking=%v body=%q", recorder.Code, seenModel, seenThinking, recorder.Body.String())
	}
	receipt := receipts.last(t)
	if receipt.PromptTokens != 11 || receipt.CompletionTokens != 7 || receipt.TotalTokens != 18 || receipt.FinishReason != "stop" || receipt.Outcome != "completed" || !receipt.Stream {
		t.Fatalf("unexpected request receipt: %+v", receipt)
	}
}

func TestEdgeRecordsBufferedUsageWithoutContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"do not retain me"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`+"\n")
	}))
	defer upstream.Close()
	receipts := &memoryReceiptRecorder{}
	edge := &Edge{Catalog: testCatalog(), PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "upstream", APIKey: "secret", UpstreamURL: upstream.URL, MaxInFlight: 1, ReceiptRecorder: receipts}
	handler, err := edge.Handler()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3.8-27b","messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "do not retain me") || !strings.HasSuffix(recorder.Body.String(), "\n") {
		t.Fatalf("unexpected buffered response: %d %q", recorder.Code, recorder.Body.String())
	}
	receipt := receipts.last(t)
	encoded, _ := json.Marshal(receipt)
	if receipt.PromptTokens != 5 || receipt.CompletionTokens != 3 || receipt.TotalTokens != 8 || receipt.FinishReason != "stop" || bytesContain(encoded, []byte("do not retain me")) {
		t.Fatalf("unexpected content-free receipt: %s", encoded)
	}
}

func TestHuggingFaceModelsRequestIdentityAndBilling(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	}))
	defer upstream.Close()
	catalog := validCatalog()
	catalog.Data[0].InputModalities[0].SupportedInputs = map[string]any{"max_context_length": map[string]any{"value": 32768}}
	receipts := &memoryReceiptRecorder{}
	edge := &Edge{
		Catalog: &catalog, PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "upstream", UpstreamURL: upstream.URL, MaxInFlight: 4,
		Credentials: []ChannelCredential{
			{Channel: ChannelOpenRouter, APIKey: "openrouter-secret"},
			{Channel: ChannelHuggingFace, APIKey: "huggingface-secret"},
		},
		ChannelPolicies: map[string]ChannelPolicy{
			ChannelHuggingFace: {MaxInFlight: 2, InputPricePerMillionUSD: 0.10, OutputPricePerMillionUSD: 2.20, BillingLookupEnabled: true},
		},
		ReceiptRecorder: receipts,
	}
	handler, err := edge.Handler()
	if err != nil {
		t.Fatal(err)
	}
	modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRequest.Header.Set("Authorization", "Bearer huggingface-secret")
	models := httptest.NewRecorder()
	handler.ServeHTTP(models, modelsRequest)
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), `"owned_by":"infercrane"`) || !strings.Contains(models.Body.String(), `"context_length":32768`) || !strings.Contains(models.Body.String(), `"output":2.2`) {
		t.Fatalf("unexpected Hugging Face model response code=%d body=%s", models.Code, models.Body.String())
	}

	completionRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3.8-27b","messages":[]}`))
	completionRequest.Header.Set("Authorization", "Bearer huggingface-secret")
	completion := httptest.NewRecorder()
	handler.ServeHTTP(completion, completionRequest)
	requestID := completion.Header().Get("Inference-Id")
	if completion.Code != http.StatusOK || requestID == "" || completion.Header().Get("X-Request-ID") != requestID {
		t.Fatalf("unexpected completion identity code=%d inference_id=%q request_id=%q", completion.Code, requestID, completion.Header().Get("X-Request-ID"))
	}

	billingRequest := httptest.NewRequest(http.MethodPost, "/v1/billing/requests", strings.NewReader(`{"requestIds":["`+requestID+`","unknown"]}`))
	billingRequest.Header.Set("Authorization", "Bearer huggingface-secret")
	billing := httptest.NewRecorder()
	handler.ServeHTTP(billing, billingRequest)
	if billing.Code != http.StatusOK || !strings.Contains(billing.Body.String(), `"requestId":"`+requestID+`"`) || !strings.Contains(billing.Body.String(), `"costNanoUsd":7100`) || strings.Contains(billing.Body.String(), "unknown") {
		t.Fatalf("unexpected billing response code=%d body=%s", billing.Code, billing.Body.String())
	}

	openRouterBilling := httptest.NewRequest(http.MethodPost, "/v1/billing/requests", strings.NewReader(`{"requestIds":["`+requestID+`"]}`))
	openRouterBilling.Header.Set("Authorization", "Bearer openrouter-secret")
	forbidden := httptest.NewRecorder()
	handler.ServeHTTP(forbidden, openRouterBilling)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("expected OpenRouter billing lookup to fail closed, got %d", forbidden.Code)
	}
}

func TestGatewayMarketplaceChannelsUseScopedCredentialsPoliciesAndReceipts(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	}))
	defer upstream.Close()

	for _, test := range []struct {
		channel     string
		credential  string
		inputPrice  float64
		outputPrice float64
		costNanoUSD int64
	}{
		{channel: ChannelRequesty, credential: "requesty-secret", inputPrice: 0.11, outputPrice: 2.30, costNanoUSD: 7_450},
		{channel: ChannelVercel, credential: "vercel-secret", inputPrice: 0.12, outputPrice: 2.40, costNanoUSD: 7_800},
	} {
		t.Run(test.channel, func(t *testing.T) {
			receipts := &memoryReceiptRecorder{}
			edge := &Edge{
				Catalog: testCatalog(), PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "upstream", UpstreamURL: upstream.URL, MaxInFlight: 4,
				Credentials: []ChannelCredential{
					{Channel: ChannelOpenRouter, APIKey: "openrouter-secret"},
					{Channel: test.channel, APIKey: test.credential},
				},
				ChannelPolicies: map[string]ChannelPolicy{
					test.channel: {MaxInFlight: 2, InputPricePerMillionUSD: test.inputPrice, OutputPricePerMillionUSD: test.outputPrice},
				},
				ReceiptRecorder: receipts,
			}
			handler, err := edge.Handler()
			if err != nil {
				t.Fatal(err)
			}

			modelsRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			modelsRequest.Header.Set("Authorization", "Bearer "+test.credential)
			models := httptest.NewRecorder()
			handler.ServeHTTP(models, modelsRequest)
			if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), fmt.Sprintf(`"input":%g`, test.inputPrice)) || !strings.Contains(models.Body.String(), fmt.Sprintf(`"output":%g`, test.outputPrice)) {
				t.Fatalf("unexpected %s models response code=%d body=%s", test.channel, models.Code, models.Body.String())
			}

			completionRequest := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3.8-27b","messages":[]}`))
			completionRequest.Header.Set("Authorization", "Bearer "+test.credential)
			completion := httptest.NewRecorder()
			handler.ServeHTTP(completion, completionRequest)
			if completion.Code != http.StatusOK || completion.Header().Get("X-Request-ID") == "" || completion.Header().Get("Inference-Id") == "" {
				t.Fatalf("unexpected %s completion response code=%d headers=%v", test.channel, completion.Code, completion.Header())
			}
			receipt := receipts.last(t)
			if receipt.Channel != test.channel || receipt.CostNanoUSD != test.costNanoUSD || receipt.Outcome != "completed" {
				t.Fatalf("unexpected %s receipt: %+v", test.channel, receipt)
			}

			billingRequest := httptest.NewRequest(http.MethodPost, "/v1/billing/requests", strings.NewReader(`{"requestIds":["`+receipt.RequestID+`"]}`))
			billingRequest.Header.Set("Authorization", "Bearer "+test.credential)
			billing := httptest.NewRecorder()
			handler.ServeHTTP(billing, billingRequest)
			if billing.Code != http.StatusForbidden {
				t.Fatalf("expected %s billing lookup to fail closed, got %d", test.channel, billing.Code)
			}
		})
	}
}

func TestTranslateReasoningRequestCanDisableThinking(t *testing.T) {
	payload := map[string]any{
		"reasoning":            map[string]any{"enabled": false},
		"chat_template_kwargs": map[string]any{"custom": "kept"},
	}
	translateReasoningRequest(payload)
	kwargs := payload["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != false || kwargs["preserve_thinking"] != false || kwargs["custom"] != "kept" {
		t.Fatalf("unexpected translated kwargs: %#v", kwargs)
	}
}

func TestTranslateReasoningEffortPreservesQwenNativeLevel(t *testing.T) {
	for _, effort := range []string{"low", "medium", "xhigh"} {
		t.Run(effort, func(t *testing.T) {
			payload := map[string]any{"reasoning_effort": effort}
			translateReasoningRequest(payload)
			kwargs := payload["chat_template_kwargs"].(map[string]any)
			if kwargs["enable_thinking"] != true || kwargs["preserve_thinking"] != true || kwargs["reasoning_effort"] != effort {
				t.Fatalf("unexpected translated kwargs: %#v", kwargs)
			}
			if _, found := payload["reasoning_effort"]; found {
				t.Fatal("provider-only reasoning_effort reached upstream")
			}
		})
	}
}

func TestTranslateReasoningEffortNoneDisablesThinking(t *testing.T) {
	payload := map[string]any{"reasoning_effort": "none"}
	translateReasoningRequest(payload)
	kwargs := payload["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != false || kwargs["preserve_thinking"] != false {
		t.Fatalf("unexpected translated kwargs: %#v", kwargs)
	}
	if _, found := kwargs["reasoning_effort"]; found {
		t.Fatalf("disabled reasoning retained an effort: %#v", kwargs)
	}
}

func TestValidateTextOnlyMessages(t *testing.T) {
	text := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}}},
	}}
	if err := validateTextOnlyMessages(text); err != nil {
		t.Fatalf("text request rejected: %v", err)
	}

	for _, kind := range []string{"image_url", "input_image", "video_url", "file"} {
		t.Run(kind, func(t *testing.T) {
			payload := map[string]any{"messages": []any{map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": kind}},
			}}}
			if err := validateTextOnlyMessages(payload); err == nil {
				t.Fatalf("%s input was not rejected", kind)
			}
		})
	}
}

func TestEdgeSendsSSEKeepAliveWhileUpstreamIsQuiet(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	edge := &Edge{
		Catalog: testCatalog(), PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "upstream",
		APIKey: "secret", UpstreamURL: upstream.URL, MaxInFlight: 1, StreamKeepAliveInterval: 5 * time.Millisecond,
	}
	handler, err := edge.Handler()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3.8-27b","stream":true}`))
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(recorder, request)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-done
	if !strings.Contains(recorder.Body.String(), ": keep-alive\n\n") || !strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("expected heartbeat and completion, got %q", recorder.Body.String())
	}
}

func TestEdgeRejectsUnknownModelAndMalformedJSON(t *testing.T) {
	edge := &Edge{Catalog: testCatalog(), PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "upstream", APIKey: "secret", UpstreamURL: "http://127.0.0.1:30000", MaxInFlight: 1}
	handler, err := edge.Handler()
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"model":"other","messages":[]}`, `{bad`} {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code < 400 || recorder.Code >= 500 {
			t.Fatalf("expected client error for %q, got %d", body, recorder.Code)
		}
	}
}

func TestEdgeReturnsImmediate429AtCapacity(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(started)
		<-release
		_, _ = io.WriteString(w, `{}`)
	}))
	defer upstream.Close()
	receipts := &memoryReceiptRecorder{}
	edge := &Edge{Catalog: testCatalog(), PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "upstream", APIKey: "secret", UpstreamURL: upstream.URL, MaxInFlight: 1, ReceiptRecorder: receipts}
	handler, err := edge.Handler()
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3.8-27b"}`))
		request.Header.Set("Authorization", "Bearer secret")
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(firstDone)
	}()
	<-started
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3.8-27b"}`))
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	startedAt := time.Now()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests || time.Since(startedAt) > 100*time.Millisecond || recorder.Header().Get("Retry-After") == "" {
		t.Fatalf("expected immediate 429, got code=%d elapsed=%s", recorder.Code, time.Since(startedAt))
	}
	close(release)
	<-firstDone
	if calls.Load() != 1 {
		t.Fatalf("expected one upstream call, got %d", calls.Load())
	}
	if receipt := receipts.findOutcome(t, "capacity_rejected"); receipt.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("unexpected capacity receipt: %+v", receipt)
	}
}

func TestAdaptiveAdmissionDecreasesOnTTFTMissAndRecoversWhenSaturated(t *testing.T) {
	controller, err := newAdmissionController(AdmissionConfig{
		MinLimit: 2, InitialLimit: 4, MaxLimit: 5, Window: 4,
		TargetTTFT: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if controller.tryAcquire(1) != admissionAccepted {
			t.Fatal("expected initial admission")
		}
		controller.releasePrefill(1)
		controller.complete(admissionObservation{StatusCode: 200, Outcome: "completed", TTFT: 250 * time.Millisecond, CompletionTokens: 10})
	}
	if got := controller.snapshot().limit; got != 3 {
		t.Fatalf("multiplicative decrease limit=%d, want 3", got)
	}
	for range 4 {
		if controller.tryAcquire(1) != admissionAccepted {
			t.Fatal("expected recovery admission")
		}
		controller.releasePrefill(1)
		controller.mu.Lock()
		controller.windowSaturated = true
		controller.mu.Unlock()
		controller.complete(admissionObservation{StatusCode: 200, Outcome: "completed", TTFT: 50 * time.Millisecond, CompletionTokens: 10})
	}
	if got := controller.snapshot().limit; got != 4 {
		t.Fatalf("additive recovery limit=%d, want 4", got)
	}
}

func TestAdaptiveAdmissionDoesNotTreatBufferedSuccessAsTTFTMiss(t *testing.T) {
	controller, err := newAdmissionController(AdmissionConfig{
		MinLimit: 2, InitialLimit: 4, MaxLimit: 5, Window: 4,
		TargetTTFT: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if controller.tryAcquire(1) != admissionAccepted {
			t.Fatal("expected buffered request admission")
		}
		controller.releasePrefill(1)
		controller.complete(admissionObservation{StatusCode: 200, Outcome: "completed", CompletionTokens: 10})
	}
	snapshot := controller.snapshot()
	if snapshot.sloMissed != 0 {
		t.Fatalf("buffered successes produced %d TTFT misses, want 0", snapshot.sloMissed)
	}
	if snapshot.productiveOutputTokens != 0 {
		t.Fatalf("buffered output was counted as TTFT-qualified: %d", snapshot.productiveOutputTokens)
	}
}

func TestAdaptiveAdmissionKeepsClientCancellationOutOfFailureWindow(t *testing.T) {
	controller, err := newAdmissionController(AdmissionConfig{
		MinLimit: 2, InitialLimit: 4, MaxLimit: 5, Window: 4,
		TargetTTFT: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if controller.tryAcquire(1) != admissionAccepted {
		t.Fatal("expected cancellation test admission")
	}
	controller.releasePrefill(1)
	controller.complete(admissionObservation{StatusCode: 200, Outcome: "client_canceled", Duration: 50 * time.Millisecond})

	snapshot := controller.snapshot()
	if snapshot.completed != 1 || snapshot.canceled != 1 || snapshot.failed != 0 {
		t.Fatalf("unexpected cancellation counters: %+v", snapshot)
	}
	if controller.windowCompleted != 0 || controller.windowFailures != 0 || snapshot.limit != 4 {
		t.Fatalf("client cancellation affected adaptive capacity: window_completed=%d window_failures=%d limit=%d", controller.windowCompleted, controller.windowFailures, snapshot.limit)
	}
}

func TestAdmissionProtectsPrefillSeparatelyFromDecode(t *testing.T) {
	controller, err := newAdmissionController(AdmissionConfig{
		MinLimit: 2, InitialLimit: 2, MaxLimit: 2, Window: 4,
		MaxPrefillTokensInFlight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision := controller.tryAcquire(70); decision != admissionAccepted {
		t.Fatalf("first request decision=%q", decision)
	}
	if decision := controller.tryAcquire(40); decision != admissionPrefillRejected {
		t.Fatalf("overlapping long prefill decision=%q", decision)
	}
	controller.releasePrefill(70)
	if decision := controller.tryAcquire(40); decision != admissionAccepted {
		t.Fatalf("decode-overlapped request decision=%q", decision)
	}
	snapshot := controller.snapshot()
	if snapshot.prefillTokensInFlight != 40 || snapshot.inFlight != 2 || snapshot.prefillRejected != 1 {
		t.Fatalf("unexpected prefill snapshot: %+v", snapshot)
	}
	controller.releasePrefill(40)
	controller.complete(admissionObservation{StatusCode: 200, Outcome: "completed"})
	controller.complete(admissionObservation{StatusCode: 200, Outcome: "completed"})
}

func TestEdgeReleasesPrefillBudgetAtFirstStreamToken(t *testing.T) {
	firstStreaming := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ready\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		if call == 1 {
			close(firstStreaming)
			<-releaseFirst
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":60,\"completion_tokens\":1,\"total_tokens\":61}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	edge := &Edge{
		Catalog: testCatalog(), PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "upstream",
		APIKey: "secret", UpstreamURL: upstream.URL, MaxInFlight: 2, MaxPrefillTokensInFlight: 100,
	}
	handler, err := edge.Handler()
	if err != nil {
		t.Fatal(err)
	}
	body := `{"model":"qwen/qwen3.8-27b","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", 180) + `"}]}`
	firstDone := make(chan struct{})
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(firstDone)
	}()
	<-firstStreaming
	deadline := time.Now().Add(time.Second)
	for edge.admission.snapshot().prefillTokensInFlight != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	second := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(second, request)
	if second.Code != http.StatusOK {
		t.Fatalf("decode-overlapped prefill returned %d: %s", second.Code, second.Body.String())
	}
	close(releaseFirst)
	<-firstDone
	if calls.Load() != 2 {
		t.Fatalf("expected two upstream calls, got %d", calls.Load())
	}
}

func TestEstimatePromptTokensDoesNotRetainOrMarshalContent(t *testing.T) {
	payload := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "123456"},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "abcdef"}}},
	}, "tools": []any{map[string]any{"name": "read_file"}}}
	if got := estimatePromptTokens(payload); got != 12 {
		// The estimator includes role/type strings as a conservative boundary.
		t.Fatalf("estimated tokens=%d, want 12", got)
	}
}

func TestEdgeProtectsOperationalMetricsAndReportsProductiveUtilization(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ready\"},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	edge := &Edge{
		Catalog: testCatalog(), PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "upstream",
		APIKey: "secret", MetricsKey: "metrics-secret", UpstreamURL: upstream.URL,
		MaxInFlight: 2, TargetTTFT: time.Second, QualifiedOutputTokensPerSecond: 100,
		InputPricePerMillionUSD: 0.10, OutputPricePerMillionUSD: 2.20, GPUHourlyCostUSD: 1.68,
	}
	handler, err := edge.Handler()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3.8-27b","stream":true}`))
	request.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("metrics without dedicated key returned %d", unauthorized.Code)
	}
	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRequest.Header.Set("Authorization", "Bearer metrics-secret")
	metrics := httptest.NewRecorder()
	handler.ServeHTTP(metrics, metricsRequest)
	for _, expected := range []string{
		"infercrane_provider_productive_output_tokens_total 50",
		"infercrane_provider_revenue_usd_total",
		"infercrane_provider_productive_utilization_ratio",
		"infercrane_provider_admission_limit 2",
	} {
		if !strings.Contains(metrics.Body.String(), expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, metrics.Body.String())
		}
	}
}

func TestEdgeRejectsRevenuePriceThatDiffersFromDiscountedCatalog(t *testing.T) {
	catalog := validCatalog()
	catalog.Data[0].DiscountToUser = 0.19
	edge := &Edge{
		Catalog: &catalog, PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "upstream",
		APIKey: "secret", UpstreamURL: "http://127.0.0.1:30000", MaxInFlight: 1,
		InputPricePerMillionUSD: 0.11, OutputPricePerMillionUSD: 2.50,
	}
	if _, err := edge.Handler(); err == nil || !strings.Contains(err.Error(), "effective catalog price") {
		t.Fatalf("expected mismatched undiscounted price to fail, got %v", err)
	}
}

func TestJSONLReceiptRecorderUsesOwnerOnlyContentFreeRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts", "requests.ndjson")
	recorder, err := NewJSONLReceiptRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = recorder.Record(RequestReceipt{RequestID: "req-1", Model: "model", UpstreamModel: "upstream", StatusCode: 200, Outcome: "completed", PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if err = recorder.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 || !strings.Contains(string(body), receiptSchemaVersion) || strings.Contains(string(body), "messages") {
		t.Fatalf("unexpected receipt file mode=%o body=%q", info.Mode().Perm(), body)
	}
}

func bytesContain(body, value []byte) bool {
	return strings.Contains(string(body), string(value))
}

func TestEdgePropagatesCancellation(t *testing.T) {
	canceled := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		close(canceled)
		return nil, request.Context().Err()
	})}
	edge := &Edge{Catalog: testCatalog(), PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "upstream", APIKey: "secret", UpstreamURL: "http://127.0.0.1:30000", MaxInFlight: 1, Client: client}
	handler, err := edge.Handler()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3.8-27b"}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer secret")
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("upstream context was not canceled")
	}
	<-done
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
