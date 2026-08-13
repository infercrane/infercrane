package curatedrecipe

import (
	"regexp"
	"testing"
)

func TestCatalogIsPinnedDistinctAndTruthful(t *testing.T) {
	sha := regexp.MustCompile(`^[a-f0-9]{40}$`)
	seen := map[string]bool{}
	for _, entry := range All() {
		if seen[entry.Name] || entry.Name == "" || entry.Model == "" || !sha.MatchString(entry.Revision) || entry.EvidenceClass != "configuration-only" || entry.Source == "" || entry.License == "" {
			t.Fatalf("invalid entry: %+v", entry)
		}
		seen[entry.Name] = true
	}
	if len(Search("embeddings")) != 1 || len(Search("vllm")) != len(All()) {
		t.Fatal("catalog search is not deterministic")
	}
}
