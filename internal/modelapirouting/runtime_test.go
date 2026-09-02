package modelapirouting

import (
	"bytes"
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

	"github.com/infercrane/infercrane/internal/supplieradapter"
)

type flushingRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *flushingRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}

type billingFake struct {
	mu              sync.Mutex
	reserveErr      error
	requests        []ReservationRequest
	transmitted     int
	responseStarted int
	settlements     []Usage
	released        int
}

type hostedCredentialFake struct {
	credential []byte
	err        error
	calls      int
	operator   string
	reference  string
}

func (c *hostedCredentialFake) ResolveHostedModelCredential(_ context.Context, operator, reference string) ([]byte, error) {
	c.calls++
	c.operator, c.reference = operator, reference
	if c.err != nil {
		return nil, c.err
	}
	return append([]byte(nil), c.credential...), nil
}

type routingRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f routingRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (b *billingFake) Reserve(_ context.Context, request ReservationRequest) (Reservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, request)
	if b.reserveErr != nil {
		return Reservation{}, b.reserveErr
	}
	return Reservation{ID: request.ID, TenantID: request.TenantID, ProductID: request.ProductID, EntitlementID: request.EntitlementID, State: "reserved"}, nil
}
func (b *billingFake) MarkTransmitted(context.Context, string, string, time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.transmitted++
	return nil
}
func (b *billingFake) MarkResponseStarted(context.Context, string, string, time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.responseStarted++
	return nil
}
func (b *billingFake) Settle(_ context.Context, _, _ string, usage Usage) (Reservation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.settlements = append(b.settlements, usage)
	return Reservation{State: "settled"}, nil
}
func (b *billingFake) ReleaseUnsent(context.Context, string, string, string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.released++
	return nil
}

func runtimeFixture(t *testing.T, server *httptest.Server, billing *billingFake) (*Runtime, time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	route := routeFixture(now, "customer")
	route.Candidates[0].Endpoint = server.URL + "/v1"
	directory := NewDirectory()
	directory.now = func() time.Time { return now }
	if err := directory.Publish([]PublishedRoute{route}); err != nil {
		t.Fatal(err)
	}
	return &Runtime{Routes: directory, Billing: billing, Client: server.Client(), now: func() time.Time { return now }}, now
}

func strictRuntimeFixture(t *testing.T, billing *billingFake, transport http.RoundTripper, credentials *hostedCredentialFake) *Runtime {
	t.Helper()
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	route := routeFixture(now, "customer")
	route.Entitlement.ProductID = "deepseek-v4-flash"
	route.Publication.ProductID = "deepseek-v4-flash"
	route.Rate.ProductID = "deepseek-v4-flash"
	route.Candidates[0].ProductID = "deepseek-v4-flash"
	route.Candidates[0].Supplier = supplieradapter.DeepSeekSupplier
	route.Candidates[0].SupplierModelID = supplieradapter.DeepSeekV4FlashModelID
	route.Candidates[0].Endpoint = supplieradapter.DeepSeekBaseURL
	route.Candidates[0].Credential = ""
	route.Candidates[0].Adapter = supplieradapter.DeepSeekAdapterName
	route.Candidates[0].CredentialReference = "deepseek-secret-reference"
	directory := NewDirectory()
	directory.now = func() time.Time { return now }
	if err := directory.Publish([]PublishedRoute{route}); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: transport}
	registry, err := supplieradapter.NewRegistry(supplieradapter.NewDeepSeekAdapter(client))
	if err != nil {
		t.Fatal(err)
	}
	return &Runtime{Routes: directory, Billing: billing, Client: client, Adapters: registry, Credentials: credentials, now: func() time.Time { return now }}
}

func strictProxyRequest(stream bool) ProxyRequest {
	return ProxyRequest{
		TenantID: "customer", ProductID: "deepseek-v4-flash", Operation: "chat", Resource: "chat/completions",
		RequestID: "public-request", TraceParent: "00-12345678901234567890123456789012-1234567890123456-01",
		Payload: map[string]any{
			"model": "deepseek-v4-flash", "messages": []any{map[string]any{"role": "user", "content": "Hello"}},
			"max_tokens": float64(256), "stream": stream,
		},
	}
}

