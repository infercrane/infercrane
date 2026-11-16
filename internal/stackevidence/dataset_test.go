package stackevidence

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
)

func TestEmbeddedDatasetIsValidAndCited(t *testing.T) {
	t.Parallel()
	dataset, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	if len(dataset.Stacks) == 0 || len(dataset.Sources) == 0 {
		t.Fatal("embedded dataset is empty")
	}
	used := map[string]bool{}
	for _, stack := range dataset.Stacks {
		used[stack.Source] = true
		source, ok := dataset.Source(stack.Source)
		if !ok {
			t.Fatalf("stack %s cites an unknown source", stack.ID)
		}
		if source.Kind == SourceMeasuredByInferCrane && !strings.HasPrefix(source.URL, "https://github.com/infercrane/infercrane/") {
			t.Fatalf("stack %s: InferCrane measurements must link a receipt in this repository", stack.ID)
		}
	}
	for _, source := range dataset.Sources {
		if !used[source.ID] {
			t.Fatalf("source %s is cited by no stack", source.ID)
		}
	}
}

// A stack that says it deploys must point at a reviewed profile that launches
// exactly what was measured: the same GPU, GPU count, engine version and image.
func TestDeployReferencesMatchReviewedProfiles(t *testing.T) {
	t.Parallel()
	for _, stack := range MustDefault().Stacks {
		if stack.Deploy == nil {
			continue
		}
		entry, ok := curatedrecipe.Get(stack.Deploy.CatalogModel)
		if !ok {
			t.Fatalf("stack %s: catalog model %s does not exist", stack.ID, stack.Deploy.CatalogModel)
		}
		if !strings.EqualFold(entry.Model, stack.Model) {
			t.Fatalf("stack %s measured %s but deploys %s", stack.ID, stack.Model, entry.Model)
		}
		if stack.ModelRevision != "" && entry.Revision != stack.ModelRevision {
			t.Fatalf("stack %s measured revision %s but the catalog pins %s", stack.ID, stack.ModelRevision, entry.Revision)
		}
		index := slices.IndexFunc(entry.Profiles, func(profile curatedrecipe.ServingProfile) bool { return profile.Name == stack.Deploy.Profile })
		if index < 0 {
			t.Fatalf("stack %s: profile %s does not exist", stack.ID, stack.Deploy.Profile)
		}
		profile := entry.Profiles[index]
		if !sameGPU(profile.GPUHint, stack.GPU) || profile.GPUCount != stack.GPUCount {
			t.Fatalf("stack %s runs %d × %s but profile %s launches %d × %s", stack.ID, stack.GPUCount, stack.GPU, profile.Name, profile.GPUCount, profile.GPUHint)
		}
		if profile.RuntimeVersion != stack.EngineVersion {
			t.Fatalf("stack %s measured engine %s but profile pins %s", stack.ID, stack.EngineVersion, profile.RuntimeVersion)
		}
		if stack.Image != "" && profile.Workload.Image != stack.Image {
			t.Fatalf("stack %s measured image %s but profile launches %s", stack.ID, stack.Image, profile.Workload.Image)
		}
	}
}

func TestDefaultReturnsAnIndependentCopy(t *testing.T) {
	t.Parallel()
	first := MustDefault()
	first.Stacks[0].Points[0].Concurrency = -1
	first.Stacks[0].Techniques = append(first.Stacks[0].Techniques, "mutated")
	second := MustDefault()
	if second.Stacks[0].Points[0].Concurrency == -1 || slices.Contains(second.Stacks[0].Techniques, "mutated") {
		t.Fatal("Default shares mutable state")
	}
}

