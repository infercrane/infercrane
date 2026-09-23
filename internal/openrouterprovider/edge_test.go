package openrouterprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testCatalog() *Catalog {
	return &Catalog{Data: []Model{{
		SchemaVersion: "2.4", ID: "qwen/qwen3.8-27b", Name: "Qwen3.8", HuggingFaceID: "Qwen/Qwen3.8-27B-FP8", Created: 1,
		InputModalities: []InputModality{{Type: "text"}}, OutputModalities: []OutputModality{{Type: "text", SupportedParameters: map[string]any{}}},
		OpenRouter: OpenRouterIdentity{Slug: "qwen/qwen3.8-27b"},
	}}}
}

func TestEdgeAuthenticatesRewritesAndStreams(t *testing.T) {
	var seenModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		seenModel, _ = payload["model"].(string)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	edge := &Edge{Catalog: testCatalog(), PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "Qwen/Qwen3.8-27B-FP8", APIKey: "provider-secret", UpstreamURL: upstream.URL, MaxInFlight: 1}
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
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen/qwen3.8-27b","stream":true,"messages":[]}`))
	request.Header.Set("Authorization", "Bearer provider-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || seenModel != "Qwen/Qwen3.8-27B-FP8" || !strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("unexpected proxy response code=%d model=%q body=%q", recorder.Code, seenModel, recorder.Body.String())
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
	edge := &Edge{Catalog: testCatalog(), PublicModel: "qwen/qwen3.8-27b", UpstreamModel: "upstream", APIKey: "secret", UpstreamURL: upstream.URL, MaxInFlight: 1}
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
