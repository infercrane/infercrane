package releaseguard

import (
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
)

// Per-eval quality verdicts. PASS and WATCH leave the candidate promotable,
// and WATCH also records a follow-up. BLOCK rejects the candidate. UNKNOWN
// means the evidence cannot be compared under the persisted policy, so Release
// Guard waits instead of guessing.
const (
	QualityPass    = "PASS"
	QualityWatch   = "WATCH"
	QualityBlock   = "BLOCK"
	QualityUnknown = "UNKNOWN"
)

// QualityEval is the verdict for one evaluation suite of the candidate against
// the reference revision.
type QualityEval struct {
	Suite               string   `json:"suite"`
	SuiteVersion        string   `json:"suite_version"`
	Evaluator           string   `json:"evaluator"`
	EvaluatorVersion    string   `json:"evaluator_version"`
	Verdict             string   `json:"verdict"`
	Code                string   `json:"code"`
	Message             string   `json:"message"`
	CandidateEvidenceID string   `json:"candidate_evidence_id"`
	CandidateScore      float64  `json:"candidate_score"`
	CandidatePassed     bool     `json:"candidate_passed"`
	ReferenceEvidenceID string   `json:"reference_evidence_id,omitempty"`
	ReferenceScore      *float64 `json:"reference_score,omitempty"`
	// Delta is the candidate score minus the reference score. Negative is a drop.
	Delta *float64 `json:"delta,omitempty"`
	// RegressionPercent is the drop relative to the reference score.
	RegressionPercent *float64 `json:"regression_percent,omitempty"`
	// Limit is the persisted threshold that decided a WATCH or BLOCK verdict.
	Limit     *float64             `json:"limit,omitempty"`
	Bootstrap *BootstrapComparison `json:"bootstrap,omitempty"`
	FollowUp  string               `json:"follow_up,omitempty"`
}

// QualitySubject identifies one side of the comparison. CompareQuality leaves
// it empty; the caller that resolved the revisions fills it in.
type QualitySubject struct {
	RevisionID string `json:"revision_id"`
	Model      string `json:"model"`
}

// QualityReport is the per-model view of semantic quality. Verdict is the most
// severe eval verdict, in the order BLOCK, UNKNOWN, WATCH, PASS. A report with
// no evals is UNKNOWN because missing evidence never passes.
type QualityReport struct {
	Verdict                string         `json:"verdict"`
	ComparisonMode         string         `json:"comparison_mode"`
	MinimumScore           *float64       `json:"minimum_score,omitempty"`
	WatchRegressionPercent *float64       `json:"watch_regression_percent,omitempty"`
	MaxRegressionPercent   *float64       `json:"max_regression_percent,omitempty"`
	Reference              QualitySubject `json:"reference"`
	Candidate              QualitySubject `json:"candidate"`
	Evals                  []QualityEval  `json:"evals"`
}

// QualityGated reports whether the persisted policy gates rollouts on semantic
// quality evidence.
func QualityGated(policy domain.ReleaseGuardPolicy) bool {
	return policy.Enabled && (policy.RequireQualityEvidence || policy.MinimumQualityScore != nil || policy.MaxQualityRegressionPercent != nil)
}

// CompareQuality classifies every candidate eval under the persisted policy.
// It is deterministic and does no I/O, so the API view and Release Guard
// decisions always agree.
func CompareQuality(policy domain.ReleaseGuardPolicy, evals []domain.QualityEvalEvidence) QualityReport {
	mode := qualityMode(policy)
	report := QualityReport{Verdict: QualityUnknown, ComparisonMode: mode, MinimumScore: policy.MinimumQualityScore, WatchRegressionPercent: policy.QualityWatchRegressionPercent, MaxRegressionPercent: policy.MaxQualityRegressionPercent, Evals: make([]QualityEval, 0, len(evals))}
	if len(evals) == 0 {
		return report
	}
	report.Verdict = QualityPass
	for _, eval := range evals {
		classified := classifyQualityEval(policy, mode, eval)
		report.Evals = append(report.Evals, classified)
		if qualitySeverity(classified.Verdict) > qualitySeverity(report.Verdict) {
			report.Verdict = classified.Verdict
		}
	}
	return report
}

