package optimizationcampaign

import (
	"context"
	"fmt"
	"math"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/qualitycheck"
)

// A gain in one measurement window can come from the machines rather than
// the change: the candidate's and the control's servers run on separate
// machines, and pairing runs in time cannot remove a fixed speed difference
// between them. At the run-to-run noise of a real GPU (well under 1%), a 1%
// machine difference alone makes a copy of the control look better about one
// time in five (TestJudgeCalibration). So a candidate that shows a gain on
// the objective is measured again on fresh servers before the gain counts:
// its server and the control's are retired and provisioned again, and a
// second window of paired rounds must show the gain too. The control of a
// run that evolves a live endpoint serves production traffic, so its server
// is kept and only the candidate's is replaced.
const (
	// RunKindConfirmation is the interruption that retires a server so the
	// candidate can be measured again on a fresh one.
	RunKindConfirmation = "confirmation"

	ConfirmRetireCandidate = "retire_candidate"
	ConfirmRetireControl   = "retire_control"
	ConfirmMeasure         = "measure"

	// RecoveryMachineRetired resolves a confirmation interruption: the server
	// was retired and its exact revision provisioned again.
	RecoveryMachineRetired = "machine_retired"
	// ReplacementPurposeConfirmation marks a replacement made for a
	// confirmation. It does not count against MaxMachineReplacements, which
	// bounds machines that were lost.
	ReplacementPurposeConfirmation = "confirmation"

	// DecisionNotConfirmed is a candidate that measured better than the
	// control in its first window but not again in a valid window on fresh
	// servers. A gain whose second window is missing, invalid, or without
	// the objective is unknown instead (controlDecisionUnknown).
	DecisionNotConfirmed = "not_confirmed"
	// FailureNotConfirmed is its rejection code.
	FailureNotConfirmed = "measured_gain_not_confirmed"
	// FailureConfirmationNotCovered stops a setup whose fresh servers the
	// approved spend cap does not cover.
	FailureConfirmationNotCovered = "optimization_confirmation_not_covered"
)

// ConfirmationState is the confirmation's progress in the checkpoint. It is
// set when the first window ended with a gain on the objective.
type ConfirmationState struct {
	Phase string `json:"phase"`
	// KeepControl is true when the control's server serves production and is
	// not retired.
	KeepControl bool `json:"keep_control,omitempty"`
	// Attempt is 1 once an invalid window on fresh servers is being
	// measured again (measurementSession.measureAgain).
	Attempt int `json:"attempt,omitempty"`
	// FirstAnswers is the task set as the setup's first servers and the
	// reference answered it before the candidate's server was retired
	// (FirstAnswersAsker). The answer check after the fresh window pools it.
	FirstAnswers *qualitycheck.ServerPair `json:"first_answers,omitempty"`
	// FirstAnswersSkipped says why the first servers' answers were not read;
	// the verdict then rests on the fresh servers alone.
	FirstAnswersSkipped string `json:"first_answers_skipped,omitempty"`
}

// FirstAnswersAsker asks a setup's first servers the campaign's task set
// before a confirmation retires them (BuiltinQualityCheck.AskFirstPair).
type FirstAnswersAsker interface {
	AskFirstPair(context.Context, domain.OptimizationCampaign, domain.OptimizationCandidateRun) (qualitycheck.ServerPair, bool, error)
}

// askFirstAnswers keeps the first servers' answers in the checkpoint before
// the candidate's server is retired for a confirmation, once. The built-in
// check asks them only for a setup that changes answers, on a task set. A
// failure to ask is recorded and does not stop the confirmation: the verdict
// then rests on the fresh servers alone, as before.
func (d CompositeDriver) askFirstAnswers(ctx context.Context, campaign domain.OptimizationCampaign, measured domain.OptimizationCandidateRun, checkpoint *MeasurementCheckpoint, save func() error) error {
	confirmation := checkpoint.Confirmation
	if confirmation == nil || confirmation.FirstAnswers != nil || confirmation.FirstAnswersSkipped != "" {
		return nil
	}
	asker, ok := d.Quality.(FirstAnswersAsker)
	if !ok || QualityWaitsForCustomer(campaign) || campaignRequest(campaign).Migration != nil {
		return nil
	}
	pair, asked, err := asker.AskFirstPair(ctx, campaign, measured)
	switch {
	case ctx.Err() != nil:
		return operations.Retryable(retryQualityCheckFailed, ctx.Err())
	case err != nil:
		reason := "the first servers' answers could not be read: " + err.Error()
		if len(reason) > 512 {
			reason = reason[:512]
		}
		confirmation.FirstAnswersSkipped = reason
	case asked:
		confirmation.FirstAnswers = &pair
	default:
		return nil
	}
	return save()
}

