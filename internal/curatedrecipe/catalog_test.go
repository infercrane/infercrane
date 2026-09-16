package curatedrecipe

import (
	"regexp"
	"strings"
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
			if profile.Name == "" || profiles[profile.Name] || profile.Runtime == "" || profile.ComputeMode == "" || profile.GPUCount < 1 || profile.GPUCount > 1024 || profile.MinReplicas < 0 || profile.MaxReplicas < max(1, profile.MinReplicas) || profile.EvidenceClass != configurationEvidence || profile.QualificationScope == "" {
				t.Fatalf("invalid profile for %s: %+v", entry.Name, profile)
			}
			if len(profile.CompatibleGPUs) > 0 && !contains(profile.CompatibleGPUs, profile.GPUHint) {
				t.Fatalf("profile GPU hint is outside its reviewed compatibility set: %+v", profile)
			}
			if !profile.Workload.Empty() {
				if err := profile.Workload.Validate(); err != nil {
					t.Fatalf("invalid workload for %s/%s: %v", entry.Name, profile.Name, err)
				}
				if profile.Runtime != "custom-oci" || profile.RuntimeVersion == "" {
					t.Fatalf("portable workload must declare a custom-oci runtime version: %+v", profile)
				}
			}
			profiles[profile.Name] = true
		}
		seen[entry.Name] = true
	}
	if len(Search("embeddings")) != 1 || len(Search("vllm")) == 0 || len(Search("Mistral AI")) != 1 {
		t.Fatal("catalog search is not deterministic")
	}
}

func TestFrontierProfilesUseTheGeneralImmutableWorkloadContract(t *testing.T) {
	tests := []struct {
		name       string
		revision   string
		gpuCount   int
		image      string
		commandArg string
	}{
		{name: "glm-5.3-flash", revision: "3f1971b7b5f7a528c9c4ef6212c8785298a8c24a", gpuCount: 8, image: "vllm/vllm-openai@sha256:2c6da6c6f16ed15c91e412d896dba13701f25fe1861eaec9ddaa4db34d1d21c4", commandArg: "glm47"},
		{name: "qwen3.8-flash-next", revision: "bcd9f01ddc9cff2316eb84281bebcd5b058bddce", gpuCount: 8, image: "vllm/vllm-openai@sha256:fc120ece0a388cc0aa1caad4a9f1cd92113484ab7ec2fd0efadd62585be05bf8", commandArg: "--enable-expert-parallel"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry, ok := Get(test.name)
			expectedProfiles := 2
			if test.name == "qwen3.8-flash-next" {
				expectedProfiles = 3
			}
			if !ok || entry.Revision != test.revision || len(entry.Profiles) != expectedProfiles {
				t.Fatalf("recipe identity is not pinned: %+v", entry)
			}
			profile := entry.Profiles[0]
			if profile.Runtime != "custom-oci" || profile.GPUCount != test.gpuCount || profile.CloudHint != "runpod" || profile.ProviderAdapterHint != "runpod-pods" || profile.Workload.Image != test.image || !contains(profile.Workload.Command, test.commandArg) {
				t.Fatalf("profile does not use the portable serving contract: %+v", profile)
			}
			if err := profile.Workload.Validate(); err != nil {
				t.Fatalf("workload is invalid: %v", err)
			}
		})
	}
}

