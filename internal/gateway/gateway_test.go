package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/routes"
)

type fakeAuthenticator struct{ principal domain.Principal }

func (f fakeAuthenticator) AuthenticatePrincipal(context.Context, string) (domain.Principal, error) {
	return f.principal, nil
}

type roundTripper func(*http.Request) (*http.Response, error)

func (fn roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

type captureRecorder struct {
	mu     sync.Mutex
	record domain.InferenceRecord
}

func (c *captureRecorder) RecordRequest(_ context.Context, record domain.InferenceRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record = record
	return nil
}

func TestCompletionRewritesAlias(t *testing.T) {
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if !validTraceParent.MatchString(r.Header.Get("traceparent")) {
			t.Errorf("invalid traceparent %q", r.Header.Get("traceparent"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "upstream-model" {
			t.Errorf("model = %v", body["model"])
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "upstream-model", RouterURL: "http://router"})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client}).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	req.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != 200 {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !validTraceParent.MatchString(recorder.Header().Get("traceparent")) {
		t.Fatalf("response traceparent=%q", recorder.Header().Get("traceparent"))
	}
}

func TestCompletionRecordsStreamingTelemetry(t *testing.T) {
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		body := "data: {\"model\":\"Qwen/Qwen3-8B\",\"choices\":[]}\n\n" +
			"data: {\"model\":\"Qwen/Qwen3-8B\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":7}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", RevisionID: "rev-3", Alias: "alias", UpstreamModel: "Qwen/Qwen3-8B", RouterURL: "http://router", Provider: "runpod", Runtime: "vllm", ComputeMode: "elastic"})
	captured := &captureRecorder{}
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client, Recorder: captured}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true,"messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	captured.mu.Lock()
	record := captured.record
	captured.mu.Unlock()
	if record.DeploymentID != "d1" || record.RevisionID != "rev-3" || record.Provider != "runpod" || record.Runtime != "vllm" || record.ComputeMode != "elastic" || record.OperationName != "chat" || record.RequestModel != "alias" || record.SemanticConventionSchema != "https://opentelemetry.io/schemas/gen-ai/1.42.0" {
		t.Fatalf("metadata=%+v", record)
	}
	if !record.Streaming || record.TTFTMS == nil || *record.TTFTMS < 0 || record.LatencyMS < *record.TTFTMS {
		t.Fatalf("timings=%+v", record)
	}
	if record.InputTokens == nil || *record.InputTokens != 12 || record.OutputTokens == nil || *record.OutputTokens != 7 || record.ResponseModel != "Qwen/Qwen3-8B" {
		t.Fatalf("usage=%+v", record)
	}
	if record.StartedAt.After(time.Now()) || record.ErrorType != "" {
		t.Fatalf("record=%+v", record)
	}
}

