package main

import "testing"

func TestResponseHelpers(t *testing.T) {
	value := map[string]any{
		"usage":   map[string]any{"prompt_tokens": float64(4), "completion_tokens": float64(2)},
		"choices": []any{map[string]any{"finish_reason": "stop", "message": map[string]any{"content": `{"status":"ready"}`, "reasoning_content": "thinking", "tool_calls": []any{map[string]any{"function": map[string]any{"name": "read_file", "arguments": `{"path":"src/cache.py"}`}}}}}},
	}
	if !positiveUsage(value) || finishReason(value) != "stop" || !structuredReady(value) || !expectedToolCall(value) {
		t.Fatalf("helpers rejected valid response: %#v", value)
	}
	reasoning := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "42", "reasoning_content": "17 + 25"}}}}
	if !reasoningReady(reasoning) {
		t.Fatalf("reasoning helper rejected valid response: %#v", reasoning)
	}
}

func TestPercentileUsesSortedInput(t *testing.T) {
	if got := percentile([]float64{1, 2, 3, 4, 5}, 0.95); got != 4 {
		t.Fatalf("p95=%v", got)
	}
}

func TestDeltaHasOutput(t *testing.T) {
	tests := []struct {
		name  string
		delta map[string]any
		want  bool
	}{
		{name: "empty", delta: map[string]any{}, want: false},
		{name: "role only", delta: map[string]any{"role": "assistant"}, want: false},
		{name: "content", delta: map[string]any{"content": "ready"}, want: true},
		{name: "reasoning", delta: map[string]any{"reasoning": "thinking"}, want: true},
		{name: "reasoning content", delta: map[string]any{"reasoning_content": "thinking"}, want: true},
		{name: "tool call", delta: map[string]any{"tool_calls": []any{map[string]any{"index": 0}}}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := deltaHasOutput(test.delta); got != test.want {
				t.Fatalf("deltaHasOutput() = %v, want %v", got, test.want)
			}
		})
	}
}
