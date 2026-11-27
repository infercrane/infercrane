// Package proofgatefixture builds the persisted evidence of finished
// two-setup runs, one per proof-gate outcome, and assembles their reports
// with the real report code. Tests and the fake control plane of the proof
// gate's dry run share it. It is a development fake and never ships in the
// production image.
package proofgatefixture

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// Scenarios, each a finished run except Running.
const (
	// Pass: no measurable difference in time to first token, answers held.
	Pass = "pass"
	// Gain: 18% faster, and 16% faster again on fresh servers.
	Gain = "gain"
	// Watch: no speed difference, answers 7% different (watch band).
	Watch = "watch"
	// Block: time to first token 14.1% worse against a 5.1% noise floor.
	Block = "block"
	// AnswersBlocked: no speed difference, answers 15% different.
	AnswersBlocked = "answers-blocked"
	// Unknown: the current setup drifted, so the change was not compared.
	Unknown = "unknown"
	// Running: the run is still measuring.
	Running = "running"
)

// Scenarios lists every scenario in a stable order.
func Scenarios() []string {
	return []string{Pass, Gain, Watch, Block, AnswersBlocked, Unknown, Running}
}

// CampaignID is the run ID a scenario is served under.
func CampaignID(scenario string) string { return "run-" + scenario }

// ScenarioOf returns the scenario a run ID names.
func ScenarioOf(campaignID string) (string, bool) {
	scenario, found := strings.CutPrefix(campaignID, "run-")
	if !found {
		return "", false
	}
	for _, known := range Scenarios() {
		if known == scenario {
			return scenario, true
		}
	}
	return "", false
}

const (
	tenant   = "tenant-fixture"
	model    = "Qwen/Qwen3-8B"
	revision = "b968826d9c46dd6066d109eabc6255188de91218"
	metric   = "ttft_p95_ms"
)

type side struct {
	runID, proposalID, revision, state, failure string
	args                                        []string
	ttft                                        float64
	measured                                    *optimizationcampaign.MeasuredEvidence
	quality                                     *domain.QualityEvidence
}

type scenario struct {
	campaignState     string
	current, proposed side
}

// Evidence returns the persisted evidence of a scenario's run.
func Evidence(name string) (optimizationreport.Evidence, error) {
	s, err := build(name)
	if err != nil {
		return optimizationreport.Evidence{}, err
	}
	proposal := optimizer.Proposal{SchemaVersion: optimizer.SchemaVersion, AlgorithmVersion: optimizer.ComparisonAlgorithmVersion, Input: optimizer.Request{ModelIdentity: model, ModelRevision: revision, Provider: "runpod", GPU: "NVIDIA H100 80GB HBM3", GPUCount: 1, Objective: "interactive", WorkloadProfile: "interactive", WorkloadSource: optimizer.WorkloadSourceReviewedDefault}, StartingPoint: &optimizer.StartingPoint{CandidateID: s.current.proposalID}}
	proposalJSON, _ := json.Marshal(proposal)
	campaign := domain.OptimizationCampaign{ID: CampaignID(name), TenantID: tenant, InputDigest: strings.Repeat("c", 64), ModelIdentity: model, Objective: "interactive", Source: optimizer.ComparisonAlgorithmVersion, Intent: optimizationcampaign.IntentNewEndpoint, State: s.campaignState, ProposalJSON: string(proposalJSON)}
	evidence := optimizationreport.Evidence{Benchmarks: map[string]domain.BenchmarkResult{}, Quality: map[string]domain.QualityEvidence{}, ReleaseGuard: map[string]domain.ReleaseGuardEvaluation{}, Rankings: map[string]domain.LabEvaluation{}}
	for rank, arm := range []side{s.current, s.proposed} {
		run := domain.OptimizationCandidateRun{ID: arm.runID, TenantID: tenant, CampaignID: campaign.ID, ProposalCandidateID: arm.proposalID, State: arm.state, FailureCode: arm.failure, Rank: rank + 1, DeploymentSpecJSON: draft(arm.args), DeploymentName: "qwen-chat-opt-" + arm.runID, RevisionID: arm.revision}
		if arm.ttft > 0 {
			run.BenchmarkID = "bench-" + arm.runID
			evidence.Benchmarks[run.BenchmarkID] = benchmark(run, arm)
		}
		if arm.quality != nil {
			run.QualityEvidenceID = arm.quality.ID
			evidence.Quality[arm.quality.ID] = *arm.quality
		}
		campaign.Candidates = append(campaign.Candidates, run)
	}
	evidence.Campaign = campaign
	return evidence, nil
}

