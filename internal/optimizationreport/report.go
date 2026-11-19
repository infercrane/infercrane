// Package optimizationreport assembles the customer outcome of one
// optimization campaign: the baseline (the campaign control) against the
// winning setup on cost, speed, and answer quality, with the measurement that
// proves it. It reads persisted evidence only. A missing or incomparable input
// stays unknown; nothing is estimated and nothing defaults to zero.
package optimizationreport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/releaseguard"
	"github.com/infercrane/infercrane/internal/replay"
)

const (
	SchemaVersion    = "infercrane.optimization-report/v1"
	AlgorithmVersion = "campaign-outcome-report-v2"

	// CPUBanner is shown on every report whose campaign ran on a simulated
	// accelerator or a CPU. Its numbers are never GPU results.
	CPUBanner = "CPU run: proves the pipeline, not GPU speed"

	// HoursPerMonth is the month length used for monthly projections.
	HoursPerMonth = 730.0

	OutcomeWinnerSelected = "winner_selected"
	OutcomeBaselineKept   = "baseline_kept"
	OutcomeNoWinner       = "no_winner"
	OutcomeInProgress     = "in_progress"
	OutcomeCancelled      = "cancelled"

	DecisionImproved     = "improved"
	DecisionRegressed    = "regressed"
	DecisionNoDifference = "no_measurable_difference"
	DecisionUnchanged    = "unchanged"
	DecisionUnknown      = "unknown"

	BasisInterleaved   = "interleaved_control"
	BasisSeparateRuns  = "separate_runs"
	BasisBaselineKept  = "baseline_kept"
	BasisReleaseGuard  = "release_guard"
	BasisCampaignGate  = "campaign_quality_gate"
	BasisBuiltinCheck  = "builtin_answer_check"
	BasisReplayVolume  = "replayed_traffic"
	BasisEngineMetrics = "engine_metrics_during_measurement"
	BasisUnknown       = "unknown"

	AcceleratorGPU       = "gpu"
	AcceleratorSimulated = "simulated_cpu"
	AcceleratorUnknown   = "unknown"

	MetricCostPerMillion  = "cost_per_million_output_tokens"
	MetricTTFTP95         = "ttft_p95_ms"
	MetricTokensPerUser   = "output_tokens_per_second_per_user"
	MetricTokensPerAccel  = "output_tokens_per_second_per_accelerator"
	MetricRequestsPerSec  = "requests_per_second"
	sourceOutputTPS       = "output_token_throughput"
	sourceRequestTPS      = "request_throughput"
	sourceTTFTP95         = "ttft_p95_ms"
	sourceTPOTP95         = "tpot_p95_ms"
	maxEngineSignals      = 12
	monthlyAssumptionText = "measured cost per output token times your replayed output-token volume; assumes servers stay busy, so idle capacity is not included"
)

// Report is the customer outcome of one campaign. Every pointer is null when
// its input is missing or incomparable. Unknown lists those fields by path.
// Warnings name every setup that failed more requests or answered shorter
// than the baseline: a speed or cost gain may come from that.
type Report struct {
	SchemaVersion        string            `json:"schema_version"`
	AlgorithmVersion     string            `json:"algorithm_version"`
	CampaignID           string            `json:"campaign_id"`
	ModelIdentity        string            `json:"model_identity"`
	Objective            string            `json:"objective"`
	Intent               string            `json:"intent"`
	CampaignState        string            `json:"campaign_state"`
	Finished             bool              `json:"finished"`
	SimulatedAccelerator bool              `json:"simulated_accelerator"`
	Banner               string            `json:"banner,omitempty"`
	Outcome              Outcome           `json:"outcome"`
	Metrics              []Metric          `json:"metrics"`
	Quality              Quality           `json:"quality"`
	Noise                Noise             `json:"noise"`
	Monthly              Monthly           `json:"monthly"`
	Workload             Workload          `json:"workload"`
	Candidates           []CandidateResult `json:"candidates"`
	Warnings             []string          `json:"warnings"`
	Bottleneck           Bottleneck        `json:"bottleneck"`
	Appendix             Appendix          `json:"appendix"`
	Unknown              []string          `json:"unknown"`
	ReportDigest         string            `json:"report_digest"`
}

// Outcome is the decision in plain words.
type Outcome struct {
	Kind                string `json:"kind"`
	Headline            string `json:"headline"`
	BaselineCandidateID string `json:"baseline_candidate_id"`
	BaselineLabel       string `json:"baseline_label"`
	WinnerCandidateID   string `json:"winner_candidate_id"`
	WinnerLabel         string `json:"winner_label"`
	Activated           bool   `json:"activated"`
}

// Metric compares the baseline and the winner on one outcome. ChangePercent
// is the winner relative to the baseline. Decision is measured against the
// interleaved control's noise floor; without it the decision is unknown.
type Metric struct {
	Key               string   `json:"key"`
	Label             string   `json:"label"`
	Unit              string   `json:"unit"`
	HigherIsBetter    bool     `json:"higher_is_better"`
	Baseline          *float64 `json:"baseline"`
	Winner            *float64 `json:"winner"`
	ChangePercent     *float64 `json:"change_percent"`
	NoiseFloorPercent *float64 `json:"noise_floor_percent"`
	Decision          string   `json:"decision"`
	Basis             string   `json:"basis"`
	Reason            string   `json:"reason,omitempty"`
}

// Quality is the answer-quality verdict of the winner against the baseline.
type Quality struct {
	Verdict                  string   `json:"verdict"`
	Basis                    string   `json:"basis"`
	Eval                     string   `json:"eval,omitempty"`
	BaselineScore            *float64 `json:"baseline_score"`
	WinnerScore              *float64 `json:"winner_score"`
	Delta                    *float64 `json:"delta"`
	RegressionPercent        *float64 `json:"regression_percent"`
	WatchThresholdPercent    *float64 `json:"watch_threshold_percent"`
	BlockThresholdPercent    *float64 `json:"block_threshold_percent"`
	ReleaseGuardEvaluationID string   `json:"release_guard_evaluation_id,omitempty"`
	// Source names who produced the winner's deciding evidence: infercrane
	// for the built-in answer check, customer for signed customer evidence.
	Source string        `json:"source,omitempty"`
	Reason string        `json:"reason,omitempty"`
	Evals  []QualityEval `json:"evals"`
}

// QualityEval is one evaluation suite's verdict.
type QualityEval struct {
	Suite             string   `json:"suite"`
	Evaluator         string   `json:"evaluator"`
	Verdict           string   `json:"verdict"`
	BaselineScore     *float64 `json:"baseline_score"`
	WinnerScore       float64  `json:"winner_score"`
	Delta             *float64 `json:"delta"`
	RegressionPercent *float64 `json:"regression_percent"`
	Message           string   `json:"message"`
	FollowUp          string   `json:"follow_up,omitempty"`
}

// Noise is the measurement noise band from the interleaved control runs.
type Noise struct {
	Basis                      string   `json:"basis"`
	CandidateID                string   `json:"candidate_id,omitempty"`
	ControlRuns                int      `json:"control_runs"`
	CandidateRuns              int      `json:"candidate_runs"`
	ControlDriftPercent        *float64 `json:"control_drift_percent"`
	DriftLimitPercent          *float64 `json:"drift_limit_percent"`
	ObjectiveMetric            string   `json:"objective_metric,omitempty"`
	ObjectiveChangePercent     *float64 `json:"objective_change_percent"`
	ObjectiveNoiseFloorPercent *float64 `json:"objective_noise_floor_percent"`
	ObjectiveDecision          string   `json:"objective_decision"`
	Reason                     string   `json:"reason,omitempty"`
}

