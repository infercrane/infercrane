// Package releaseguard evaluates rollout evidence without probabilistic logic.
package releaseguard

import (
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
)

type Input struct {
	Policy    domain.ReleaseGuardPolicy `json:"policy"`
	Active    domain.RevisionMetrics    `json:"active"`
	Candidate domain.RevisionMetrics    `json:"candidate"`
	// QualityEvals holds one entry per candidate eval. Release Guard gates
	// semantic quality on these, never on the summary fields in the metrics.
	QualityEvals []domain.QualityEvalEvidence `json:"-"`
}

type Reason struct {
	Code      string               `json:"code"`
	Metric    string               `json:"metric,omitempty"`
	Active    *float64             `json:"active,omitempty"`
	Candidate *float64             `json:"candidate,omitempty"`
	Limit     *float64             `json:"limit,omitempty"`
	Message   string               `json:"message"`
	Bootstrap *BootstrapComparison `json:"bootstrap,omitempty"`
	// Eval names the suite a quality reason came from.
	Eval string `json:"eval,omitempty"`
	// FollowUp is the operator action a WATCH verdict records.
	FollowUp string `json:"follow_up,omitempty"`
}

type Result struct {
	Decision string   `json:"decision"`
	Reasons  []Reason `json:"reasons"`
	// Quality is the per-eval and per-model verdict report. It is nil when the
	// policy does not gate on semantic quality.
	Quality *QualityReport `json:"quality,omitempty"`
}

