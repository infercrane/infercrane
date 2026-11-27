package speculator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizedartifact"
	"github.com/infercrane/infercrane/internal/technique"
)

func l4Setup() technique.Setup {
	return technique.Setup{
		Model: "Qwen/Qwen3-8B", ModelRevision: qwenRevision, Engine: "vllm", EngineVersion: "0.34.0", Accelerator: "L4", GPUCount: 1,
		Args: []string{"--max-model-len", "4096", "--speculative-config", `{"method":"ngram","num_speculative_tokens":4}`},
		Env:  map[string]string{"VLLM_NO_USAGE_STATS": "1"},
	}
}

func TestTechniqueManifestMeetsTheContract(t *testing.T) {
	t.Parallel()
	manifest := Technique{}.Manifest()
	if err := manifest.Validate(); err != nil {
		t.Fatalf("manifest refused by the contract: %v", err)
	}
	if manifest.Artifact == nil || manifest.Artifact.Kind != optimizedartifact.KindSpeculator || manifest.Artifact.RunsIn != technique.RunsInCustomerAccount || !manifest.Artifact.UsesCustomerContent {
		t.Fatalf("artifact = %+v", manifest.Artifact)
	}
	for _, cost := range []technique.Limit{technique.LimitKVCacheRoom, technique.LimitBuildTime} {
		if !contains(manifest.Costs, cost) {
			t.Fatalf("costs %v lack %s", manifest.Costs, cost)
		}
	}
	if !manifest.ChangesAnswers || manifest.Fallback.Serves != technique.FallbackStartingSetup {
		t.Fatalf("manifest = %+v", manifest)
	}
	// A result is judged against the manifest: one that leaves the acceptance
	// evidence out is "not shown", whatever else it carries.
	if len(manifest.Evidence) != 1 || manifest.Evidence[0] != EvidenceDraftAcceptance {
		t.Fatalf("evidence = %v", manifest.Evidence)
	}
	bare := technique.Result{Technique: TechniqueID, TechniqueVersion: TechniqueVersion}
	if manifest.ClaimOf(bare) != technique.ClaimNotShown {
		t.Fatal("a result without draft acceptance was shown")
	}
}

func contains[T comparable](values []T, want T) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestTechniqueProposesAPinnedDraftThroughTheRegistry(t *testing.T) {
	t.Parallel()
	published := trainedDraft()
	published.Name, published.Source, published.Training, published.ArtifactDigest = "redhat", SourcePublished, nil, ""
	registry, err := technique.NewRegistry(Technique{Drafts: []Draft{trainedDraft(), published}})
	if err != nil {
		t.Fatal(err)
	}
	candidates, skipped, err := registry.Candidates(context.Background(), l4Setup())
	if err != nil {
		t.Fatalf("the registry refused the candidate: %v", err)
	}
	if len(candidates) != 1 || len(skipped) != 0 {
		t.Fatalf("candidates = %+v skipped = %+v", candidates, skipped)
	}
	candidate := candidates[0]
	if candidate.Artifact == nil || candidate.Artifact.ID != "oasst-chat" || candidate.Artifact.Digest != trainedDraft().ArtifactDigest || candidate.Artifact.Kind != optimizedartifact.KindSpeculator {
		t.Fatalf("artifact = %+v", candidate.Artifact)
	}
	joined := strings.Join(candidate.Args, " ")
	if strings.Contains(joined, "ngram") || !strings.Contains(joined, `"method":"eagle3"`) || !strings.Contains(joined, "--max-model-len 4096") {
		t.Fatalf("args = %q", candidate.Args)
	}
	if candidate.Env["VLLM_NO_USAGE_STATS"] != "1" {
		t.Fatalf("env = %v", candidate.Env)
	}
	prediction := candidate.Prediction
	// Held-out acceptance 0.8, 0.6, 0.5 writes 2.52 tokens a step; three draft
	// steps cost 4% to 12% of a target step each.
	if prediction == nil || prediction.Low == nil || prediction.High == nil || *prediction.Low < 0.85 || *prediction.High > 1.25 || *prediction.Low >= *prediction.High || prediction.Basis != "acceptance-offline" {
		t.Fatalf("prediction = %+v", prediction)
	}
}