// Document assembles a scenario's report with the real report code and
// signs it with key; a nil key leaves it unsigned.
func Document(name string, key ed25519.PrivateKey) (optimizationreport.Document, error) {
	return DocumentAs(name, CampaignID(name), key)
}

// DocumentAs is Document for a run served under another ID.
func DocumentAs(name, campaignID string, key ed25519.PrivateKey) (optimizationreport.Document, error) {
	evidence, err := Evidence(name)
	if err != nil {
		return optimizationreport.Document{}, err
	}
	evidence.Campaign.ID = campaignID
	for index := range evidence.Campaign.Candidates {
		evidence.Campaign.Candidates[index].CampaignID = campaignID
	}
	return optimizationreport.Sign(optimizationreport.Assemble(evidence), key)
}

func build(name string) (scenario, error) {
	current := side{runID: "current", proposalID: "cand-current", revision: "rev-current", state: optimizationcampaign.CandidateQualified, ttft: 312, quality: reference()}
	proposed := side{runID: "proposed", proposalID: "cand-proposed", revision: "rev-proposed", state: optimizationcampaign.CandidateRejected, args: []string{"--max-num-batched-tokens", "8192"}}
	s := scenario{campaignState: optimizationcampaign.CampaignQualified, current: current, proposed: proposed}
	switch name {
	case Pass:
		s.proposed.failure, s.proposed.ttft = optimizationcampaign.FailureNoMeasurableDifference, 308
		s.proposed.measured = measured(-0.012, 0.04, optimizationcampaign.DecisionNoMeasurableDifference)
		s.proposed.quality = answers(0.985)
	case Gain:
		s.current.state, s.current.failure = optimizationcampaign.CandidateRejected, "measured_candidate_superseded"
		s.proposed.state, s.proposed.ttft = optimizationcampaign.CandidateQualified, 256
		s.proposed.measured = measured(-0.18, 0.04, optimizationcampaign.DecisionImproved)
		s.proposed.measured.Confirmation = &optimizationcampaign.ConfirmationEvidence{Control: s.proposed.measured.Control, Comparison: map[string]optimizationcampaign.MetricComparison{metric: comparison(-0.16, 0.05, optimizationcampaign.DecisionImproved)}, FreshCandidate: true, FreshControl: true}
		s.proposed.quality = answers(0.99)
	case Watch:
		s.proposed.failure, s.proposed.ttft = optimizationcampaign.FailureNoMeasurableDifference, 315
		s.proposed.measured = measured(0.009, 0.04, optimizationcampaign.DecisionNoMeasurableDifference)
		s.proposed.quality = answers(0.93)
	case Block:
		s.proposed.failure, s.proposed.ttft = optimizationcampaign.FailureRegressed, 356
		s.proposed.measured = measured(0.141, 0.051, optimizationcampaign.DecisionRegressed)
		s.proposed.quality = answers(0.99)
	case AnswersBlocked:
		s.proposed.failure, s.proposed.ttft = optimizationcampaign.FailureQualityRejected, 309
		s.proposed.measured = measured(-0.01, 0.04, optimizationcampaign.DecisionNoMeasurableDifference)
		s.proposed.quality = answers(0.85)
	case Unknown:
		s.proposed.failure, s.proposed.ttft = optimizationcampaign.FailureComparisonUnavailable, 290
		s.proposed.measured = measured(-0.07, 0.04, optimizationcampaign.DecisionImproved)
		s.proposed.measured.Control.Role, s.proposed.measured.Control.Reason = "drifted", "control p95 time to first token drifted 6.9% between adjacent runs, above the 5% limit"
		s.proposed.quality = answers(0.99)
	case Running:
		s.campaignState = optimizationcampaign.CampaignRunning
		s.current.state = optimizationcampaign.CandidateRanked
		s.proposed.state = optimizationcampaign.CandidateMeasuring
	default:
		return scenario{}, fmt.Errorf("unknown proof gate scenario %q", name)
	}
	return s, nil
}

