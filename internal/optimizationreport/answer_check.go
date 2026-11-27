package optimizationreport

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

// answerCheck is one setup's own answer check, read from the evidence row it
// recorded with the same rules the run applied when it gated the setup. It is
// nil while the setup has not reached its check.
func answerCheck(run domain.OptimizationCandidateRun, quality map[string]domain.QualityEvidence) *AnswerCheck {
	check := &AnswerCheck{Verdict: releaseguard.QualityUnknown}
	if run.QualityEvidenceID == "" {
		switch {
		case run.FailureCode == optimizationcampaign.FailureQualityRejected:
			check.Verdict, check.Reason = releaseguard.QualityBlock, "the answer check blocked this setup"
		case run.FailureCode == optimizationcampaign.FailureQualityReferenceUnavailable:
			check.Reason = "no serving reference to compare answers with, so this setup's answers were not checked"
		case run.FailureCode == optimizationcampaign.FailureQualityReferenceUnanswered:
			check.Reason = "the reference gave no answer text to some prompts, so this setup's answers were not checked"
		case answerCheckAhead(run.State):
			return nil
		case run.State == optimizationcampaign.CandidateValidating:
			check.Reason = "the answer check is still running"
		default:
			check.Reason = "the answer check did not run for this setup"
		}
		return check
	}
	check.EvidenceID = run.QualityEvidenceID
	row, found := quality[run.QualityEvidenceID]
	if !found {
		check.Reason = "its answer check record could not be found"
		if run.FailureCode == optimizationcampaign.FailureQualityRejected {
			check.Verdict, check.Reason = releaseguard.QualityBlock, "the answer check blocked this setup"
		}
		return check
	}
	score := row.Score
	check.Score, check.Suite, check.Prompts = &score, row.Suite, row.SampleCount
	check.Source = row.Source
	if check.Source == "" {
		check.Source = domain.QualityEvidenceSourceCustomer
	}
	if check.Source == domain.QualityEvidenceSourceCustomer {
		check.Verdict, check.Reason = releaseguard.QualityPass, fmt.Sprintf("the signed eval %s passed with score %.2f", row.Suite, row.Score)
		if !row.Passed {
			check.Verdict, check.Reason = releaseguard.QualityBlock, fmt.Sprintf("the signed eval %s failed with score %.2f", row.Suite, row.Score)
		}
		return check
	}
	watch, block := qualitycheck.WatchRegressionPercent, qualitycheck.BlockRegressionPercent
	check.WatchThresholdPercent, check.BlockThresholdPercent = &watch, &block
	if row.ReferenceRevisionID != "" && row.ReferenceRevisionID == run.RevisionID {
		check.Verdict, check.Reference = releaseguard.QualityPass, true
		check.Reason = fmt.Sprintf("its answers to the %d prompts are the reference the other setups are compared with", row.SampleCount)
		return check
	}
	eval := qualitycheck.VerdictForScore(row.Score)
	if eval.RegressionPercent != nil {
		drop := math.Round(*eval.RegressionPercent*10) / 10
		check.RegressionPercent = &drop
	}
	var distribution qualityevidence.Distribution
	if json.Unmarshal([]byte(row.DistributionJSON), &distribution) == nil && distribution.ToolCalls != nil {
		check.ToolCallCases, check.ToolCallsBroken = distribution.ToolCalls.Cases, distribution.ToolCalls.Broken
	}
	matched := fmt.Sprintf("answers matched the reference %.0f%% across %d prompts", row.Score*100, row.SampleCount)
	switch {
	case check.ToolCallsBroken > 0:
		check.Verdict = releaseguard.QualityBlock
		check.Reason = fmt.Sprintf("%s; it broke %d of %d tool calls the reference made correctly", matched, check.ToolCallsBroken, check.ToolCallCases)
	case !row.Passed || eval.Verdict == releaseguard.QualityBlock:
		check.Verdict = releaseguard.QualityBlock
		check.Reason = fmt.Sprintf("%s; more than %.0f%% different blocks a setup", matched, block)
	case eval.Verdict == releaseguard.QualityWatch:
		check.Verdict = releaseguard.QualityWatch
		check.Reason = fmt.Sprintf("%s, inside the %.0f%% to %.0f%% watch band", matched, watch, block)
	default:
		check.Verdict, check.Reason = releaseguard.QualityPass, matched
	}
	return check
}

// answerCheckAhead is a setup that has not been measured yet, so its answer
// check has not started.
func answerCheckAhead(state string) bool {
	switch state {
	case optimizationcampaign.CandidateProposed, optimizationcampaign.CandidateProvisioning, optimizationcampaign.CandidateReady, optimizationcampaign.CandidateMeasuring:
		return true
	}
	return false
}

// confirmation is the objective comparison of the second window, measured on
// fresh servers, or nil when the setup has none or it was not valid.
func confirmation(measured *optimizationcampaign.MeasuredEvidence, metric string) *Comparison {
	if measured == nil || measured.Confirmation == nil || measured.Confirmation.Control.Role != optimizationcampaign.ControlRoleBracketed {
		return nil
	}
	comparison, found := measured.Confirmation.Comparison[metric]
	if !found {
		return nil
	}
	return &Comparison{Metric: metric, ChangePercent: comparison.Delta * 100, NoiseFloorPercent: comparison.NoiseFloor * 100, Decision: comparison.Decision}
}
