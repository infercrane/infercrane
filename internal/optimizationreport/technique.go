package optimizationreport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/support"
	"github.com/infercrane/infercrane/internal/technique"
)

// TechniqueView is the technique that made a setup, as a campaign candidate
// and the report's candidate row show it: the manifest's declaration, the
// result the campaign measured (technique.Result), and the claim the result
// supports. The claim is judged against the manifest itself
// (Manifest.ClaimOf), so a record can never drop a requirement.
type TechniqueView struct {
	ID               string              `json:"id"`
	Version          string              `json:"version"`
	Label            string              `json:"label"`
	Relieves         []string            `json:"relieves"`
	Costs            []string            `json:"costs"`
	RequiredEvidence []string            `json:"required_evidence"`
	ChangesAnswers   bool                `json:"changes_answers"`
	Artifact         *TechniqueArtifact  `json:"artifact,omitempty"`
	Fallback         *technique.Fallback `json:"fallback,omitempty"`
	Result           *technique.Result   `json:"result,omitempty"`
	// Claim is "shown" or "not shown"; Unknown names what a "shown" claim
	// still needs.
	Claim   string   `json:"claim"`
	Unknown []string `json:"unknown"`
}

// TechniqueArtifact is the artifact a setup serves: the manifest's kind and
// where it builds, and the candidate's digest.
type TechniqueArtifact struct {
	Kind                string `json:"kind"`
	Digest              string `json:"digest,omitempty"`
	RunsIn              string `json:"runs_in"`
	UsesCustomerContent bool   `json:"uses_customer_content"`
}

// unknownMeasurement is what a setup that has not been measured lacks.
const unknownMeasurement = "measured"

// CandidateTechnique builds a candidate's technique view from the campaign's
// stored records: its proposal's technique, its deployment spec, its
// measurement, and the quality evidence that decided it (nil when none).
// It returns nil for a setup no contract technique made.
func CandidateTechnique(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, quality *domain.QualityEvidence) *TechniqueView {
	var proposal optimizer.Proposal
	if json.Unmarshal([]byte(campaign.ProposalJSON), &proposal) != nil {
		return nil
	}
	var made *optimizer.CandidateTechnique
	var limitations []string
	for _, proposed := range proposal.Candidates {
		if proposed.ID == candidate.ProposalCandidateID {
			made, limitations = proposed.Technique, proposed.Limitations
		}
	}
	if made == nil {
		return nil
	}
	manifest, known := optimizer.TechniqueManifest(made.ID)
	if !known {
		return nil
	}
	view := &TechniqueView{ID: manifest.ID, Version: made.Version, Label: manifest.Label, Relieves: limits(manifest.Relieves), Costs: limits(manifest.Costs),
		RequiredEvidence: append([]string{}, manifest.Evidence...), ChangesAnswers: manifest.ChangesAnswers, Claim: technique.ClaimNotShown, Unknown: []string{}}
	if build := manifest.Artifact; build != nil {
		view.Artifact = &TechniqueArtifact{Kind: build.Kind, RunsIn: build.RunsIn, UsesCustomerContent: build.UsesCustomerContent}
		if made.Artifact != nil {
			view.Artifact.Digest = made.Artifact.Digest
		}
	}
	fallback := manifest.Fallback
	view.Fallback = &fallback
	result := techniqueResult(campaign, proposal, candidate, manifest.ID, made.Version, manifest.Evidence, quality)
	if result != nil {
		result.Disclosures = carriedDisclosures(manifest, limitations)
	}
	if result == nil {
		view.Unknown = []string{unknownMeasurement}
		return view
	}
	view.Result = result
	view.Claim = manifest.ClaimOf(*result)
	view.Unknown = append([]string{}, result.Unknown()...)
	return view
}

func limits(values []technique.Limit) []string {
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = string(value)
	}
	return out
}

// SetupResult is the campaign's measurement of any candidate as a technique
// result, and the claim it supports under the one shared rule: a technique's
// setup is judged against its manifest (Manifest.ClaimOf), any other setup by
// technique.Result.Claim, so a setup the planner's rules made needs the same
// fresh-machine confirmation, the same answers held, and the same fields. It
// returns nil and "not shown" for a candidate with no measurement yet.
func SetupResult(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, quality *domain.QualityEvidence) (*technique.Result, string) {
	if view := CandidateTechnique(campaign, candidate, quality); view != nil {
		return view.Result, view.Claim
	}
	var proposal optimizer.Proposal
	if json.Unmarshal([]byte(campaign.ProposalJSON), &proposal) != nil {
		return nil, technique.ClaimNotShown
	}
	result := techniqueResult(campaign, proposal, candidate, "", "", nil, quality)
	if result == nil {
		return nil, technique.ClaimNotShown
	}
	return result, result.Claim()
}

