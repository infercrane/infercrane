package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/admission"
	"github.com/infercrane/infercrane/internal/contextpassport"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/routes"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

type fakeAuthenticator struct{ principal domain.Principal }

func (f fakeAuthenticator) AuthenticatePrincipal(context.Context, string) (domain.Principal, error) {
	return f.principal, nil
}

type roundTripper func(*http.Request) (*http.Response, error)

func (fn roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

type oneRequestBudget struct{ remaining int }

func (b *oneRequestBudget) Authorize(policy string) (domain.ExternalBudgetLease, error) {
	if policy == "policy" && b.remaining > 0 {
		b.remaining--
		return domain.ExternalBudgetLease{PolicyID: policy, Requests: 1, ReservedCostMicrousd: 100, MaxRequestCostMicrousd: 100}, nil
	}
	return domain.ExternalBudgetLease{}, errors.New("exhausted")
}

type denyRequestQuota struct{ tenant string }

func (d *denyRequestQuota) Authorize(tenant string) error {
	d.tenant = tenant
	return errors.New("exhausted")
}

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

func TestContextPassportAffinityHitsAndFallsBackWithoutDatabaseLookup(t *testing.T) {
	selected := make(chan string, 2)
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		selected <- r.URL.Host
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[]}`))}, nil
	})}
	directory := routes.New()
	directory.PublishEndpoint(routes.EndpointRoute{TenantID: "global", Alias: "coder", RoutingPolicy: "manual", Routes: []routes.Snapshot{
		{TenantID: "global", Alias: "coder", BindingID: "active", RouterURL: "http://active", UpstreamModel: "model"},
		{TenantID: "global", Alias: "coder", BindingID: "preferred", RouterURL: "http://preferred", UpstreamModel: "model"},
	}})
	passports := contextpassport.New()
	passports.Put(contextpassport.Hint{ID: "session", TenantID: "global", SubjectID: "coder", PreferredBindingID: "preferred", ExpiresAt: time.Now().Add(time.Hour)})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client, ContextPassports: passports}).Handler()
	invoke := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coder","messages":[]}`))
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("X-InferCrane-Context-Passport", "session")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	response := invoke()
	if host := <-selected; host != "preferred" {
		t.Fatalf("selected=%s", host)
	}
	if response.Header().Get("X-InferCrane-Affinity") != "hit" {
		t.Fatalf("affinity=%q", response.Header().Get("X-InferCrane-Affinity"))
	}
	directory.PublishEndpoint(routes.EndpointRoute{TenantID: "global", Alias: "coder", RoutingPolicy: "manual", Routes: []routes.Snapshot{{TenantID: "global", Alias: "coder", BindingID: "active", RouterURL: "http://active", UpstreamModel: "model"}}})
	response = invoke()
	if host := <-selected; host != "active" {
		t.Fatalf("fallback selected=%s", host)
	}
	if response.Header().Get("X-InferCrane-Affinity") != "fallback" {
		t.Fatalf("fallback affinity=%q", response.Header().Get("X-InferCrane-Affinity"))
	}
}

