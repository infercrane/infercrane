package store

import "testing"

func TestNormalizeAdoptionURLTreatsOpenAIBaseAsServiceRoot(t *testing.T) {
	for _, raw := range []string{"https://inference.example.test", "https://inference.example.test/", "https://inference.example.test/v1", "https://inference.example.test/v1/"} {
		got, err := normalizeAdoptionURL(raw)
		if err != nil {
			t.Fatalf("normalize %q: %v", raw, err)
		}
		if got != "https://inference.example.test" {
			t.Fatalf("normalize %q = %q", raw, got)
		}
	}
}