// techniqueResult is the campaign's measurement of one candidate as a
// technique result, or nil when the candidate has no measurement yet. A
// field the campaign did not record stays empty, so the claim is "not
// shown" and Unknown names it. id, version and evidence are the technique's
// manifest; all three are empty for a setup no technique made.
func techniqueResult(campaign domain.OptimizationCampaign, proposal optimizer.Proposal, candidate domain.OptimizationCandidateRun, id, version string,
	evidence []string, quality *domain.QualityEvidence) *technique.Result {
	var actual struct {
		Workload     json.RawMessage `json:"workload"`
		RequestCount int             `json:"request_count"`
	}
	if json.Unmarshal([]byte(candidate.ActualEvidenceJSON), &actual) != nil || len(actual.Workload) == 0 {
		return nil
	}
	var workload struct {
		Concurrency int                                    `json:"concurrency"`
		Measured    *optimizationcampaign.MeasuredEvidence `json:"measured"`
		// MeasuredBy is "agent" for a setup an InferCrane agent measured in
		// the customer's cluster.
		MeasuredBy string `json:"measured_by"`
		// LiveSetup says what an agent run's comparisons with the
		// endpoint's own server rest on.
		LiveSetup string `json:"live_setup"`
	}
	if json.Unmarshal(actual.Workload, &workload) != nil || workload.Measured == nil {
		return nil
	}
	var draft optimizer.DeploymentDraft
	_ = json.Unmarshal([]byte(candidate.DeploymentSpecJSON), &draft)
	result := technique.Result{SchemaVersion: technique.ResultSchemaV1, Technique: id, TechniqueVersion: version, Candidate: candidate.ID,
		RequiredEvidence: append([]string{}, evidence...)}
	count := draft.Resources.GPUCount
	if count < 1 {
		count = 1
	}
	image := imageDigest(draft)
	result.Hardware = technique.Hardware{Accelerator: draft.Resources.GPU, Count: count}
	result.Software = technique.Software{Engine: draft.Runtime.Engine, EngineVersion: draft.Runtime.Version, ImageDigest: image}
	shape := ""
	if strings.HasPrefix(proposal.Input.WorkloadFingerprint, "sha256:") {
		shape = proposal.Input.WorkloadFingerprint
	}
	kind := proposal.Input.WorkloadProfile
	if kind == "" {
		kind = proposal.Input.WorkloadSource
	}
	result.Workload = technique.Workload{ShapeDigest: shape, Kind: kind, Load: workload.Concurrency, Requests: actual.RequestCount}
	result.Precision = precision(draft)
	result.Baseline = technique.Baseline{Setup: "the run's current setup", Basis: "interleaved"}
	if workload.MeasuredBy == "agent" {
		// The baseline is the customer's own server; what it runs is the
		// operator's declaration, or not known.
		result.Baseline.Setup = "the endpoint's own server, its setup " + workload.LiveSetup
	}
	result.Correctness = correctness(quality)
	metric, _ := lab.ObjectiveMetric(campaign.Objective, proposal.Input.WorkloadProfile)
	// The objective's own comparison; a cost-scaled objective's claim is
	// stated in the raw metric it scales, which Measured.Metric names.
	result.Measured = measured(*workload.Measured, metric.Name)
	if confirmation := workload.Measured.Confirmation; confirmation != nil && workload.MeasuredBy == "agent" && !confirmation.FreshCandidate {
		// An agent started the setup again on the same node, or on one it
		// could not name: not a fresh machine, so the claim is not shown.
		result.Baseline.Basis = "interleaved; measured again on the same node or an unnamed one, so a fresh-machine confirmation needs a second GPU node"
	} else if confirmation != nil {
		// The fresh-server window's own record is the result's fresh-machine
		// confirmation (technique.EvidenceFreshMachineConfirmation).
		result.Baseline.Basis = "interleaved+fresh-servers"
		if body, err := json.Marshal(confirmation); err == nil {
			sum := sha256.Sum256(body)
			result.Evidence = append(result.Evidence, technique.EvidenceItem{Name: technique.EvidenceFreshMachineConfirmation, Digest: "sha256:" + hex.EncodeToString(sum[:]),
				Summary: "the campaign's fresh-server window"})
		}
	}
	if id == optimizer.TechniqueAdaptiveSpeculationOneEngine {
		result.Evidence = append(result.Evidence, stateByDraftEvidence(draft, *workload.Measured)...)
	}
	result.Reproduction = technique.Reproduction{Command: command(draft), Commit: buildCommit(), ImageDigest: image, TrafficShape: shape}
	return &result
}

