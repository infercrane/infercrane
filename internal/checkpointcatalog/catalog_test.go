package checkpointcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

const (
	qwenRevision        = "482ca0f3832238542f8f5295dde86b5f22711d80"
	deepSeekPinned      = "3431dde3247c13b5957f682b1e3c6fcae2566079"
	deepSeekBase        = "dba1be0a40aa45a94ad051997016db3960a90277"
	sglangPinnedVersion = "0.5.12"
)

func TestEmbeddedCatalogIsReviewedPinnedAndHardwareBound(t *testing.T) {
	catalog, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	policyRaw, err := os.ReadFile("policy.json")
	if err != nil {
		t.Fatal(err)
	}
	policy, digest, err := DecodePolicy(policyRaw)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Snapshot.PolicyDigest != digest {
		t.Fatal("catalog.json was not regenerated after the last policy change; re-run the sync plan")
	}
	if len(catalog.Publishers) != len(policy.Publishers) {
		t.Fatalf("catalog publishers %d != policy publishers %d", len(catalog.Publishers), len(policy.Publishers))
	}
	vendors := map[string]bool{}
	for _, entry := range catalog.Entries {
		vendors[entry.Hardware.Vendor] = true
		if !RunnableVendor(entry.Hardware.Vendor) && (entry.Hardware.ProductSupport != SupportUnsupported || entry.Eligibility.State == EligibilityCandidate) {
			t.Fatalf("an entry for hardware without reviewed accelerators is runnable: %+v", entry)
		}
		if entry.Eligibility.State == EligibilityCandidate && entry.Artifact.ModelCardDigest == "" {
			t.Fatalf("candidate lacks a pinned model card: %s", entry.ID)
		}
		for _, prior := range entry.PublisherPriors {
			if prior.EvidenceState != EvidenceUnmeasured || !strings.Contains(prior.Source, entry.Revision) {
				t.Fatalf("publisher prior is not an unmeasured, revision-pinned copy: %+v", prior)
			}
		}
	}
	for _, vendor := range []string{"nvidia", "amd", "intel"} {
		if !vendors[vendor] {
			t.Fatalf("catalog has no %s entries", vendor)
		}
	}
	find := func(repository, revision string) *Entry {
		for index := range catalog.Entries {
			if catalog.Entries[index].Repository == repository && (revision == "" || catalog.Entries[index].Revision == revision) {
				return &catalog.Entries[index]
			}
		}
		return nil
	}
	if entry := find("nvidia/Qwen3.8-27B-NVFP4", qwenRevision); entry == nil || entry.Eligibility.State != EligibilityCandidate {
		t.Fatalf("reviewed Qwen checkpoint is missing or blocked: %+v", entry)
	}
	if entry := find("nvidia/DeepSeek-V4.1-Flash-NVFP4", deepSeekPinned); entry == nil || entry.Eligibility.State != EligibilityCandidate || entry.Base.DeclaredRevision != deepSeekBase {
		t.Fatalf("runnable DeepSeek pin is missing: %+v", entry)
	}
	for _, repository := range []string{"nvidia/DeepSeek-V4.1-Flash-1017-NVFP4", "nvidia/GLM-5.3-Flash-FP4-Local"} {
		if entry := find(repository, ""); entry == nil || !ImmutableRevision(entry.Revision) {
			t.Fatalf("newer NVIDIA build %s is not pinned in the catalog", repository)
		}
	}
	if entry := find("nvidia/DeepSeek-V4.1-Flash-1017-NVFP4", ""); entry.Eligibility.State == EligibilityCandidate {
		t.Fatal("a build that needs an unmerged vLLM change became a candidate")
	}
	if got := catalog.Match("Qwen/Qwen3.8-27B", strings.Repeat("a", 40), "vllm", "0.11.0", "H200"); len(got) != 0 {
		t.Fatalf("Blackwell checkpoint leaked into Hopper: %+v", got)
	}
	if got := catalog.Match("Qwen/Qwen3.8-27B", strings.Repeat("a", 40), "vllm", "0.11.0", "B200"); len(got) != 1 || got[0].Revision != qwenRevision {
		t.Fatalf("Qwen match = %+v", got)
	}
	if got := catalog.Match("deepseek-ai/DeepSeek-V4.1-Flash", strings.Repeat("a", 40), "sglang", sglangPinnedVersion, "GB300"); len(got) != 0 {
		t.Fatalf("exact lineage accepted the wrong base revision: %+v", got)
	}
	if got := catalog.Match("deepseek-ai/DeepSeek-V4.1-Flash", deepSeekBase, "sglang", sglangPinnedVersion, "GB300"); len(got) != 1 || got[0].Revision != deepSeekPinned {
		t.Fatalf("DeepSeek exact match = %+v", got)
	}
	if got := catalog.Match("zai-org/GLM-5.3-Flash", strings.Repeat("a", 40), "sglang", sglangPinnedVersion, "MI355X"); len(got) != 0 {
		t.Fatalf("an AMD accelerator matched an NVIDIA checkpoint: %+v", got)
	}
	sum := sha256.Sum256(policyRaw)
	if digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatal("policy digest is not the SHA-256 of the policy bytes")
	}
}

