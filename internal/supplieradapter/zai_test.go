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

func zaiTarget(model string) Target {
	return Target{Supplier: ZAISupplier, BaseURL: ZAIBaseURL, SupplierModelID: model, Region: "global", CredentialReference: "secret://zai/mvp"}
}

func zaiRequest(stream bool) Request {
	maximum := 4096
	temperature := 0.4
	return Request{
		ID: "request-1", Operation: OperationChatCompletions, ModelID: "infercrane/glm",
		Messages: []Message{
			{Role: "system", Content: []ContentPart{{Type: "text", Text: "Be concise."}}},
			{Role: "user", Content: []ContentPart{{Type: "text", Text: "Hello"}}},
		},
		MaxOutputTokens: &maximum, Temperature: &temperature, Stream: stream,
	}
}

func zaiUpstreamRequest(t *testing.T, model string, stream bool) *http.Request {
	t.Helper()
	request, err := NewZAIAdapter(nil).BuildRequest(context.Background(), zaiTarget(model), zaiRequest(stream), credentialResolverFunc(func(context.Context, string) ([]byte, error) {
		return []byte("test-secret"), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestZAIBuildRequestPinsPayAsYouGoWireContractForEveryModel(t *testing.T) {
	for _, model := range []string{ZAIGLM52ModelID, ZAIGLM53ModelID, ZAIGLM53FlashModelID} {
		t.Run(model, func(t *testing.T) {
			credential := []byte("supplier-secret")
			request, err := NewZAIAdapter(nil).BuildRequest(context.Background(), zaiTarget(model), zaiRequest(true), credentialResolverFunc(func(_ context.Context, reference string) ([]byte, error) {
				if reference != "secret://zai/mvp" {
					t.Fatalf("credential reference=%q", reference)
				}
				return credential, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			if request.Method != http.MethodPost || request.URL.String() != ZAIBaseURL+"/chat/completions" {
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
			thinking, _ := payload["thinking"].(map[string]any)
			if payload["model"] != model || payload["max_tokens"] != float64(4096) || payload["stream"] != true || thinking["type"] != "enabled" {
				t.Fatalf("wire contract=%s", body)
			}
			if _, exists := payload["stream_options"]; exists {
				t.Fatalf("undocumented stream_options escaped into Z.ai payload: %s", body)
			}
			if strings.Contains(string(body), "infercrane/glm") || strings.Contains(string(body), "supplier-secret") {
				t.Fatalf("public identity or credential leaked into supplier body: %s", body)
			}
		})
	}
}

func TestZAIBuildRequestFailsClosedOutsideQualifiedSubset(t *testing.T) {
	valid := zaiRequest(false)
	resolve := credentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("secret"), nil })
	tests := map[string]struct {
		target Target
		mutate func(*Request)
	}{
		"other supplier": {target: Target{Supplier: "other", BaseURL: ZAIBaseURL, SupplierModelID: ZAIGLM53ModelID, CredentialReference: "secret"}},
		"unknown model":  {target: zaiTarget("glm-latest")},
		"coding endpoint": {target: Target{
			Supplier: ZAISupplier, BaseURL: "https://api.z.ai/api/coding/paas/v4", SupplierModelID: ZAIGLM53ModelID, CredentialReference: "secret",
		}},
		"trailing slash": {target: Target{Supplier: ZAISupplier, BaseURL: ZAIBaseURL + "/", SupplierModelID: ZAIGLM53ModelID, CredentialReference: "secret"}},
		"lookalike host": {target: Target{Supplier: ZAISupplier, BaseURL: "https://api.z.ai.example/api/paas/v4", SupplierModelID: ZAIGLM53ModelID, CredentialReference: "secret"}},
		"responses operation": {target: zaiTarget(ZAIGLM53ModelID), mutate: func(request *Request) {
			request.Operation = OperationResponses
		}},
		"tool": {target: zaiTarget(ZAIGLM53ModelID), mutate: func(request *Request) {
			request.Tools = []Tool{{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}
		}},
		"image": {target: zaiTarget(ZAIGLM53ModelID), mutate: func(request *Request) {
			request.Messages[0].Content[0] = ContentPart{Type: "image", ImageURL: "https://example.test/image.png"}
		}},
		"unbounded output": {target: zaiTarget(ZAIGLM53ModelID), mutate: func(request *Request) {
			tooMany := ZAIMVPMaxOutputTokens + 1
			request.MaxOutputTokens = &tooMany
		}},
		"supplier temperature limit": {target: zaiTarget(ZAIGLM53ModelID), mutate: func(request *Request) {
			tooHigh := 1.01
			request.Temperature = &tooHigh
		}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			request := valid
			request.Messages = append([]Message(nil), valid.Messages...)
			request.Messages[0].Content = append([]ContentPart(nil), valid.Messages[0].Content...)
			if test.mutate != nil {
				test.mutate(&request)
			}
			_, err := NewZAIAdapter(nil).BuildRequest(context.Background(), test.target, request, resolve)
			var normalized *Error
			if !errors.As(err, &normalized) || normalized.Code != ErrorInvalidRequest || normalized.Billing != BillingNotTransmitted || normalized.SafeToRetry() {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestZAIBuildRequestRejectsUnsafeCredentialBeforeTransmission(t *testing.T) {
	_, err := NewZAIAdapter(nil).BuildRequest(context.Background(), zaiTarget(ZAIGLM53ModelID), zaiRequest(false), credentialResolverFunc(func(context.Context, string) ([]byte, error) {
		return []byte("secret\r\ninjected: value"), nil
	}))
	var normalized *Error
	if !errors.As(err, &normalized) || normalized.Code != ErrorInternal || normalized.Billing != BillingNotTransmitted || normalized.ResponseStarted {
		t.Fatalf("error=%#v", err)
	}
}

func TestZAIDecodeResponseNormalizesSupplierIdentityUsageCacheAndFinishReason(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     testHeaders(map[string]string{"Content-Type": "application/json; charset=utf-8"}),
		Request:    zaiUpstreamRequest(t, ZAIGLM53ModelID, false),
		Body: io.NopCloser(strings.NewReader(`{
			"id":"chatcmpl-1","request_id":"zai-request-7","model":"glm-5.3",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello","reasoning_content":"private"},"finish_reason":"model_context_window_exceeded"}],
			"usage":{"prompt_tokens":19,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":11}}
		}`)),
	}
	decoded, err := NewZAIAdapter(nil).DecodeResponse(context.Background(), response)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ID != "chatcmpl-1" || decoded.ModelID != ZAIGLM53ModelID || decoded.SupplierRequestID != "zai-request-7" || len(decoded.Choices) != 1 || decoded.Choices[0].Message.Content[0].Text != "hello" || decoded.Choices[0].FinishReason != "length" {
		t.Fatalf("decoded response=%+v", decoded)
	}
	if decoded.Usage.State != UsageComplete || *decoded.Usage.InputTokens != 19 || *decoded.Usage.OutputTokens != 5 || *decoded.Usage.CachedInput != 11 {
		t.Fatalf("decoded usage=%+v", decoded.Usage)
	}
}

func TestZAIDecodeResponseRequiresExactModelCompleteUsageAndRequestIdentity(t *testing.T) {
	tests := map[string]string{
		"wrong model":         `{"id":"one","model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		"partial usage":       `{"id":"one","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1}}`,
		"cache exceeds input": `{"id":"one","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`,
		"network finish":      `{"id":"one","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"network_error"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			response := &http.Response{StatusCode: http.StatusOK, Header: testHeaders(map[string]string{"Content-Type": "application/json"}), Request: zaiUpstreamRequest(t, ZAIGLM53ModelID, false), Body: io.NopCloser(strings.NewReader(body))}
			_, err := NewZAIAdapter(nil).DecodeResponse(context.Background(), response)
			var normalized *Error
			if !errors.As(err, &normalized) || normalized.Code != ErrorProtocol || normalized.Billing != BillingAmbiguous {
				t.Fatalf("error=%#v", err)
			}
		})
	}

	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     testHeaders(map[string]string{"Content-Type": "application/json", "X-Request-ID": "header-id"}),
		Request:    zaiUpstreamRequest(t, ZAIGLM53ModelID, false),
		Body:       io.NopCloser(strings.NewReader(`{"id":"one","request_id":"body-id","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)),
	}
	if _, err := NewZAIAdapter(nil).DecodeResponse(context.Background(), response); err == nil {
		t.Fatal("conflicting supplier request identities were accepted")
	}
}

func TestZAIHTTPErrorIsSanitizedAmbiguousAndNeverRetryable(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     testHeaders(map[string]string{"Retry-After": "7", "X-Request-ID": "zai-429"}),
		Body:       io.NopCloser(strings.NewReader(`{"error":{"code":"1302","message":"raw supplier detail must not escape"}}`)),
	}
	_, err := NewZAIAdapter(nil).DecodeResponse(context.Background(), response)
	var normalized *Error
	if !errors.As(err, &normalized) {
		t.Fatalf("error=%#v", err)
	}
	if normalized.Code != ErrorRateLimited || normalized.Retry != RetryNever || normalized.RetryAfter != 7*time.Second || normalized.SupplierRequestID != "zai-429" || normalized.Billing != BillingAmbiguous || normalized.SafeToRetry() {
		t.Fatalf("normalized error=%+v", normalized)
	}
	if strings.Contains(normalized.Error(), "raw supplier") || strings.Contains(normalized.Error(), "1302") {
		t.Fatal("raw supplier body escaped the adapter boundary")
	}
}

func TestZAIStreamNormalizesTerminalUsageAndCRLF(t *testing.T) {
	body := "data: {\"id\":\"stream-response\",\"request_id\":\"zai-stream-1\",\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hel\",\"reasoning_content\":\"hidden\"},\"finish_reason\":null}]}\r\n\r\n" +
		"data: {\"id\":\"stream-response\",\"request_id\":\"zai-stream-1\",\"model\":\"glm-5.3-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	stream, err := NewZAIAdapter(nil).OpenStream(context.Background(), &http.Response{
		StatusCode: http.StatusOK,
		Header:     testHeaders(map[string]string{"Content-Type": "text/event-stream; charset=utf-8"}),
		Request:    zaiUpstreamRequest(t, ZAIGLM53FlashModelID, true),
		Body:       io.NopCloser(strings.NewReader(body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Next(context.Background())
	if err != nil || first.Type != StreamEventContent || first.TextDelta != "hel" || first.SupplierRequestID != "zai-stream-1" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	terminal, err := stream.Next(context.Background())
	if err != nil || terminal.Type != StreamEventFinish || terminal.TextDelta != "lo" || terminal.FinishReason != "stop" || terminal.Usage == nil || terminal.Usage.State != UsageComplete || *terminal.Usage.CachedInput != 3 {
		t.Fatalf("terminal=%+v err=%v", terminal, err)
	}
	if _, err = stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("stream terminal err=%v", err)
	}
}

func TestZAIStreamFailsClosedOnIncompleteOrChangedContract(t *testing.T) {
	tests := map[string]string{
		"early EOF":     "data: {\"id\":\"one\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
		"wrong model":   "data: {\"id\":\"one\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\n",
		"changed id":    "data: {\"id\":\"one\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"two\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"y\"},\"finish_reason\":null}]}\n\n",
		"missing usage": "data: {\"id\":\"one\",\"model\":\"glm-5.3\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			stream, err := NewZAIAdapter(nil).OpenStream(context.Background(), &http.Response{
				StatusCode: http.StatusOK, Header: testHeaders(map[string]string{"Content-Type": "text/event-stream"}),
				Request: zaiUpstreamRequest(t, ZAIGLM53ModelID, true), Body: io.NopCloser(strings.NewReader(body)),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			var streamErr error
			for streamErr == nil {
				_, streamErr = stream.Next(context.Background())
			}
			var normalized *Error
			if !errors.As(streamErr, &normalized) || normalized.Code != ErrorProtocol || normalized.Billing != BillingAmbiguous || !normalized.ResponseStarted {
				t.Fatalf("error=%#v", streamErr)
			}
		})
	}
}

func TestZAIProbeProvesExactModelWithMinimalNonStreamingCompletion(t *testing.T) {
	observedAt := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.URL.String() != ZAIBaseURL+"/chat/completions" || request.Header.Get("Authorization") != "Bearer probe-secret" || request.Header.Get("Accept") != "application/json" {
			t.Fatalf("probe request=%s %s headers=%#v", request.Method, request.URL, request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err = json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != ZAIGLM52ModelID || payload["max_tokens"] != float64(1) || payload["stream"] != false {
			t.Fatalf("probe payload=%s", body)
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: testHeaders(map[string]string{"Content-Type": "application/json"}), Request: request,
			Body: io.NopCloser(strings.NewReader(`{"id":"probe-1","request_id":"supplier-probe-1","model":"glm-5.2","choices":[{"index":0,"message":{"role":"assistant","content":"."},"finish_reason":"length"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":0}}}`)),
		}, nil
	})}
	adapter := NewZAIAdapter(client)
	adapter.now = func() time.Time { return observedAt }
	observation, err := adapter.Probe(context.Background(), zaiTarget(ZAIGLM52ModelID), credentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("probe-secret"), nil }))
	if err != nil {
		t.Fatal(err)
	}
	if observation.Access != "authorized" || observation.Availability != "available" || observation.Health != "healthy" || !observation.ObservedAt.Equal(observedAt) || len(observation.Inventory) != 1 || observation.Inventory[0].SupplierModelID != ZAIGLM52ModelID {
		t.Fatalf("observation=%+v", observation)
	}
}

func TestZAIProbeNeverFollowsSupplierRedirect(t *testing.T) {
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
	_, err := NewZAIAdapter(client).Probe(context.Background(), zaiTarget(ZAIGLM53ModelID), credentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("probe-secret"), nil }))
	var normalized *Error
	if !errors.As(err, &normalized) || normalized.HTTPStatus != http.StatusFound || normalized.Billing != BillingAmbiguous || normalized.Retry != RetryNever || calls != 1 {
		t.Fatalf("error=%#v calls=%d", err, calls)
	}
}
