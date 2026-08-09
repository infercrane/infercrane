package metrics

import "testing"

func TestParse(t *testing.T) {
	m := Parse("vllm:num_requests_running 4\nvllm:prompt_tokens_total{model=\"a\"} 10\nvllm:prompt_tokens_total{model=\"b\"} 20\n")
	if m.RequestsRunning == nil || *m.RequestsRunning != 4 {
		t.Fatalf("running = %v", m.RequestsRunning)
	}
	if m.PromptTokensTotal == nil || *m.PromptTokensTotal != 30 {
		t.Fatalf("prompt = %v", m.PromptTokensTotal)
	}
	if m.PrefixCacheHits != nil {
		t.Fatalf("missing metric = %v", m.PrefixCacheHits)
	}
}
