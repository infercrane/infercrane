package supplieradapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type credentialResolverFunc func(context.Context, string) ([]byte, error)

func (f credentialResolverFunc) Resolve(ctx context.Context, reference string) ([]byte, error) {
	return f(ctx, reference)
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func testHeaders(values map[string]string) http.Header {
	header := make(http.Header, len(values))
	for name, value := range values {
		header.Set(name, value)
	}
	return header
}

func deepSeekTarget() Target {
	return Target{Supplier: DeepSeekSupplier, BaseURL: DeepSeekBaseURL, SupplierModelID: DeepSeekV4FlashModelID, Region: "global", CredentialReference: "secret://deepseek/mvp"}
}

func deepSeekRequest() Request {
	maximum := 4096
	temperature := 0.4
	return Request{
		ID: "request-1", Operation: OperationChatCompletions, ModelID: "infercrane/deepseek-v4-flash",
		Messages:        []Message{{Role: "system", Content: []ContentPart{{Type: "text", Text: "Be concise."}}}, {Role: "user", Content: []ContentPart{{Type: "text", Text: "Hello"}}}},
		MaxOutputTokens: &maximum, Temperature: &temperature, Stream: true,
	}
}

func TestDeepSeekBuildRequestPinsQualifiedWireContract(t *testing.T) {
	credential := []byte("supplier-secret")
	request, err := NewDeepSeekAdapter(nil).BuildRequest(context.Background(), deepSeekTarget(), deepSeekRequest(), credentialResolverFunc(func(_ context.Context, reference string) ([]byte, error) {
		if reference != "secret://deepseek/mvp" {
			t.Fatalf("credential reference=%q", reference)
		}
		return credential, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if request.Method != http.MethodPost || request.URL.String() != DeepSeekBaseURL+"/chat/completions" {
		t.Fatalf("unexpected upstream request: %s %s", request.Method, request.URL)
	}
	if request.Header.Get("Authorization") != "Bearer supplier-secret" || request.Header.Get("X-Request-ID") != "request-1" || request.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("unexpected upstream headers: %#v", request.Header)
	}
	for index, value := range credential {
		if value != 0 {
			t.Fatalf("resolved credential byte %d was not cleared", index)
		}
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != DeepSeekV4FlashModelID || payload["max_tokens"] != float64(4096) || payload["stream"] != true {
		t.Fatalf("wire contract=%s", body)
	}
	thinking, _ := payload["thinking"].(map[string]any)
	streamOptions, _ := payload["stream_options"].(map[string]any)
	if thinking["type"] != "disabled" || streamOptions["include_usage"] != true {
		t.Fatalf("MVP thinking/usage policy=%s", body)
	}
	if strings.Contains(string(body), "infercrane/deepseek") || strings.Contains(string(body), "supplier-secret") {
		t.Fatalf("public identity or credential leaked into supplier body: %s", body)
	}
}

func TestDeepSeekBuildRequestFailsClosedOutsideMVPSubset(t *testing.T) {
	valid := deepSeekRequest()
	valid.Stream = false
	resolve := credentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("secret"), nil })
	tests := map[string]struct {
		target Target
		mutate func(*Request)
	}{
		"unqualified base URL": {target: Target{Supplier: DeepSeekSupplier, BaseURL: "https://proxy.example", SupplierModelID: DeepSeekV4FlashModelID, CredentialReference: "secret"}},
		"responses operation":  {target: deepSeekTarget(), mutate: func(request *Request) { request.Operation = OperationResponses }},
		"tool": {target: deepSeekTarget(), mutate: func(request *Request) {
			request.Tools = []Tool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}
		}},
		"image": {target: deepSeekTarget(), mutate: func(request *Request) {
			request.Messages[0].Content[0] = ContentPart{Type: "image", ImageURL: "https://example.test/image.png"}
		}},
		"unbounded output": {target: deepSeekTarget(), mutate: func(request *Request) { tooMany := DeepSeekMVPMaxOutputTokens + 1; request.MaxOutputTokens = &tooMany }},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			request := valid
			request.Messages = append([]Message(nil), valid.Messages...)
			request.Messages[0].Content = append([]ContentPart(nil), valid.Messages[0].Content...)
			if test.mutate != nil {
				test.mutate(&request)
			}
			_, err := NewDeepSeekAdapter(nil).BuildRequest(context.Background(), test.target, request, resolve)
			var normalized *Error
			if !errors.As(err, &normalized) || normalized.Code != ErrorInvalidRequest || normalized.Billing != BillingNotTransmitted || normalized.SafeToRetry() {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestDeepSeekBuildRequestRejectsUnsafeCredentialBeforeTransmission(t *testing.T) {
	_, err := NewDeepSeekAdapter(nil).BuildRequest(context.Background(), deepSeekTarget(), deepSeekRequest(), credentialResolverFunc(func(context.Context, string) ([]byte, error) {
		return []byte("secret\r\ninjected: value"), nil
	}))
	var normalized *Error
	if !errors.As(err, &normalized) || normalized.Code != ErrorInternal || normalized.Billing != BillingNotTransmitted || normalized.ResponseStarted {
		t.Fatalf("error=%#v", err)
	}
}

func TestDeepSeekDecodeResponseNormalizesIdentityUsageAndCache(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     testHeaders(map[string]string{"X-Request-ID": "supplier-request-7"}),
		Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl-1","model":"deepseek-v4-flash",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":19,"completion_tokens":5,"prompt_cache_hit_tokens":11,"completion_tokens_details":{"reasoning_tokens":2}}
		}`)),
	}
	decoded, err := NewDeepSeekAdapter(nil).DecodeResponse(context.Background(), response)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ID != "chatcmpl-1" || decoded.ModelID != DeepSeekV4FlashModelID || decoded.SupplierRequestID != "supplier-request-7" || len(decoded.Choices) != 1 || decoded.Choices[0].Message.Content[0].Text != "hello" {
		t.Fatalf("decoded response=%+v", decoded)
	}
	if decoded.Usage.State != UsageComplete || *decoded.Usage.InputTokens != 19 || *decoded.Usage.OutputTokens != 5 || *decoded.Usage.CachedInput != 11 || *decoded.Usage.ReasoningTokens != 2 {
		t.Fatalf("decoded usage=%+v", decoded.Usage)
	}
}

func TestDeepSeekHTTPErrorIsSanitizedAndNeverAssumesNoCharge(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     testHeaders(map[string]string{"Retry-After": "7", "X-DS-Request-ID": "ds-429"}),
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"raw supplier detail must not escape"}}`)),
	}
	_, err := NewDeepSeekAdapter(nil).DecodeResponse(context.Background(), response)
	var normalized *Error
	if !errors.As(err, &normalized) {
		t.Fatalf("error=%#v", err)
	}
	if normalized.Code != ErrorRateLimited || normalized.RetryAfter != 7*time.Second || normalized.SupplierRequestID != "ds-429" || normalized.Billing != BillingAmbiguous || normalized.SafeToRetry() {
		t.Fatalf("normalized error=%+v", normalized)
	}
	if strings.Contains(normalized.Error(), "raw supplier") {
		t.Fatal("raw supplier body escaped the adapter boundary")
	}
}