func TestReplayShapeHashesSessionAndPrefixWithoutContent(t *testing.T) {
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":12,"completion_tokens":3},"choices":[]}`))}, nil
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "model", RouterURL: "http://router"})
	recorder := &captureRecorder{}
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client, Recorder: recorder}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[{"role":"system","content":"private policy"},{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("X-InferCrane-Session-ID", "session-raw")
	request.Header.Set("X-InferCrane-Parent-Session-ID", "parent-raw")
	request.Header.Set("X-InferCrane-Tool-Pause-MS", "25.5")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	record := recorder.record
	if response.Code != 200 || len(record.SessionIDHash) != 64 || len(record.ParentSessionIDHash) != 64 || len(record.SharedPrefixHash) != 64 || record.ToolPauseMS == nil || *record.ToolPauseMS != 25.5 {
		t.Fatalf("status=%d record=%#v", response.Code, record)
	}
	encoded, _ := json.Marshal(record)
	if strings.Contains(string(encoded), "private policy") || strings.Contains(string(encoded), "session-raw") || strings.Contains(string(encoded), "parent-raw") {
		t.Fatalf("raw replay content persisted: %s", encoded)
	}
}

func TestCompletionEnforcesTenantQuotaBeforeUpstream(t *testing.T) {
	upstreamCalled := false
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		upstreamCalled = true
		return nil, errors.New("must not be called")
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{TenantID: "team", DeploymentID: "d1", Alias: "alias", UpstreamModel: "model", RouterURL: "http://router"})
	quota := &denyRequestQuota{}
	handler := (&Gateway{Routes: directory, Authenticator: fakeAuthenticator{principal: domain.Principal{ID: "p1", TenantID: "team", Role: "viewer", Scopes: []string{"read"}}}, Client: client, RequestAuthorizer: quota}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	request.Header.Set("Authorization", "Bearer tenant-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests || quota.tenant != "team" || upstreamCalled {
		t.Fatalf("status=%d tenant=%q upstream_called=%t body=%s", response.Code, quota.tenant, upstreamCalled, response.Body.String())
	}
}

func TestCompletionPreservesOpenAIParametersAndStructuredTools(t *testing.T) {
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "upstream-model" || body["temperature"] != 0.25 || body["max_tokens"] != float64(64) || body["tool_choice"] != "auto" {
			t.Fatalf("scalar parameters changed: %#v", body)
		}
		if _, ok := body["tools"].([]any); !ok {
			t.Fatalf("tools changed: %#v", body["tools"])
		}
		format, ok := body["response_format"].(map[string]any)
		if !ok || format["type"] != "json_schema" {
			t.Fatalf("response_format changed: %#v", body["response_format"])
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[]}`))}, nil
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "upstream-model", RouterURL: "http://router"})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client}).Handler()
	requestBody := `{"model":"alias","messages":[],"temperature":0.25,"max_tokens":64,"tool_choice":"auto","tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestQualifiedProtocolSurfacesPreservePayloads(t *testing.T) {
	for _, test := range []struct {
		path      string
		operation string
		body      string
	}{
		{path: "/v1/responses", operation: "responses", body: `{"model":"alias","input":"hello","reasoning":{"effort":"low"}}`},
		{path: "/v1/embeddings", operation: "embeddings", body: `{"model":"alias","input":["one","two"],"encoding_format":"float"}`},
		{path: "/v1/completions", operation: "completions", body: `{"model":"alias","prompt":"hello","logprobs":2}`},
		{path: "/v1/chat/completions/batch", operation: "batch", body: `{"model":"alias","messages":[[{"role":"user","content":"one"}],[{"role":"user","content":"two"}]]}`},
	} {
		t.Run(test.operation, func(t *testing.T) {
			client := &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != test.path {
					t.Fatalf("upstream path=%q want %q", request.URL.Path, test.path)
				}
				var payload map[string]any
				if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if payload["model"] != "upstream-model" {
					t.Fatalf("model=%v", payload["model"])
				}
				switch test.operation {
				case "responses":
					if payload["input"] != "hello" || payload["reasoning"].(map[string]any)["effort"] != "low" {
						t.Fatalf("responses payload changed: %#v", payload)
					}
				case "embeddings":
					if len(payload["input"].([]any)) != 2 || payload["encoding_format"] != "float" {
						t.Fatalf("embeddings payload changed: %#v", payload)
					}
				case "completions":
					if payload["prompt"] != "hello" || payload["logprobs"] != float64(2) {
						t.Fatalf("completions payload changed: %#v", payload)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"upstream-model","usage":{"prompt_tokens":2,"completion_tokens":1}}`))}, nil
			})}
			directory := routes.New()
			directory.Put(routes.Snapshot{TenantID: "global", DeploymentID: "d1", Alias: "alias", UpstreamModel: "upstream-model", RouterURL: "http://runtime", ProtocolCapabilities: runtimecontract.ProtocolCapabilities{Responses: true, Embeddings: true, Completions: true, Batch: true}})
			captured := &captureRecorder{}
			handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client, Recorder: captured}).Handler()
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer secret")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			captured.mu.Lock()
			operation := captured.record.OperationName
			captured.mu.Unlock()
			if operation != test.operation {
				t.Fatalf("recorded operation=%q", operation)
			}
		})
	}
}

func TestUnqualifiedProtocolFailsBeforeUpstream(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected upstream request")
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{TenantID: "global", Alias: "alias", UpstreamModel: "model", RouterURL: "http://runtime", ProtocolCapabilities: runtimecontract.ProtocolCapabilities{ChatCompletions: true}})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"alias","input":"hello"}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || called || !strings.Contains(response.Body.String(), "unsupported_protocol") {
		t.Fatalf("status=%d called=%t body=%s", response.Code, called, response.Body.String())
	}
}

func TestAdmissionRejectsBeforeUpstream(t *testing.T) {
	called := false
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("unexpected upstream request")
	})}
	pool := admission.New()
	pool.Replace([]admission.Policy{{Key: "global\x00alias", MaxConcurrency: 1, MaxQueueDepth: 0, QueueTimeout: time.Second, MaxRequestBytes: 10, MaxOutputTokens: 4, AllowedPriorities: map[string]struct{}{"normal": {}}, Enabled: true}})
	directory := routes.New()
	directory.Put(routes.Snapshot{TenantID: "global", Alias: "alias", UpstreamModel: "model", RouterURL: "http://runtime", ProtocolCapabilities: runtimecontract.ProtocolCapabilities{ChatCompletions: true}})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client, AdmissionAuthorizer: pool}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge || called {
		t.Fatalf("status=%d called=%t body=%s", response.Code, called, response.Body.String())
	}
}

func TestAdmissionRetryBudgetRetriesOnlyBufferedInternalRequest(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		attempts++
		if request.Header.Get("X-InferCrane-Attempt") != string(rune('0'+attempts)) {
			t.Fatalf("attempt header=%q", request.Header.Get("X-InferCrane-Attempt"))
		}
		if attempts < 3 {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":"starting"}`))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"model","choices":[]}`))}, nil
	})}
	pool := admission.New()
	pool.Replace([]admission.Policy{{Key: "global\x00alias", MaxConcurrency: 1, MaxQueueDepth: 0, QueueTimeout: time.Second, MaxRequestBytes: 4096, MaxOutputTokens: 4096, AllowedPriorities: map[string]struct{}{"normal": {}}, RetryBudget: 2, Enabled: true}})
	directory := routes.New()
	directory.Put(routes.Snapshot{TenantID: "global", Alias: "alias", UpstreamModel: "model", RouterURL: "http://runtime", ProtocolCapabilities: runtimecontract.ProtocolCapabilities{ChatCompletions: true}})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client, AdmissionAuthorizer: pool}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[]}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || attempts != 3 {
		t.Fatalf("status=%d attempts=%d body=%s", response.Code, attempts, response.Body.String())
	}
}

func TestAdmissionRetryBudgetDoesNotRetryStreaming(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) { attempts++; return nil, errors.New("unavailable") })}
	pool := admission.New()
	pool.Replace([]admission.Policy{{Key: "global\x00alias", MaxConcurrency: 1, MaxQueueDepth: 0, QueueTimeout: time.Second, MaxRequestBytes: 4096, MaxOutputTokens: 4096, AllowedPriorities: map[string]struct{}{"normal": {}}, RetryBudget: 3, Enabled: true}})
	directory := routes.New()
	directory.Put(routes.Snapshot{TenantID: "global", Alias: "alias", UpstreamModel: "model", RouterURL: "http://runtime", ProtocolCapabilities: runtimecontract.ProtocolCapabilities{ChatCompletions: true, Streaming: true}})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client, AdmissionAuthorizer: pool}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","messages":[],"stream":true}`))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if attempts != 1 {
		t.Fatalf("stream was retried %d times", attempts)
	}
}

func TestResponsesUsageNormalizesInputAndOutputTokens(t *testing.T) {
	input, output := 7, 3
	_, actualInput, actualOutput := (responseObservation{body: []byte(`{"model":"model","usage":{"input_tokens":7,"output_tokens":3}}`)}).usage()
	if actualInput == nil || *actualInput != input || actualOutput == nil || *actualOutput != output {
		t.Fatalf("input=%v output=%v", actualInput, actualOutput)
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
	directory.Put(routes.Snapshot{DeploymentID: "d1", RevisionID: "rev-3", LogicalModelID: "model-1", EnvironmentID: "environment-1", EndpointID: "endpoint-1", ServingPlanID: "plan-1", BindingID: "binding-1", Alias: "alias", UpstreamModel: "Qwen/Qwen3-8B", RouterURL: "http://router", Provider: "runpod", Runtime: "vllm", ComputeMode: "elastic"})
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
	if record.LogicalModelID != "model-1" || record.EnvironmentID != "environment-1" || record.EndpointID != "endpoint-1" || record.ServingPlanID != "plan-1" || record.BindingID != "binding-1" {
		t.Fatalf("endpoint metadata=%+v", record)
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

func TestDashboardIsMountedWithoutWeakeningAPIAuthentication(t *testing.T) {
	dashboard := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("dashboard")) })
	control := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	handler := (&Gateway{Routes: routes.New(), Dashboard: dashboard, Control: control}).Handler()
	page := httptest.NewRecorder()
	handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/dashboard/", nil))
	if page.Code != http.StatusOK || page.Body.String() != "dashboard" {
		t.Fatalf("dashboard=%d %q", page.Code, page.Body.String())
	}
	api := httptest.NewRecorder()
	handler.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/v1/deployments", nil))
	if api.Code != http.StatusUnauthorized {
		t.Fatalf("control status=%d", api.Code)
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

func TestInferenceRequiresReadScope(t *testing.T) {
	directory := routes.New()
	directory.Put(routes.Snapshot{TenantID: "tenant-a", Alias: "model"})
	handler := (&Gateway{Routes: directory, Authenticator: fakeAuthenticator{principal: domain.Principal{ID: "p", TenantID: "tenant-a", Role: "operator", Scopes: []string{"deploy"}}}}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer scoped")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
}

func TestExternalFallbackConsumesHardBudgetBeforeTransmissionAndNeverReplaysStream(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "deployment", Alias: "alias", UpstreamModel: "provider/model", RouterURL: "https://external.invalid/api", Provider: "openrouter", ComputeMode: "external", ExternalPolicyID: "policy", UpstreamAPIKey: "provider-key"})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client, ExternalAuthorizer: &oneRequestBudget{remaining: 1}}).Handler()
	for attempt := 0; attempt < 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true,"messages":[]}`))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if attempt == 0 && response.Code != http.StatusOK {
			t.Fatalf("first response=%d %s", response.Code, response.Body.String())
		}
		if attempt == 1 && response.Code != http.StatusTooManyRequests {
			t.Fatalf("second response=%d %s", response.Code, response.Body.String())
		}
	}
	if requests != 1 {
		t.Fatalf("external stream was transmitted %d times", requests)
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

func TestClientCancellationPropagatesToRuntime(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCancelled := make(chan struct{})
	client := &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		close(upstreamStarted)
		<-request.Context().Done()
		close(upstreamCancelled)
		return nil, request.Context().Err()
	})}
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", Alias: "alias", UpstreamModel: "model", RouterURL: "http://runtime", Runtime: "vllm"})
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: client}).Handler()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"alias","stream":true,"messages":[]}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()
	<-upstreamStarted
	cancel()
	select {
	case <-upstreamCancelled:
	case <-time.After(time.Second):
		t.Fatal("runtime request did not observe client cancellation")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway did not finish after cancellation")
	}
}