func qualityMode(policy domain.ReleaseGuardPolicy) string {
	if policy.QualityComparisonMode == "" {
		return "threshold"
	}
	return policy.QualityComparisonMode
}

func qualitySeverity(verdict string) int {
	switch verdict {
	case QualityBlock:
		return 3
	case QualityUnknown:
		return 2
	case QualityWatch:
		return 1
	}
	return 0
}

func classifyQualityEval(policy domain.ReleaseGuardPolicy, mode string, eval domain.QualityEvalEvidence) QualityEval {
	out := QualityEval{Suite: eval.Suite, SuiteVersion: eval.SuiteVersion, Evaluator: eval.Evaluator, EvaluatorVersion: eval.EvaluatorVersion, CandidateEvidenceID: eval.Candidate.EvidenceID, CandidateScore: eval.Candidate.Score, CandidatePassed: eval.Candidate.Passed}
	if eval.Reference != nil {
		reference, delta := eval.Reference.Score, eval.Candidate.Score-eval.Reference.Score
		out.ReferenceEvidenceID, out.ReferenceScore, out.Delta = eval.Reference.EvidenceID, &reference, &delta
		if drop, ok := percentDrop(&reference, &out.CandidateScore); ok {
			out.RegressionPercent = &drop
		}
	}
	needsReference := policy.RequireQualityEvidence || policy.MaxQualityRegressionPercent != nil || mode == "bootstrap"
	switch {
	case !eval.Candidate.Passed:
		return qualityVerdict(out, QualityBlock, "quality_evaluation_failed", nil, "Candidate failed its external semantic evaluation suite")
	case policy.MinimumQualityScore != nil && eval.Candidate.Score < *policy.MinimumQualityScore:
		return qualityVerdict(out, QualityBlock, "quality_score_below_minimum", policy.MinimumQualityScore, fmt.Sprintf("Candidate score %.4f is below the policy minimum %.4f", eval.Candidate.Score, *policy.MinimumQualityScore))
	case eval.Reference == nil && needsReference:
		return qualityVerdict(out, QualityUnknown, "quality_evidence_not_comparable", nil, "No reference evidence from the same suite and evaluator versions")
	case mode == "bootstrap":
		return classifyBootstrapEval(policy, eval, out)
	case out.RegressionPercent != nil && policy.MaxQualityRegressionPercent != nil && *out.RegressionPercent > *policy.MaxQualityRegressionPercent:
		return qualityVerdict(out, QualityBlock, "quality_score_regression", policy.MaxQualityRegressionPercent, fmt.Sprintf("Candidate semantic quality regression %.1f%% exceeds policy %.1f%%", *out.RegressionPercent, *policy.MaxQualityRegressionPercent))
	case out.RegressionPercent != nil && policy.QualityWatchRegressionPercent != nil && *out.RegressionPercent > *policy.QualityWatchRegressionPercent:
		return watchVerdict(policy, out, *out.RegressionPercent)
	}
	return qualityVerdict(out, QualityPass, "quality_within_policy", nil, "Candidate is within the persisted quality policy")
}