func TestDeepSeekStreamNormalizesTerminalUsage(t *testing.T) {
	body := "data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}],\"usage\":null}\n\n" +
		"data: {\"model\":\"deepseek-v4-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2,\"prompt_cache_hit_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	stream, err := NewDeepSeekAdapter(nil).OpenStream(context.Background(), &http.Response{StatusCode: http.StatusOK, Header: testHeaders(map[string]string{"Content-Type": "text/event-stream; charset=utf-8", "X-Request-ID": "stream-1"}), Body: io.NopCloser(strings.NewReader(body))})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next(context.Background())
	if err != nil || first.Type != StreamEventContent || first.TextDelta != "hel" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	terminal, err := stream.Next(context.Background())
	if err != nil || terminal.Type != StreamEventFinish || terminal.TextDelta != "lo" || terminal.FinishReason != "stop" || terminal.Usage == nil || terminal.Usage.State != UsageComplete || *terminal.Usage.CachedInput != 3 || terminal.SupplierRequestID != "stream-1" {
		t.Fatalf("terminal=%+v err=%v", terminal, err)
	}
	if _, err = stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("stream terminal err=%v", err)
	}
}

func TestDeepSeekStreamFailsClosedOnWrongModel(t *testing.T) {
	body := "data: {\"model\":\"deepseek-v4-pro\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"wrong\"},\"finish_reason\":null}]}\n\n"
	stream, err := NewDeepSeekAdapter(nil).OpenStream(context.Background(), &http.Response{StatusCode: http.StatusOK, Header: testHeaders(map[string]string{"Content-Type": "text/event-stream"}), Body: io.NopCloser(strings.NewReader(body))})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	_, err = stream.Next(context.Background())
	var normalized *Error
	if !errors.As(err, &normalized) || normalized.Code != ErrorProtocol || normalized.Billing != BillingAmbiguous || !normalized.ResponseStarted {
		t.Fatalf("error=%#v", err)
	}
}

func TestDeepSeekProbeUsesExactInventoryEndpoint(t *testing.T) {
	observedAt := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != DeepSeekBaseURL+"/models" || request.Header.Get("Authorization") != "Bearer probe-secret" {
			t.Fatalf("probe request=%s %s headers=%#v", request.Method, request.URL, request.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: testHeaders(map[string]string{"X-RateLimit-Remaining-Requests": "17"}), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"deepseek-v4-flash"},{"id":"deepseek-v4-pro"}]}`))}, nil
	})}
	adapter := NewDeepSeekAdapter(client)
	adapter.now = func() time.Time { return observedAt }
	observation, err := adapter.Probe(context.Background(), deepSeekTarget(), credentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("probe-secret"), nil }))
	if err != nil {
		t.Fatal(err)
	}
	if observation.Access != "authorized" || observation.Availability != "available" || observation.Health != "healthy" || !observation.ObservedAt.Equal(observedAt) || len(observation.Inventory) != 2 || observation.RateLimit.RequestsRemaining == nil || *observation.RateLimit.RequestsRemaining != 17 {
		t.Fatalf("observation=%+v", observation)
	}
}

func TestDeepSeekProbeNeverFollowsSupplierRedirect(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     testHeaders(map[string]string{"Location": "https://attacker.example/collect"}),
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    request,
		}, nil
	})}
	_, err := NewDeepSeekAdapter(client).Probe(context.Background(), deepSeekTarget(), credentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("probe-secret"), nil }))
	var normalized *Error
	if !errors.As(err, &normalized) || normalized.HTTPStatus != http.StatusFound || calls != 1 {
		t.Fatalf("error=%#v calls=%d", err, calls)
	}
}
