package supplieradapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func runPodSGLangLBTarget() Target {
	return Target{
		Supplier: RunPodSupplier, BaseURL: "https://qwen38pilot.api.runpod.ai",
		SupplierModelID: RunPodQwen38SupplierModelID, Region: "EU-RO-1", CredentialReference: "secret://runpod/qwen38",
	}
}

func runPodQwen38Request() Request {
	maximum := 512
	return Request{
		ID: "request-qwen38", Operation: OperationChatCompletions, ModelID: "qwen3.8-27b-fp8",
		Messages:        []Message{{Role: "user", Content: []ContentPart{{Type: "text", Text: "Hello"}}}},
		MaxOutputTokens: &maximum, Stream: true,
	}
}

func TestRunPodSGLangLBBuildRequestPinsDirectEndpointAndExactRecipe(t *testing.T) {
	credential := []byte("runpod-secret")
	request, err := NewRunPodSGLangLBAdapter(nil).BuildRequest(context.Background(), runPodSGLangLBTarget(), runPodQwen38Request(), runPodCredentialResolverFunc(func(_ context.Context, reference string) ([]byte, error) {
		if reference != "secret://runpod/qwen38" {
			t.Fatalf("credential reference=%q", reference)
		}
		return credential, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if request.URL.String() != "https://qwen38pilot.api.runpod.ai/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer runpod-secret" {
		t.Fatalf("request=%s headers=%#v", request.URL, request.Header)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != RunPodQwen38SupplierModelID || payload["max_tokens"] != float64(512) || payload["stream"] != true {
		t.Fatalf("payload=%s", body)
	}
	for index, value := range credential {
		if value != 0 {
			t.Fatalf("resolved credential byte %d was not cleared", index)
		}
	}
}

func TestRunPodSGLangLBRejectsQueueEndpointsModelsAndUnqualifiedOutput(t *testing.T) {
	resolve := runPodCredentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("secret"), nil })
	tests := map[string]struct {
		target Target
		mutate func(*Request)
	}{
		"queue endpoint":         {target: Target{Supplier: RunPodSupplier, BaseURL: "https://api.runpod.ai/v2/abc/openai", SupplierModelID: RunPodQwen38SupplierModelID, Region: "EU-RO-1", CredentialReference: "secret"}},
		"foreign host":           {target: Target{Supplier: RunPodSupplier, BaseURL: "https://runpod.example", SupplierModelID: RunPodQwen38SupplierModelID, Region: "EU-RO-1", CredentialReference: "secret"}},
		"wrong model":            {target: Target{Supplier: RunPodSupplier, BaseURL: "https://qwen38pilot.api.runpod.ai", SupplierModelID: "Qwen/Qwen3-8B", Region: "EU-RO-1", CredentialReference: "secret"}},
		"above qualified output": {target: runPodSGLangLBTarget(), mutate: func(request *Request) { value := 513; request.MaxOutputTokens = &value }},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			request := runPodQwen38Request()
			if test.mutate != nil {
				test.mutate(&request)
			}
			_, err := NewRunPodSGLangLBAdapter(nil).BuildRequest(context.Background(), test.target, request, resolve)
			var normalized *Error
			if !errors.As(err, &normalized) || normalized.Code != ErrorInvalidRequest || normalized.Billing != BillingNotTransmitted {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestRunPodSGLangLBNormalizesBufferedResponse(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     runPodHeaders(map[string]string{"X-Runpod-Request-Id": "lb-1"}),
		Request:    runPodResponseRequest(RunPodQwen38SupplierModelID),
		Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","model":"Qwen/Qwen3.8-27B-FP8","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":2}}`)),
	}
	decoded, err := NewRunPodSGLangLBAdapter(nil).DecodeResponse(context.Background(), response)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SupplierRequestID != "lb-1" || decoded.ModelID != RunPodQwen38SupplierModelID || *decoded.Usage.InputTokens != 7 || *decoded.Usage.OutputTokens != 2 {
		t.Fatalf("decoded=%+v", decoded)
	}
}

func TestRunPodSGLangLBProbeUsesDirectModelsPath(t *testing.T) {
	client := &http.Client{Transport: runPodRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://qwen38pilot.api.runpod.ai/v1/models" || request.Header.Get("Authorization") != "Bearer runpod-secret" {
			t.Fatalf("probe=%s headers=%#v", request.URL, request.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":[{"id":"Qwen/Qwen3.8-27B-FP8"}]}`)), Request: request}, nil
	})}
	observation, err := NewRunPodSGLangLBAdapter(client).Probe(context.Background(), runPodSGLangLBTarget(), runPodCredentialResolverFunc(func(context.Context, string) ([]byte, error) { return []byte("runpod-secret"), nil }))
	if err != nil {
		t.Fatal(err)
	}
	if observation.Availability != "available" || observation.Health != "healthy" || len(observation.Inventory) != 1 {
		t.Fatalf("observation=%+v", observation)
	}
}