func TestCompletionReplacesPublicCredentialForServerlessUpstream(t *testing.T) {
	client := &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("Authorization") != "Bearer runpod-secret" {
			t.Fatalf("upstream authorization=%q", request.Header.Get("Authorization"))
		}
		if request.URL.String() != "https://api.runpod.invalid/v2/endpoint/openai/v1/chat/completions" {
			t.Fatalf("upstream URL=%s", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[]}`))}, nil
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "Qwen/Qwen3-8B", RouterURL: "https://api.runpod.invalid/v2/endpoint/openai", Provider: "runpod", Runtime: "vllm", ComputeMode: "serverless", UpstreamAPIKey: "runpod-secret"})
	handler := (&Gateway{Routes: directory, APIKey: "infercrane-secret", Client: client}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	request.Header.Set("Authorization", "Bearer infercrane-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServerlessColdEvidenceClassifiesOnlyTriggeringRequest(t *testing.T) {
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"model":"Qwen/Qwen3-8B","choices":[]}`))}, nil
	})}
	workers := 0
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", RevisionID: "rev-1", Alias: "alias", UpstreamModel: "Qwen/Qwen3-8B", RouterURL: "https://api.runpod.invalid/openai", Provider: "runpod", Runtime: "vllm", ComputeMode: "serverless", UpstreamAPIKey: "runpod-secret", ProviderWorkers: &workers, ProviderObservedAt: time.Now()})
	captured := &captureRecorder{}
	handler := (&Gateway{Routes: directory, APIKey: "infercrane-secret", Client: client, Recorder: captured}).Handler()
	send := func() {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
		request.Header.Set("Authorization", "Bearer infercrane-secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	send()
	captured.mu.Lock()
	first := captured.record
	captured.mu.Unlock()
	if first.ColdStart == nil || !*first.ColdStart || first.ProviderWorkersAtArrival == nil || *first.ProviderWorkersAtArrival != 0 || first.ProviderCapacityObservedAt == nil {
		t.Fatalf("first=%+v", first)
	}
	published, _ := directory.Get("alias")
	if published.ProviderWorkers != nil || !published.ProviderObservedAt.IsZero() {
		t.Fatalf("cold evidence was not invalidated: %+v", published)
	}
	send()
	captured.mu.Lock()
	second := captured.record
	captured.mu.Unlock()
	if second.ColdStart != nil || second.ProviderWorkersAtArrival != nil {
		t.Fatalf("second request reused cold evidence: %+v", second)
	}
}

func TestServerlessNonzeroWorkerEvidenceClassifiesWarmRequest(t *testing.T) {
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[]}`))}, nil
	})}
	workers := 1
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "model", RouterURL: "https://api.runpod.invalid/openai", Provider: "runpod", Runtime: "vllm", ComputeMode: "serverless", ProviderWorkers: &workers, ProviderObservedAt: time.Now()})
	captured := &captureRecorder{}
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client, Recorder: captured}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	captured.mu.Lock()
	record := captured.record
	captured.mu.Unlock()
	if response.Code != http.StatusOK || record.ColdStart == nil || *record.ColdStart || record.ProviderWorkersAtArrival == nil || *record.ProviderWorkersAtArrival != 1 {
		t.Fatalf("status=%d record=%+v", response.Code, record)
	}
}

func TestServerlessFreshCapacityOverridesStaleRouteSnapshot(t *testing.T) {
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"choices":[]}`))}, nil
	})}
	staleWorkers := 2
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "model", RouterURL: "https://api.runpod.invalid/openai", Provider: "runpod", ProviderResourceID: "endpoint-1", Runtime: "vllm", ComputeMode: "serverless", ProviderWorkers: &staleWorkers, ProviderObservedAt: time.Now()})
	captured := &captureRecorder{}
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client, Recorder: captured, CapacityObservers: map[string]CapacityObserver{"runpod": func(context.Context, string) (int, error) { return 0, nil }}}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	captured.mu.Lock()
	record := captured.record
	captured.mu.Unlock()
	if response.Code != http.StatusOK || record.ColdStart == nil || !*record.ColdStart || record.ProviderWorkersAtArrival == nil || *record.ProviderWorkersAtArrival != 0 {
		t.Fatalf("status=%d record=%+v", response.Code, record)
	}
}

func TestCopyResponseStopsAtTerminalSSEMarker(t *testing.T) {
	response := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))}
	recorder := httptest.NewRecorder()
	observation := responseObservation{}
	if err := copyResponse(recorder, response, &observation); err != nil || !hasSSEDone(observation.body) {
		t.Fatalf("body=%q err=%v", observation.body, err)
	}
}

func TestAuthentication(t *testing.T) {
	handler := (&Gateway{Routes: routes.New(), APIKey: "secret"}).Handler()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
}

func TestModelsAreTenantScoped(t *testing.T) {
	directory := routes.New()
	directory.Put(routes.Snapshot{TenantID: "tenant-a", Alias: "shared"})
	directory.Put(routes.Snapshot{TenantID: "tenant-b", Alias: "private"})
	handler := (&Gateway{Routes: directory, Authenticator: fakeAuthenticator{principal: domain.Principal{ID: "p", TenantID: "tenant-a", Role: "viewer"}}}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer scoped")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "shared") || strings.Contains(response.Body.String(), "private") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestActiveStreamKeepsSelectedRouterAcrossGenerationPublish(t *testing.T) {
	selected := make(chan string, 1)
	release := make(chan struct{})
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		selected <- r.URL.Host
		<-release
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: first\n\ndata: [DONE]\n\n"))}, nil
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "model", RouterURL: "http://router-g1", RouterProcessID: "d1-g1"})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true,"messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()
	if host := <-selected; host != "router-g1" {
		t.Fatalf("selected router=%s", host)
	}
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "model", RouterURL: "http://router-g2", RouterProcessID: "d1-g2"})
	close(release)
	<-done
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "[DONE]") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}
