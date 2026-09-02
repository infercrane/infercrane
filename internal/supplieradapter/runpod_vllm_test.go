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

type runPodCredentialResolverFunc func(context.Context, string) ([]byte, error)

func (f runPodCredentialResolverFunc) Resolve(ctx context.Context, reference string) ([]byte, error) {
	return f(ctx, reference)
}

type runPodRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f runPodRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func runPodHeaders(values map[string]string) http.Header {
	header := make(http.Header, len(values))
	for name, value := range values {
		header.Set(name, value)
	}
	return header
}

func runPodTarget() Target {
	return Target{
		Supplier: RunPodSupplier, BaseURL: "https://api.runpod.ai/v2/abc123/openai",
		SupplierModelID: "Qwen/Qwen3-8B", Region: "EU-RO-1", CredentialReference: "secret://runpod/catalog",
	}
}

func runPodRequest() Request {
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

func runPodResponseRequest(model string) *http.Request {
	ctx := context.WithValue(context.Background(), runPodExpectedModelKey{}, model)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.runpod.ai", nil)
	return request
}

func TestRunPodVLLMBuildRequestPinsQueueOpenAIContract(t *testing.T) {
	credential := []byte("runpod-secret")
	request, err := NewRunPodVLLMAdapter(nil).BuildRequest(context.Background(), runPodTarget(), runPodRequest(), runPodCredentialResolverFunc(func(_ context.Context, reference string) ([]byte, error) {
		if reference != "secret://runpod/catalog" {
			t.Fatalf("credential reference=%q", reference)
		}
		return credential, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if request.Method != http.MethodPost || request.URL.String() != "https://api.runpod.ai/v2/abc123/openai/v1/chat/completions" {
		t.Fatalf("upstream request=%s %s", request.Method, request.URL)
	}
	if request.Header.Get("Authorization") != "Bearer runpod-secret" || request.Header.Get("X-Request-ID") != "request-1" || request.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("upstream headers=%#v", request.Header)
	}
	if expected, ok := request.Context().Value(runPodExpectedModelKey{}).(string); !ok || expected != "Qwen/Qwen3-8B" {
		t.Fatalf("expected model context=%q ok=%t", expected, ok)
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
	if payload["model"] != "Qwen/Qwen3-8B" || payload["max_tokens"] != float64(4096) || payload["stream"] != true || streamOptions["include_usage"] != true {
		t.Fatalf("wire payload=%s", body)
	}
	if strings.Contains(string(body), "infercrane/qwen3") || strings.Contains(string(body), "runpod-secret") {
		t.Fatalf("public identity or credential leaked into supplier body: %s", body)
	}
}

func TestRunPodVLLMBuildRequestFailsClosedOutsideMVP(t *testing.T) {
	resolve := runPodCredentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("secret"), nil })
	tests := map[string]struct {
		target Target
		mutate func(*Request)
	}{
		"wrong host":           {target: Target{Supplier: RunPodSupplier, BaseURL: "https://proxy.example/v2/abc123/openai", SupplierModelID: "Qwen/Qwen3-8B", Region: "global", CredentialReference: "secret"}},
		"wrong endpoint path":  {target: Target{Supplier: RunPodSupplier, BaseURL: "https://api.runpod.ai/v2/abc123/runsync", SupplierModelID: "Qwen/Qwen3-8B", Region: "global", CredentialReference: "secret"}},
		"endpoint query":       {target: Target{Supplier: RunPodSupplier, BaseURL: "https://api.runpod.ai/v2/abc123/openai?redirect=1", SupplierModelID: "Qwen/Qwen3-8B", Region: "global", CredentialReference: "secret"}},
		"responses operation":  {target: runPodTarget(), mutate: func(request *Request) { request.Operation = OperationResponses }},
		"missing output bound": {target: runPodTarget(), mutate: func(request *Request) { request.MaxOutputTokens = nil }},
		"tool": {target: runPodTarget(), mutate: func(request *Request) {
			request.Tools = []Tool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}
		}},
		"image": {target: runPodTarget(), mutate: func(request *Request) {
			request.Messages[0].Content[0] = ContentPart{Type: "image", ImageURL: "https://example.test/image.png"}
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			request := runPodRequest()
			request.Messages = append([]Message(nil), request.Messages...)
			request.Messages[0].Content = append([]ContentPart(nil), request.Messages[0].Content...)
			if test.mutate != nil {
				test.mutate(&request)
			}
			_, err := NewRunPodVLLMAdapter(nil).BuildRequest(context.Background(), test.target, request, resolve)
			var normalized *Error
			if !errors.As(err, &normalized) || normalized.Code != ErrorInvalidRequest || normalized.Billing != BillingNotTransmitted || normalized.SafeToRetry() {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestRunPodVLLMBuildRequestRejectsUnsafeCredential(t *testing.T) {
	_, err := NewRunPodVLLMAdapter(nil).BuildRequest(context.Background(), runPodTarget(), runPodRequest(), runPodCredentialResolverFunc(func(context.Context, string) ([]byte, error) {
		return []byte("secret\r\ninjected: value"), nil
	}))
	var normalized *Error
	if !errors.As(err, &normalized) || normalized.Code != ErrorInternal || normalized.Billing != BillingNotTransmitted || normalized.ResponseStarted {
		t.Fatalf("error=%#v", err)
	}
}

func TestRunPodVLLMDecodeResponseRequiresExactIdentityAndCompleteUsage(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     runPodHeaders(map[string]string{"X-Runpod-Request-Id": "runpod-request-7"}),
		Request:    runPodResponseRequest("Qwen/Qwen3-8B"),
		Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl-1","model":"Qwen/Qwen3-8B",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":19,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":11},"completion_tokens_details":{"reasoning_tokens":2}}
		}`)),
	}
	decoded, err := NewRunPodVLLMAdapter(nil).DecodeResponse(context.Background(), response)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ID != "chatcmpl-1" || decoded.ModelID != "Qwen/Qwen3-8B" || decoded.SupplierRequestID != "runpod-request-7" || len(decoded.Choices) != 1 || decoded.Choices[0].Message.Content[0].Text != "hello" {
		t.Fatalf("decoded response=%+v", decoded)
	}
	if decoded.Usage.State != UsageComplete || *decoded.Usage.InputTokens != 19 || *decoded.Usage.OutputTokens != 5 || *decoded.Usage.CachedInput != 11 || *decoded.Usage.ReasoningTokens != 2 {
		t.Fatalf("decoded usage=%+v", decoded.Usage)
	}

	for name, body := range map[string]string{
		"wrong model":   `{"id":"chatcmpl-1","model":"Qwen/Other","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		"missing usage": `{"id":"chatcmpl-1","model":"Qwen/Qwen3-8B","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewRunPodVLLMAdapter(nil).DecodeResponse(context.Background(), &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: runPodResponseRequest("Qwen/Qwen3-8B"), Body: io.NopCloser(strings.NewReader(body))})
			var normalized *Error
			if !errors.As(err, &normalized) || normalized.Code != ErrorProtocol || normalized.Billing != BillingAmbiguous {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestRunPodVLLMHTTPErrorIsSanitizedAndAmbiguous(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     runPodHeaders(map[string]string{"Retry-After": "7", "X-Runpod-Job-Id": "job-429"}),
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"raw supplier detail must not escape"}}`)),
	}
	_, err := NewRunPodVLLMAdapter(nil).DecodeResponse(context.Background(), response)
	var normalized *Error
	if !errors.As(err, &normalized) {
		t.Fatalf("error=%#v", err)
	}
	if normalized.Code != ErrorRateLimited || normalized.Retry != RetryOtherOffer || normalized.RetryAfter != 7*time.Second || normalized.SupplierRequestID != "job-429" || normalized.Billing != BillingAmbiguous || normalized.SafeToRetry() {
		t.Fatalf("normalized error=%+v", normalized)
	}
	if strings.Contains(normalized.Error(), "raw supplier") {
		t.Fatal("raw supplier body escaped the adapter boundary")
	}
}

func TestRunPodVLLMStreamRequiresFinishUsageAndDone(t *testing.T) {
	body := "data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\n" +
		"data: [DONE]\n\n"
	stream, err := NewRunPodVLLMAdapter(nil).OpenStream(context.Background(), &http.Response{
		StatusCode: http.StatusOK,
		Header:     runPodHeaders(map[string]string{"Content-Type": "text/event-stream; charset=utf-8", "X-Runpod-Request-Id": "stream-1"}),
		Request:    runPodResponseRequest("Qwen/Qwen3-8B"), Body: io.NopCloser(strings.NewReader(body)),
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

func TestRunPodVLLMStreamFailsClosedOnIncompleteOrWrongIdentity(t *testing.T) {
	tests := map[string]string{
		"missing usage":       "data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
		"truncated":           "data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n",
		"wrong model":         "data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Other\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"wrong\"},\"finish_reason\":null}]}\n\n",
		"usage before finish": "data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2}}\n\n",
		"choice after finish": "data: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"chatcmpl-1\",\"model\":\"Qwen/Qwen3-8B\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"late\"},\"finish_reason\":null}]}\n\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			stream, err := NewRunPodVLLMAdapter(nil).OpenStream(context.Background(), &http.Response{StatusCode: http.StatusOK, Header: runPodHeaders(map[string]string{"Content-Type": "text/event-stream"}), Request: runPodResponseRequest("Qwen/Qwen3-8B"), Body: io.NopCloser(strings.NewReader(body))})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			var normalized *Error
			for {
				_, err = stream.Next(context.Background())
				if err != nil {
					break
				}
			}
			if !errors.As(err, &normalized) || normalized.Code != ErrorProtocol || normalized.Billing != BillingAmbiguous || !normalized.ResponseStarted {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestRunPodVLLMProbeUsesExactModelsEndpoint(t *testing.T) {
	observedAt := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	client := &http.Client{Transport: runPodRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.String() != "https://api.runpod.ai/v2/abc123/openai/v1/models" || request.Header.Get("Authorization") != "Bearer probe-secret" {
			t.Fatalf("probe request=%s %s headers=%#v", request.Method, request.URL, request.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"Qwen/Qwen3-8B"}]}`)), Request: request}, nil
	})}
	adapter := NewRunPodVLLMAdapter(client)
	adapter.now = func() time.Time { return observedAt }
	observation, err := adapter.Probe(context.Background(), runPodTarget(), runPodCredentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("probe-secret"), nil }))
	if err != nil {
		t.Fatal(err)
	}
	if observation.Access != "authorized" || observation.Availability != "available" || observation.Health != "healthy" || !observation.ObservedAt.Equal(observedAt) || len(observation.Inventory) != 1 || observation.Inventory[0].Region != "EU-RO-1" {
		t.Fatalf("observation=%+v", observation)
	}
}

func TestRunPodVLLMProbeNeverFollowsRedirect(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: runPodRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusFound, Header: runPodHeaders(map[string]string{"Location": "https://attacker.example/collect"}), Body: io.NopCloser(strings.NewReader("redirect")), Request: request}, nil
	})}
	_, err := NewRunPodVLLMAdapter(client).Probe(context.Background(), runPodTarget(), runPodCredentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("probe-secret"), nil }))
	var normalized *Error
	if !errors.As(err, &normalized) || normalized.HTTPStatus != http.StatusFound || calls != 1 {
		t.Fatalf("error=%#v calls=%d", err, calls)
	}
}
