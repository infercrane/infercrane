package main

import "testing"

func TestResponseHelpers(t *testing.T) {
	value := map[string]any{
		"usage":   map[string]any{"prompt_tokens": float64(4), "completion_tokens": float64(2)},
		"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": `{"status":"ready"}`, "tool_calls": []any{map[string]any{"function": map[string]any{"name": "read_file", "arguments": `{"path":"src/cache.py"}`}}}}}},
	}
	if !positiveUsage(value) || finishReason(value) != "stop" || !structuredReady(value) || !expectedToolCall(value) {
		t.Fatalf("helpers rejected valid response: %#v", value)
	}
}

func TestPercentileUsesSortedInput(t *testing.T) {
	if got := percentile([]float64{1, 2, 3, 4, 5}, 0.95); got != 4 {
		t.Fatalf("p95=%v", got)
	}
}