// advanceConfirmation moves the confirmation on once arm's server was
// retired and replaced.
func advanceConfirmation(checkpoint *MeasurementCheckpoint, arm string) {
	confirmation := checkpoint.Confirmation
	if confirmation == nil {
		return
	}
	switch {
	case arm == ArmCandidate && confirmation.Phase == ConfirmRetireCandidate:
		confirmation.Phase = ConfirmRetireControl
		if confirmation.KeepControl {
			confirmation.Phase = ConfirmMeasure
		}
	case arm == ArmControl && confirmation.Phase == ConfirmRetireControl:
		confirmation.Phase = ConfirmMeasure
	}
}

// ConfirmationEvidence is the second window, stored under the measured
// evidence. FreshCandidate and FreshControl say whether each arm ran on a
// newly provisioned server; a serverless provider places workers itself.
type ConfirmationEvidence struct {
	Control        ControlEvidence             `json:"control"`
	Runs           []RunSummary                `json:"runs"`
	Comparison     map[string]MetricComparison `json:"comparison,omitempty"`
	FreshCandidate bool                        `json:"fresh_candidate"`
	FreshControl   bool                        `json:"fresh_control"`
	// Discarded is an earlier try at this window that came out invalid; the
	// window was then measured once more on the same servers.
	Discarded *DiscardedWindow `json:"discarded,omitempty"`
}

// DiscardedWindow is a try at the window on fresh servers whose control
// check failed. It is kept for audit and decides nothing.
type DiscardedWindow struct {
	Control ControlEvidence `json:"control"`
	Runs    []RunSummary    `json:"runs"`
}

// scaleByCost turns a throughput comparison into one of throughput per hourly
// cost, given control cost / candidate cost. The interval's width is kept.
func scaleByCost(comparison MetricComparison, ratio float64) MetricComparison {
	comparison.Delta = (1+comparison.Delta)*ratio - 1
	comparison.Decision = DecisionNoMeasurableDifference
	if math.Abs(comparison.Delta) > comparison.NoiseFloor {
		comparison.Decision = DecisionRegressed
		if comparison.Delta > 0 {
			comparison.Decision = DecisionImproved
		}
	}
	return comparison
}

// objectiveComparison reads the objective metric from a window's
// comparisons, scaled by cost when the objective is cost efficiency. ratio is
// control cost / candidate cost and is ignored otherwise.
func objectiveComparison(comparisons map[string]MetricComparison, metric string, costScale bool, ratio float64) (MetricComparison, bool) {
	comparison, found := comparisons[metric]
	if !found {
		return MetricComparison{}, false
	}
	if costScale {
		comparison = scaleByCost(comparison, ratio)
	}
	return comparison, true
}

// confirmedDecision applies the confirmation to a first window that improved
// on the objective. It returns the decision, the comparison to report (the
// second window's, which is free of the first window's selection), and the
// reason. A measurement made before confirmations existed is returned
// unchanged.
//
// Only a valid second window that compared the objective can show that a
// gain did not hold. Without one, whether the gain holds is unknown: the
// candidate is not selected, and it is not reported as a gain that failed
// to repeat, because nothing measured it failing.
func confirmedDecision(measured *MeasuredEvidence, first MetricComparison, metric, label string, costScale bool, ratio float64) (string, MetricComparison, string) {
	reason := fmt.Sprintf("%s changed %+.1f%% against a %.1f%% noise floor in interleaved runs", label, first.Delta*100, first.NoiseFloor*100)
	if first.Decision != DecisionImproved || measured == nil || measured.Method != MeasurementMethod {
		return first.Decision, first, reason
	}
	const unknown = ", so whether the gain holds is unknown"
	confirmation := measured.Confirmation
	if confirmation == nil {
		return controlDecisionUnknown, first, reason + "; it was not measured again on fresh servers" + unknown
	}
	if confirmation.Control.Role != ControlRoleBracketed {
		twice := ""
		if confirmation.Discarded != nil {
			twice = ", and again when measured once more"
		}
		return controlDecisionUnknown, first, reason + "; the measurement on fresh servers was invalid (" + confirmation.Control.Reason + ")" + twice + unknown
	}
	second, found := objectiveComparison(confirmation.Comparison, metric, costScale, ratio)
	if !found {
		return controlDecisionUnknown, first, reason + "; " + metric + " was not comparable on fresh servers" + unknown
	}
	reason = fmt.Sprintf("%s changed %+.1f%% (noise ±%.1f%%), then %+.1f%% (noise ±%.1f%%) measured again on fresh servers", label, first.Delta*100, first.NoiseFloor*100, second.Delta*100, second.NoiseFloor*100)
	if confirmation.Discarded != nil {
		reason += " (the first try there was invalid, " + confirmation.Discarded.Control.Reason + ", so the window was measured once more)"
	}
	if second.Decision != DecisionImproved {
		return DecisionNotConfirmed, second, reason
	}
	return DecisionImproved, second, reason
}
