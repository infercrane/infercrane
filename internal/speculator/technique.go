package speculator

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/infercrane/infercrane/internal/optimizedartifact"
	"github.com/infercrane/infercrane/internal/technique"
)

// TechniqueID and TechniqueVersion name the trained-draft technique in the
// technique contract (internal/technique). A change of method is a new
// version.
const (
	TechniqueID      = "trained-draft"
	TechniqueVersion = "0.1.0"
)

// EvidenceDraftAcceptance names the evidence every trained-draft result must
// carry (technique.Manifest.Evidence): acceptance read from the serving
// engine's own speculation counters (drafts, draft tokens, accepted tokens,
// and accepted tokens per position) over the measured runs, in a file whose
// sha256 the result records. Without it a speed result is "not shown".
const EvidenceDraftAcceptance = "draft-acceptance"

// Engine versions and accelerators the technique is reviewed for: vLLM 0.34.0
// serves an EAGLE-3 draft by repository and commit, and the proof ladder runs
// on L4 and H100. A draft is still served only where it is pinned.
var (
	reviewedEngines      = []technique.Engine{{Name: engineVLLM, Versions: []string{"0.34.0"}}}
	reviewedAccelerators = []string{"L4", "H100"}
)

// Prediction bounds the speed a draft's offline acceptance can buy at low
// load: each verify step writes ExpectedTokensPerStep tokens and costs one
// target step plus the draft's own steps. An EAGLE-3 draft is one decoder
// layer, so its step costs a few percent of a target step; low and high
// bracket that cost. At higher load the gain shrinks, which only a measured
// load band shows.
const (
	draftStepCostLow  = 0.04
	draftStepCostHigh = 0.12
)

// Technique adapts drafts trained on the customer's traffic to the technique
// contract. It proposes one candidate per draft pinned to the setup's exact
// model, engine version and GPU; it never measures. Drafts come from the
// training reports the customer registered (Report.Draft).
type Technique struct {
	Drafts []Draft
}

// Manifest declares the technique. Training reads the customer's prompts, so
// the build runs in the customer's account; a draft costs cache room (its own
// cache and the verify tokens) and build time; speculation can change greedy
// answers through numerics, so it is declared answer-changing and every
// candidate needs the task score and divergence checks.
func (t Technique) Manifest() technique.Manifest {
	return technique.Manifest{
		SchemaVersion: technique.SchemaV1, ID: TechniqueID, Version: TechniqueVersion,
		Label: "A draft trained on your traffic", License: "Apache-2.0",
		Requires:   technique.Requires{Engines: reviewedEngines, Accelerators: reviewedAccelerators},
		Relieves:   []technique.Limit{technique.LimitDecodeSteps},
		Costs:      []technique.Limit{technique.LimitKVCacheRoom, technique.LimitGPUMemory, technique.LimitBuildTime},
		Prediction: technique.Method{Metric: "output_tokens_per_second", Basis: "acceptance-offline"},
		Artifact: &technique.Build{
			Kind: optimizedartifact.KindSpeculator, RunsIn: technique.RunsInCustomerAccount, UsesCustomerContent: true,
			Produces: "an EAGLE-3 draft checkpoint pinned to the model revision, the engine version, and the GPU it was qualified on",
		},
		ChangesAnswers: true,
		Runtime:        technique.Runtime{Policy: technique.RuntimeStatic},
		Fallback:       technique.Fallback{Serves: technique.FallbackStartingSetup, Reason: "without its draft the starting setup serves unchanged, so a missing or stale draft only loses the gain"},
		Evidence:       []string{EvidenceDraftAcceptance},
	}
}

// Candidates proposes one setup per trained draft pinned where the setup
// runs: the starting setup's speculation removed and the draft's flags added.
// A draft for another model, commit, engine version or GPU proposes nothing,
// and the reason says which.
func (t Technique) Candidates(_ context.Context, setup technique.Setup) ([]technique.Candidate, string, error) {
	var out []technique.Candidate
	var reasons []string
	for _, draft := range t.Drafts {
		if draft.Source != SourceTrained {
			continue
		}
		if err := draft.Validate(); err != nil {
			return nil, "", err
		}
		model, _, _ := strings.Cut(setup.Model, "@")
		switch {
		case !strings.EqualFold(draft.BaseModel, model) || draft.BaseRevision != strings.ToLower(setup.ModelRevision):
			reasons = append(reasons, fmt.Sprintf("draft %s was trained against %s at %s", draft.Name, draft.BaseModel, shortRevision(draft.BaseRevision)))
			continue
		case !draft.Matches(setup.Engine, setup.EngineVersion, setup.Accelerator):
			reasons = append(reasons, fmt.Sprintf("draft %s is pinned to %s; this setup runs %s %s on %s, so it serves without the draft until the draft is qualified there", draft.Name, pinWords(draft.Pinned), setup.Engine, setup.EngineVersion, setup.Accelerator))
			continue
		}
		flags, err := RuntimeArgs(setup.Engine, draft)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("draft %s: %v", draft.Name, err))
			continue
		}
		args := withoutFlags(setup.Args, SpeculativeFlags)
		args = append(args, flags...)
		candidate := technique.Candidate{
			Technique: TechniqueID, TechniqueVersion: TechniqueVersion, Label: "Your trained draft " + draft.Name,
			Args: args, Env: cloneEnv(setup.Env), ChangesAnswers: true,
			Artifact: &technique.ArtifactRef{ID: draft.Name, Kind: optimizedartifact.KindSpeculator, Digest: draft.ArtifactDigest},
			Reason:   fmt.Sprintf("A %s; it replaces the starting setup's speculation, if any, and falls back to the starting setup without it.", Describe(draft)),
		}
		if prediction, ok := predict(draft); ok {
			candidate.Prediction = &prediction
		}
		out = append(out, candidate)
	}
	if len(out) == 0 {
		if len(reasons) == 0 {
			return nil, "no draft trained on your traffic is registered for this model", nil
		}
		return nil, strings.Join(reasons, "; "), nil
	}
	return out, "", nil
}