func TestStrictRuntimeResolvesCredentialPerRequestAndRewritesBufferedResponse(t *testing.T) {
	calls := 0
	transport := routingRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.String() != supplieradapter.DeepSeekBaseURL+"/chat/completions" || request.Header.Get("Authorization") != "Bearer supplier-secret" || request.Header.Get("X-InferCrane-Attempt") != "1" {
			t.Fatalf("strict supplier request=%s headers=%#v", request.URL, request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "deepseek-v4-flash\"") == false || strings.Contains(string(body), "supplier-secret") {
			t.Fatalf("strict supplier body=%s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}, "X-DS-Request-ID": {"private-supplier-id"}}, Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl-private","model":"deepseek-v4-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":19,"completion_tokens":5,"prompt_cache_hit_tokens":11}
		}`))}, nil
	})
	billing := &billingFake{}
	credentials := &hostedCredentialFake{credential: []byte("supplier-secret")}
	runtime := strictRuntimeFixture(t, billing, transport, credentials)
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), strictProxyRequest(false))
	if recorder.Code != http.StatusOK || calls != 1 || credentials.calls != 1 || credentials.operator != "operator" || credentials.reference != "deepseek-secret-reference" {
		t.Fatalf("status=%d calls=%d credentials=%#v body=%s", recorder.Code, calls, credentials, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "private-supplier-id") || strings.Contains(recorder.Body.String(), "supplier-secret") || !strings.Contains(recorder.Body.String(), `"model":"deepseek-v4-flash"`) || !strings.Contains(recorder.Body.String(), `"cached_tokens":11`) {
		t.Fatalf("public buffered response=%s", recorder.Body.String())
	}
	if billing.transmitted != 1 || billing.responseStarted != 1 || len(billing.settlements) != 1 || *billing.settlements[0].InputTokens != 19 || *billing.settlements[0].OutputTokens != 5 {
		t.Fatalf("strict billing=%#v", billing)
	}
}

func TestStrictRuntimeCredentialRevocationFailsBeforeTransmission(t *testing.T) {
	calls := 0
	transport := routingRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("must not transmit")
	})
	billing := &billingFake{}
	credentials := &hostedCredentialFake{err: errors.New("secret reference was deleted")}
	runtime := strictRuntimeFixture(t, billing, transport, credentials)
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), strictProxyRequest(false))
	if recorder.Code != http.StatusServiceUnavailable || calls != 0 || credentials.calls != 1 || billing.transmitted != 0 || billing.released != 1 || len(billing.settlements) != 0 {
		t.Fatalf("status=%d calls=%d credentials=%d transmitted=%d released=%d settlements=%d", recorder.Code, calls, credentials.calls, billing.transmitted, billing.released, len(billing.settlements))
	}
}

func TestStrictRuntimeRewritesSSEAndSettlesTerminalUsageOnce(t *testing.T) {
	calls := 0
	transport := routingRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		stream := "data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":3}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-ID": {"private-id"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})
	billing := &billingFake{}
	runtime := strictRuntimeFixture(t, billing, transport, &hostedCredentialFake{credential: []byte("supplier-secret")})
	recorder := &flushingRecorder{ResponseRecorder: httptest.NewRecorder()}
	runtime.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), strictProxyRequest(true))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || calls != 1 || recorder.flushes < 4 || !strings.Contains(body, `"model":"deepseek-v4-flash"`) || !strings.Contains(body, "data: [DONE]") || strings.Contains(body, "private-id") {
		t.Fatalf("status=%d calls=%d flushes=%d body=%s", recorder.Code, calls, recorder.flushes, body)
	}
	if len(billing.settlements) != 1 || *billing.settlements[0].InputTokens != 17 || *billing.settlements[0].OutputTokens != 3 {
		t.Fatalf("strict stream settlement=%#v", billing.settlements)
	}
}

func TestStrictRuntimeRejectsUnqualifiedFieldsBeforeReservation(t *testing.T) {
	billing := &billingFake{}
	runtime := strictRuntimeFixture(t, billing, routingRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("strict invalid request was transmitted")
		return nil, nil
	}), &hostedCredentialFake{credential: []byte("secret")})
	request := strictProxyRequest(false)
	request.Payload["tools"] = []any{}
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), request)
	if recorder.Code != http.StatusBadRequest || len(billing.requests) != 0 {
		t.Fatalf("status=%d reservations=%d", recorder.Code, len(billing.requests))
	}
}

func TestRuntimeInsufficientBalanceDoesNotSend(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	billing := &billingFake{reserveErr: ErrInsufficientPrepaidBalance}
	runtime, _ := runtimeFixture(t, server, billing)
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), ProxyRequest{
		TenantID: "customer", ProductID: "glm-5.3", Operation: "chat", Resource: "chat/completions",
		RequestID: "request", TraceParent: "trace", Payload: map[string]any{"model": "glm-5.3"},
	})
	if recorder.Code != http.StatusPaymentRequired || requests != 0 || billing.transmitted != 0 {
		t.Fatalf("status=%d requests=%d transmitted=%d", recorder.Code, requests, billing.transmitted)
	}
}

func TestRuntimeDoesNotMisreportBillingInfrastructureFailureAsInsufficientCredit(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	billing := &billingFake{reserveErr: errors.New("database unavailable")}
	runtime, _ := runtimeFixture(t, server, billing)
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), ProxyRequest{
		TenantID: "customer", ProductID: "glm-5.3", Operation: "chat", Resource: "chat/completions",
		RequestID: "request", TraceParent: "trace", Payload: map[string]any{"model": "glm-5.3"},
	})
	if recorder.Code != http.StatusServiceUnavailable || requests != 0 || billing.transmitted != 0 {
		t.Fatalf("status=%d requests=%d transmitted=%d", recorder.Code, requests, billing.transmitted)
	}
}

func TestRuntimePinsRetailRateAndSettlesObservedUsage(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":20}}`))
	}))
	defer server.Close()
	billing := &billingFake{}
	runtime, now := runtimeFixture(t, server, billing)
	updated := routeFixture(now, "customer")
	updated.Rate.ID, updated.Rate.Version, updated.Rate.ContractDigest = "rate-two", 2, "sha256:rate-two"
	updated.Entitlement.RetailRateID, updated.Entitlement.RetailRateVersion = "rate-two", 2
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), ProxyRequest{
		TenantID: "customer", ProductID: "glm-5.3", Operation: "chat", Resource: "chat/completions",
		RequestID: "request", TraceParent: "trace", Payload: map[string]any{"model": "glm-5.3"},
	})
	if err := runtime.Routes.Publish([]PublishedRoute{updated}); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK || len(billing.requests) != 1 || billing.requests[0].RetailRate.ID != "rate" {
		t.Fatalf("response=%d reservations=%#v", recorder.Code, billing.requests)
	}
	if len(billing.settlements) != 1 || billing.settlements[0].InputTokens == nil || *billing.settlements[0].InputTokens != 100 || billing.settlements[0].OutputTokens == nil || *billing.settlements[0].OutputTokens != 20 {
		t.Fatalf("settlement=%#v", billing.settlements)
	}
}