func Evaluate(input Input) Result {
	if !input.Policy.Enabled {
		return Result{Decision: "ACCEPT", Reasons: []Reason{{Code: "guard_disabled", Message: "Release Guard is disabled by persisted policy"}}}
	}
	var rejected, waiting []Reason
	if input.Candidate.ReadyReplicas < 1 {
		rejected = append(rejected, Reason{Code: "candidate_not_ready", Metric: "ready_replicas", Message: "Candidate has no ready replica"})
	}
	if input.Policy.RequireCompatibilityEvidence && input.Candidate.Compatible == nil {
		waiting = append(waiting, Reason{Code: "compatibility_unproven", Metric: "compatibility", Message: "Candidate compatibility is not proven by comparable persisted evidence"})
	}
	if input.Policy.RequireCompatibilityEvidence && input.Candidate.Compatible != nil && !*input.Candidate.Compatible {
		rejected = append(rejected, Reason{Code: "compatibility_mismatch", Metric: "compatibility", Message: "Candidate benchmark identity is incompatible with the active revision"})
	}
	if input.Policy.RequireSyntheticEvidence && (!input.Active.SyntheticValidation || !input.Candidate.SyntheticValidation) {
		waiting = append(waiting, Reason{Code: "synthetic_validation_unproven", Metric: "synthetic_validation", Message: "Policy requires bounded direct-revision AIPerf evidence for both revisions"})
	}
	if increase := input.Candidate.ErrorRate - input.Active.ErrorRate; increase > input.Policy.MaxErrorRateIncrease {
		rejected = append(rejected, comparisonReason("error_rate_regression", "error_rate", input.Active.ErrorRate, input.Candidate.ErrorRate, input.Policy.MaxErrorRateIncrease, "Candidate error-rate increase exceeds policy"))
	}
	if regression, ok := percentIncrease(input.Active.P95TTFTMS, input.Candidate.P95TTFTMS); ok && regression > input.Policy.MaxTTFTRegressionPercent {
		rejected = append(rejected, comparisonReason("ttft_regression", "p95_ttft_ms", value(input.Active.P95TTFTMS), value(input.Candidate.P95TTFTMS), input.Policy.MaxTTFTRegressionPercent, fmt.Sprintf("Candidate TTFT regression %.1f%% exceeds policy", regression)))
	}
	if regression, ok := percentIncrease(input.Active.P95LatencyMS, input.Candidate.P95LatencyMS); ok && regression > input.Policy.MaxLatencyRegressionPercent {
		rejected = append(rejected, comparisonReason("latency_regression", "p95_latency_ms", value(input.Active.P95LatencyMS), value(input.Candidate.P95LatencyMS), input.Policy.MaxLatencyRegressionPercent, fmt.Sprintf("Candidate latency regression %.1f%% exceeds policy", regression)))
	}
	if drop, ok := percentDrop(input.Active.OutputTokensPerSecond, input.Candidate.OutputTokensPerSecond); ok && drop > input.Policy.MaxOutputThroughputDropPercent {
		rejected = append(rejected, comparisonReason("output_throughput_regression", "output_tokens_per_second", value(input.Active.OutputTokensPerSecond), value(input.Candidate.OutputTokensPerSecond), input.Policy.MaxOutputThroughputDropPercent, fmt.Sprintf("Candidate output throughput drop %.1f%% exceeds policy", drop)))
	}
	if input.Policy.MaxCostRegressionPercent != nil {
		if regression, ok := percentIncrease(input.Active.SourcedHourlyCost, input.Candidate.SourcedHourlyCost); ok && regression > *input.Policy.MaxCostRegressionPercent {
			rejected = append(rejected, comparisonReason("cost_regression", "sourced_hourly_cost", value(input.Active.SourcedHourlyCost), value(input.Candidate.SourcedHourlyCost), *input.Policy.MaxCostRegressionPercent, fmt.Sprintf("Candidate sourced cost regression %.1f%% exceeds policy", regression)))
		} else if input.Active.SourcedHourlyCost == nil || input.Candidate.SourcedHourlyCost == nil {
			waiting = append(waiting, Reason{Code: "cost_evidence_unavailable", Metric: "sourced_hourly_cost", Message: "Cost policy requires sourced cost evidence for both revisions"})
		}
	}
	var watched []Reason
	var quality *QualityReport
	if QualityGated(input.Policy) {
		report := CompareQuality(input.Policy, input.QualityEvals)
		quality = &report
		if len(input.QualityEvals) == 0 {
			waiting = append(waiting, Reason{Code: "quality_evidence_unavailable", Metric: "quality_score", Message: "Quality policy requires signed revision-bound semantic evaluation evidence"})
		}
		for _, eval := range report.Evals {
			switch eval.Verdict {
			case QualityBlock:
				rejected = append(rejected, qualityReason(eval))
			case QualityUnknown:
				waiting = append(waiting, qualityReason(eval))
			case QualityWatch:
				watched = append(watched, qualityReason(eval))
			}
		}
	}
	if len(rejected) > 0 {
		return Result{Decision: "REJECT", Reasons: rejected, Quality: quality}
	}
	if len(waiting) > 0 {
		return Result{Decision: "WAIT", Reasons: waiting, Quality: quality}
	}
	if input.Active.Requests < input.Policy.MinimumRequests || input.Candidate.Requests < input.Policy.MinimumRequests {
		return Result{Decision: "WAIT", Reasons: []Reason{{Code: "insufficient_requests", Metric: "requests", Message: fmt.Sprintf("Need at least %d requests for both revisions; active=%d candidate=%d", input.Policy.MinimumRequests, input.Active.Requests, input.Candidate.Requests)}}, Quality: quality}
	}
	if input.Active.P95TTFTMS == nil || input.Candidate.P95TTFTMS == nil {
		return Result{Decision: "WAIT", Reasons: []Reason{{Code: "ttft_unavailable", Metric: "p95_ttft_ms", Message: "TTFT evidence is unavailable for one or both revisions"}}, Quality: quality}
	}
	// WATCH evals keep the decision ACCEPT. Their reasons come first so the
	// follow-up is the first thing an operator reads.
	reasons := append(watched, Reason{Code: "within_policy", Message: "Candidate measurements are within persisted Release Guard policy"})
	return Result{Decision: "ACCEPT", Reasons: reasons, Quality: quality}
}

func comparisonReason(code, metric string, active, candidate, limit float64, message string) Reason {
	return Reason{Code: code, Metric: metric, Active: &active, Candidate: &candidate, Limit: &limit, Message: message}
}

func percentIncrease(active, candidate *float64) (float64, bool) {
	if active == nil || candidate == nil || *active <= 0 {
		return 0, false
	}
	return (*candidate - *active) / *active * 100, true
}

func percentDrop(active, candidate *float64) (float64, bool) {
	if active == nil || candidate == nil || *active <= 0 {
		return 0, false
	}
	return (*active - *candidate) / *active * 100, true
}

func value(input *float64) float64 {
	if input == nil {
		return 0
	}
	return *input
}
