package openrouterprovider

import (
	"context"
	"encoding/json"
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
		SchemaVersion: "2.4", ID: "qwen/qwen3.8-27b", Name: "Qwen3.8", HuggingFaceID: "Qwen/Qwen3.8-27B-FP8", Created: 1,
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

func TestTranslateReasoningEffortEnablesThinking(t *testing.T) {
	payload := map[string]any{"reasoning_effort": "high"}
	translateReasoningRequest(payload)
	kwargs := payload["chat_template_kwargs"].(map[string]any)
	if kwargs["enable_thinking"] != true {
		t.Fatalf("unexpected translated kwargs: %#v", kwargs)
	}
	if _, found := payload["reasoning_effort"]; found {
		t.Fatal("provider-only reasoning_effort reached upstream")
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

func TestJSONLReceiptRecorderUsesOwnerOnlyContentFreeRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts", "requests.ndjson")
	recorder, err := NewJSONLReceiptRecorder(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = recorder.Record(RequestReceipt{RequestID: "req-1", Model: "model", StatusCode: 200, Outcome: "completed", PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}); err != nil {
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
