package curatedrecipe

import (
	"regexp"
	"testing"
)

func TestCatalogIsPinnedDistinctAndTruthful(t *testing.T) {
	sha := regexp.MustCompile(`^[a-f0-9]{40}$`)
	seen := map[string]bool{}
	for _, entry := range All() {
		if seen[entry.Name] || entry.Name == "" || entry.DisplayName == "" || entry.Publisher == "" || entry.Model == "" || !sha.MatchString(entry.Revision) || entry.EvidenceClass != configurationEvidence || entry.EvidenceSummary == "" || entry.Source == "" || entry.License == "" || entry.LicenseURL == "" || len(entry.Tasks) == 0 || len(entry.Capabilities) == 0 || len(entry.Profiles) == 0 {
			t.Fatalf("invalid entry: %+v", entry)
		}
		profiles := map[string]bool{}
		for _, profile := range entry.Profiles {
			if profile.Name == "" || profiles[profile.Name] || profile.Runtime == "" || profile.ComputeMode == "" || profile.MinReplicas < 0 || profile.MaxReplicas < max(1, profile.MinReplicas) || profile.EvidenceClass != configurationEvidence || profile.QualificationScope == "" {
				t.Fatalf("invalid profile for %s: %+v", entry.Name, profile)
			}
			profiles[profile.Name] = true
		}
		seen[entry.Name] = true
	}
	if len(Search("embeddings")) != 1 || len(Search("vllm")) != len(All()) || len(Search("Mistral AI")) != 1 {
		t.Fatal("catalog search is not deterministic")
	}
}

func TestCatalogResultsCannotMutateReviewedSource(t *testing.T) {
	entries := All()
	entries[0].Tasks[0] = "changed"
	entries[0].Profiles[0].Limitations[0] = "changed"

	fresh, ok := Get(entries[0].Name)
	if !ok {
		t.Fatal("reviewed catalog entry disappeared")
	}
	if fresh.Tasks[0] == "changed" || fresh.Profiles[0].Limitations[0] == "changed" {
		t.Fatal("catalog returned mutable aliases to reviewed source data")
	}
	if fresh.RuntimeArgs == nil || fresh.Profiles[0].RuntimeArgs == nil {
		t.Fatal("catalog arrays must serialize as [] instead of null")
	}
}

func TestCatalogCoversCommonApplicationWorkloads(t *testing.T) {
	expected := map[string]string{
		"qwen2.5-coder-7b-instruct":   "coding",
		"deepseek-r1-distill-qwen-7b": "reasoning",
		"gemma-3-4b-it":               "vision",
		"qwen2.5-vl-7b-instruct":      "vision",
		"granite-3.3-8b-instruct":     "extraction",
		"bge-m3-embeddings":           "embeddings",
	}
	for name, task := range expected {
		entry, ok := Get(name)
		if !ok {
			t.Fatalf("reviewed starting point %q is missing", name)
		}
		if !contains(entry.Tasks, task) {
			t.Fatalf("%s does not declare task %q: %v", name, task, entry.Tasks)
		}
	}
	vision, _ := Get("qwen2.5-vl-7b-instruct")
	if !contains(vision.InputModalities, "image") || !contains(vision.Capabilities, "vision") {
		t.Fatalf("vision template lacks protocol disclosure: %+v", vision)
	}
}

func TestGenerationRecipesExposeUnclaimedPerformanceCandidates(t *testing.T) {
	entry, ok := Get("mistral-7b-instruct")
	if !ok {
		t.Fatal("mistral recipe missing")
	}
	wanted := map[string]string{
		"vllm-balanced":    "",
		"vllm-interactive": "2048",
		"vllm-throughput":  "16384",
	}
	if len(entry.Profiles) != len(wanted) {
		t.Fatalf("profiles=%d, want %d: %+v", len(entry.Profiles), len(wanted), entry.Profiles)
	}
	for _, profile := range entry.Profiles {
		expected, found := wanted[profile.Name]
		if !found || profile.EvidenceClass != configurationEvidence || profile.QualificationScope == "" {
			t.Fatalf("unexpected profile: %+v", profile)
		}
		if !contains(profile.RuntimeArgs, "--enable-prefix-caching") {
			t.Fatalf("profile does not preserve prefix-cache candidate: %+v", profile)
		}
		if expected != "" && !contains(profile.RuntimeArgs, expected) {
			t.Fatalf("profile %s missing token budget %s: %v", profile.Name, expected, profile.RuntimeArgs)
		}
	}
	embeddings, _ := Get("bge-m3-embeddings")
	if len(embeddings.Profiles) != 1 || contains(embeddings.Profiles[0].RuntimeArgs, "--enable-prefix-caching") {
		t.Fatalf("generation tuning leaked into embedding recipe: %+v", embeddings.Profiles)
	}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