func classifyBootstrapEval(policy domain.ReleaseGuardPolicy, eval domain.QualityEvalEvidence, out QualityEval) QualityEval {
	if eval.Reference == nil || policy.MaxQualityRegressionPercent == nil || eval.Reference.PairingDigest == "" || eval.Reference.PairingDigest != eval.Candidate.PairingDigest {
		return qualityVerdict(out, QualityUnknown, "quality_bootstrap_evidence_unavailable", nil, "Bootstrap quality policy requires comparable paired distributions, a shared pairing digest, and a regression limit")
	}
	comparison, err := PairedBootstrap(eval.Reference.Scores, eval.Candidate.Scores, policy.QualityBootstrapAlpha, policy.QualityBootstrapMinSamples, policy.QualityBootstrapSeed, *policy.MaxQualityRegressionPercent)
	out.Bootstrap = &comparison
	if err != nil {
		return qualityVerdict(out, QualityUnknown, "quality_bootstrap_invalid", nil, err.Error())
	}
	switch comparison.Status {
	case "reject":
		return qualityVerdict(out, QualityBlock, "quality_bootstrap_regression", policy.MaxQualityRegressionPercent, "Paired-bootstrap confidence interval exceeds the persisted quality-regression limit")
	case "inconclusive":
		return qualityVerdict(out, QualityUnknown, "quality_bootstrap_inconclusive", nil, "Paired-bootstrap confidence interval overlaps the persisted quality-regression limit")
	case "accept":
		// The interval clears the limit. The observed paired regression still
		// decides WATCH so both comparison modes share one watch band.
		if observed := comparison.ObservedRegressionPercent; observed != nil && policy.QualityWatchRegressionPercent != nil && *observed > *policy.QualityWatchRegressionPercent {
			return watchVerdict(policy, out, *observed)
		}
		return qualityVerdict(out, QualityPass, "quality_within_policy", nil, "Paired-bootstrap confidence interval is within the persisted quality-regression limit")
	}
	return qualityVerdict(out, QualityUnknown, "quality_bootstrap_insufficient", nil, fmt.Sprintf("Bootstrap quality policy needs at least %d paired samples; reference=%d candidate=%d", policy.QualityBootstrapMinSamples, len(eval.Reference.Scores), len(eval.Candidate.Scores)))
}

func watchVerdict(policy domain.ReleaseGuardPolicy, out QualityEval, regression float64) QualityEval {
	out.FollowUp = fmt.Sprintf("Re-run %s@%s with %s@%s on this revision after promotion and review the drop before promoting a candidate built on it", out.Suite, out.SuiteVersion, out.Evaluator, out.EvaluatorVersion)
	band := fmt.Sprintf("above the %.1f%% watch threshold", *policy.QualityWatchRegressionPercent)
	if policy.MaxQualityRegressionPercent != nil {
		band = fmt.Sprintf("inside the %.1f%% to %.1f%% watch band", *policy.QualityWatchRegressionPercent, *policy.MaxQualityRegressionPercent)
	}
	return qualityVerdict(out, QualityWatch, "quality_watch", policy.QualityWatchRegressionPercent, fmt.Sprintf("Candidate semantic quality dropped %.1f%%, %s", regression, band))
}

func qualityVerdict(out QualityEval, verdict, code string, limit *float64, message string) QualityEval {
	out.Verdict, out.Code, out.Message = verdict, code, message
	if limit != nil {
		value := *limit
		out.Limit = &value
	}
	return out
}

// qualityReason turns a WATCH, BLOCK, or UNKNOWN eval into a persisted Release
// Guard reason that names the eval it came from.
func qualityReason(eval QualityEval) Reason {
	metric := "quality_score"
	if eval.Bootstrap != nil || eval.Code == "quality_bootstrap_evidence_unavailable" {
		metric = "quality_distribution"
	}
	candidate := eval.CandidateScore
	reason := Reason{Code: eval.Code, Metric: metric, Eval: eval.Suite + "@" + eval.SuiteVersion, Active: eval.ReferenceScore, Candidate: &candidate, Limit: eval.Limit, Message: eval.Suite + "@" + eval.SuiteVersion + ": " + eval.Message, Bootstrap: eval.Bootstrap, FollowUp: eval.FollowUp}
	if eval.FollowUp != "" {
		reason.Message += ". Follow-up: " + eval.FollowUp
	}
	return reason
}