func TestParseRejectsUnsourcedOrAmbiguousEvidence(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*map[string]any){
		"unknown field":         func(doc *map[string]any) { (*doc)["surprise"] = true },
		"missing license":       func(doc *map[string]any) { source(doc)["license"] = "" },
		"plain http source":     func(doc *map[string]any) { source(doc)["url"] = "http://example.com" },
		"unknown source kind":   func(doc *map[string]any) { source(doc)["kind"] = "blog_post" },
		"unknown source":        func(doc *map[string]any) { stack(doc)["source"] = "nobody" },
		"loose model":           func(doc *map[string]any) { stack(doc)["model"] = "llama 70b" },
		"unpinned image":        func(doc *map[string]any) { stack(doc)["image"] = "vllm/vllm-openai:latest" },
		"empty engine version":  func(doc *map[string]any) { stack(doc)["engine_version"] = "" },
		"unknown precision":     func(doc *map[string]any) { stack(doc)["precision"] = "fp3" },
		"no points":             func(doc *map[string]any) { stack(doc)["points"] = []any{} },
		"no throughput":         func(doc *map[string]any) { delete(point(doc), "output_tokens_per_second") },
		"zero throughput":       func(doc *map[string]any) { point(doc)["output_tokens_per_second"] = 0 },
		"negative ttft":         func(doc *map[string]any) { point(doc)["ttft_ms"] = map[string]any{"p95": -1} },
		"unnamed statistic":     func(doc *map[string]any) { point(doc)["per_user_statistic"] = "" },
		"missing point ref":     func(doc *map[string]any) { point(doc)["ref"] = "" },
		"measured after source": func(doc *map[string]any) { stack(doc)["measured_at"] = "2030-01-01" },
		"bad date":              func(doc *map[string]any) { stack(doc)["measured_at"] = "Sep 2026" },
		"duplicate stack": func(doc *map[string]any) {
			stacks := (*doc)["stacks"].([]any)
			(*doc)["stacks"] = append(stacks, stacks[0])
		},
		"half deploy reference": func(doc *map[string]any) { stack(doc)["deploy"] = map[string]any{"catalog_model": "x"} },
		"one-member family": func(doc *map[string]any) {
			(*doc)["families"] = []any{map[string]any{"id": "solo", "label": "Solo", "basis": "b", "members": []any{"a/b"}}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			doc := validDocument(t)
			mutate(&doc)
			data, err := json.Marshal(doc)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(data); err == nil {
				t.Fatal("Parse accepted invalid evidence")
			}
		})
	}
	data, _ := json.Marshal(validDocument(t))
	if _, err := Parse(data); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
}

func TestThroughputDerivesOnlyByGPUCount(t *testing.T) {
	t.Parallel()
	perGPU, total := 500.0, 4000.0
	stack := Stack{GPUCount: 8}
	if got, each, derived := stack.Throughput(Point{OutputTokensPerSecondPerGPU: &perGPU}); got != 4000 || each != 500 || derived != "output_tokens_per_second" {
		t.Fatalf("per-GPU source: got %v %v %q", got, each, derived)
	}
	if got, each, derived := stack.Throughput(Point{OutputTokensPerSecond: &total}); got != 4000 || each != 500 || derived != "output_tokens_per_second_per_gpu" {
		t.Fatalf("total source: got %v %v %q", got, each, derived)
	}
	if _, _, derived := stack.Throughput(Point{OutputTokensPerSecond: &total, OutputTokensPerSecondPerGPU: &perGPU}); derived != "" {
		t.Fatalf("both published, derived %q", derived)
	}
}