func TestTechniqueFallsBackWhereTheDraftIsNotPinned(t *testing.T) {
	t.Parallel()
	registry, err := technique.NewRegistry(Technique{Drafts: []Draft{trainedDraft()}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*technique.Setup) string{
		"other GPU":       func(s *technique.Setup) string { s.Accelerator = "H100"; return "pinned to vllm 0.34.0 on L4" },
		"other revision":  func(s *technique.Setup) string { s.ModelRevision = strings.Repeat("c", 40); return "trained against" },
		"other model":     func(s *technique.Setup) string { s.Model = "Qwen/Qwen3-14B"; return "trained against" },
		"other engine":    func(s *technique.Setup) string { s.EngineVersion = "0.33.0"; return "not checked on vllm 0.33.0" },
		"unreviewed card": func(s *technique.Setup) string { s.Accelerator = "A100"; return "not checked on A100" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			setup := l4Setup()
			want := mutate(&setup)
			candidates, skipped, err := registry.Candidates(context.Background(), setup)
			if err != nil || len(candidates) != 0 || len(skipped) != 1 {
				t.Fatalf("candidates = %+v skipped = %+v err = %v", candidates, skipped, err)
			}
			if !strings.Contains(skipped[0].Reason, want) {
				t.Fatalf("reason = %q, want it to mention %q", skipped[0].Reason, want)
			}
		})
	}
	unpinned := trainedDraft()
	unpinned.Pinned = nil
	candidates, why, err := Technique{Drafts: []Draft{unpinned}}.Candidates(context.Background(), l4Setup())
	if err != nil || len(candidates) != 0 || !strings.Contains(why, "no qualification recorded") {
		t.Fatalf("an unpinned draft was proposed: %+v %q %v", candidates, why, err)
	}
	if _, why, _ := (Technique{}).Candidates(context.Background(), l4Setup()); !strings.Contains(why, "no draft") {
		t.Fatalf("empty technique reason = %q", why)
	}
}

func TestJobAndReportMapOntoTheArtifactStore(t *testing.T) {
	t.Parallel()
	plan, err := sampleJob().ArtifactPlan("base-qwen3-8b", "sha256:"+strings.Repeat("a", 64))
	if err != nil {
		t.Fatalf("plan refused: %v", err)
	}
	if plan.Kind != optimizedartifact.KindSpeculator || plan.Tool != "vllm-speculators" || plan.ToolVersion != SpeculatorsVersion || !plan.RequiresQualityReview || plan.BuilderImageDigest != "sha256:"+strings.Repeat("c", 64) {
		t.Fatalf("plan = %+v", plan)
	}
	if _, err := optimizedartifact.InputDigest(plan); err != nil {
		t.Fatal(err)
	}
	var configuration map[string]any
	if err := json.Unmarshal(plan.Configuration, &configuration); err != nil || configuration["data_source"] != "prompt_set" {
		t.Fatalf("configuration = %s", plan.Configuration)
	}
	attestation, err := sampleReport().Attestation("acme/qwen3-8b-chat-draft", draftRevision)
	if err != nil {
		t.Fatalf("attestation refused: %v", err)
	}
	if attestation.OutputDigest != sampleReport().ArtifactDigest || attestation.OutputImmutableRevision != draftRevision {
		t.Fatalf("attestation = %+v", attestation)
	}
	if strings.Contains(string(attestation.BuildEvidence), "prompt") && !strings.Contains(string(attestation.BuildEvidence), "data") {
		t.Fatalf("evidence carries text: %s", attestation.BuildEvidence)
	}
	if _, err := sampleReport().Attestation("acme/x", "main"); err == nil {
		t.Fatal("an attestation to a moving branch was accepted")
	}
}
