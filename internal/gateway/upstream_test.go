package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/admission"
	"github.com/infercrane/infercrane/internal/routes"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

// routerConnectionFailure is what vLLM Router 0.1.15 answers when it cannot
// reach the worker behind it, as seen on 2026-11-20 with the worker scaled to 0.
const routerConnectionFailure = "Request failed: error sending request for url (http://infercrane-da49b2854aad0465fb8a314a.infercrane-system.svc:8080/v1/chat/completions)"

type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func upstreamTestRoute(routerURL string) *routes.Directory {
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "d1", TenantID: "global", Alias: "alias", UpstreamModel: "model", RouterURL: routerURL, ProtocolCapabilities: runtimecontract.ProtocolCapabilities{ChatCompletions: true, Streaming: true}})
	return directory
}

func serveUpstreamTest(gateway *Gateway, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("X-Request-ID", "req-upstream-test")
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	return response
}

// assertGatewayError checks that the caller got the gateway's own OpenAI error
// and nothing of the upstream's text.
func assertGatewayError(t *testing.T, response *httptest.ResponseRecorder, status int, message string, upstreamText ...string) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Message != message || body.Error.Type != "server_error" {
		t.Fatalf("body=%s err=%v", response.Body.String(), err)
	}
	for _, text := range append(upstreamText, "http://", "svc", ".local") {
		if strings.Contains(response.Body.String(), text) {
			t.Fatalf("upstream text %q reached the caller: %s", text, response.Body.String())
		}
	}
}

func TestUpstreamErrorTextNeverReachesCaller(t *testing.T) {
	for name, upstream := range map[string]struct {
		status      int
		contentType string
		body        string
		stream      bool
	}{
		"router connection failure":           {http.StatusInternalServerError, "text/plain; charset=utf-8", routerConnectionFailure, false},
		"router connection failure on stream": {http.StatusInternalServerError, "text/plain; charset=utf-8", routerConnectionFailure, true},
		"router body read failure":            {http.StatusInternalServerError, "text/plain; charset=utf-8", "Failed to get response body: request or response body error for url (http://10.0.3.7:8000/v1/chat/completions)", false},
		"proxy error page":                    {http.StatusInternalServerError, "text/html", "<html><body>upstream 10.0.3.7:8000 failed; worker-0.infercrane-system.svc.cluster.local</body></html>", false},
		"other server error without json":     {http.StatusNotImplemented, "text/plain", "no route to http://worker.infercrane-system.svc:8080", false},
		"json that is not an object":          {http.StatusInternalServerError, "application/json", `"Request failed: http://worker.infercrane-system.svc:8080"`, false},
		"empty body":                          {http.StatusInternalServerError, "", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: upstream.status, Header: http.Header{"Content-Type": {upstream.contentType}}, Body: io.NopCloser(strings.NewReader(upstream.body))}, nil
			})}
			logs := &lockedBuffer{}
			captured := &captureRecorder{}
			gateway := &Gateway{Routes: upstreamTestRoute("http://router"), APIKey: "secret", Client: client, Recorder: captured, Logger: slog.New(slog.NewTextHandler(logs, nil))}
			body := `{"model":"alias","messages":[]}`
			if upstream.stream {
				body = `{"model":"alias","messages":[],"stream":true}`
			}
			response := serveUpstreamTest(gateway, body)
			assertGatewayError(t, response, http.StatusServiceUnavailable, "Inference upstream is unavailable", "Request failed", "10.0.3.7", "infercrane-system", "<html")
			if response.Header().Get("X-Request-ID") != "req-upstream-test" {
				t.Fatalf("request id header=%q", response.Header().Get("X-Request-ID"))
			}
			if captured.record.StatusCode != http.StatusServiceUnavailable || captured.record.ErrorType != "upstream_unavailable" || captured.record.RequestID != "req-upstream-test" {
				t.Fatalf("record=%#v", captured.record)
			}
			// The detail stays with the operator, under the request ID.
			logged := logs.String()
			if !strings.Contains(logged, "inference upstream failed") || !strings.Contains(logged, "request_id=req-upstream-test") || !strings.Contains(logged, "upstream_status="+strconv.Itoa(upstream.status)) {
				t.Fatalf("log=%s", logged)
			}
			if upstream.body != "" && !strings.Contains(logged, strings.ReplaceAll(upstream.body[:min(len(upstream.body), 40)], `"`, `\"`)) {
				t.Fatalf("upstream detail missing from log=%s", logged)
			}
		})
	}
}

func TestRouterConnectionFailureRetriesWithinBudgetLikeAConnectionFailure(t *testing.T) {
	attempts := 0
	client := &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{"Content-Type": {"text/plain; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(routerConnectionFailure))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"model":"model","choices":[]}`))}, nil
	})}
	pool := admission.New()
	pool.Replace([]admission.Policy{{Key: "global\x00alias", MaxConcurrency: 1, QueueTimeout: time.Second, MaxRequestBytes: 4096, MaxOutputTokens: 4096, AllowedPriorities: map[string]struct{}{"normal": {}}, RetryBudget: 1, Enabled: true}})
	captured := &captureRecorder{}
	response := serveUpstreamTest(&Gateway{Routes: upstreamTestRoute("http://router"), APIKey: "secret", Client: client, AdmissionAuthorizer: pool, Recorder: captured}, `{"model":"alias","messages":[]}`)
	if response.Code != http.StatusOK || attempts != 2 || captured.record.RetryCount != 1 {
		t.Fatalf("status=%d attempts=%d record=%#v body=%s", response.Code, attempts, captured.record, response.Body.String())
	}
}