// Monthly projects cost at the replayed customer traffic volume.
// WindowSeconds is the time the volume is divided by: the capture window, or,
// when the capture was cut off at its request limit, the span from the first
// to the last kept request. CaptureCutOff is null for a capture that did not
// record whether it was cut off.
type Monthly struct {
	Basis                string   `json:"basis"`
	TraceDigest          string   `json:"trace_digest,omitempty"`
	WindowSeconds        *float64 `json:"window_seconds"`
	CaptureWindowSeconds *float64 `json:"capture_window_seconds"`
	CaptureCutOff        *bool    `json:"capture_cut_off"`
	RequestsInWindow     *int     `json:"requests_in_window"`
	OutputTokensInWindow *int64   `json:"output_tokens_in_window"`
	MonthlyOutputTokens  *float64 `json:"monthly_output_tokens"`
	HoursPerMonth        float64  `json:"hours_per_month"`
	BaselineUSD          *float64 `json:"baseline_usd"`
	WinnerUSD            *float64 `json:"winner_usd"`
	ChangeUSD            *float64 `json:"change_usd"`
	ChangePercent        *float64 `json:"change_percent"`
	Decision             string   `json:"decision"`
	Assumption           string   `json:"assumption,omitempty"`
	Reason               string   `json:"reason,omitempty"`
}

// Workload describes the load every setup received.
type Workload struct {
	Source         string `json:"source"`
	Profile        string `json:"profile,omitempty"`
	Concurrency    *int   `json:"concurrency"`
	RequestsPerRun *int   `json:"requests_per_run"`
	Method         string `json:"method,omitempty"`
}

