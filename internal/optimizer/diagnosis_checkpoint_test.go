package optimizer

import (
	"context"
	"slices"
	"testing"
)

const qwen3Revision = "b968826d9c46dd6066d109eabc6255188de91218"

func checkpointRequest(gpu string) Request {
	return Request{ModelIdentity: "Qwen/Qwen3-8B", ModelRevision: qwen3Revision, Provider: "runpod-pods", GPU: gpu, Runtimes: []string{"vllm"}, MaxCandidates: 8, Diagnosis: lowConcurrencyDiagnosis()}
}

// A reviewed publisher's FP8 checkpoint of the exact model is measured beside
// FP8 at load when a rule chose FP8 weights: the starting setup with only its
// weights swapped, one setup per checkpoint, its own task score required.
func TestAPublishedFP8CheckpointIsMeasuredBesideFP8AtLoad(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), checkpointRequest("H100"))
	if err != nil {
		t.Fatal(err)
	}
	if got := reasonOrder(proposal); !slices.Equal(got, []string{ReasonControl, TechniqueFP8Weights, TechniqueFP8Checkpoint}) {
		t.Fatalf("order = %v", got)
	}
	control := proposal.Candidates[0]
	var checkpoints []Candidate
	for _, candidate := range proposal.Candidates {
		if candidate.Reason != nil && candidate.Reason.Technique == TechniqueFP8Checkpoint {
			checkpoints = append(checkpoints, candidate)
		}
	}
	if len(checkpoints) != 1 {
		t.Fatalf("checkpoint setups = %d", len(checkpoints))
	}
	published := checkpoints[0]
	if published.Deployment.Model.ID != "nvidia/Qwen3-8B-FP8" || published.Deployment.Model.Revision != "2cebc4c89e25abc17668c81b01dceaf3d8b914d5" || published.ArtifactSeed == nil || published.ArtifactSeed.CatalogID != "nvidia-qwen3-8b-fp8-2cebc4c8" {
		t.Fatalf("checkpoint setup = %+v", published.Deployment.Model)
	}
	if !slices.Equal(published.Deployment.Runtime.Args, control.Deployment.Runtime.Args) || !slices.Contains(published.RequiredEvidence, TaskScoreEvidence) {
		t.Fatalf("args = %v against %v, evidence = %v", published.Deployment.Runtime.Args, control.Deployment.Runtime.Args, published.RequiredEvidence)
	}
	if planned := plannedTechnique(t, proposal, TechniqueFP8Checkpoint); planned.Skipped != "" || len(planned.CandidateIDs) != 1 || !slices.Contains(planned.Rules, "weights.published_checkpoint") {
		t.Fatalf("planned = %+v", planned)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatalf("proposal with a checkpoint setup failed validation: %v", err)
	}
}

// Outside the catalog's reviewed hardware, or for another revision of the
// model, no checkpoint is proposed and FP8 at load stays.
func TestNoCheckpointOutsideTheCatalogsExactMatch(t *testing.T) {
	for name, request := range map[string]Request{
		"a GPU the catalog has not reviewed": checkpointRequest("L4"),
		"another revision of the model": func() Request {
			r := checkpointRequest("H100")
			r.ModelRevision = "0123456789abcdef0123456789abcdef01234567"
			return r
		}(),
	} {
		proposal, err := catalogSource(t).Propose(context.Background(), request)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if slices.Contains(reasonOrder(proposal), TechniqueFP8Checkpoint) {
			t.Fatalf("%s: order = %v", name, reasonOrder(proposal))
		}
	}
	control := Candidate{}
	control.Deployment.Model.ID, control.Deployment.Model.Revision = "Qwen/Qwen3-8B", qwen3Revision
	seeded := func(base, revision, screened, algorithm string) Candidate {
		return Candidate{ArtifactSeed: &ArtifactSeed{BaseRepository: base, BaseRevision: revision, BaseScreeningRevision: screened, Algorithm: algorithm}}
	}
	for name, test := range map[string]struct {
		candidate Candidate
		want      bool
	}{
		"declared exact base":                        {seeded("qwen/qwen3-8b", qwen3Revision, qwen3Revision, "fp8"), true},
		"declared by repository, screened at it":     {seeded("Qwen/Qwen3-8B", "", qwen3Revision, "fp8_block"), true},
		"declared by repository, screened elsewhere": {seeded("Qwen/Qwen3-8B", "", "0123456789abcdef0123456789abcdef01234567", "fp8"), false},
		"declared by repository, never screened":     {seeded("Qwen/Qwen3-8B", "", "", "fp8"), false},
		"another base revision":                      {seeded("Qwen/Qwen3-8B", "0123", "", "fp8"), false},
		"another model":                              {seeded("Qwen/Qwen3-14B", qwen3Revision, qwen3Revision, "fp8"), false},
		"4-bit with an FP8 part":                     {seeded("Qwen/Qwen3-8B", qwen3Revision, qwen3Revision, "mixed-nvfp4-fp8"), false},
		"no catalog provenance":                      {Candidate{}, false},
	} {
		if got := publishedFP8(test.candidate, control); got != test.want {
			t.Errorf("%s: published = %v", name, got)
		}
	}
}