func draft(args []string) string {
	var d optimizer.DeploymentDraft
	d.APIVersion, d.Kind, d.Name = "infercrane.dev/v1", "Deployment", "qwen-chat"
	d.Model.ID, d.Model.Revision = model, revision
	d.Runtime.Engine, d.Runtime.Version, d.Runtime.Args = "vllm", "0.22.1", append([]string{}, args...)
	d.Resources.GPU, d.Resources.GPUCount = "NVIDIA H100 80GB HBM3", 1
	d.Provider.Cloud, d.Compute.Mode = "runpod", "elastic"
	d.Scaling.MinReplicas, d.Scaling.MaxReplicas = 1, 1
	d.Routing.Strategy = "round-robin"
	encoded, _ := json.Marshal(d)
	return string(encoded)
}

func benchmark(run domain.OptimizationCandidateRun, arm side) domain.BenchmarkResult {
	document := map[string]any{"profile": "interactive", "profile_version": "v1", "concurrency": 8, "request_count_per_run": 60, "workload_source": "synthetic_profile", "measurement_method": optimizationcampaign.MeasurementMethod, "confidence_95": map[string]any{}}
	if arm.measured != nil {
		document["measured"] = arm.measured
	}
	workload, _ := json.Marshal(document)
	gpus := 1
	tps, rps := 1450.0, 4.1
	return domain.BenchmarkResult{ID: run.BenchmarkID, TenantID: tenant, DeploymentName: run.DeploymentName, RevisionID: run.RevisionID, ModelIdentity: model + "@" + revision, Runtime: "vllm", GPU: "NVIDIA H100 80GB HBM3", GPUCount: &gpus, WorkloadJSON: string(workload), OutputTokenThroughput: &tps, RequestThroughput: &rps, TTFTP95MS: ptr(arm.ttft), CostMetadataJSON: `{"available":false}`}
}

func measured(delta, floor float64, decision string) *optimizationcampaign.MeasuredEvidence {
	var runs []optimizationcampaign.RunSummary
	for index := range 7 {
		arm := "control"
		if index%2 == 1 {
			arm = "candidate"
		}
		runs = append(runs, optimizationcampaign.RunSummary{Arm: arm})
	}
	return &optimizationcampaign.MeasuredEvidence{
		Method:     optimizationcampaign.MeasurementMethod,
		Runs:       runs,
		Control:    optimizationcampaign.ControlEvidence{Role: optimizationcampaign.ControlRoleBracketed, CandidateID: "current", Runs: 4, MaxDrift: 0.013, DriftLimit: 0.05},
		Comparison: map[string]optimizationcampaign.MetricComparison{metric: comparison(delta, floor, decision)},
	}
}

func comparison(delta, floor float64, decision string) optimizationcampaign.MetricComparison {
	return optimizationcampaign.MetricComparison{ControlMean: 312, CandidateMean: 312 * (1 + delta), Delta: delta, NoiseFloor: floor, Decision: decision}
}

func reference() *domain.QualityEvidence {
	return &domain.QualityEvidence{ID: "quality-current", TenantID: tenant, RevisionID: "rev-current", Suite: "infercrane-answer-check", SuiteVersion: "v1", Evaluator: "infercrane", EvaluatorVersion: "v1", Source: domain.QualityEvidenceSourceInferCrane, ReferenceRevisionID: "rev-current", Score: 1, Passed: true, SampleCount: 64, EvaluatedAt: time.Unix(1, 0).UTC()}
}

func answers(score float64) *domain.QualityEvidence {
	row := reference()
	row.ID, row.RevisionID, row.Score, row.Passed = "quality-proposed", "rev-proposed", score, score >= 0.9
	return row
}

func ptr(value float64) *float64 { return &value }