// The case that found the leak: the owned primary of a primary-fallback plan
// was scaled to 0 before routing marked it unroutable, so its router answered
// with its own 500.
func TestRouterConnectionFailureFailsOverToTheFallback(t *testing.T) {
	routerFailure := func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{"Content-Type": {"text/plain; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(routerConnectionFailure))}, nil
	}
	upstream := newUpstreams(map[string]func(*http.Request) (*http.Response, error){
		"owned":  routerFailure,
		"closed": answerOK(`{"model":"closed-model","choices":[{"message":{"content":"served by fallback"}}]}`),
	})
	directory := routes.New()
	publishPlan(directory, "primary-fallback", endpointRoute("owned", "owned", "qwen3"), endpointRoute("closed", "closed", "closed-model"))
	records := &allRecords{}
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client(), Recorder: records}).Handler()
	response := invokeCoder(handler, `{"model":"coder","messages":[]}`)
	if record := records.only(t); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "served by fallback") || record.BindingID != "closed" || record.FallbackReason != "upstream_connection_failed" {
		t.Fatalf("response=%d %s record=%#v", response.Code, response.Body.String(), record)
	}

	upstream.mu.Lock()
	upstream.answer["closed"] = routerFailure
	upstream.mu.Unlock()
	records = &allRecords{}
	handler = (&Gateway{Routes: directory, APIKey: "secret", Client: upstream.client(), Recorder: records}).Handler()
	response = invokeCoder(handler, `{"model":"coder","messages":[]}`)
	assertGatewayError(t, response, http.StatusServiceUnavailable, "Inference upstream is unavailable", "Request failed", "infercrane-system")
	if record := records.only(t); record.ErrorType != "upstream_unavailable" {
		t.Fatalf("record=%#v", record)
	}
}

func TestModelServerErrorsStillReachCaller(t *testing.T) {
	for name, upstream := range map[string]struct {
		contentType, body string
	}{
		"json error":                  {"application/json", `{"object":"error","message":"CUDA error: an illegal memory access was encountered","type":"InternalServerError","param":null,"code":500}`},
		"json error relayed as sse":   {"text/event-stream", `{"error":{"message":"engine dead","type":"InternalServerError"}}`},
		"json error without a type":   {"text/plain", `{"detail":"Internal Server Error"}`},
		"server-sent error on stream": {"text/event-stream", "data: {\"error\":{\"message\":\"engine dead\"}}\n\n"},
	} {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{"Content-Type": {upstream.contentType}}, Body: io.NopCloser(strings.NewReader(upstream.body))}, nil
			})}
			captured := &captureRecorder{}
			response := serveUpstreamTest(&Gateway{Routes: upstreamTestRoute("http://router"), APIKey: "secret", Client: client, Recorder: captured}, `{"model":"alias","messages":[]}`)
			if response.Code != http.StatusInternalServerError || response.Body.String() != upstream.body {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if captured.record.StatusCode != http.StatusInternalServerError {
				t.Fatalf("record=%#v", captured.record)
			}
		})
	}
}

func TestUpstreamConnectionFailureAndTimeoutNameNoUpstream(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	host := strings.TrimPrefix(closedURL, "http://")
	logs := &lockedBuffer{}
	// No Client: the gateway's own default client is the one under test.
	response := serveUpstreamTest(&Gateway{Routes: upstreamTestRoute(closedURL), APIKey: "secret", Logger: slog.New(slog.NewTextHandler(logs, nil))}, `{"model":"alias","messages":[]}`)
	assertGatewayError(t, response, http.StatusServiceUnavailable, "Inference upstream is unavailable", host, "127.0.0.1", "dial tcp", "connection refused")
	if logged := logs.String(); !strings.Contains(logged, "inference upstream failed") || !strings.Contains(logged, host) {
		t.Fatalf("connection failure detail missing from log=%s", logged)
	}

	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	pool := admission.New()
	pool.Replace([]admission.Policy{{Key: "global\x00alias", MaxConcurrency: 1, QueueTimeout: time.Second, RequestTimeout: 50 * time.Millisecond, MaxRequestBytes: 4096, MaxOutputTokens: 4096, AllowedPriorities: map[string]struct{}{"normal": {}}, Enabled: true}})
	captured := &captureRecorder{}
	response = serveUpstreamTest(&Gateway{Routes: upstreamTestRoute(slow.URL), APIKey: "secret", Client: slow.Client(), AdmissionAuthorizer: pool, Recorder: captured}, `{"model":"alias","messages":[]}`)
	assertGatewayError(t, response, http.StatusGatewayTimeout, "Inference request exceeded its endpoint deadline", strings.TrimPrefix(slow.URL, "http://"), "127.0.0.1", "context deadline")
	if captured.record.ErrorType != "request_deadline_exceeded" {
		t.Fatalf("record=%#v", captured.record)
	}
}