// predict turns offline acceptance into a low-load speed bracket: tokens a
// verify step writes over the cost of that step with the draft's own steps.
func predict(draft Draft) (technique.Prediction, bool) {
	if draft.Training == nil {
		return technique.Prediction{}, false
	}
	expected, ok := draft.Training.ExpectedTokensPerStep(draft.DraftTokens)
	if !ok {
		return technique.Prediction{}, false
	}
	steps := float64(draft.DraftTokens)
	low := expected/(1+steps*draftStepCostHigh) - 1
	high := expected/(1+steps*draftStepCostLow) - 1
	middle := (low + high) / 2
	return technique.Prediction{Metric: "output_tokens_per_second", Change: round3(middle), Low: ptr(round3(low)), High: ptr(round3(high)), Basis: "acceptance-offline"}, true
}

func withoutFlags(args, flags []string) []string {
	out := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		name, _, inline := strings.Cut(args[index], "=")
		if !slices.Contains(flags, name) {
			out = append(out, args[index])
			continue
		}
		if !inline && index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
			index++
		}
	}
	return out
}

func cloneEnv(env map[string]string) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env))
	for key, value := range env {
		out[key] = value
	}
	return out
}

func pinWords(pin *Pin) string {
	if pin == nil {
		return "nowhere (no qualification recorded)"
	}
	return fmt.Sprintf("%s %s on %s", pin.Engine, pin.EngineVersion, pin.Accelerator)
}

func round3(value float64) float64 { return math.Round(value*1000) / 1000 }

func ptr(value float64) *float64 { return &value }

// ArtifactPlan is the job as an optimized artifact plan: the provenance the
// existing artifact store keeps (internal/optimizedartifact). The training
// set is named by its fingerprint once known, never by content.
func (j TrainingJob) ArtifactPlan(baseModelArtifactID, datasetFingerprint string) (optimizedartifact.Plan, error) {
	if err := j.Validate(); err != nil {
		return optimizedartifact.Plan{}, err
	}
	configuration, err := json.Marshal(map[string]any{
		"method": j.Method, "draft_tokens": j.DraftTokens, "epochs": j.Epochs, "max_new_tokens": j.MaxNewTokens,
		"max_training_tokens": j.MaxTrainingTokens, "held_out_percent": j.HeldOutPercent, "data_source": j.DataSource, "start_from": j.StartFrom,
	})
	if err != nil {
		return optimizedartifact.Plan{}, err
	}
	hardware, err := json.Marshal(map[string]any{"accelerator": j.Accelerator, "gpu_count": j.GPUCount})
	if err != nil {
		return optimizedartifact.Plan{}, err
	}
	plan := optimizedartifact.Plan{
		BaseModelArtifactID: baseModelArtifactID, Kind: optimizedartifact.KindSpeculator, Format: "speculators", Tool: "vllm-speculators", ToolVersion: SpeculatorsVersion,
		Algorithm: j.Method, BuilderImageDigest: j.Image[strings.LastIndex(j.Image, "@")+1:], CalibrationDigest: datasetFingerprint,
		LicenseSPDX: "Apache-2.0", Configuration: configuration, HardwareConstraints: hardware, RequiresQualityReview: true,
	}
	return plan, optimizedartifact.ValidatePlan(plan)
}

// SpeculatorsVersion is the trainer the job's image is built with.
const SpeculatorsVersion = "0.9.0"

// Attestation records a finished job's output where the customer put it,
// in the shape the artifact store attests: repository, immutable revision,
// and the report's manifest digest, with the report's numbers as evidence.
func (r Report) Attestation(repository, revision string) (optimizedartifact.Attestation, error) {
	if err := r.Validate(); err != nil {
		return optimizedartifact.Attestation{}, err
	}
	if err := validateLocation(repository, revision); err != nil {
		return optimizedartifact.Attestation{}, err
	}
	evidence, err := json.Marshal(map[string]any{
		"schema": r.Schema, "examples": r.Examples, "held_out_examples": r.HeldOutExamples, "training_tokens": r.TrainingTokens,
		"dataset_fingerprint": r.DatasetFingerprint, "held_out_acceptance": r.HeldOutAcceptance, "start_acceptance": r.StartAcceptance,
		"framework": r.Framework, "framework_version": r.FrameworkVersion, "wall_seconds": r.WallSeconds,
	})
	if err != nil {
		return optimizedartifact.Attestation{}, err
	}
	attestation := optimizedartifact.Attestation{OutputRepository: repository, OutputImmutableRevision: revision, OutputDigest: r.ArtifactDigest, BuildEvidence: evidence}
	return attestation, optimizedartifact.ValidateAttestation(optimizedartifact.StateReady, attestation)
}