func validEntry() Entry {
	revision := strings.Repeat("a", 40)
	return Entry{
		ID: "acme-model-aaaaaaaa", Kind: KindQuantized, Publisher: "acme", Repository: "acme/model", Revision: revision,
		RevisionState: RevisionCurrent, CreatedAt: "2026-10-01T00:00:00Z", LastModified: "2026-10-01T00:00:00Z",
		Base:         Base{Repository: "acme/base", Lineage: LineagePublisherDeclared},
		Quantization: &Quantization{Tool: "modelopt", ToolVersion: "0.48.0", Method: "modelopt", Scheme: "nvfp4"},
		Hardware:     Hardware{Vendor: "nvidia", Architectures: []string{"blackwell"}, ProductSupport: SupportRunnable},
		Runtimes:     []string{"vllm"},
		License:      License{Tag: "mit", SPDX: "MIT", State: LicenseApproved},
		Artifact:     Artifact{Format: "safetensors", ManifestDigest: "sha256:" + strings.Repeat("b", 64), ModelCardDigest: "sha256:" + strings.Repeat("c", 64), FileCount: 3, SizeBytes: 10},
		Review:       Review{State: ReviewApproved},
		Eligibility:  Eligibility{State: EligibilityCandidate},
		SourceURL:    "https://huggingface.co/acme/model",
		Limitations:  []string{"unmeasured"},
	}
}

