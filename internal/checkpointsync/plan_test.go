package checkpointsync

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
)

var update = flag.Bool("update", false, "rewrite golden plan outputs from the recorded fixtures")

const (
	qwenRevision       = "482ca0f3832238542f8f5295dde86b5f22711d80"
	deepSeekPinned     = "3431dde3247c13b5957f682b1e3c6fcae2566079"
	deepSeekHead       = "c48dca0146b56d64c8116dbb6fcace8b7cc835f8"
	deepSeek1017       = "3ae9c0d3fbbea5370bd06e679d22044db15883f1"
	removedGLMRevision = "09b04e5e74bca08ca8549fc736d4cdd8624bfde3"
)

func fixturePolicy(t *testing.T) (checkpointcatalog.Policy, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	policy, digest, err := checkpointcatalog.DecodePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	return policy, digest
}

func fixturePrevious(t *testing.T) checkpointcatalog.Catalog {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "previous.json"))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := checkpointcatalog.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func fixturePlan(t *testing.T, policy checkpointcatalog.Policy, digest string, previous checkpointcatalog.Catalog) Result {
	t.Helper()
	snapshot, err := OpenSnapshot(filepath.Join("testdata", "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Plan(snapshot, policy, digest, previous, "2026-11-11")
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func render(t *testing.T, result Result) ([]byte, []byte) {
	t.Helper()
	catalog, err := FormatCatalog(result.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	return catalog, RenderDiff(result)
}

func TestPlanFromRecordedSnapshotIsDeterministicAndMatchesGolden(t *testing.T) {
	policy, digest := fixturePolicy(t)
	previous := fixturePrevious(t)
	firstCatalog, firstDiff := render(t, fixturePlan(t, policy, digest, previous))
	secondCatalog, secondDiff := render(t, fixturePlan(t, policy, digest, previous))
	if !bytes.Equal(firstCatalog, secondCatalog) || !bytes.Equal(firstDiff, secondDiff) {
		t.Fatal("planning the same snapshot twice produced different output")
	}
	if _, err := checkpointcatalog.Decode(firstCatalog); err != nil {
		t.Fatalf("formatted catalog does not load: %v", err)
	}
	for name, got := range map[string][]byte{"catalog.json": firstCatalog, "catalog-diff.md": firstDiff} {
		path := filepath.Join("testdata", "golden", name)
		if *update {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s differs from the golden file; run go test ./internal/checkpointsync -update and review the change", name)
		}
	}
}

func TestPlanAppliesReviewHardwareLicenseAndEngineGates(t *testing.T) {
	policy, digest := fixturePolicy(t)
	result := fixturePlan(t, policy, digest, fixturePrevious(t))
	entries := map[string]checkpointcatalog.Entry{}
	for _, entry := range result.Catalog.Entries {
		entries[entry.Repository+"@"+entry.Revision] = entry
	}
	candidate := func(key string) bool {
		entry, found := entries[key]
		return found && entry.Eligibility.State == checkpointcatalog.EligibilityCandidate
	}
	if !candidate("nvidia/Qwen3.8-27B-NVFP4@"+qwenRevision) || entries["nvidia/Qwen3.8-27B-NVFP4@"+qwenRevision].Quantization.Scheme != "mixed-nvfp4-fp8" {
		t.Fatalf("reviewed NVIDIA checkpoint lost its candidate state: %+v", entries["nvidia/Qwen3.8-27B-NVFP4@"+qwenRevision])
	}
	head := entries["nvidia/DeepSeek-V4.1-Flash-NVFP4@"+deepSeekHead]
	if head.Eligibility.State != checkpointcatalog.EligibilityCatalogOnly || head.Review.State != checkpointcatalog.ReviewApproved || len(head.EngineRequirements) != 2 {
		t.Fatalf("engine-gated head revision became a candidate: %+v", head)
	}
	if head.EngineRequirements[0].State != checkpointcatalog.EngineBlockedByReview || head.EngineRequirements[1].PullRequest != "vllm-project/vllm#61530" {
		t.Fatalf("engine requirements were not parsed and applied: %+v", head.EngineRequirements)
	}
	pinned := entries["nvidia/DeepSeek-V4.1-Flash-NVFP4@"+deepSeekPinned]
	if !candidate("nvidia/DeepSeek-V4.1-Flash-NVFP4@"+deepSeekPinned) || pinned.RevisionState != checkpointcatalog.RevisionSuperseded || pinned.SupersededBy != deepSeekHead || pinned.Base.Lineage != checkpointcatalog.LineageExact || pinned.Base.DeclaredRevision != "dba1be0a40aa45a94ad051997016db3960a90277" {
		t.Fatalf("runnable pinned revision was not kept behind a blocked head: %+v", pinned)
	}
	if candidate("nvidia/DeepSeek-V4.1-Flash-1017-NVFP4@" + deepSeek1017) {
		t.Fatal("a build that needs unreleased engine changes became a candidate")
	}
	if local := entries["nvidia/GLM-5.3-Flash-FP4-Local@"+strings.Repeat("a1", 20)]; local.License.State != checkpointcatalog.LicenseUnresolved || local.Eligibility.State == checkpointcatalog.EligibilityCandidate {
		t.Fatalf("unresolved license became a candidate: %+v", local)
	}
	if speculator := entries["nvidia/Qwen3.8-27B-DSpark@"+strings.Repeat("b2", 20)]; speculator.Kind != checkpointcatalog.KindSpeculator || speculator.Speculator.Method != "dspark" || speculator.Eligibility.State == checkpointcatalog.EligibilityCandidate {
		t.Fatalf("speculator crossed into checkpoint candidates: %+v", speculator)
	}
	redHat := entries["RedHatAI/Qwen3.8-27B-NVFP4@"+strings.Repeat("c3", 20)]
	if redHat.Review.State != checkpointcatalog.ReviewPending || len(redHat.Review.Reasons) != 2 || len(redHat.PublisherPriors) != 1 || redHat.PublisherPriors[0].EvidenceState != checkpointcatalog.EvidenceUnmeasured {
		t.Fatalf("new publisher and format did not require review, or priors were not kept unmeasured: %+v", redHat)
	}
	amd := entries["amd/GLM-5.3-Flash-Quark-MXFP4@"+strings.Repeat("d4", 20)]
	if amd.Hardware.Vendor != "amd" || amd.Hardware.ProductSupport != checkpointcatalog.SupportUnsupported || amd.Eligibility.State == checkpointcatalog.EligibilityCandidate || amd.Quantization.Scheme != "mxfp4" {
		t.Fatalf("AMD build was not marked as unsupported hardware: %+v", amd)
	}
	for _, entry := range result.Catalog.Entries {
		if entry.Hardware.Vendor != "nvidia" && entry.Eligibility.State == checkpointcatalog.EligibilityCandidate {
			t.Fatalf("non-NVIDIA entry became a candidate: %+v", entry)
		}
		if entry.Repository == "nvidia/Old-Model-NVFP4" {
			t.Fatal("a repository created before the listing window entered the catalog")
		}
	}
	if len(result.Diff.Removed) != 1 || result.Diff.Removed[0].Entry.Revision != removedGLMRevision || !strings.Contains(result.Diff.Removed[0].Reason, "404") {
		t.Fatalf("missing upstream repository was not reported as removed: %+v", result.Diff.Removed)
	}
	if len(result.Diff.Revised) != 1 || result.Diff.Unchanged != 1 || len(result.Diff.Added) != 9 {
		t.Fatalf("unexpected diff shape: revised=%d unchanged=%d added=%d", len(result.Diff.Revised), result.Diff.Unchanged, len(result.Diff.Added))
	}
	if !slices.Contains(result.Diff.Skipped[skipNotLLMLibrary], "nvidia/Cosmos3-Nano-FP8") || !slices.Contains(result.Diff.Skipped[skipNoBase], "RedHatAI/GLM-5.3-Flash") {
		t.Fatalf("skipped repositories are not reported: %+v", result.Diff.Skipped)
	}
}

func TestNewRevisionWithMaterialChangeNeedsHumanReview(t *testing.T) {
	policy, digest := fixturePolicy(t)
	previous := fixturePrevious(t)
	for index := range previous.Entries {
		if previous.Entries[index].Revision == deepSeekPinned {
			previous.Entries[index].License.Tag = "apache-2.0"
		}
	}
	result := fixturePlan(t, policy, digest, previous)
	for _, entry := range result.Catalog.Entries {
		if entry.Revision == deepSeekHead {
			if entry.Review.State != checkpointcatalog.ReviewPending || !strings.Contains(strings.Join(entry.Review.Reasons, " "), "changes license") {
				t.Fatalf("license change on a new revision skipped human review: %+v", entry.Review)
			}
			return
		}
	}
	t.Fatal("head revision missing from plan")
}

func TestApprovingPublisherAndFormatPromotesEntryOnSameSnapshot(t *testing.T) {
	policy, digest := fixturePolicy(t)
	for index := range policy.Publishers {
		if policy.Publishers[index].Organization == "RedHatAI" {
			policy.Publishers[index].ReviewState = checkpointcatalog.ReviewApproved
			policy.Publishers[index].Formats = append(policy.Publishers[index].Formats, checkpointcatalog.FormatPolicy{
				Kind: checkpointcatalog.KindQuantized, Tool: "llm-compressor", Scheme: "mixed-nvfp4-fp8", ReviewState: checkpointcatalog.ReviewApproved,
				Hardware: checkpointcatalog.HardwareTarget{Vendor: "nvidia", Architectures: []string{"blackwell"}}, Runtimes: []string{"vllm"},
			})
		}
	}
	result := fixturePlan(t, policy, digest, fixturePrevious(t))
	for _, entry := range result.Catalog.Entries {
		if entry.Repository == "RedHatAI/Qwen3.8-27B-NVFP4" {
			if entry.Eligibility.State != checkpointcatalog.EligibilityCandidate || entry.EngineRequirements[0].MinimumVersion != "0.28.1" {
				t.Fatalf("approved publisher format did not become a candidate: %+v", entry)
			}
			return
		}
	}
	t.Fatal("Red Hat entry missing from plan")
}

func TestPlanSelectsOlderBuildsByDownloadsOrReviewedCollection(t *testing.T) {
	policy, digest := fixturePolicy(t)
	result := fixturePlan(t, policy, digest, fixturePrevious(t))
	entries := map[string]checkpointcatalog.Entry{}
	for _, entry := range result.Catalog.Entries {
		entries[entry.Repository] = entry
	}
	popular, found := entries["nvidia/Nemotron-Old-30B-FP8"]
	if !found || popular.Eligibility.State != checkpointcatalog.EligibilityCandidate || popular.Quantization.Scheme != "fp8" || !slices.Equal(popular.Hardware.Architectures, []string{"hopper", "blackwell"}) {
		t.Fatalf("an older FP8 build above the download threshold did not become a Hopper candidate: %+v", popular)
	}
	if why := result.Diff.Selected["nvidia/Nemotron-Old-30B-FP8"]; !strings.Contains(why, "250000 downloads") {
		t.Fatalf("download selection is not explained: %q", why)
	}
	listed, found := entries["nvidia/Old-Listed-NVFP4"]
	if !found || listed.Eligibility.State != checkpointcatalog.EligibilityCandidate || !strings.HasPrefix(result.Diff.Selected["nvidia/Old-Listed-NVFP4"], "listed in collection ") {
		t.Fatalf("an older build that only the reviewed collection lists was not tracked: %+v %q", listed, result.Diff.Selected["nvidia/Old-Listed-NVFP4"])
	}
	if why := result.Diff.Selected["nvidia/Qwen3.8-27B-NVFP4"]; !strings.HasPrefix(why, "created 2026-09-04") || !strings.Contains(why, "; listed in collection ") {
		t.Fatalf("a listed build in the collection does not name both reasons: %q", why)
	}
	partner, found := entries["stepfun-ai/Step-Test-NVFP4"]
	if !found || partner.Publisher != "stepfun-ai" || partner.Review.State != checkpointcatalog.ReviewPending || partner.Eligibility.State == checkpointcatalog.EligibilityCandidate || !slices.Contains(partner.Review.Reasons, "new publisher stepfun-ai requires review") {
		t.Fatalf("a collection build from another organization skipped publisher review: %+v", partner)
	}
	hidden, found := entries["nvidia/Hidden-Signal-8B-FP8"]
	if !found || hidden.Quantization.Tool != "modelopt" || hidden.Quantization.Scheme != "fp8" || hidden.Quantization.ToolVersion != "0.37.0" || hidden.Eligibility.State != checkpointcatalog.EligibilityCandidate {
		t.Fatalf("a collection build whose ModelOpt signal is only in hf_quant_config.json was not classified from its files: %+v", hidden)
	}
	if why := result.Diff.Selected["nvidia/Hidden-Signal-8B-FP8"]; !strings.HasPrefix(why, "15000 downloads") || !strings.Contains(why, "; listed in collection ") {
		t.Fatalf("a listing item without a listing signal was not kept for its collection membership: %q", why)
	}
	if _, found = entries["nvidia/Old-Model-NVFP4"]; found {
		t.Fatal("an old build below the download threshold and outside the collection entered the catalog")
	}
	if !slices.Contains(result.Diff.Skipped[skipUnknownPublisher], "black-forest-labs/FLUX.3-Test-NVFP4") || !slices.Contains(result.Diff.Skipped[skipNotTextModel], "nvidia/bigvgan-test-22khz") {
		t.Fatalf("collection and download selections that were not catalogued are not reported: %+v", result.Diff.Skipped)
	}
}

func TestPlanAppliesTheSelectionWindowsTheSnapshotRecorded(t *testing.T) {
	policy, digest := fixturePolicy(t)
	snapshot, err := OpenSnapshot(filepath.Join("testdata", "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	for index := range snapshot.Manifest.Publishers {
		if snapshot.Manifest.Publishers[index].Organization == "nvidia" {
			snapshot.Manifest.Publishers[index].MinDownloads = 500000
		}
	}
	snapshot.Manifest.Collections = nil
	result, err := Plan(snapshot, policy, digest, fixturePrevious(t), "2026-11-11")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range result.Catalog.Entries {
		switch entry.Repository {
		case "nvidia/Nemotron-Old-30B-FP8", "nvidia/Old-Listed-NVFP4", "nvidia/Hidden-Signal-8B-FP8", "stepfun-ai/Step-Test-NVFP4":
			t.Fatalf("%s was tracked without the threshold or collection the snapshot recorded", entry.Repository)
		}
	}
	if !slices.ContainsFunc(result.Catalog.Entries, func(entry checkpointcatalog.Entry) bool { return entry.Repository == "nvidia/Qwen3.8-27B-NVFP4" }) {
		t.Fatal("the created_after window stopped applying when the other rules were off")
	}
}
