package optimizer

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/speculator"
	"github.com/infercrane/infercrane/internal/support"
)

// Every contract technique's setup enters a proposal the same way: made by
// its own package, carrying the technique's name and version, the artifact
// it serves, and the evidence its manifest requires.
func TestContractTechniquesEnterAProposalThroughOnePath(t *testing.T) {
	request := genericRequest("H100", lowConcurrencyDiagnosis())
	request.MaxCandidates = 10
	request.SpeculativeDrafts = []speculator.Draft{testTrainedDraft()}
	request.KernelConfigs = tunedKernelSet(t, support.DefaultRuntimeVersion, kerneltuning.DeviceForGPU("H100"))
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	for id, evidence := range map[string]string{
		TechniqueSpeculativeTrainedDraft: "output parity and speculative acceptance evidence",
		TechniqueTunedKernels:            "the engine's log showing it read the tuned file, and the starting setup's showing its default for that file",
	} {
		candidate := candidateFor(t, proposal, id)
		if candidate.Source.Name != techniqueSource || !strings.HasPrefix(candidate.Source.Version, id+"@") {
			t.Fatalf("%s source = %+v", id, candidate.Source)
		}
		if !slices.Contains(candidate.RequiredEvidence, evidence) || !slices.Contains(candidate.RequiredEvidence, TaskScoreEvidence) {
			t.Fatalf("%s evidence = %v", id, candidate.RequiredEvidence)
		}
		if !slices.ContainsFunc(candidate.Features, func(feature Feature) bool {
			return feature.Name == "technique" && feature.State == candidate.Source.Version
		}) {
			t.Fatalf("%s features = %+v", id, candidate.Features)
		}
	}
	if kernels := candidateFor(t, proposal, TechniqueTunedKernels); kernels.Deployment.Runtime.KernelConfigs == nil || kernels.Deployment.Runtime.KernelConfigs.Digest != request.KernelConfigs.Digest {
		t.Fatalf("the tuned set did not travel with its setup: %+v", kernels.Deployment.Runtime)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatalf("a proposal with contract setups failed validation: %v", err)
	}
}

// A trained draft is served only where it is pinned: the technique says so,
// and the plan records its reason.
func TestAnUnpinnedTrainedDraftIsSkippedWithTheTechniquesReason(t *testing.T) {
	request := genericRequest("H100", lowConcurrencyDiagnosis())
	request.MaxCandidates = 10
	draft := testTrainedDraft()
	draft.Pinned = nil
	request.SpeculativeDrafts = []speculator.Draft{draft}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	planned := plannedTechnique(t, proposal, TechniqueSpeculativeTrainedDraft)
	if len(planned.CandidateIDs) != 0 || !strings.Contains(planned.Skipped, "no qualification recorded") {
		t.Fatalf("unpinned draft plan = %+v", planned)
	}
	// No other path serves it either: no setup names the draft, and the
	// rules' own draft derivation refuses a trained draft.
	for _, candidate := range proposal.Candidates {
		if strings.Contains(strings.Join(candidate.Deployment.Runtime.Args, " "), draft.Repository) {
			t.Fatalf("an unpinned trained draft was served by %+v", candidate.Reason)
		}
	}
	if args, why := draftArgs(nil, "vllm", support.DefaultRuntimeVersion, techniques[TechniqueSpeculativeTrainedDraft], request); args != nil || !strings.Contains(why, "only through the trained-draft technique") {
		t.Fatalf("the rules derived a trained draft: %v %q", args, why)
	}
}

// A proposal stored before the techniques took their contract names still
// validates.
func TestEarlierTechniqueNamesStillValidate(t *testing.T) {
	for alias, current := range map[string]string{"tuned-kernels": TechniqueTunedKernels, "speculative-trained-draft": TechniqueSpeculativeTrainedDraft} {
		if canonicalTechnique(alias) != current {
			t.Fatalf("%s is now %s", alias, canonicalTechnique(alias))
		}
		reason := CandidateReason{Role: ReasonDiagnosis, Technique: alias, Label: "x", Priority: 30, ExpectedBenefit: "low", Rules: []string{}, Evidence: []ReasonEvidence{}}
		if err := validateCandidateReason(reason); err != nil {
			t.Fatalf("%s: %v", alias, err)
		}
	}
	if TechniqueTunedKernels != kerneltuning.TechniqueID || TechniqueSpeculativeTrainedDraft != speculator.TechniqueID {
		t.Fatal("a contract technique's planner name differs from its manifest's")
	}
}

// The technique packages read the model features their manifests require
// from the same pinned config as the planner, and a tuned block-FP8 set
// carries the feature of the model it was built from.
func TestTechniquesReadTheirModelFeaturesFromThePinnedConfig(t *testing.T) {
	config := `{"text_config":{"num_experts":256,"moe_intermediate_size":512,"num_nextn_predict_layers":1},"quantization_config":{"quant_method":"fp8","weight_block_size":[128,128]}}`
	evidence, err := InspectModelConfig([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(evidence.Features, kerneltuning.ModelFeatureFusedMoEFP8Block) || !slices.Contains(evidence.Features, FeatureNativeMTP) {
		t.Fatalf("features = %v", evidence.Features)
	}
	request := genericRequest("H100", nil)
	request.KernelConfigs = tunedKernelSet(t, support.DefaultRuntimeVersion, kerneltuning.DeviceForGPU("H100"))
	control := Candidate{}
	control.Deployment.Runtime.Engine, control.Deployment.Resources.GPU = "vllm", "H100"
	if setup := contractSetup(control, request); !slices.Contains(setup.ModelFeatures, kerneltuning.ModelFeatureFusedMoEFP8Block) {
		t.Fatalf("setup features = %v", setup.ModelFeatures)
	}
}
