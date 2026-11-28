package modelcheck

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
)

// reviewedWithoutSizes are reviewed catalog models on a built-in runtime
// whose config.json needs a token, so their sizes are not recorded.
var reviewedWithoutSizes = []string{"meta-llama/Llama-3.1-8B-Instruct", "google/gemma-3-4b-it"}

// Every reviewed model on a built-in runtime has recorded sizes at its
// catalog commit, or is listed as unreadable; none are recorded for a model
// or commit the catalog does not review.
func TestReviewedSizesMatchTheCatalogPins(t *testing.T) {
	registries, err := Registries()
	if err != nil {
		t.Fatal(err)
	}
	pinned := map[string]curatedrecipe.Entry{}
	for _, entry := range curatedrecipe.All() {
		if entry.Runtime != "vllm" && entry.Runtime != "sglang" {
			continue
		}
		pinned[entry.Model] = entry
		model, recorded := ReviewedModel(entry.Model, "")
		if recorded == slices.Contains(reviewedWithoutSizes, entry.Model) {
			t.Fatalf("%s: recorded=%t, but its config.json readability says otherwise", entry.Model, recorded)
		}
		if !recorded {
			continue
		}
		if model.Commit != entry.Revision || model.Repository != entry.Model {
			t.Fatalf("%s: sizes recorded at %s, catalog pins %s", entry.Model, model.Commit, entry.Revision)
		}
		want := map[string]string{"chat": TaskChat, "embeddings": TaskEmbeddings}[entry.Protocol]
		report := Evaluate(model, EvaluateOptions{Concurrency: 1, GPUs: []GPU{{Name: "H100", MemoryGiB: 80}}, Registries: registries})
		if report.Task != want {
			t.Fatalf("%s: task %q, catalog protocol %q", entry.Model, report.Task, entry.Protocol)
		}
		if want == TaskChat && (report.Memory.State != Fits || report.Config.ContextLength == nil || report.Config.KVCacheBytesPerToken == nil) {
			t.Fatalf("%s: recorded sizes do not size a GPU: %+v %+v", entry.Model, report.Memory, report.Config)
		}
	}
	for _, entry := range reviewedSizes {
		catalog, ok := pinned[entry.Repository]
		if !ok || catalog.Revision != entry.Commit || !strings.HasPrefix(entry.ConfigDigest, "sha256:") || len(entry.ConfigDigest) != len("sha256:")+64 {
			t.Fatalf("%s@%s is recorded but not a reviewed built-in catalog pin", entry.Repository, entry.Commit)
		}
	}
}

// The recorded Qwen3-8B sizes give the same estimate as reading the model
// from Hugging Face at the same commit.
func TestReviewedSizesEstimateAsTheLiveRead(t *testing.T) {
	resolver := newResolver(t, newHub(t), ResolverOptions{})
	live, err := resolver.Resolve(context.Background(), Request{Repository: "Qwen/Qwen3-8B", Revision: "b968826d9c46dd6066d109eabc6255188de91218"})
	if err != nil {
		t.Fatal(err)
	}
	recorded, ok := ReviewedModel("qwen/qwen3-8b", "B968826D9C46DD6066D109EABC6255188DE91218")
	if !ok {
		t.Fatal("Qwen/Qwen3-8B has no recorded sizes at its pinned commit")
	}
	registries, err := Registries()
	if err != nil {
		t.Fatal(err)
	}
	for _, contextTokens := range []int{0, 8192, MaxContextTokens} {
		options := EvaluateOptions{ContextTokens: contextTokens, Concurrency: 1, GPUs: testGPUs, Registries: registries}
		a, b := Evaluate(live, options), Evaluate(recorded, options)
		if a.Task != b.Task || *a.Memory.WeightsBytes != *b.Memory.WeightsBytes || *a.Memory.KVCacheBytes != *b.Memory.KVCacheBytes || a.Memory.ContextTokens != b.Memory.ContextTokens || !slices.Equal(a.Memory.Layouts, b.Memory.Layouts) {
			t.Fatalf("context %d: live %+v, recorded %+v", contextTokens, a.Memory, b.Memory)
		}
	}
}

func TestReviewedModelNeedsItsCommit(t *testing.T) {
	if _, ok := ReviewedModel("Qwen/Qwen3-8B", "main"); ok {
		t.Fatal("a branch name was taken for the recorded commit")
	}
	if _, ok := ReviewedModel("Qwen/Qwen3-8B", strings.Repeat("a", 40)); ok {
		t.Fatal("another commit got the recorded sizes")
	}
	if _, ok := ReviewedModel("microsoft/phi-4", ""); ok {
		t.Fatal("an unreviewed model got recorded sizes")
	}
	first, _ := ReviewedModel("Qwen/Qwen3-8B", "")
	first.Weights.ByDType["BF16"] = 1
	first.Config.Layers = 1
	second, _ := ReviewedModel("Qwen/Qwen3-8B", "")
	if second.Weights.ByDType["BF16"] != 8190735360 || second.Config.Layers != 36 {
		t.Fatal("a caller changed the recorded sizes")
	}
}

// A GPU's usable memory, when known, sizes the fit instead of its nominal
// size: one 24576-token Qwen3-8B request fits 90% of a nominal 24 GiB L4 but
// not 90% of the 23034 MiB its driver reports.
func TestUsableMemorySizesTheFit(t *testing.T) {
	model, _ := ReviewedModel("Qwen/Qwen3-8B", "")
	registries, err := Registries()
	if err != nil {
		t.Fatal(err)
	}
	fit := func(gpu GPU) MemoryFit {
		return Evaluate(model, EvaluateOptions{ContextTokens: 24576, Concurrency: 1, GPUs: []GPU{gpu}, Registries: registries}).Memory
	}
	nominal, usable := fit(GPU{Name: "L4", MemoryGiB: 24}), fit(GPU{Name: "L4", MemoryGiB: 24, UsableMiB: 23034})
	if nominal.State != Fits || *nominal.GPUCount != 1 || usable.State != Fits || *usable.GPUCount != 2 {
		t.Fatalf("nominal %+v, usable %+v", nominal, usable)
	}
}