// validDocument is a small, self-consistent dataset for rank and parse tests.
// Its numbers are test inputs, not evidence.
func validDocument(t *testing.T) map[string]any {
	t.Helper()
	document := `{
  "schema_version": "infercrane.stack-evidence/v1",
  "version": "test",
  "reviewed_at": "2026-11-01",
  "stale_after_days": 365,
  "sources": [
    {"id": "bench", "kind": "public_benchmark", "label": "Bench", "publisher": "Bench Inc", "title": "Bench results", "url": "https://bench.example/results", "license": "Apache-2.0", "license_url": "https://bench.example/license", "published_at": "2026-10-01", "retrieved_at": "2026-11-01"},
    {"id": "ours", "kind": "measured_by_infercrane", "label": "InferCrane", "publisher": "InferCrane", "title": "Receipt", "url": "https://github.com/infercrane/infercrane/blob/main/receipt.json", "license": "Apache-2.0", "license_url": "https://github.com/infercrane/infercrane/blob/main/LICENSE", "published_at": "2026-10-02", "retrieved_at": "2026-11-01"},
    {"id": "old", "kind": "vendor_recipe", "label": "Vendor", "publisher": "Vendor", "title": "Old recipe", "url": "https://vendor.example/recipe", "license": "CC-BY-4.0", "license_url": "https://creativecommons.org/licenses/by/4.0/", "published_at": "2024-06-01", "retrieved_at": "2026-11-01"}
  ],
  "families": [
    {"id": "fam", "label": "Example 70B", "basis": "Same architecture.", "members": ["org/model-a", "org/model-b"]}
  ],
  "stacks": [
    {"id": "a-h100-vllm", "source": "bench", "model": "org/model-a", "engine": "vllm", "engine_version": "0.30.0", "gpu": "H100", "gpu_vendor": "nvidia", "gpu_count": 1, "precision": "fp8", "parallelism": "TP1", "techniques": [], "workload": {"input_tokens": 1024, "output_tokens": 1024, "dataset": "random"}, "measured_at": "2026-10-01", "source_ref": "row 1", "limitations": [],
     "points": [
       {"concurrency": 4, "output_tokens_per_second": 400, "per_user_output_tokens_per_second": 100, "per_user_statistic": "median", "ttft_ms": {"p95": 200}, "itl_ms": {}, "ref": "c4"},
       {"concurrency": 32, "output_tokens_per_second": 2000, "per_user_output_tokens_per_second": 60, "per_user_statistic": "median", "ttft_ms": {"p95": 900}, "itl_ms": {}, "ref": "c32"}
     ]},
    {"id": "a-h200-sglang", "source": "ours", "model": "org/model-a", "engine": "sglang", "engine_version": "0.5.20", "gpu": "H200", "gpu_vendor": "nvidia", "gpu_count": 2, "precision": "fp8", "parallelism": "TP2", "techniques": ["x"], "workload": {"input_tokens": 1024, "output_tokens": 1024, "dataset": "random"}, "measured_at": "2026-10-02", "source_ref": "row 2", "limitations": ["one run"],
     "points": [
       {"concurrency": 4, "output_tokens_per_second_per_gpu": 300, "per_user_output_tokens_per_second": 150, "per_user_statistic": "median", "ttft_ms": {"p95": 150}, "itl_ms": {}, "ref": "c4"},
       {"concurrency": 64, "output_tokens_per_second_per_gpu": 1500, "per_user_output_tokens_per_second": 40, "per_user_statistic": "median", "ttft_ms": {"p95": 2500}, "itl_ms": {}, "ref": "c64"}
     ]},
    {"id": "a-mi300x-vllm-unknown-ttft", "source": "bench", "model": "org/model-a", "engine": "vllm", "engine_version": "not stated", "gpu": "MI300X", "gpu_vendor": "amd", "gpu_count": 1, "precision": "fp8", "parallelism": "TP1", "techniques": [], "workload": {"input_tokens": 1024, "output_tokens": 1024, "dataset": "random"}, "measured_at": "2026-10-01", "source_ref": "row 3", "limitations": [],
     "points": [{"concurrency": 16, "output_tokens_per_second": 3000, "ttft_ms": {"p50": 120}, "itl_ms": {}, "ref": "c16"}]},
    {"id": "a-h100-old", "source": "old", "model": "org/model-a", "engine": "trt-llm", "engine_version": "0.9.0", "gpu": "H100", "gpu_vendor": "nvidia", "gpu_count": 1, "precision": "fp8", "parallelism": "TP1", "techniques": [], "workload": {"input_tokens": 1024, "output_tokens": 1024, "dataset": "random"}, "measured_at": "2024-06-01", "source_ref": "table", "limitations": [],
     "points": [{"concurrency": 8, "output_tokens_per_second": 9000, "ttft_ms": {"p95": 50}, "itl_ms": {}, "ref": "t"}]},
    {"id": "a-h100-long", "source": "bench", "model": "org/model-a", "engine": "vllm", "engine_version": "0.30.0", "gpu": "H100", "gpu_vendor": "nvidia", "gpu_count": 1, "precision": "fp8", "parallelism": "TP1", "techniques": [], "workload": {"input_tokens": 8192, "output_tokens": 1024, "dataset": "random"}, "measured_at": "2026-10-01", "source_ref": "row 5", "limitations": [],
     "points": [{"concurrency": 8, "output_tokens_per_second": 800, "ttft_ms": {"p95": 1200}, "itl_ms": {}, "ref": "c8"}]},
    {"id": "b-h100-vllm", "source": "bench", "model": "org/model-b", "engine": "vllm", "engine_version": "0.30.0", "gpu": "H100", "gpu_vendor": "nvidia", "gpu_count": 1, "precision": "bf16", "parallelism": "TP1", "techniques": [], "workload": {"input_tokens": 1024, "output_tokens": 8192, "dataset": "random"}, "measured_at": "2026-10-01", "source_ref": "row 6", "limitations": [],
     "points": [{"concurrency": 8, "output_tokens_per_second": 700, "ttft_ms": {"p95": 300}, "itl_ms": {}, "ref": "c8"}]}
  ]
}`
	var doc map[string]any
	if err := json.Unmarshal([]byte(document), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func testDataset(t *testing.T) Dataset {
	t.Helper()
	data, err := json.Marshal(validDocument(t))
	if err != nil {
		t.Fatal(err)
	}
	dataset, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return dataset
}

func source(doc *map[string]any) map[string]any {
	return (*doc)["sources"].([]any)[0].(map[string]any)
}

func stack(doc *map[string]any) map[string]any {
	return (*doc)["stacks"].([]any)[0].(map[string]any)
}

func point(doc *map[string]any) map[string]any {
	return stack(doc)["points"].([]any)[0].(map[string]any)
}

var testNow = time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