func TestRuntimeNeverRetriesAfterSupplierResponseStarts(t *testing.T) {
	firstCalls, fallbackCalls := 0, 0
	primary := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls++
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer primary.Close()
	fallback := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls++
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer fallback.Close()
	billing := &billingFake{}
	runtime, now := runtimeFixture(t, primary, billing)
	route := routeFixture(now, "customer")
	route.Candidates[0].Endpoint = primary.URL + "/v1"
	second := route.Candidates[0]
	second.ID, second.OfferID, second.Endpoint = "fallback", "fallback-offer", fallback.URL+"/v1"
	route.Candidates = append(route.Candidates, second)
	if err := runtime.Routes.Publish([]PublishedRoute{route}); err != nil {
		t.Fatal(err)
	}
	// The TLS trust pools differ. A client that trusts both is unnecessary for
	// this assertion because the selected primary has its own trusted client.
	runtime.Client = primary.Client()
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), ProxyRequest{
		TenantID: "customer", ProductID: "glm-5.3", Operation: "chat", Resource: "chat/completions",
		RequestID: "request", TraceParent: "trace", Payload: map[string]any{"model": "glm-5.3"},
	})
	if recorder.Code != http.StatusServiceUnavailable || firstCalls != 1 || fallbackCalls != 0 || len(billing.settlements) != 1 {
		t.Fatalf("status=%d primary=%d fallback=%d settlements=%d", recorder.Code, firstCalls, fallbackCalls, len(billing.settlements))
	}
	if billing.settlements[0].InputTokens != nil || billing.settlements[0].OutputTokens != nil {
		t.Fatalf("missing usage was not sent to reconciliation: %#v", billing.settlements[0])
	}
}

