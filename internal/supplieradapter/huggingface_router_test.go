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

type huggingFaceCredentialResolverFunc func(context.Context, string) ([]byte, error)

func (f huggingFaceCredentialResolverFunc) Resolve(ctx context.Context, reference string) ([]byte, error) {
	return f(ctx, reference)
}

type huggingFaceRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f huggingFaceRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func huggingFaceTarget() Target {
	return Target{
		Supplier: HuggingFaceSupplier, BaseURL: HuggingFaceRouterBaseURL,
		SupplierModelID: "Qwen/Qwen3-8B:together", Region: "global", CredentialReference: "secret://hf/router", BillingPrincipal: "infercrane",
	}
}

func huggingFaceRequest() Request {
	maximum := 4096
	temperature := 0.3
	return Request{
		ID: "request-1", Operation: OperationChatCompletions, ModelID: "infercrane/qwen3-8b",
		Messages: []Message{
			{Role: "system", Content: []ContentPart{{Type: "text", Text: "Be concise."}}},
			{Role: "user", Content: []ContentPart{{Type: "text", Text: "Hello"}}},
		},
		MaxOutputTokens: &maximum, Temperature: &temperature, Stream: true,
	}
}

func huggingFaceResponseRequest(model string) *http.Request {
	repository, _, _ := splitPinnedHuggingFaceModel(model)
	ctx := context.WithValue(context.Background(), huggingFaceExpectedModelKey{}, huggingFaceExpectedModel{pinned: model, repository: repository})
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, HuggingFaceRouterBaseURL+"/chat/completions", nil)
	return request
}