func TestValidateRejectsEntriesThatCrossTheProductBoundary(t *testing.T) {
	if err := ValidateEntry(validEntry()); err != nil {
		t.Fatalf("valid entry rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Entry){
		"AMD family name":    func(e *Entry) { e.Hardware.Vendor, e.Hardware.Architectures = "amd", []string{"cdna3"} },
		"AMD target missing": func(e *Entry) { e.Hardware.Vendor, e.Hardware.Architectures = "amd", []string{"gfx90a"} },
		"Intel runnable":     func(e *Entry) { e.Hardware.Vendor, e.Hardware.Architectures = "intel", []string{"gpu"} },
		"no architectures":   func(e *Entry) { e.Hardware.Architectures = nil },
		"unknown family":     func(e *Entry) { e.Hardware.Architectures = []string{"ampere"} },
		"unresolved license": func(e *Entry) { e.License = License{Tag: "other", State: LicenseUnresolved} },
		"pending review":     func(e *Entry) { e.Review = Review{State: ReviewPending, Reasons: []string{"new publisher"}} },
		"speculator candidate": func(e *Entry) {
			e.Kind, e.Quantization, e.Speculator = KindSpeculator, nil, &Speculator{Method: "dspark", Targets: []string{"acme/base"}}
		},
		"unsupported tool": func(e *Entry) { e.Quantization.Tool = "quark" },
		"mutable revision": func(e *Entry) { e.Revision = "main" },
		"measured prior": func(e *Entry) {
			e.PublisherPriors = []PriorTable{{Source: "card", EvidenceState: "measured", Columns: []string{"a"}, Rows: [][]string{{"1"}}}}
		},
		"exact lineage without pin": func(e *Entry) { e.Base.Lineage = LineageExact },
		"orphan supersession":       func(e *Entry) { e.RevisionState = RevisionSuperseded },
		"foreign repository":        func(e *Entry) { e.Repository, e.SourceURL = "other/model", "https://huggingface.co/other/model" },
	} {
		entry := validEntry()
		mutate(&entry)
		if err := ValidateEntry(entry); err == nil {
			t.Errorf("%s: entry was accepted", name)
		}
	}
}

func TestMatchHonorsEngineRequirements(t *testing.T) {
	entry := validEntry()
	catalog := Catalog{Entries: []Entry{entry}}
	if got := catalog.Match("acme/base", strings.Repeat("d", 40), "vllm", "0.11.0", "B300"); len(got) != 1 {
		t.Fatalf("unconstrained entry did not match: %+v", got)
	}
	catalog.Entries[0].EngineRequirements = []EngineRequirement{{Engine: "vllm", State: EngineMinimumVersion, MinimumVersion: "0.29.0"}}
	if got := catalog.Match("acme/base", strings.Repeat("d", 40), "vllm", "0.11.0", "B300"); len(got) != 0 {
		t.Fatal("pinned engine older than the publisher minimum was accepted")
	}
	if got := catalog.Match("acme/base", strings.Repeat("d", 40), "vllm", "0.29.1", "B300"); len(got) != 1 {
		t.Fatal("pinned engine newer than the publisher minimum was rejected")
	}
	catalog.Entries[0].EngineRequirements = []EngineRequirement{{Engine: "vllm", State: EngineUnreleasedChange, PullRequest: "vllm-project/vllm#1"}}
	if got := catalog.Match("acme/base", strings.Repeat("d", 40), "vllm", "9.9.9", "B300"); len(got) != 0 {
		t.Fatal("an unreleased engine change was treated as satisfied")
	}
	for _, test := range []struct {
		a, b string
		want int
	}{{"0.5.10.post1", "0.5.10", 1}, {"0.29.0", "0.29", 0}, {"v0.31.0", "0.30.9", 1}, {"0.9", "0.10", -1}} {
		if got := CompareVersions(test.a, test.b); got != test.want {
			t.Errorf("CompareVersions(%s, %s) = %d", test.a, test.b, got)
		}
	}
}

func amdEntry(architectures ...string) Entry {
	entry := validEntry()
	entry.ID, entry.Publisher, entry.Repository, entry.SourceURL = "amd-model-aaaaaaaa", "amd", "amd/model", "https://huggingface.co/amd/model"
	entry.Quantization = &Quantization{Tool: "llm-compressor", ToolVersion: "0.12.3", Method: "compressed-tensors", Scheme: "w8a8"}
	entry.Hardware = Hardware{Vendor: "amd", Architectures: architectures, ProductSupport: SupportRunnable}
	return entry
}

func TestMatchPairsAMDBuildsOnlyWithTheirCompilerTarget(t *testing.T) {
	both, cdna4 := amdEntry("gfx942", "gfx950"), amdEntry("gfx950")
	cdna4.ID, cdna4.Repository, cdna4.SourceURL = "amd-model-mxfp4-aaaaaaaa", "amd/model-mxfp4", "https://huggingface.co/amd/model-mxfp4"
	for _, entry := range []Entry{both, cdna4} {
		if err := ValidateEntry(entry); err != nil {
			t.Fatalf("runnable AMD entry rejected: %v", err)
		}
	}
	catalog := Catalog{Entries: []Entry{both, cdna4}}
	revision := strings.Repeat("d", 40)
	for gpu, want := range map[string][]string{
		"MI300X":                  {"amd/model"},
		"AMD Instinct MI300X OAM": {"amd/model"},
		"MI325X":                  {"amd/model"},
		"AMD Instinct MI350 OAM":  {"amd/model", "amd/model-mxfp4"},
		"AMD_Instinct_MI355X_OAM": {"amd/model", "amd/model-mxfp4"},
		"H100":                    nil,
		"B200":                    nil,
		"MI250X":                  nil,
	} {
		var got []string
		for _, entry := range catalog.Match("acme/base", revision, "vllm", "0.22.1", gpu) {
			got = append(got, entry.Repository)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("Match on %s = %v, want %v", gpu, got, want)
		}
	}
	catalog.Entries[0].EngineRequirements = []EngineRequirement{{Engine: "vllm", State: EngineMinimumVersion, MinimumVersion: "0.28.0"}}
	if got := catalog.Match("acme/base", revision, "vllm", "0.22.1", "MI300X"); len(got) != 0 {
		t.Fatalf("an AMD build that needs a newer vLLM than the pinned image matched: %+v", got)
	}
}

func TestAcceleratorArchitectureAndPolicyNamesResolveToCompilerTargets(t *testing.T) {
	for gpu, want := range map[string]string{
		"MI300X": "gfx942", "AMD Instinct MI325X": "gfx942", "MI355X": "gfx950", "AMD_Instinct_MI350X_OAM": "gfx950",
		"H100": "hopper", "NVIDIA GB300": "blackwell", "A100": "", "MI250X": "",
	} {
		if got := AcceleratorArchitecture(gpu); got != want {
			t.Errorf("AcceleratorArchitecture(%q) = %q, want %q", gpu, got, want)
		}
	}
	for _, test := range []struct {
		vendor string
		names  []string
		want   string
	}{
		{"amd", []string{"cdna3", "cdna4"}, "gfx942,gfx950"},
		{"amd", []string{"CDNA 4"}, "gfx950"},
		{"amd", []string{"gfx942", "cdna3"}, "gfx942"},
		{"amd", []string{"cdna5"}, "cdna5"},
		{"nvidia", []string{"hopper", "blackwell"}, "hopper,blackwell"},
		{"intel", []string{"gpu"}, "gpu"},
		{"intel", nil, ""},
	} {
		if got := strings.Join(ResolveArchitectures(test.vendor, test.names), ","); got != test.want {
			t.Errorf("ResolveArchitectures(%s, %v) = %q, want %q", test.vendor, test.names, got, test.want)
		}
	}
	for vendor, want := range map[string]string{"nvidia": "blackwell,hopper", "amd": "gfx942,gfx950", "intel": ""} {
		if got := strings.Join(RunnableArchitectures(vendor), ","); got != want {
			t.Errorf("RunnableArchitectures(%s) = %q, want %q", vendor, got, want)
		}
	}
}