// CandidateResult is one measured setup in the measurement detail.
// ErrorRatePercent and OutputTokensPerRequest come from the setup's own runs:
// the share of its requests that failed, and the mean output tokens of a
// successful request by the server's count. Each is null when not recorded.
type CandidateResult struct {
	ID                         string      `json:"id"`
	Label                      string      `json:"label"`
	Role                       string      `json:"role"`
	State                      string      `json:"state"`
	FailureCode                string      `json:"failure_code,omitempty"`
	OutputTokensPerSecond      *float64    `json:"output_tokens_per_second"`
	RequestsPerSecond          *float64    `json:"requests_per_second"`
	TTFTP95MS                  *float64    `json:"ttft_p95_ms"`
	TPOTP95MS                  *float64    `json:"tpot_p95_ms"`
	LatencyP95MS               *float64    `json:"latency_p95_ms"`
	CostPerMillionOutputTokens *float64    `json:"cost_per_million_output_tokens"`
	ErrorRatePercent           *float64    `json:"error_rate_percent"`
	OutputTokensPerRequest     *float64    `json:"output_tokens_per_request"`
	VersusBaseline             *Comparison `json:"versus_baseline"`
	// Regressions are the headline outcomes on which the setup measured worse
	// than the baseline in its own window, so a rejection can name them.
	Regressions []Comparison `json:"regressions,omitempty"`
	// Guards are why the setup cannot be chosen whatever it gained: it failed
	// more requests than the baseline, or answered shorter.
	Guards []Guard `json:"guards,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

// Guard is one result that keeps a setup from being chosen (see
// optimizationcampaign.CheckGuards). Baseline and Value are percent of
// requests for error_rate_percent and tokens for output_tokens_per_request.
// Basis says whether the baseline's side was measured in the setup's own
// window (same_window) or in the baseline's own runs (separate_runs).
type Guard struct {
	Code     string  `json:"code"`
	Metric   string  `json:"metric"`
	Baseline float64 `json:"baseline"`
	Value    float64 `json:"value"`
	Basis    string  `json:"basis"`
	Reason   string  `json:"reason"`
}

// Comparison is a candidate's result on one metric against the baseline,
// measured in interleaved runs. Basis is set on a regression read from runs
// whose control drifted (see optimizationcampaign.MeasuredRegressions).
type Comparison struct {
	Metric            string  `json:"metric"`
	ChangePercent     float64 `json:"change_percent"`
	NoiseFloorPercent float64 `json:"noise_floor_percent"`
	Decision          string  `json:"decision"`
	Basis             string  `json:"basis,omitempty"`
}

// Bottleneck holds the serving-engine signals reported while the baseline and
// the winner were measured. It is evidence, not a diagnosis.
type Bottleneck struct {
	State        string         `json:"state"`
	Source       string         `json:"source"`
	GPUTelemetry string         `json:"gpu_telemetry"`
	Signals      []EngineSignal `json:"signals"`
	Reason       string         `json:"reason,omitempty"`
}

// EngineSignal is one runtime metric statistic for both setups.
type EngineSignal struct {
	Name     string   `json:"name"`
	Stat     string   `json:"stat"`
	Unit     string   `json:"unit,omitempty"`
	Baseline *float64 `json:"baseline"`
	Winner   *float64 `json:"winner"`
}

// Appendix holds the exact setups and the evidence identities behind them.
type Appendix struct {
	Winner   *Setup           `json:"winner"`
	Baseline *Setup           `json:"baseline"`
	Evidence []EvidenceDigest `json:"evidence"`
}

// Setup is one exact serving configuration.
type Setup struct {
	CandidateID   string   `json:"candidate_id"`
	Label         string   `json:"label"`
	Model         string   `json:"model"`
	Engine        string   `json:"engine"`
	EngineVersion string   `json:"engine_version"`
	Args          []string `json:"args"`
	// Env is the setup's reviewed engine environment; absent when none is set.
	Env              map[string]string `json:"env,omitempty"`
	Image            string            `json:"image,omitempty"`
	Accelerator      string            `json:"accelerator"`
	AcceleratorCount *int              `json:"accelerator_count"`
	AcceleratorClass string            `json:"accelerator_class"`
	Provider         string            `json:"provider"`
	Region           string            `json:"region,omitempty"`
	ComputeMode      string            `json:"compute_mode,omitempty"`
	MinReplicas      int               `json:"min_replicas"`
	MaxReplicas      int               `json:"max_replicas"`
	DeploymentName   string            `json:"deployment_name,omitempty"`
	RevisionID       string            `json:"revision_id,omitempty"`
	HourlyUSD        *float64          `json:"hourly_usd_per_replica"`
	PriceSource      string            `json:"price_source,omitempty"`
}

// EvidenceDigest names one persisted record the report rests on.
type EvidenceDigest struct {
	Kind    string `json:"kind"`
	ID      string `json:"id,omitempty"`
	Digest  string `json:"digest"`
	Subject string `json:"subject,omitempty"`
}

// Evidence is everything Assemble reads. Lookups are keyed by record ID.
type Evidence struct {
	Campaign     domain.OptimizationCampaign
	Benchmarks   map[string]domain.BenchmarkResult
	Quality      map[string]domain.QualityEvidence
	ReleaseGuard map[string]domain.ReleaseGuardEvaluation
	// Rankings are the persisted campaign rankings the candidates recorded,
	// keyed by Lab evaluation ID. They still say what a finished campaign
	// selected after cleanup rewrote every candidate state.
	Rankings map[string]domain.LabEvaluation
	// Replay is the customer trace the campaign replayed, when it is bound to
	// one and was found. ReplayReason says why it is absent.
	Replay       *domain.ReplayTrace
	ReplayReason string
}

// arm is one candidate with its decoded evidence.
type arm struct {
	label    string
	run      domain.OptimizationCandidateRun
	draft    optimizer.DeploymentDraft
	draftOK  bool
	bench    *domain.BenchmarkResult
	measured *optimizationcampaign.MeasuredEvidence
	workload map[string]json.RawMessage
	hourly   *float64
	source   string
	reason   string
}

// Assemble builds the report from persisted evidence. It does no I/O.
func Assemble(e Evidence) Report {
	campaign := e.Campaign
	report := Report{SchemaVersion: SchemaVersion, AlgorithmVersion: AlgorithmVersion, CampaignID: campaign.ID, ModelIdentity: campaign.ModelIdentity, Objective: campaign.Objective, Intent: campaign.Intent, CampaignState: campaign.State, Finished: finished(campaign.State)}
	var proposal optimizer.Proposal
	proposalOK := json.Unmarshal([]byte(campaign.ProposalJSON), &proposal) == nil

	arms := map[string]*arm{}
	labels := optimizationcampaign.CandidateLabels(campaign)
	for _, run := range campaign.Candidates {
		arms[run.ID] = newArm(run, e)
		arms[run.ID].label = labels[run.ID]
	}
	var baseline, winner *arm
	if control, ok := optimizationcampaign.CampaignControl(campaign); ok {
		baseline = arms[control.ID]
	}
	winner = selectWinner(campaign, arms)
	if winner == nil && report.Finished {
		winner = decidedBeforeCleanup(campaign, arms, e)
	}

	for _, current := range arms {
		if current.draftOK && acceleratorClass(acceleratorOf(current)) == AcceleratorSimulated {
			report.SimulatedAccelerator = true
		}
	}
	if report.SimulatedAccelerator {
		report.Banner = CPUBanner
	}

	report.Outcome = outcome(campaign, baseline, winner, report.Finished)
	kind := report.Outcome.Kind
	report.Metrics = metrics(kind, baseline, winner, report.SimulatedAccelerator)
	report.Quality = quality(campaign, kind, baseline, winner, e)
	report.Noise = noise(campaign.Objective, kind, baseline, winner, arms)
	report.Monthly = monthly(proposal, proposalOK, kind, report.Metrics, e)
	report.Workload = workload(baseline, winner)
	report.Candidates = candidateResults(campaign, campaign.Objective, baseline, winner, arms)
	report.Warnings = warnings(report.Candidates)
	report.Bottleneck = bottleneck(kind, baseline, winner)
	report.Appendix = appendix(campaign, proposal, proposalOK, baseline, winner, e)
	report.Outcome.Headline = headline(report, released(campaign, winner))
	report.Unknown = unknownFields(report)
	report.ReportDigest = digest(report)
	return report
}

func finished(state string) bool {
	switch state {
	case optimizationcampaign.CampaignQualified, optimizationcampaign.CampaignGuardPassed, optimizationcampaign.CampaignRejected,
		optimizationcampaign.CampaignInconclusive, optimizationcampaign.CampaignPromoted, optimizationcampaign.CampaignObserved,
		optimizationcampaign.CampaignCancelled, optimizationcampaign.CampaignFailed, optimizationcampaign.CampaignCleaned:
		return true
	}
	return false
}

func newArm(run domain.OptimizationCandidateRun, e Evidence) *arm {
	current := &arm{run: run}
	if json.Unmarshal([]byte(run.DeploymentSpecJSON), &current.draft) == nil && current.draft.Runtime.Engine != "" {
		current.draftOK = true
	}
	if run.BenchmarkID == "" {
		current.reason = "not measured"
		return current
	}
	row, found := e.Benchmarks[run.BenchmarkID]
	if !found {
		current.reason = "its benchmark record could not be found"
		return current
	}
	current.bench = &row
	var document map[string]json.RawMessage
	if json.Unmarshal([]byte(row.WorkloadJSON), &document) == nil {
		current.workload = document
		var measured optimizationcampaign.MeasuredEvidence
		if raw, ok := document["measured"]; ok && json.Unmarshal(raw, &measured) == nil && measured.Method != "" {
			current.measured = &measured
		}
	}
	var cost struct {
		Available bool     `json:"available"`
		Hourly    *float64 `json:"hourly"`
		Currency  string   `json:"currency"`
		Source    string   `json:"source"`
	}
	if json.Unmarshal([]byte(row.CostMetadataJSON), &cost) == nil && cost.Available && cost.Source != "" && (cost.Currency == "" || cost.Currency == "USD") && finitePositive(cost.Hourly) {
		replicas := 1
		if current.draftOK && current.draft.Scaling.MaxReplicas > 1 {
			replicas = current.draft.Scaling.MaxReplicas
		}
		// A sourced quote covers the maximum replica count; the benchmark
		// measured one replica directly.
		perReplica := *cost.Hourly / float64(replicas)
		current.hourly, current.source = &perReplica, cost.Source
	}
	return current
}

// selectWinner returns the candidate the campaign chose: activated first,
// then the one waiting for activation or promotion.
func selectWinner(campaign domain.OptimizationCampaign, arms map[string]*arm) *arm {
	var waiting *arm
	for _, run := range campaign.Candidates {
		switch run.State {
		case optimizationcampaign.CandidatePromoted, optimizationcampaign.CandidateObserved:
			return arms[run.ID]
		case optimizationcampaign.CandidateQualified, optimizationcampaign.CandidateGuardPassed:
			if waiting == nil {
				waiting = arms[run.ID]
			}
		}
	}
	return waiting
}

// decidedBeforeCleanup recovers the setup a finished campaign chose when no
// candidate is live or waiting any more. Cleanup and cancellation rewrite
// every candidate to cleaned or cancelled, and the proposal rank is only the
// planner's order, so neither says what qualified. What survives is the
// ranking each candidate recorded when it was decided. A candidate counts as
// chosen only when its own persisted ranking selected it, it was measured, its
// answers did not fail, no gate failed it afterwards, and, for an evolution,
// Release Guard accepted it. Anything less leaves the campaign without a
// winner. Should several qualify, the lowest proposal rank wins, so the
// result never depends on row order.
func decidedBeforeCleanup(campaign domain.OptimizationCampaign, arms map[string]*arm, e Evidence) *arm {
	runs := append([]domain.OptimizationCandidateRun(nil), campaign.Candidates...)
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Rank < runs[j].Rank })
	for _, run := range runs {
		if qualifiedBeforeCleanup(campaign, run, arms[run.ID], e) {
			return arms[run.ID]
		}
	}
	return nil
}

// releasedWithoutFailure are the failure codes a qualified candidate gets when
// it is let go without any gate failing it: none when the operator cancelled
// the run, its approval ending before activation, or execution cancellation.
var releasedWithoutFailure = map[string]bool{"": true, "approval_expired_before_activation": true, "execution_cancelled": true}

func qualifiedBeforeCleanup(campaign domain.OptimizationCampaign, run domain.OptimizationCandidateRun, current *arm, e Evidence) bool {
	switch run.State {
	case optimizationcampaign.CandidateCleaned, optimizationcampaign.CandidateCancelled, optimizationcampaign.CandidateFailed:
	default:
		return false
	}
	if current == nil || current.bench == nil || run.QualityEvidenceID == "" || !releasedWithoutFailure[run.FailureCode] {
		return false
	}
	if answers, found := e.Quality[run.QualityEvidenceID]; found && !answers.Passed {
		return false
	}
	ranking, found := e.Rankings[run.LabEvaluationID]
	if run.LabEvaluationID == "" || !found {
		return false
	}
	decisions, err := optimizationcampaign.RankingDecisions(ranking)
	if err != nil || decisions[run.ID] != optimizationcampaign.RankSelect {
		return false
	}
	if campaign.Intent != optimizationcampaign.IntentEvolveEndpoint {
		return true
	}
	evaluation, found := e.ReleaseGuard[run.ReleaseGuardEvaluationID]
	decision := strings.ToUpper(strings.TrimSpace(evaluation.Decision))
	return run.ReleaseGuardEvaluationID != "" && found && (decision == "ACCEPT" || decision == "PASS")
}

// released says how a chosen setup that never went live was let go, for the
// headline. It is empty while the setup is live or still waiting.
func released(campaign domain.OptimizationCampaign, winner *arm) string {
	if winner == nil {
		return ""
	}
	removed := "its server is being removed"
	switch winner.run.State {
	case optimizationcampaign.CandidateQualified, optimizationcampaign.CandidateGuardPassed, optimizationcampaign.CandidatePromoted, optimizationcampaign.CandidateObserved:
		return ""
	case optimizationcampaign.CandidateCleaned:
		removed = "its server was removed"
	}
	switch {
	case winner.run.FailureCode == "approval_expired_before_activation":
		return removed + " because the approval ended"
	case campaign.CancelRequested || winner.run.FailureCode == "execution_cancelled":
		return removed + " because the run was cancelled"
	default:
		return removed
	}
}

func outcome(campaign domain.OptimizationCampaign, baseline, winner *arm, done bool) Outcome {
	result := Outcome{}
	if baseline != nil {
		result.BaselineCandidateID, result.BaselineLabel = baseline.run.ID, label(baseline)
	}
	if winner != nil {
		result.WinnerCandidateID, result.WinnerLabel = winner.run.ID, label(winner)
		result.Activated = winner.run.State == optimizationcampaign.CandidatePromoted || winner.run.State == optimizationcampaign.CandidateObserved
	}
	switch {
	case winner == nil && campaign.State == optimizationcampaign.CampaignCancelled:
		result.Kind = OutcomeCancelled
	case winner == nil && !done:
		result.Kind = OutcomeInProgress
	case winner == nil:
		result.Kind = OutcomeNoWinner
	case baseline != nil && winner.run.ID == baseline.run.ID:
		result.Kind = OutcomeBaselineKept
	default:
		result.Kind = OutcomeWinnerSelected
	}
	return result
}

// metricSpec derives one outcome from a measured source metric. scale is the
// per-arm divisor that turns the source into the outcome (hourly cost for
// cost per token, accelerator count for per-accelerator throughput); a nil
// scale means the outcome is the source itself.
type metricSpec struct {
	key, label, unit string
	higherBetter     bool
	source           string
	fromRow          func(domain.BenchmarkResult) *float64
	value            func(source float64, a *arm) (*float64, string)
	scaled           func(a *arm) (*float64, string)
}

func specs(simulated bool) []metricSpec {
	perAccelerator := "Output tokens/s per GPU"
	if simulated {
		perAccelerator = "Output tokens/s per simulated accelerator"
	}
	return []metricSpec{
		{key: MetricCostPerMillion, label: "Cost per 1M output tokens", unit: "usd", source: sourceOutputTPS, fromRow: rowOutputTPS,
			value: func(tps float64, a *arm) (*float64, string) {
				if a.hourly == nil {
					return nil, "no sourced hourly price"
				}
				if tps <= 0 {
					return nil, "no measured output throughput"
				}
				return ptr(*a.hourly / (tps * 3600) * 1_000_000), ""
			},
			scaled: func(a *arm) (*float64, string) {
				if a.hourly == nil {
					return nil, "no sourced hourly price"
				}
				return a.hourly, ""
			}},
		{key: MetricTTFTP95, label: "p95 time to first token", unit: "ms", source: sourceTTFTP95, fromRow: func(row domain.BenchmarkResult) *float64 { return row.TTFTP95MS },
			value: func(value float64, _ *arm) (*float64, string) { return ptr(value), "" }},
		{key: MetricTokensPerUser, label: "Output tokens/s per user", unit: "tokens_per_second", higherBetter: true, source: sourceTPOTP95, fromRow: func(row domain.BenchmarkResult) *float64 { return row.TPOTP95MS },
			value: func(tpot float64, _ *arm) (*float64, string) {
				if tpot <= 0 {
					return nil, "no measured time per output token"
				}
				return ptr(1000 / tpot), ""
			}},
		{key: MetricTokensPerAccel, label: perAccelerator, unit: "tokens_per_second", higherBetter: true, source: sourceOutputTPS, fromRow: rowOutputTPS,
			value: func(tps float64, a *arm) (*float64, string) {
				count := acceleratorCount(a)
				if count == nil {
					return nil, "accelerator count is unknown"
				}
				return ptr(tps / float64(*count)), ""
			},
			scaled: func(a *arm) (*float64, string) {
				count := acceleratorCount(a)
				if count == nil {
					return nil, "accelerator count is unknown"
				}
				return ptr(float64(*count)), ""
			}},
		{key: MetricRequestsPerSec, label: "Requests/s", unit: "requests_per_second", higherBetter: true, source: sourceRequestTPS, fromRow: func(row domain.BenchmarkResult) *float64 { return row.RequestThroughput },
			value: func(value float64, _ *arm) (*float64, string) { return ptr(value), "" }},
	}
}

func rowOutputTPS(row domain.BenchmarkResult) *float64 { return row.OutputTokenThroughput }

func metrics(kind string, baseline, winner *arm, simulated bool) []Metric {
	out := make([]Metric, 0, 5)
	for _, spec := range specs(simulated) {
		metric := Metric{Key: spec.key, Label: spec.label, Unit: spec.unit, HigherIsBetter: spec.higherBetter, Decision: DecisionUnknown, Basis: BasisUnknown}
		switch kind {
		case OutcomeInProgress:
			metric.Reason = "the campaign is still running"
		case OutcomeCancelled:
			metric.Reason = "the campaign was cancelled before a setup qualified"
		case OutcomeNoWinner:
			metric.Reason = "no setup qualified"
			metric.Baseline, _ = ownValue(spec, baseline)
		case OutcomeBaselineKept:
			value, reason := ownValue(spec, baseline)
			metric.Baseline, metric.Winner, metric.Basis = value, value, BasisBaselineKept
			if value != nil {
				metric.Decision = DecisionUnchanged
			} else {
				metric.Reason = "baseline: " + reason
			}
		case OutcomeWinnerSelected:
			compareWinner(&metric, spec, baseline, winner)
		}
		out = append(out, metric)
	}
	return out
}

// ownValue reads an outcome from an arm's own pooled benchmark row.
func ownValue(spec metricSpec, a *arm) (*float64, string) {
	if a == nil {
		return nil, "the campaign has no baseline"
	}
	if a.bench == nil {
		return nil, a.reason
	}
	source := spec.fromRow(*a.bench)
	if !finitePositive(source) {
		return nil, spec.source + " was not measured"
	}
	return spec.value(*source, a)
}

func compareWinner(metric *Metric, spec metricSpec, baseline, winner *arm) {
	if baseline == nil {
		metric.Winner, _ = ownValue(spec, winner)
		metric.Reason = "the campaign has no baseline to compare with"
		return
	}
	measured := winner.measured
	if measured != nil && measured.Control.Role == optimizationcampaign.ControlRoleBracketed && measured.Control.CandidateID == baseline.run.ID {
		comparison, found := measured.Comparison[spec.source]
		if !found {
			metric.Baseline, _ = ownValue(spec, baseline)
			metric.Winner, _ = ownValue(spec, winner)
			metric.Reason = spec.source + " was not comparable between the interleaved runs"
			return
		}
		metric.Basis = BasisInterleaved
		base, baseReason := spec.value(comparison.ControlMean, baseline)
		won, wonReason := spec.value(comparison.CandidateMean, winner)
		metric.Baseline, metric.Winner = base, won
		if base == nil || won == nil {
			metric.Reason = firstReason("baseline", baseReason, "winner", wonReason)
			return
		}
		metric.ChangePercent = ptr((*won / *base - 1) * 100)
		metric.NoiseFloorPercent = ptr(comparison.NoiseFloor * 100)
		if spec.scaled == nil {
			metric.Decision = comparison.Decision
			return
		}
		// Per-cost and per-accelerator outcomes rescale the measured source
		// delta exactly as the campaign ranking does, against the same floor.
		baseScale, _ := spec.scaled(baseline)
		winScale, _ := spec.scaled(winner)
		delta := (1+comparison.Delta)*(*baseScale / *winScale) - 1
		metric.Decision = decide(delta, comparison.NoiseFloor)
		return
	}
	metric.Basis = BasisSeparateRuns
	base, baseReason := ownValue(spec, baseline)
	won, wonReason := ownValue(spec, winner)
	metric.Baseline, metric.Winner = base, won
	switch {
	case base == nil || won == nil:
		metric.Reason = firstReason("baseline", baseReason, "winner", wonReason)
	case !sameWorkload(baseline, winner):
		metric.Reason = "the baseline and the winner were measured on different workloads"
	default:
		metric.ChangePercent = ptr((*won / *base - 1) * 100)
		metric.Reason = "the baseline was not measured alongside the winner" + controlReason(measured) + ", so the change is not checked against the noise floor"
	}
}

func decide(delta, floor float64) string {
	switch {
	case math.Abs(delta) <= floor:
		return DecisionNoDifference
	case delta > 0:
		return DecisionImproved
	default:
		return DecisionRegressed
	}
}

func controlReason(measured *optimizationcampaign.MeasuredEvidence) string {
	if measured != nil && measured.Control.Reason != "" {
		return " (" + measured.Control.Reason + ")"
	}
	return ""
}

func firstReason(firstName, first, secondName, second string) string {
	if first != "" {
		return firstName + ": " + first
	}
	if second != "" {
		return secondName + ": " + second
	}
	return "a required measurement is missing"
}

func sameWorkload(a, b *arm) bool {
	if a.bench == nil || b.bench == nil {
		return false
	}
	return lab.WorkloadDigest(a.bench.WorkloadJSON) == lab.WorkloadDigest(b.bench.WorkloadJSON)
}

func quality(campaign domain.OptimizationCampaign, kind string, baseline, winner *arm, e Evidence) Quality {
	result := Quality{Verdict: releaseguard.QualityUnknown, Basis: BasisUnknown, Evals: []QualityEval{}}
	if winner == nil {
		result.Reason = "no setup qualified"
		if kind == OutcomeInProgress {
			result.Reason = "the campaign is still running"
		}
		return result
	}
	if winner.run.ReleaseGuardEvaluationID != "" {
		if evaluation, found := e.ReleaseGuard[winner.run.ReleaseGuardEvaluationID]; found {
			var metrics struct {
				Quality *releaseguard.QualityReport `json:"quality"`
			}
			if json.Unmarshal([]byte(evaluation.MetricsJSON), &metrics) == nil && metrics.Quality != nil {
				result = fromQualityReport(*metrics.Quality)
				result.Basis, result.ReleaseGuardEvaluationID = BasisReleaseGuard, evaluation.ID
				return result
			}
		}
	}
	winnerEvidence, found := e.Quality[winner.run.QualityEvidenceID]
	if winner.run.QualityEvidenceID == "" || !found {
		result.Reason = "no signed quality evidence was found for the winner"
		return result
	}
	eval := domain.QualityEvalEvidence{Suite: winnerEvidence.Suite, SuiteVersion: winnerEvidence.SuiteVersion, Evaluator: winnerEvidence.Evaluator, EvaluatorVersion: winnerEvidence.EvaluatorVersion, Candidate: qualitySide(winnerEvidence)}
	if kind == OutcomeWinnerSelected && baseline != nil {
		if reference, ok := e.Quality[baseline.run.QualityEvidenceID]; ok && baseline.run.QualityEvidenceID != "" && reference.Suite == winnerEvidence.Suite && reference.SuiteVersion == winnerEvidence.SuiteVersion && reference.Evaluator == winnerEvidence.Evaluator && reference.EvaluatorVersion == winnerEvidence.EvaluatorVersion {
			side := qualitySide(reference)
			eval.Reference = &side
		}
	}
	source := winnerEvidence.Source
	if source == "" {
		source = domain.QualityEvidenceSourceCustomer
	}
	if source == domain.QualityEvidenceSourceInferCrane {
		// The built-in answer check carries its own fixed watch and block
		// thresholds, the same ones it applied when it gated the candidate.
		result = fromQualityReport(releaseguard.CompareQuality(qualitycheck.Policy(), []domain.QualityEvalEvidence{eval}))
		result.Basis, result.Source = BasisBuiltinCheck, source
		switch {
		case kind == OutcomeBaselineKept:
			result.Basis, result.Reason = BasisBaselineKept, "your current setup is kept, so answer quality is unchanged"
		case eval.Reference == nil:
			result.Reason = "the baseline has no answer check from the same prompts, so the change is unknown"
		default:
			result.Reason = "InferCrane asked both setups the same prompts and compared the winner's answers with the baseline's"
		}
		return result
	}
	// The campaign's own gate is pass or fail on signed evidence; without a
	// Release Guard policy there is no watch band, so WATCH cannot occur.
	result = fromQualityReport(releaseguard.CompareQuality(domain.ReleaseGuardPolicy{}, []domain.QualityEvalEvidence{eval}))
	result.Basis, result.Source = BasisCampaignGate, source
	switch {
	case kind == OutcomeBaselineKept:
		result.Basis, result.Reason = BasisBaselineKept, "your current setup is kept, so answer quality is unchanged"
	case eval.Reference == nil:
		result.Reason = "the baseline has no signed evidence from the same eval and evaluator versions, so the change is unknown"
	default:
		result.Reason = "no Release Guard watch band applies to this campaign; the verdict is the signed eval's pass or fail"
	}
	return result
}

func qualitySide(row domain.QualityEvidence) domain.QualityEvalSide {
	return domain.QualityEvalSide{EvidenceID: row.ID, Score: row.Score, Passed: row.Passed, SampleCount: row.SampleCount}
}

func fromQualityReport(report releaseguard.QualityReport) Quality {
	result := Quality{Verdict: report.Verdict, WatchThresholdPercent: report.WatchRegressionPercent, BlockThresholdPercent: report.MaxRegressionPercent, Evals: make([]QualityEval, 0, len(report.Evals))}
	if result.Verdict == "" {
		result.Verdict = releaseguard.QualityUnknown
	}
	deciding := -1
	for index, eval := range report.Evals {
		result.Evals = append(result.Evals, QualityEval{Suite: eval.Suite + "@" + eval.SuiteVersion, Evaluator: eval.Evaluator + "@" + eval.EvaluatorVersion, Verdict: eval.Verdict, BaselineScore: eval.ReferenceScore, WinnerScore: eval.CandidateScore, Delta: eval.Delta, RegressionPercent: eval.RegressionPercent, Message: eval.Message, FollowUp: eval.FollowUp})
		if deciding < 0 && eval.Verdict == report.Verdict {
			deciding = index
		}
	}
	if deciding >= 0 {
		chosen := result.Evals[deciding]
		score := chosen.WinnerScore
		result.Eval, result.BaselineScore, result.WinnerScore, result.Delta, result.RegressionPercent = chosen.Suite, chosen.BaselineScore, &score, chosen.Delta, chosen.RegressionPercent
	}
	if len(report.Evals) == 0 {
		result.Reason = "no signed quality evidence was compared"
	}
	return result
}

func objectiveSource(objective string) string {
	switch objective {
	case lab.ObjectiveLatency, lab.ObjectiveInteractive:
		return sourceTTFTP95
	case lab.ObjectiveThroughput, lab.ObjectiveCostEfficiency:
		return sourceOutputTPS
	}
	return ""
}

func noise(objective, kind string, baseline, winner *arm, arms map[string]*arm) Noise {
	result := Noise{Basis: BasisUnknown, ObjectiveMetric: objectiveSource(objective), ObjectiveDecision: DecisionUnknown}
	switch kind {
	case OutcomeWinnerSelected:
		if baseline == nil {
			result.Reason = "the campaign has no baseline"
			return result
		}
		if !interleavedWith(winner, baseline) {
			result.Reason = "the baseline was not measured alongside the winner" + controlReason(winner.measured)
			return result
		}
		fillNoise(&result, winner)
	case OutcomeBaselineKept, OutcomeNoWinner:
		// No challenger replaced the baseline. The closest challenger shows
		// how large a change the measurement could resolve.
		if baseline == nil {
			result.Reason = "the campaign has no baseline"
			return result
		}
		var closest *arm
		best := math.Inf(-1)
		ids := make([]string, 0, len(arms))
		for id := range arms {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			challenger := arms[id]
			if challenger == baseline || !interleavedWith(challenger, baseline) {
				continue
			}
			comparison, ok := challenger.measured.Comparison[result.ObjectiveMetric]
			if !ok {
				continue
			}
			gain := comparison.Delta
			if !comparison.HigherIsBetter {
				gain = -gain
			}
			if gain > best {
				best, closest = gain, challenger
			}
		}
		if closest == nil {
			result.Reason = "no challenger was measured alongside the baseline"
			return result
		}
		fillNoise(&result, closest)
	default:
		result.Reason = "no setup qualified"
		if kind == OutcomeInProgress {
			result.Reason = "the campaign is still running"
		}
	}
	return result
}

func interleavedWith(candidate, baseline *arm) bool {
	return candidate != nil && baseline != nil && candidate.measured != nil && candidate.measured.Control.Role == optimizationcampaign.ControlRoleBracketed && candidate.measured.Control.CandidateID == baseline.run.ID
}

func fillNoise(result *Noise, candidate *arm) {
	measured := candidate.measured
	result.Basis, result.CandidateID = BasisInterleaved, candidate.run.ID
	result.ControlRuns = measured.Control.Runs
	for _, run := range measured.Runs {
		if run.Arm == "candidate" {
			result.CandidateRuns++
		}
	}
	result.ControlDriftPercent, result.DriftLimitPercent = ptr(measured.Control.MaxDrift*100), ptr(measured.Control.DriftLimit*100)
	if comparison, ok := measured.Comparison[result.ObjectiveMetric]; ok {
		result.ObjectiveChangePercent, result.ObjectiveNoiseFloorPercent, result.ObjectiveDecision = ptr(comparison.Delta*100), ptr(comparison.NoiseFloor*100), comparison.Decision
	} else {
		result.Reason = result.ObjectiveMetric + " was not comparable between the interleaved runs"
	}
}

func monthly(proposal optimizer.Proposal, proposalOK bool, kind string, metrics []Metric, e Evidence) Monthly {
	result := Monthly{Basis: BasisUnknown, HoursPerMonth: HoursPerMonth, Decision: DecisionUnknown}
	if !proposalOK || proposal.Input.WorkloadSource != optimizer.WorkloadSourceCustomerObserved {
		result.Reason = "the campaign did not replay your traffic, so there is no traffic volume to project"
		return result
	}
	if e.Replay == nil {
		result.Reason = e.ReplayReason
		if result.Reason == "" {
			result.Reason = "the replayed traffic record was not found"
		}
		return result
	}
	trace := *e.Replay
	result.TraceDigest = "sha256:" + trace.ShapeDigest
	window := trace.WindowEnd.Sub(trace.WindowStart).Seconds()
	if window <= 0 {
		result.Reason = "the replayed traffic has no capture window"
		return result
	}
	captureWindow := window
	result.CaptureWindowSeconds, result.CaptureCutOff = &captureWindow, trace.RequestLimitReached
	volume, err := replay.TraceOutputVolume(trace)
	if err != nil {
		result.Reason = "the replayed traffic could not be read"
		return result
	}
	if volume.GenerationRequests == 0 {
		result.Reason = "the replayed traffic has no generation requests"
		return result
	}
	if volume.Missing > 0 {
		result.Reason = fmt.Sprintf("%d of %d replayed requests have no output-token count", volume.Missing, volume.GenerationRequests)
		return result
	}
	if trace.RequestLimitReached != nil && *trace.RequestLimitReached {
		// The capture kept only its newest requests, so the window overstates
		// the time they arrived in. Dividing by it would project too little.
		if volume.KeptSpanSeconds <= 0 {
			result.Reason = "the capture reached its request limit and its kept requests arrived at one instant, so there is no traffic rate to project"
			return result
		}
		window = volume.KeptSpanSeconds
	}
	requests, tokens := volume.GenerationRequests, volume.OutputTokens
	result.WindowSeconds, result.RequestsInWindow, result.OutputTokensInWindow = &window, &requests, &tokens
	monthlyTokens := float64(tokens) / window * HoursPerMonth * 3600
	result.MonthlyOutputTokens, result.Basis, result.Assumption = &monthlyTokens, BasisReplayVolume, monthlyAssumptionText
	var cost Metric
	for _, metric := range metrics {
		if metric.Key == MetricCostPerMillion {
			cost = metric
		}
	}
	if cost.Baseline != nil {
		result.BaselineUSD = ptr(monthlyTokens / 1_000_000 * *cost.Baseline)
	}
	if cost.Winner != nil {
		result.WinnerUSD = ptr(monthlyTokens / 1_000_000 * *cost.Winner)
	}
	switch {
	case kind == OutcomeBaselineKept && result.BaselineUSD != nil:
		result.Decision = DecisionUnchanged
	case kind == OutcomeWinnerSelected && result.BaselineUSD != nil && result.WinnerUSD != nil && cost.ChangePercent != nil:
		result.ChangeUSD, result.ChangePercent, result.Decision = ptr(*result.WinnerUSD-*result.BaselineUSD), cost.ChangePercent, cost.Decision
	case result.BaselineUSD == nil || result.WinnerUSD == nil:
		result.Reason = "cost per output token is unknown: " + cost.Reason
	default:
		result.Reason = cost.Reason
	}
	return result
}

func workload(baseline, winner *arm) Workload {
	result := Workload{Source: BasisUnknown}
	source := winner
	if source == nil || source.workload == nil {
		source = baseline
	}
	if source == nil || source.workload == nil {
		return result
	}
	var fields struct {
		Source      string `json:"workload_source"`
		Profile     string `json:"profile"`
		Concurrency *int   `json:"concurrency"`
		Requests    *int   `json:"request_count_per_run"`
		Method      string `json:"measurement_method"`
	}
	raw, _ := json.Marshal(source.workload)
	if json.Unmarshal(raw, &fields) != nil {
		return result
	}
	if fields.Source != "" {
		result.Source = fields.Source
	}
	result.Profile, result.Concurrency, result.RequestsPerRun, result.Method = fields.Profile, fields.Concurrency, fields.Requests, fields.Method
	return result
}

func candidateResults(campaign domain.OptimizationCampaign, objective string, baseline, winner *arm, arms map[string]*arm) []CandidateResult {
	runs := append([]domain.OptimizationCandidateRun(nil), campaign.Candidates...)
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Rank < runs[j].Rank })
	metric := objectiveSource(objective)
	out := make([]CandidateResult, 0, len(runs))
	for _, run := range runs {
		current := arms[run.ID]
		result := CandidateResult{ID: run.ID, Label: label(current), Role: "candidate", State: run.State, FailureCode: run.FailureCode}
		if baseline != nil && run.ID == baseline.run.ID {
			result.Role = "baseline"
		}
		if winner != nil && run.ID == winner.run.ID {
			if result.Role == "baseline" {
				result.Role = "baseline_kept"
			} else {
				result.Role = "winner"
			}
		}
		if current.bench == nil {
			result.Reason = current.reason
			out = append(out, result)
			continue
		}
		row := *current.bench
		result.OutputTokensPerSecond, result.RequestsPerSecond, result.TTFTP95MS, result.TPOTP95MS, result.LatencyP95MS = row.OutputTokenThroughput, row.RequestThroughput, row.TTFTP95MS, row.TPOTP95MS, row.LatencyP95MS
		if finitePositive(row.OutputTokenThroughput) && current.hourly != nil {
			result.CostPerMillionOutputTokens = ptr(*current.hourly / (*row.OutputTokenThroughput * 3600) * 1_000_000)
		}
		if outcome, ok := optimizationcampaign.OwnOutcome(row); ok {
			if rate := outcome.ErrorRate(); rate != nil {
				result.ErrorRatePercent = ptr(*rate * 100)
			}
			result.OutputTokensPerRequest = outcome.OutputTokensPerRequest
		}
		if result.Role != "baseline" && result.Role != "baseline_kept" && baseline != nil && baseline.bench != nil && sameWorkload(current, baseline) {
			if check, ok := optimizationcampaign.CheckGuards(row, *baseline.bench, baseline.run.ID); ok {
				for _, guard := range check.Guards {
					result.Guards = append(result.Guards, reportGuard(guard, check.Basis))
				}
			}
		}
		if result.Role != "baseline" && result.Role != "baseline_kept" && baseline != nil && current.measured != nil && current.measured.Control.CandidateID == baseline.run.ID {
			for _, regression := range optimizationcampaign.MeasuredRegressions(current.measured) {
				result.Regressions = append(result.Regressions, Comparison{Metric: regression.Metric, ChangePercent: regression.Delta * 100, NoiseFloorPercent: regression.NoiseFloor * 100, Decision: DecisionRegressed, Basis: regression.Basis})
			}
		}
		switch {
		case result.Role == "baseline" || result.Role == "baseline_kept":
		case interleavedWith(current, baseline):
			if comparison, ok := current.measured.Comparison[metric]; ok {
				result.VersusBaseline = &Comparison{Metric: metric, ChangePercent: comparison.Delta * 100, NoiseFloorPercent: comparison.NoiseFloor * 100, Decision: comparison.Decision}
			} else {
				result.Reason = metric + " was not comparable between the interleaved runs"
			}
		default:
			result.Reason = "not measured alongside the baseline" + controlReason(current.measured)
		}
		out = append(out, result)
	}
	return out
}

// reportGuard states a campaign guard in the report's units.
func reportGuard(guard optimizationcampaign.Guard, basis string) Guard {
	result := Guard{Code: guard.Code, Metric: guard.Metric, Baseline: guard.Control, Value: guard.Candidate, Basis: basis, Reason: guard.Reason}
	if guard.Metric == optimizationcampaign.GuardMetricErrorRate {
		result.Metric, result.Baseline, result.Value = "error_rate_percent", 100*guard.Control, 100*guard.Candidate
	}
	return result
}

// warnings name each setup a guard holds back. A chosen setup with a guard
// was ranked before the guards existed; its gain may come from the failures
// or the shorter answers, so the report says so.
func warnings(candidates []CandidateResult) []string {
	out := []string{}
	for _, candidate := range candidates {
		if len(candidate.Guards) == 0 {
			continue
		}
		reasons := make([]string, 0, len(candidate.Guards))
		for _, guard := range candidate.Guards {
			reasons = append(reasons, guard.Reason)
		}
		if candidate.Role == "winner" {
			out = append(out, "“"+candidate.Label+"”, the chosen setup, "+strings.Join(reasons, " · ")+". Part of its gain may come from that.")
			continue
		}
		out = append(out, "“"+candidate.Label+"” was held back: "+strings.Join(reasons, " · ")+".")
	}
	return out
}

func bottleneck(kind string, baseline, winner *arm) Bottleneck {
	result := Bottleneck{State: BasisUnknown, Source: BasisEngineMetrics, GPUTelemetry: "not_collected", Signals: []EngineSignal{}}
	if winner == nil && baseline == nil {
		result.Reason = "no setup was measured"
		return result
	}
	var baseRuns, winRuns []optimizationcampaign.RunSummary
	switch {
	case kind == OutcomeWinnerSelected && interleavedWith(winner, baseline):
		for _, run := range winner.measured.Runs {
			if run.Arm == "control" {
				baseRuns = append(baseRuns, run)
			} else {
				winRuns = append(winRuns, run)
			}
		}
	default:
		baseRuns = armRuns(baseline)
		if kind == OutcomeWinnerSelected {
			winRuns = armRuns(winner)
		} else if kind == OutcomeBaselineKept {
			winRuns = baseRuns
		}
	}
	for _, current := range []*arm{winner, baseline} {
		if current != nil && current.measured != nil && current.measured.GPUTelemetry != "" {
			result.GPUTelemetry = current.measured.GPUTelemetry
			break
		}
	}
	base, winning := summarizeSignals(baseRuns), summarizeSignals(winRuns)
	keys := map[string]struct{}{}
	for key := range base {
		keys[key] = struct{}{}
	}
	for key := range winning {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		if len(result.Signals) == maxEngineSignals {
			break
		}
		name, stat, _ := strings.Cut(key, "\x00")
		signal := EngineSignal{Name: name, Stat: stat}
		if value, ok := base[key]; ok {
			signal.Baseline, signal.Unit = ptr(value.value), value.unit
		}
		if value, ok := winning[key]; ok {
			signal.Winner, signal.Unit = ptr(value.value), value.unit
		}
		result.Signals = append(result.Signals, signal)
	}
	if len(result.Signals) == 0 {
		result.Reason = noSignalsReason(winner, baseline)
		return result
	}
	result.State = "measured"
	return result
}

// noSignalsReason says why no engine signal exists. The engine is blamed only
// when its metrics were scraped and held none of the signals read; a scrape
// that was never requested, or that collected nothing, says so instead.
func noSignalsReason(arms ...*arm) string {
	requested := false
	for _, current := range arms {
		if current == nil || current.measured == nil {
			continue
		}
		requested = requested || current.measured.ServerMetricsRequested || current.measured.Control.ServerMetrics
		for _, run := range current.measured.Runs {
			if run.ServerMetrics != nil && !run.ServerMetrics.Collected && run.ServerMetrics.Reason != "" {
				return "the serving engine's metrics were not collected: " + run.ServerMetrics.Reason
			}
		}
	}
	if !requested {
		return "the serving engine's metrics were not scraped while it was measured"
	}
	return "the serving engine reported no cache, queue, or preemption metrics while measured"
}

func armRuns(a *arm) []optimizationcampaign.RunSummary {
	if a == nil || a.measured == nil {
		return nil
	}
	var runs []optimizationcampaign.RunSummary
	for _, run := range a.measured.Runs {
		if run.Arm == "candidate" {
			runs = append(runs, run)
		}
	}
	return runs
}

type signalValue struct {
	value float64
	unit  string
}

// summarizeSignals pools one arm's per-run engine metrics: the mean of the
// run averages, the largest run maximum, and the summed counter totals.
func summarizeSignals(runs []optimizationcampaign.RunSummary) map[string]signalValue {
	type pool struct {
		sum   float64
		count int
		max   float64
		unit  string
	}
	pools := map[string]*pool{}
	add := func(key, unit string, value float64, combine func(*pool, float64)) {
		current := pools[key]
		if current == nil {
			current = &pool{max: math.Inf(-1), unit: unit}
			pools[key] = current
		}
		combine(current, value)
	}
	for _, run := range runs {
		if run.ServerMetrics == nil || !run.ServerMetrics.Collected {
			continue
		}
		for name, summary := range run.ServerMetrics.Metrics {
			for stat, value := range summary.Stats {
				if math.IsNaN(value) || math.IsInf(value, 0) {
					continue
				}
				key := name + "\x00" + stat
				switch stat {
				case "avg":
					add(key, summary.Unit, value, func(p *pool, v float64) { p.sum += v; p.count++ })
				case "max":
					add(key, summary.Unit, value, func(p *pool, v float64) { p.max = math.Max(p.max, v); p.count++ })
				case "total":
					add(key, summary.Unit, value, func(p *pool, v float64) { p.sum += v })
				}
			}
		}
	}
	out := map[string]signalValue{}
	for key, current := range pools {
		_, stat, _ := strings.Cut(key, "\x00")
		switch stat {
		case "avg":
			out[key] = signalValue{current.sum / float64(current.count), current.unit}
		case "max":
			out[key] = signalValue{current.max, current.unit}
		case "total":
			out[key] = signalValue{current.sum, current.unit}
		}
	}
	return out
}

func appendix(campaign domain.OptimizationCampaign, proposal optimizer.Proposal, proposalOK bool, baseline, winner *arm, e Evidence) Appendix {
	result := Appendix{Evidence: []EvidenceDigest{}}
	if winner != nil {
		result.Winner = setup(winner)
	}
	if baseline != nil {
		result.Baseline = setup(baseline)
	}
	result.Evidence = append(result.Evidence, EvidenceDigest{Kind: "campaign_input", ID: campaign.ID, Digest: "sha256:" + campaign.InputDigest, Subject: campaign.ModelIdentity})
	if campaign.ProposalJSON != "" {
		result.Evidence = append(result.Evidence, EvidenceDigest{Kind: "proposal", Digest: sha(campaign.ProposalJSON), Subject: proposalSource(proposal, proposalOK)})
	}
	seenLab := map[string]bool{}
	for _, entry := range []struct {
		role string
		arm  *arm
	}{{"winner", winner}, {"baseline", baseline}} {
		current := entry.arm
		if current == nil || (entry.role == "baseline" && winner != nil && winner == baseline) {
			continue
		}
		if current.bench != nil {
			result.Evidence = append(result.Evidence, EvidenceDigest{Kind: entry.role + "_benchmark", ID: current.bench.ID, Digest: sha(current.bench.WorkloadJSON), Subject: current.bench.ModelIdentity + " revision " + current.bench.RevisionID})
		}
		if evidence, ok := e.Quality[current.run.QualityEvidenceID]; ok && current.run.QualityEvidenceID != "" {
			result.Evidence = append(result.Evidence, EvidenceDigest{Kind: entry.role + "_quality", ID: evidence.ID, Digest: prefixed(evidence.PayloadDigest), Subject: evidence.Suite + "@" + evidence.SuiteVersion + " signed by " + evidence.KeyID})
		}
		// Campaign rankings are content-addressed as optlab-<sha256>; any other
		// identifier carries no digest to cite.
		if id := current.run.LabEvaluationID; contentAddressedRanking.MatchString(id) && !seenLab[id] {
			seenLab[id] = true
			result.Evidence = append(result.Evidence, EvidenceDigest{Kind: "ranking", ID: id, Digest: "sha256:" + strings.TrimPrefix(id, "optlab-"), Subject: "content-addressed measured ranking"})
		}
		if id := current.run.ReleaseGuardEvaluationID; id != "" {
			if evaluation, ok := e.ReleaseGuard[id]; ok {
				result.Evidence = append(result.Evidence, EvidenceDigest{Kind: "release_guard", ID: id, Digest: sha(evaluation.MetricsJSON), Subject: "decision " + evaluation.Decision})
			}
		}
	}
	if e.Replay != nil {
		result.Evidence = append(result.Evidence, EvidenceDigest{Kind: "replayed_traffic", ID: e.Replay.ID, Digest: "sha256:" + e.Replay.ShapeDigest, Subject: fmt.Sprintf("%d requests, content not stored", e.Replay.RequestCount)})
	}
	return result
}

func proposalSource(proposal optimizer.Proposal, ok bool) string {
	if !ok {
		return "unreadable"
	}
	return proposal.AlgorithmVersion
}

func setup(a *arm) *Setup {
	result := &Setup{CandidateID: a.run.ID, Label: label(a), Args: []string{}, DeploymentName: a.run.DeploymentName, RevisionID: a.run.RevisionID, HourlyUSD: a.hourly, PriceSource: a.source}
	if a.draftOK {
		draft := a.draft
		result.Model = draft.Model.ID
		if draft.Model.Revision != "" {
			result.Model += "@" + draft.Model.Revision
		}
		result.Engine, result.EngineVersion, result.Image = draft.Runtime.Engine, draft.Runtime.Version, draft.Runtime.Workload.Image
		if draft.Runtime.Args != nil {
			result.Args = append(result.Args, draft.Runtime.Args...)
		}
		if len(draft.Runtime.Env) > 0 {
			result.Env = maps.Clone(draft.Runtime.Env)
		}
		result.Provider, result.Region, result.ComputeMode = draft.Provider.Cloud, draft.Provider.Region, draft.Compute.Mode
		result.MinReplicas, result.MaxReplicas = draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas
	}
	if a.bench != nil {
		result.Model = a.bench.ModelIdentity
	}
	result.Accelerator = acceleratorOf(a)
	result.AcceleratorCount = acceleratorCount(a)
	result.AcceleratorClass = acceleratorClass(result.Accelerator)
	return result
}

// label describes a setup by what it changes, in the words optimize watch uses.
// label is the setup's name from optimizationcampaign.CandidateLabels, the
// same name the campaign API, console, and CLI show.
func label(a *arm) string {
	if a == nil {
		return ""
	}
	return a.label
}

func acceleratorOf(a *arm) string {
	if a.bench != nil && strings.TrimSpace(a.bench.GPU) != "" {
		return strings.TrimSpace(a.bench.GPU)
	}
	if a.draftOK {
		return strings.TrimSpace(a.draft.Resources.GPU)
	}
	return ""
}

func acceleratorCount(a *arm) *int {
	if a.bench != nil && a.bench.GPUCount != nil && *a.bench.GPUCount > 0 {
		count := *a.bench.GPUCount
		return &count
	}
	if a.draftOK {
		count := a.draft.Resources.GPUCount
		if count == 0 {
			// The benchmark executor records a zero count as one device.
			count = 1
		}
		if count > 0 {
			return &count
		}
	}
	return nil
}

// acceleratorClass marks a CPU or a simulated accelerator explicitly. The
// local proof of concept labels its CPU slots SIMULATED-CPU.
func acceleratorClass(accelerator string) string {
	normalized := strings.ToUpper(strings.TrimSpace(accelerator))
	if normalized == "" {
		return AcceleratorUnknown
	}
	if strings.Contains(normalized, "SIMULATED") {
		return AcceleratorSimulated
	}
	for _, token := range strings.FieldsFunc(normalized, func(r rune) bool { return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') }) {
		if token == "CPU" {
			return AcceleratorSimulated
		}
	}
	return AcceleratorGPU
}

// headline states the outcome in one sentence. released, when set, says how
// a chosen setup that was never rolled out was let go.
func headline(report Report, released string) string {
	outcome := report.Outcome
	switch outcome.Kind {
	case OutcomeCancelled:
		return "Cancelled. Nothing changed in production."
	case OutcomeInProgress:
		return "Still measuring. The result appears when the campaign finishes."
	case OutcomeNoWinner:
		return "No setup qualified. Nothing changes in production."
	case OutcomeBaselineKept:
		return "Keep your current setup: nothing beat it by more than the measurement noise."
	}
	improved, regressed := false, false
	for _, metric := range report.Metrics {
		improved = improved || metric.Decision == DecisionImproved
		regressed = regressed || metric.Decision == DecisionRegressed
	}
	subject, previous := "Roll out “"+outcome.WinnerLabel+"”", "your current setup"
	if outcome.Activated {
		subject, previous = "“"+outcome.WinnerLabel+"” is live", "your previous setup"
	}
	var claim string
	switch {
	case improved && !regressed:
		claim = "it measured better than " + previous + " on the same traffic"
	case improved:
		claim = "it measured better than " + previous + " on some outcomes and worse on others"
	default:
		claim = "it qualified, but no gain over " + previous + " is established"
	}
	if released != "" && !outcome.Activated {
		return "“" + outcome.WinnerLabel + "” was the best setup found but was not rolled out: " + claim + "; " + released + "."
	}
	return subject + ": " + claim + "."
}

func unknownFields(report Report) []string {
	unknown := []string{}
	for _, metric := range report.Metrics {
		if metric.Decision == DecisionUnknown {
			unknown = append(unknown, "metrics."+metric.Key)
		}
	}
	if report.Quality.Verdict == releaseguard.QualityUnknown {
		unknown = append(unknown, "quality.verdict")
	}
	if report.Quality.Delta == nil && report.Quality.Basis != BasisBaselineKept {
		unknown = append(unknown, "quality.delta")
	}
	if report.Noise.Basis == BasisUnknown || report.Noise.ObjectiveNoiseFloorPercent == nil {
		unknown = append(unknown, "noise")
	}
	if report.Monthly.Basis == BasisUnknown {
		unknown = append(unknown, "monthly")
	} else if report.Monthly.Decision == DecisionUnknown {
		unknown = append(unknown, "monthly.change")
	}
	if report.Monthly.Basis != BasisUnknown && report.Monthly.CaptureCutOff == nil {
		unknown = append(unknown, "monthly.capture_cut_off")
	}
	if report.Bottleneck.State == BasisUnknown {
		unknown = append(unknown, "bottleneck")
	}
	return unknown
}

// digest fingerprints the report content so two readers can confirm they hold
// the same result. It covers everything except the digest itself.
func digest(report Report) string {
	report.ReportDigest = ""
	encoded, err := json.Marshal(report)
	if err != nil {
		return ""
	}
	return sha(string(encoded))
}

func sha(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func prefixed(digest string) string {
	digest = strings.TrimSpace(digest)
	if digest == "" || strings.HasPrefix(digest, "sha256:") {
		return digest
	}
	return "sha256:" + digest
}

var contentAddressedRanking = regexp.MustCompile(`^optlab-[a-f0-9]{64}$`)

func finitePositive(value *float64) bool {
	return value != nil && *value > 0 && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func ptr(value float64) *float64 { return &value }