func TestQwen38QualifiedCandidateIsExposedWithoutCrossProviderClaim(t *testing.T) {
	entry, ok := Get("qwen3.8-27b-fp8")
	if !ok || entry.Model != "Qwen/Qwen3.8-27B-FP8" || entry.Revision != "017b9c7af6b5689d5dd426a76e0bc077eb5ca20a" || len(entry.Profiles) != 1 {
		t.Fatalf("pinned Qwen candidate missing: %+v", entry)
	}
	profile := entry.Profiles[0]
	wantImage := "lmsysorg/sglang@sha256:9e148f5ac788e856a06166bd6347a831831eb9fcfab4d1770874823a7c29a1a1"
	if profile.Runtime != "custom-oci" || profile.RuntimeVersion != "0.5.18+71de97b264b04dcd514cf904003028aefe9775c8" || profile.GPUHint != "H100" || profile.GPUCount != 1 || profile.Workload.Image != wantImage {
		t.Fatalf("Qwen serving tuple drifted: %+v", profile)
	}
	command := profile.Workload.Command
	for _, value := range []string{"NEXTN", "3", "1", "4", "qwen3_coder", "18432", "${MODEL_REVISION}"} {
		if !contains(command, value) {
			t.Fatalf("Qwen launch command omitted %q: %#v", value, command)
		}
	}
	limitations := strings.Join(profile.Limitations, " ")
	for _, disclosure := range []string{"Modal", "RunPod", "not recipe-v1", "no public-grade performance claim"} {
		if !strings.Contains(limitations, disclosure) {
			t.Fatalf("Qwen recipe omitted evidence boundary %q: %s", disclosure, limitations)
		}
	}
	if err := profile.Workload.Validate(); err != nil {
		t.Fatalf("Qwen workload is invalid: %v", err)
	}
}

func TestFrontierProfilesMaterializePinnedSnapshotBeforeVLLM(t *testing.T) {
	for _, name := range []string{"glm-5.3-flash", "qwen3.8-flash-next"} {
		t.Run(name, func(t *testing.T) {
			entry, ok := Get(name)
			expectedProfiles := 2
			if name == "qwen3.8-flash-next" {
				expectedProfiles = 3
			}
			if !ok || len(entry.Profiles) != expectedProfiles {
				t.Fatalf("entry=%#v ok=%t", entry, ok)
			}
			for _, profile := range entry.Profiles {
				command := profile.Workload.Command
				if len(command) < 5 || command[0] != "python3" || command[1] != "-c" {
					t.Fatalf("profile=%s command=%#v", profile.Name, command)
				}
				bootstrap := command[2]
				for _, expected := range []string{"snapshot_download", "repo_id='${MODEL}'", "revision='${MODEL_REVISION}'", "INFERCRANE_MODEL_DIR", "HF_XET_HIGH_PERFORMANCE", `"/opt/infercrane/model"`, "os.execvp('vllm'"} {
					if !strings.Contains(bootstrap, expected) {
						t.Fatalf("profile %s bootstrap omitted %q: %s", profile.Name, expected, bootstrap)
					}
				}
				if !contains(command, "--tensor-parallel-size") || !contains(command, "--served-model-name") || !contains(command, "${MODEL}") {
					t.Fatalf("vLLM argv was not preserved for %s: %#v", profile.Name, command)
				}
				if name == "qwen3.8-flash-next" && (!contains(command, "qwen3_xml") || contains(command, "qwen3_coder")) {
					t.Fatalf("Qwen profile does not use the reviewed upstream tool parser: %#v", command)
				}
			}
		})
	}
}

func TestFrontierBlackwellProfilesRemainExplicitCandidates(t *testing.T) {
	tests := []struct {
		entry, profile, requiredArg string
	}{
		{entry: "qwen3.8-flash-next", profile: "custom-oci-blackwell-tp2", requiredArg: "--tensor-parallel-size"},
		{entry: "qwen3.8-flash-next", profile: "custom-oci-blackwell-tep4", requiredArg: "--enable-expert-parallel"},
		{entry: "glm-5.3-flash", profile: "custom-oci-blackwell-tp4", requiredArg: "--kv-cache-dtype"},
	}
	for _, test := range tests {
		entry, _ := Get(test.entry)
		found := false
		for _, profile := range entry.Profiles {
			if profile.Name != test.profile {
				continue
			}
			expectedCount := 4
			if test.profile == "custom-oci-blackwell-tp2" {
				expectedCount = 2
			}
			found = profile.GPUHint == "B200" && profile.GPUCount == expectedCount && contains(profile.CompatibleGPUs, "B200") && contains(profile.Workload.Command, test.requiredArg) && strings.Contains(strings.Join(profile.Limitations, " "), "exact")
		}
		if !found {
			t.Fatalf("missing explicit unqualified Blackwell candidate %s/%s", test.entry, test.profile)
		}
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