func TestHuggingFaceRouterBuildRequestPinsExactProvider(t *testing.T) {
	credential := []byte("hf_secret")
	request, err := NewHuggingFaceRouterAdapter(nil).BuildRequest(context.Background(), huggingFaceTarget(), huggingFaceRequest(), huggingFaceCredentialResolverFunc(func(_ context.Context, reference string) ([]byte, error) {
		if reference != "secret://hf/router" {
			t.Fatalf("credential reference=%q", reference)
		}
		return credential, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if request.Method != http.MethodPost || request.URL.String() != HuggingFaceRouterBaseURL+"/chat/completions" {
		t.Fatalf("upstream request=%s %s", request.Method, request.URL)
	}
	if request.Header.Get("Authorization") != "Bearer hf_secret" || request.Header.Get("Accept") != "text/event-stream" || request.Header.Get("X-Request-ID") != "request-1" || request.Header.Get("X-HF-Bill-To") != "infercrane" {
		t.Fatalf("upstream headers=%#v", request.Header)
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
	streamOptions, _ := payload["stream_options"].(map[string]any)
	if payload["model"] != "Qwen/Qwen3-8B:together" || payload["stream"] != true || streamOptions["include_usage"] != true {
		t.Fatalf("wire payload=%s", body)
	}
	if strings.Contains(string(body), "infercrane/qwen3-8b") || strings.Contains(string(body), "hf_secret") {
		t.Fatalf("public identity or credential leaked into supplier body: %s", body)
	}
}

func TestHuggingFaceRouterRejectsAutomaticProviderPolicies(t *testing.T) {
	resolve := huggingFaceCredentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("secret"), nil })
	for _, model := range []string{
		"Qwen/Qwen3-8B", "Qwen/Qwen3-8B:fastest", "Qwen/Qwen3-8B:cheapest",
		"Qwen/Qwen3-8B:preferred", "Qwen/Qwen3-8B:auto", "Qwen/Qwen3-8B:together:fastest",
	} {
		t.Run(model, func(t *testing.T) {
			target := huggingFaceTarget()
			target.SupplierModelID = model
			_, err := NewHuggingFaceRouterAdapter(nil).BuildRequest(context.Background(), target, huggingFaceRequest(), resolve)
			var normalized *Error
			if !errors.As(err, &normalized) || normalized.Code != ErrorInvalidRequest || normalized.Billing != BillingNotTransmitted {
				t.Fatalf("model=%q error=%#v", model, err)
			}
		})
	}
}

func TestHuggingFaceRouterFailsClosedOutsideQualifiedMVP(t *testing.T) {
	resolve := huggingFaceCredentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("secret"), nil })
	tests := map[string]struct {
		target Target
		mutate func(*Request)
	}{
		"wrong host":           {target: Target{Supplier: HuggingFaceSupplier, BaseURL: "https://proxy.example/v1", SupplierModelID: "Qwen/Qwen3-8B:together", Region: "global", CredentialReference: "secret"}},
		"wrong path":           {target: Target{Supplier: HuggingFaceSupplier, BaseURL: "https://router.huggingface.co", SupplierModelID: "Qwen/Qwen3-8B:together", Region: "global", CredentialReference: "secret"}},
		"wrong region":         {target: Target{Supplier: HuggingFaceSupplier, BaseURL: HuggingFaceRouterBaseURL, SupplierModelID: "Qwen/Qwen3-8B:together", Region: "us-east", CredentialReference: "secret"}},
		"missing payer":        {target: Target{Supplier: HuggingFaceSupplier, BaseURL: HuggingFaceRouterBaseURL, SupplierModelID: "Qwen/Qwen3-8B:together", Region: "global", CredentialReference: "secret"}},
		"malformed payer":      {target: Target{Supplier: HuggingFaceSupplier, BaseURL: HuggingFaceRouterBaseURL, SupplierModelID: "Qwen/Qwen3-8B:together", Region: "global", CredentialReference: "secret", BillingPrincipal: "bad payer"}},
		"responses":            {target: huggingFaceTarget(), mutate: func(request *Request) { request.Operation = OperationResponses }},
		"missing output bound": {target: huggingFaceTarget(), mutate: func(request *Request) { request.MaxOutputTokens = nil }},
		"tools": {target: huggingFaceTarget(), mutate: func(request *Request) {
			request.Tools = []Tool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			request := huggingFaceRequest()
			if test.mutate != nil {
				test.mutate(&request)
			}
			_, err := NewHuggingFaceRouterAdapter(nil).BuildRequest(context.Background(), test.target, request, resolve)
			var normalized *Error
			if !errors.As(err, &normalized) || normalized.Code != ErrorInvalidRequest || normalized.SafeToRetry() {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestHuggingFaceRouterDecodeRequiresExactIdentityAndCompleteUsage(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Inference-Id": {"private-inference-id"}},
		Request:    huggingFaceResponseRequest("Qwen/Qwen3-8B:together"),
		Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl-1","model":"Qwen/Qwen3-8B",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":19,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":11},"completion_tokens_details":{"reasoning_tokens":2}}
		}`)),
	}
	decoded, err := NewHuggingFaceRouterAdapter(nil).DecodeResponse(context.Background(), response)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ID != "chatcmpl-1" || decoded.ModelID != "Qwen/Qwen3-8B:together" || decoded.SupplierRequestID != "private-inference-id" || decoded.Choices[0].Message.Content[0].Text != "hello" {
		t.Fatalf("decoded response=%+v", decoded)
	}
	if decoded.Usage.State != UsageComplete || *decoded.Usage.InputTokens != 19 || *decoded.Usage.OutputTokens != 5 || *decoded.Usage.CachedInput != 11 || *decoded.Usage.ReasoningTokens != 2 {
		t.Fatalf("decoded usage=%+v", decoded.Usage)
	}

	for name, body := range map[string]string{
		"wrong model":   `{"id":"chatcmpl-1","model":"Qwen/Other","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		"missing usage": `{"id":"chatcmpl-1","model":"Qwen/Qwen3-8B","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`,
		"invalid usage": `{"id":"chatcmpl-1","model":"Qwen/Qwen3-8B","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewHuggingFaceRouterAdapter(nil).DecodeResponse(context.Background(), &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Request: huggingFaceResponseRequest("Qwen/Qwen3-8B:together"), Body: io.NopCloser(strings.NewReader(body)),
			})
			var normalized *Error
			if !errors.As(err, &normalized) || normalized.Code != ErrorProtocol || normalized.Billing != BillingAmbiguous {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestHuggingFaceRouterReceivedErrorIsNoChargeAndRetryable(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": {"7"}, "Inference-Id": {"private-request"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"raw supplier detail must not escape"}}`)),
	}
	_, err := NewHuggingFaceRouterAdapter(nil).DecodeResponse(context.Background(), response)
	var normalized *Error
	if !errors.As(err, &normalized) {
		t.Fatalf("error=%#v", err)
	}
	if normalized.Code != ErrorRateLimited || normalized.Retry != RetryOtherOffer || normalized.RetryAfter != 7*time.Second || normalized.SupplierRequestID != "private-request" || normalized.Billing != BillingNoChargeConfirmed || !normalized.SafeToRetry() {
		t.Fatalf("normalized error=%+v", normalized)
	}
	if strings.Contains(normalized.Error(), "raw supplier") {
		t.Fatal("raw supplier body escaped the adapter boundary")
	}
}

func TestHuggingFaceRouterStreamRequiresFinishUsageAndDoneWithCRLF(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\r\n\r\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\r\n\r\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	stream, err := NewHuggingFaceRouterAdapter(nil).OpenStream(context.Background(), &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}, "Inference-Id": {"stream-1"}},
		Request: huggingFaceResponseRequest("Qwen/Qwen3-8B:together"), Body: io.NopCloser(strings.NewReader(body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next(context.Background())
	if err != nil || first.Type != StreamEventContent || first.TextDelta != "hel" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	finish, err := stream.Next(context.Background())
	if err != nil || finish.Type != StreamEventFinish || finish.TextDelta != "lo" || finish.FinishReason != "stop" {
		t.Fatalf("finish=%+v err=%v", finish, err)
	}
	usage, err := stream.Next(context.Background())
	if err != nil || usage.Type != StreamEventUsage || usage.Usage == nil || usage.Usage.State != UsageComplete || *usage.Usage.CachedInput != 3 || usage.SupplierRequestID != "stream-1" {
		t.Fatalf("usage=%+v err=%v", usage, err)
	}
	if _, err = stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal err=%v", err)
	}
}

func TestHuggingFaceRouterStreamFailsClosedWhenIncomplete(t *testing.T) {
	for name, body := range map[string]string{
		"missing usage":       "data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		"wrong model":         "data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Other\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"wrong\"},\"finish_reason\":null}]}\n\n",
		"usage before finish": "data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2}}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			stream, err := NewHuggingFaceRouterAdapter(nil).OpenStream(context.Background(), &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Request: huggingFaceResponseRequest("Qwen/Qwen3-8B:together"), Body: io.NopCloser(strings.NewReader(body)),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			for {
				_, err = stream.Next(context.Background())
				if err != nil {
					break
				}
			}
			var normalized *Error
			if !errors.As(err, &normalized) || normalized.Code != ErrorProtocol || normalized.Billing != BillingAmbiguous || !normalized.ResponseStarted {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestHuggingFaceRouterProbeProvesCredentialAndProviderBoundCompletion(t *testing.T) {
	client := &http.Client{Transport: huggingFaceRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != HuggingFaceRouterBaseURL+"/chat/completions" || request.Header.Get("Authorization") != "Bearer hf_secret" || request.Header.Get("Accept") != "application/json" || request.Header.Get("X-HF-Bill-To") != "infercrane" {
			t.Fatalf("probe request=%s headers=%#v", request.URL, request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err = json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "Qwen/Qwen3-8B:together" || payload["stream"] != false || payload["max_tokens"] != float64(huggingFaceProbeMaxOutputTokens) || payload["stream_options"] != nil {
			t.Fatalf("probe payload=%s", body)
		}
		header := make(http.Header)
		header.Set("X-RateLimit-Remaining-Requests", "17")
		header.Set("Inference-Id", "probe-1")
		header.Set("Content-Type", "application/json")
		return &http.Response{
			StatusCode: http.StatusOK, Header: header, Request: request,
			Body: io.NopCloser(strings.NewReader(`{"id":"probe-response","model":"Qwen/Qwen3-8B","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)),
		}, nil
	})}
	adapter := NewHuggingFaceRouterAdapter(client)
	observedAt := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	adapter.now = func() time.Time { return observedAt }
	observation, err := adapter.Probe(context.Background(), huggingFaceTarget(), huggingFaceCredentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("hf_secret"), nil }))
	if err != nil {
		t.Fatal(err)
	}
	if observation.Access != "authorized" || observation.Availability != "available" || observation.Health != "healthy" || !observation.ObservedAt.Equal(observedAt) || len(observation.Inventory) != 1 || observation.Inventory[0].SupplierModelID != "Qwen/Qwen3-8B:together" || observation.RateLimit.RequestsRemaining == nil || *observation.RateLimit.RequestsRemaining != 17 {
		t.Fatalf("observation=%+v", observation)
	}
}

func TestHuggingFaceRouterProbeRejectsInvalidCredentialViaCompletion(t *testing.T) {
	client := &http.Client{Transport: huggingFaceRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Request:    request,
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid token"}`)),
		}, nil
	})}
	_, err := NewHuggingFaceRouterAdapter(client).Probe(context.Background(), huggingFaceTarget(), huggingFaceCredentialResolverFunc(func(context.Context, string) ([]byte, error) {
		return []byte("hf_invalid"), nil
	}))
	var normalized *Error
	if !errors.As(err, &normalized) || normalized.Code != ErrorAuthentication || normalized.Billing != BillingNoChargeConfirmed || normalized.SafeToRetry() {
		t.Fatalf("error=%#v", err)
	}
}

func TestHuggingFaceRouterProbeTransportFailureKeepsBillingAmbiguous(t *testing.T) {
	client := &http.Client{Transport: huggingFaceRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}
	_, err := NewHuggingFaceRouterAdapter(client).Probe(context.Background(), huggingFaceTarget(), huggingFaceCredentialResolverFunc(func(context.Context, string) ([]byte, error) {
		return []byte("hf_secret"), nil
	}))
	var normalized *Error
	if !errors.As(err, &normalized) || normalized.Code != ErrorTransport || normalized.Billing != BillingAmbiguous || normalized.SafeToRetry() {
		t.Fatalf("error=%#v", err)
	}
}
