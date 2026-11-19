package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/routes"
)

// externalStream answers like a closed OpenAI-compatible API: with usage on
// a stream only when the request asked for it.
func externalStream(t *testing.T, seen *map[string]any) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripper(func(request *http.Request) (*http.Response, error) {
		var payload map[string]any
		body, _ := io.ReadAll(request.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("upstream body: %v", err)
		}
		*seen = payload
		stream := "data: {\"model\":\"gpt-4.1-mini\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n"
		if options, ok := payload["stream_options"].(map[string]any); ok && options["include_usage"] == true {
			stream += "data: {\"model\":\"gpt-4.1-mini\",\"choices\":[],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":32}}}\n\n"
		}
		stream += "data: [DONE]\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})}
}

func serveStream(t *testing.T, computeMode, requestBody string) (map[string]any, string, *captureRecorder) {
	t.Helper()
	var seen map[string]any
	directory := routes.New()
	directory.Put(routes.Snapshot{TargetID: "target-1", Alias: "alias", UpstreamModel: "gpt-4.1-mini", RouterURL: "https://api.openai.invalid/v1", Provider: "openai-compatible", Runtime: "openai-compatible", ComputeMode: computeMode, UpstreamAPIKey: "sk-sealed"})
	captured := &captureRecorder{}
	handler := (&Gateway{Routes: directory, APIKey: "secret", Client: externalStream(t, &seen), Recorder: captured}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	return seen, response.Body.String(), captured
}

func TestExternalStreamAsksForUsageAndHidesTheAddedChunk(t *testing.T) {
	seen, body, captured := serveStream(t, "external", `{"model":"alias","stream":true,"messages":[]}`)
	options, _ := seen["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("upstream stream_options=%v", seen["stream_options"])
	}
	if strings.Contains(body, `"usage":{`) || !strings.Contains(body, `"content":"hi"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("caller stream changed: %q", body)
	}
	captured.mu.Lock()
	record := captured.record
	captured.mu.Unlock()
	if record.InputTokens == nil || *record.InputTokens != 40 || record.OutputTokens == nil || *record.OutputTokens != 5 {
		t.Fatalf("usage not recorded: %+v", record)
	}
	if record.CachedInputTokens == nil || *record.CachedInputTokens != 32 {
		t.Fatalf("cached input tokens=%v", record.CachedInputTokens)
	}
}

func TestExternalStreamKeepsUsageTheCallerAskedFor(t *testing.T) {
	seen, body, _ := serveStream(t, "external", `{"model":"alias","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	options, _ := seen["stream_options"].(map[string]any)
	if options["include_usage"] != true || !strings.Contains(body, `"prompt_tokens":40`) {
		t.Fatalf("caller's usage chunk was removed: %q", body)
	}
}

func TestExternalStreamRespectsCallerDecliningUsage(t *testing.T) {
	seen, body, captured := serveStream(t, "external", `{"model":"alias","stream":true,"stream_options":{"include_usage":false},"messages":[]}`)
	options, _ := seen["stream_options"].(map[string]any)
	if options["include_usage"] != false || strings.Contains(body, `"usage":{`) {
		t.Fatalf("stream_options=%v body=%q", seen["stream_options"], body)
	}
	captured.mu.Lock()
	defer captured.mu.Unlock()
	if captured.record.InputTokens != nil {
		t.Fatalf("usage invented: %+v", captured.record)
	}
}

func TestOwnedStreamIsNotRewritten(t *testing.T) {
	seen, _, _ := serveStream(t, "elastic", `{"model":"alias","stream":true,"messages":[]}`)
	if _, present := seen["stream_options"]; present {
		t.Fatalf("owned route request rewritten: %v", seen)
	}
}

func TestAddedUsageFilterHandlesSplitEventsAndCRLF(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\r\n\r\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1}}\r\n\r\n" +
		": keep-alive\n\n" +
		"data: [DONE]"
	var out bytes.Buffer
	filter := &addedUsageFilter{w: &out}
	for _, piece := range []string{stream[:7], stream[7:40], stream[40:70], stream[70:]} {
		if _, err := filter.Write([]byte(piece)); err != nil {
			t.Fatal(err)
		}
	}
	if err := filter.flush(); err != nil {
		t.Fatal(err)
	}
	want := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\r\n\r\n: keep-alive\n\ndata: [DONE]"
	if out.String() != want {
		t.Fatalf("filtered=%q", out.String())
	}
}

func TestUsageIgnoresCachedTokensOutsideTheInput(t *testing.T) {
	usage := (responseObservation{body: []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":11}}}`)}).usage()
	if usage.cachedInput != nil {
		t.Fatalf("cached=%d exceeds input", *usage.cachedInput)
	}
	usage = (responseObservation{body: []byte(`{"usage":{"input_tokens":10,"output_tokens":2,"input_tokens_details":{"cached_tokens":4}}}`)}).usage()
	if usage.cachedInput == nil || *usage.cachedInput != 4 {
		t.Fatalf("responses cached tokens=%v", usage.cachedInput)
	}
}
