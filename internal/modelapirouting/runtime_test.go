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