func TestRuntimeRoutesNextRequestToFallbackWhenCanaryCircuitIsOpen(t *testing.T) {
	called := ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = r.URL.Path
		_, _ = io.WriteString(w, `{"model":"supplier/glm","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer server.Close()
	billing := &billingFake{}
	runtime, now := runtimeFixture(t, server, billing)
	route := routeFixture(now, "customer")
	route.Candidates[0].ID = "runpod"
	route.Candidates[0].Endpoint = server.URL + "/runpod/v1"
	route.Candidates[0].TrafficWeightBPS = 10_000
	upstream := route.Candidates[0]
	upstream.ID, upstream.OfferID = "upstream", "upstream-offer"
	upstream.Endpoint, upstream.TrafficWeightBPS = server.URL+"/upstream/v1", 0
	route.Candidates = append(route.Candidates, upstream)
	if err := runtime.Routes.Publish([]PublishedRoute{route}); err != nil {
		t.Fatal(err)
	}
	runtime.Circuit = NewCircuitBreaker(1, time.Minute)
	runtime.Circuit.Observe("runpod", false, now)
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), ProxyRequest{
		TenantID: "customer", ProductID: "glm-5.3", Operation: "chat", Resource: "chat/completions",
		RequestID: "request", TraceParent: "trace", Payload: map[string]any{"model": "glm-5.3"},
	})
	if recorder.Code != http.StatusOK || called != "/upstream/v1/chat/completions" {
		t.Fatalf("status=%d called=%q body=%s", recorder.Code, called, recorder.Body.String())
	}
	if len(billing.requests) != 1 || billing.requests[0].CandidateID != "upstream" {
		t.Fatalf("reservation did not pin fallback candidate: %#v", billing.requests)
	}
}

func TestHostedSSEFlushesStopsAtDoneRetainsUsageAndSettlesOnce(t *testing.T) {
	stream := "data: {\"model\":\"supplier/glm\",\"choices\":[]}\n\n" +
		"data: {\"model\":\"supplier/glm\",\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":999,\"completion_tokens\":999}}\n\n"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode supplier request: %v", err)
		}
		streamOptions, _ := payload["stream_options"].(map[string]any)
		if streamOptions["include_usage"] != true {
			t.Fatalf("stream usage was not requested from supplier: %#v", payload)
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, stream)
	}))
	defer server.Close()
	billing := &billingFake{}
	runtime, _ := runtimeFixture(t, server, billing)
	recorder := &flushingRecorder{ResponseRecorder: httptest.NewRecorder()}
	runtime.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), ProxyRequest{
		TenantID: "customer", ProductID: "glm-5.3", Operation: "chat", Resource: "chat/completions",
		RequestID: "request", TraceParent: "trace", Payload: map[string]any{"model": "glm-5.3", "stream": true},
	})
	if recorder.Code != http.StatusOK || recorder.flushes < 3 {
		t.Fatalf("status=%d flushes=%d body=%q", recorder.Code, recorder.flushes, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "999") || !strings.Contains(recorder.Body.String(), "data: [DONE]") || strings.Contains(recorder.Body.String(), "supplier/glm") || !strings.Contains(recorder.Body.String(), `"model":"glm-5.3"`) {
		t.Fatalf("hosted public stream=%q", recorder.Body.String())
	}
	if len(billing.settlements) != 1 {
		t.Fatalf("settlements=%#v", billing.settlements)
	}
	settlement := billing.settlements[0]
	if settlement.InputTokens == nil || *settlement.InputTokens != 17 || settlement.OutputTokens == nil || *settlement.OutputTokens != 5 {
		t.Fatalf("terminal usage settlement=%#v", settlement)
	}
}

func TestRuntimeUsesPublicHeaderAllowlists(t *testing.T) {
	var supplierHeaders http.Header
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		supplierHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "Application/JSON; charset=latin1")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Server", "private-supplier")
		w.Header().Set("Set-Cookie", "supplier_session=secret")
		w.Header().Set("X-Supplier-Debug", "private-route")
		_, _ = io.WriteString(w, `{"model":"supplier/glm","choices":[]}`)
	}))
	defer server.Close()
	billing := &billingFake{}
	runtime, _ := runtimeFixture(t, server, billing)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("OpenAI-Beta", "responses=v1")
	request.Header.Set("Authorization", "Bearer customer-secret")
	request.Header.Set("Cookie", "customer_session=secret")
	request.Header.Set("X-Supplier-Admin", "true")
	request.Header.Set("X-Request-ID", "customer-controlled")
	recorder := httptest.NewRecorder()
	runtime.ServeHTTP(recorder, request, ProxyRequest{
		TenantID: "customer", ProductID: "glm-5.3", Operation: "chat", Resource: "chat/completions",
		RequestID: "public-request", TraceParent: "public-trace", Payload: map[string]any{"model": "glm-5.3"},
	})
	if supplierHeaders.Get("Accept") != "application/json" || supplierHeaders.Get("OpenAI-Beta") != "responses=v1" {
		t.Fatalf("safe request headers were not forwarded: %#v", supplierHeaders)
	}
	if supplierHeaders.Get("Authorization") != "Bearer secret" || supplierHeaders.Get("X-Request-ID") != "public-request" || supplierHeaders.Get("Traceparent") != "public-trace" {
		t.Fatalf("runtime-owned request headers were not normalized: %#v", supplierHeaders)
	}
	if supplierHeaders.Get("Cookie") != "" || supplierHeaders.Get("X-Supplier-Admin") != "" {
		t.Fatalf("unapproved customer headers reached supplier: %#v", supplierHeaders)
	}
	if recorder.Header().Get("Content-Type") != "application/json" || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("public response headers were not normalized: %#v", recorder.Header())
	}
	if recorder.Header().Get("Server") != "" || recorder.Header().Get("Set-Cookie") != "" || recorder.Header().Get("X-Supplier-Debug") != "" {
		t.Fatalf("supplier-private headers leaked: %#v", recorder.Header())
	}
}

func TestPublicSSEParsesCompleteMultilineEventsAndRetainsResponsesUsage(t *testing.T) {
	stream := "event: response.completed\r\n" +
		"data: {\"type\":\"response.completed\",\"model\":\"supplier/glm\",\"sequence\":9007199254740993,\r\n" +
		"data: \"response\":{\"usage\":{\"input_tokens\":23,\"output_tokens\":7}}}\r\n\r\n" +
		"data: {\"type\":\"after-usage\",\"model\":\"supplier/glm\"}\r\n\r\n"
	recorder := &flushingRecorder{ResponseRecorder: httptest.NewRecorder()}
	input, output, err := copyPublicResponse(recorder, eventStreamResponse(stream), `public/"model`)
	if err != nil {
		t.Fatal(err)
	}
	if input == nil || *input != 23 || output == nil || *output != 7 {
		t.Fatalf("responses usage was not retained: input=%v output=%v", input, output)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "supplier/glm") || !strings.Contains(body, `"model":"public/\"model"`) || !strings.Contains(body, "9007199254740993") {
		t.Fatalf("unsafe or lossy SSE rewrite: %q", body)
	}
	if !strings.Contains(body, "event: response.completed\n") || recorder.flushes != 2 {
		t.Fatalf("SSE metadata/framing was not preserved: flushes=%d body=%q", recorder.flushes, body)
	}
}

func TestPublicSSEFailsClosedOnMalformedOrUnterminatedEvents(t *testing.T) {
	tests := map[string]string{
		"malformed JSON":     "data: not-json\n\n",
		"unterminated event": `data: {"model":"supplier/glm"}`,
		"line limit":         "data: " + strings.Repeat("x", maxSSELineBytes+1) + "\n\n",
	}
	for name, stream := range tests {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			_, _, err := copyPublicResponse(recorder, eventStreamResponse(stream), "public")
			if err == nil {
				t.Fatal("invalid SSE stream was accepted")
			}
			if recorder.Body.Len() != 0 {
				t.Fatalf("invalid event was forwarded: %q", recorder.Body.String())
			}
		})
	}
}