// measured is the objective's comparison: a first-window gain counts only
// when the fresh-server window measured one too, and then the smaller of the
// two changes against the larger noise floor.
func measured(evidence optimizationcampaign.MeasuredEvidence, metric string) technique.Measured {
	out := technique.Measured{Metric: metric, Decision: technique.DecisionUnknown}
	first, found := evidence.Comparison[metric]
	if metric == "" || !found {
		return out
	}
	out.Change, out.NoiseFloor, out.Decision = first.Delta, first.NoiseFloor, decision(first.Decision)
	if out.Decision != technique.DecisionImproved {
		return out
	}
	if evidence.Confirmation == nil {
		out.Decision = technique.DecisionUnknown
		return out
	}
	second, confirmed := evidence.Confirmation.Comparison[metric]
	if !confirmed {
		out.Decision = technique.DecisionUnknown
		return out
	}
	out.Change, out.NoiseFloor, out.Decision = min(first.Delta, second.Delta), max(first.NoiseFloor, second.NoiseFloor), decision(second.Decision)
	return out
}

func decision(value string) string {
	switch value {
	case optimizationcampaign.DecisionImproved:
		return technique.DecisionImproved
	case optimizationcampaign.DecisionRegressed:
		return technique.DecisionWorse
	case optimizationcampaign.DecisionNoMeasurableDifference:
		return technique.DecisionSame
	}
	return technique.DecisionUnknown
}

// correctness is how the deciding quality evidence checked answers.
func correctness(row *domain.QualityEvidence) technique.Correctness {
	if row == nil {
		return technique.Correctness{Method: "", Verdict: technique.DecisionUnknown}
	}
	method := row.Evaluator
	switch row.Evaluator {
	case qualitycheck.TaskEvaluator:
		method = "task-score"
	case qualitycheck.Evaluator:
		method = "answer-agreement"
	case qualitycheck.GradedEvaluator:
		method = "graded"
	}
	var distribution qualityevidence.Distribution
	if row.DistributionJSON != "" && json.Unmarshal([]byte(row.DistributionJSON), &distribution) == nil && distribution.Divergence != nil {
		method += "+kl"
	}
	verdict := "failed"
	if row.Passed {
		verdict = "passed"
	} else if distribution.TaskVerdict == qualitycheck.CodeTaskNotShownHeld {
		// Not worse: not shown to hold within a point, so unknown.
		verdict = technique.DecisionUnknown
	}
	return technique.Correctness{Method: method, Verdict: verdict}
}

// precision is what the setup's weights and KV cache served as, read from
// its model and flags.
func precision(draft optimizer.DeploymentDraft) technique.Precision {
	out := technique.Precision{Weights: "as published", KVCache: "auto"}
	if strings.Contains(strings.ToLower(draft.Model.ID), "fp8") {
		out.Weights = "fp8 (published)"
	}
	args := draft.Runtime.Args
	for index, arg := range args {
		flag, value, inline := strings.Cut(arg, "=")
		if !inline && index+1 < len(args) {
			value = args[index+1]
		}
		switch flag {
		case "--quantization":
			out.Weights = value + " (at load)"
		case "--kv-cache-dtype":
			out.KVCache = value
		}
	}
	return out
}

// imageDigest is the served image's sha256: the spec's own image, or the
// release's pinned image for a built-in runtime.
func imageDigest(draft optimizer.DeploymentDraft) string {
	image := draft.Runtime.Workload.Image
	if image == "" {
		if served, ok := support.RuntimeWorkloadFor(draft.Runtime.Engine, draft.Runtime.Version, provideridentity.AcceleratorPlatform(draft.Resources.GPU)); ok {
			image = served.Image
		}
	}
	if _, digest, found := strings.Cut(image, "@"); found && strings.HasPrefix(digest, "sha256:") {
		return digest
	}
	return ""
}

// command is the setup's serving command as the spec states it.
func command(draft optimizer.DeploymentDraft) string {
	if draft.Runtime.Engine == "" || draft.Model.ID == "" {
		return ""
	}
	parts := []string{draft.Runtime.Engine, draft.Runtime.Version, draft.Model.ID + "@" + draft.Model.Revision}
	return strings.Join(slices.Concat(parts, draft.Runtime.Args), " ")
}

// buildCommit is the commit this control plane was built from, when the
// build recorded it.
func buildCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return ""
}
