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
}

type Reason struct {
	Code      string               `json:"code"`
	Metric    string               `json:"metric,omitempty"`
	Active    *float64             `json:"active,omitempty"`
	Candidate *float64             `json:"candidate,omitempty"`
	Limit     *float64             `json:"limit,omitempty"`
	Message   string               `json:"message"`
	Bootstrap *BootstrapComparison `json:"bootstrap,omitempty"`
}

type Result struct {
	Decision string   `json:"decision"`
	Reasons  []Reason `json:"reasons"`
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
	qualityComparable := input.Candidate.QualityComparable != nil && *input.Candidate.QualityComparable && input.Active.QualityScore != nil
	if input.Policy.RequireQualityEvidence {
		if input.Candidate.QualityScore == nil || input.Candidate.QualityPassed == nil {
			waiting = append(waiting, Reason{Code: "quality_evidence_unavailable", Metric: "quality_score", Message: "Policy requires signed revision-bound semantic evaluation evidence"})
		} else if !*input.Candidate.QualityPassed {
			rejected = append(rejected, Reason{Code: "quality_evaluation_failed", Metric: "quality_score", Candidate: input.Candidate.QualityScore, Message: "Candidate failed its external semantic evaluation suite"})
		}
		if !qualityComparable {
			waiting = append(waiting, Reason{Code: "quality_evidence_not_comparable", Metric: "quality_score", Message: "Active and candidate require the same versioned evaluation suite and evaluator"})
		}
	}
	if input.Policy.MinimumQualityScore != nil {
		if input.Candidate.QualityScore == nil {
			if !input.Policy.RequireQualityEvidence {
				waiting = append(waiting, Reason{Code: "quality_evidence_unavailable", Metric: "quality_score", Message: "Minimum semantic quality policy requires signed revision-bound evidence"})
			}
		} else if *input.Candidate.QualityScore < *input.Policy.MinimumQualityScore {
			rejected = append(rejected, comparisonReason("quality_score_below_minimum", "quality_score", value(input.Active.QualityScore), value(input.Candidate.QualityScore), *input.Policy.MinimumQualityScore, "Candidate semantic quality score is below policy"))
		}
	}
	mode := input.Policy.QualityComparisonMode
	if mode == "" {
		mode = "threshold"
	}
	if mode == "bootstrap" {
		if !qualityComparable || input.Policy.MaxQualityRegressionPercent == nil || input.Active.QualityPairingDigest == "" || input.Active.QualityPairingDigest != input.Candidate.QualityPairingDigest {
			waiting = append(waiting, Reason{Code: "quality_bootstrap_evidence_unavailable", Metric: "quality_distribution", Message: "Bootstrap quality policy requires comparable paired distributions, a shared pairing digest, and a regression limit"})
		} else {
			comparison, err := PairedBootstrap(input.Active.QualityScores, input.Candidate.QualityScores, input.Policy.QualityBootstrapAlpha, input.Policy.QualityBootstrapMinSamples, input.Policy.QualityBootstrapSeed, *input.Policy.MaxQualityRegressionPercent)
			if err != nil {
				waiting = append(waiting, Reason{Code: "quality_bootstrap_invalid", Metric: "quality_distribution", Message: err.Error(), Bootstrap: &comparison})
			} else {
				reason := Reason{Metric: "quality_distribution", Bootstrap: &comparison}
				switch comparison.Status {
				case "reject":
					reason.Code, reason.Message = "quality_bootstrap_regression", "Paired-bootstrap confidence interval exceeds the persisted quality-regression limit"
					rejected = append(rejected, reason)
				case "accept":
					// The successful comparison remains in the persisted metrics and
					// final within-policy decision; it is not a failure reason.
				case "inconclusive":
					reason.Code, reason.Message = "quality_bootstrap_inconclusive", "Paired-bootstrap confidence interval overlaps the persisted quality-regression limit"
					waiting = append(waiting, reason)
				default:
					reason.Code, reason.Message = "quality_bootstrap_insufficient", fmt.Sprintf("Bootstrap quality policy needs at least %d paired samples; active=%d candidate=%d", input.Policy.QualityBootstrapMinSamples, len(input.Active.QualityScores), len(input.Candidate.QualityScores))
					waiting = append(waiting, reason)
				}
			}
		}
	} else if input.Policy.MaxQualityRegressionPercent != nil {
		if !qualityComparable {
			if !input.Policy.RequireQualityEvidence {
				waiting = append(waiting, Reason{Code: "quality_evidence_not_comparable", Metric: "quality_score", Message: "Quality regression policy requires the same versioned evaluation suite and evaluator"})
			}
		} else if regression, ok := percentDrop(input.Active.QualityScore, input.Candidate.QualityScore); ok && regression > *input.Policy.MaxQualityRegressionPercent {
			rejected = append(rejected, comparisonReason("quality_score_regression", "quality_score", value(input.Active.QualityScore), value(input.Candidate.QualityScore), *input.Policy.MaxQualityRegressionPercent, fmt.Sprintf("Candidate semantic quality regression %.1f%% exceeds policy", regression)))
		} else if input.Candidate.QualityScore == nil {
			waiting = append(waiting, Reason{Code: "quality_score_unavailable", Metric: "quality_score", Message: "Quality regression policy requires comparable scores for both revisions"})
		}
	}
	if len(rejected) > 0 {
		return Result{Decision: "REJECT", Reasons: rejected}
	}
	if len(waiting) > 0 {
		return Result{Decision: "WAIT", Reasons: waiting}
	}
	if input.Active.Requests < input.Policy.MinimumRequests || input.Candidate.Requests < input.Policy.MinimumRequests {
		return Result{Decision: "WAIT", Reasons: []Reason{{Code: "insufficient_requests", Metric: "requests", Message: fmt.Sprintf("Need at least %d requests for both revisions; active=%d candidate=%d", input.Policy.MinimumRequests, input.Active.Requests, input.Candidate.Requests)}}}
	}
	if input.Active.P95TTFTMS == nil || input.Candidate.P95TTFTMS == nil {
		return Result{Decision: "WAIT", Reasons: []Reason{{Code: "ttft_unavailable", Metric: "p95_ttft_ms", Message: "TTFT evidence is unavailable for one or both revisions"}}}
	}
	return Result{Decision: "ACCEPT", Reasons: []Reason{{Code: "within_policy", Message: "Candidate measurements are within persisted Release Guard policy"}}}
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