func TestPublicSSEBoundsWholeEventMemory(t *testing.T) {
	line := ":" + strings.Repeat("x", 1022) + "\n"
	stream := strings.Repeat(line, maxSSEEventBytes/len(line)+2) + "\n"
	recorder := httptest.NewRecorder()
	_, _, err := copyPublicResponse(recorder, eventStreamResponse(stream), "public")
	if err == nil || recorder.Body.Len() != 0 {
		t.Fatalf("oversized SSE event was not rejected: err=%v bytes=%d", err, recorder.Body.Len())
	}
}

func TestBufferedResponseRewritesWithoutRoundingAndCapturesUsage(t *testing.T) {
	response := &http.Response{
		Header: http.Header{"Content-Type": {"application/json"}},
		Body:   io.NopCloser(bytes.NewBufferString(`{"model":"supplier/glm","sequence":9007199254740993,"usage":{"input_tokens":11,"output_tokens":3}}`)),
	}
	recorder := httptest.NewRecorder()
	input, output, err := copyPublicResponse(recorder, response, "public")
	if err != nil {
		t.Fatal(err)
	}
	if input == nil || *input != 11 || output == nil || *output != 3 || !strings.Contains(recorder.Body.String(), "9007199254740993") || strings.Contains(recorder.Body.String(), "supplier/glm") {
		t.Fatalf("buffered rewrite/usage was lossy: input=%v output=%v body=%q", input, output, recorder.Body.String())
	}
}

func eventStreamResponse(stream string) *http.Response {
	return &http.Response{
		Header: http.Header{"Content-Type": {"Text/Event-Stream; charset=utf-8"}},
		Body:   io.NopCloser(strings.NewReader(stream)),
	}
}
