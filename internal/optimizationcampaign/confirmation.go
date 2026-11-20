package optimizationcampaign

import (
	"fmt"
	"math"
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
	// control in its first window but not again on fresh servers.
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
func confirmedDecision(measured *MeasuredEvidence, first MetricComparison, metric, label string, costScale bool, ratio float64) (string, MetricComparison, string) {
	reason := fmt.Sprintf("%s changed %+.1f%% against a %.1f%% noise floor in interleaved runs", label, first.Delta*100, first.NoiseFloor*100)
	if first.Decision != DecisionImproved || measured == nil || measured.Method != MeasurementMethod {
		return first.Decision, first, reason
	}
	confirmation := measured.Confirmation
	if confirmation == nil {
		return DecisionNotConfirmed, first, reason + "; it was not measured again on fresh servers"
	}
	if confirmation.Control.Role != ControlRoleBracketed {
		return DecisionNotConfirmed, first, reason + "; the measurement on fresh servers was invalid (" + confirmation.Control.Reason + ")"
	}
	second, found := objectiveComparison(confirmation.Comparison, metric, costScale, ratio)
	if !found {
		return DecisionNotConfirmed, first, reason + "; " + metric + " was not comparable on fresh servers"
	}
	reason = fmt.Sprintf("%s changed %+.1f%% (noise ±%.1f%%), then %+.1f%% (noise ±%.1f%%) measured again on fresh servers", label, first.Delta*100, first.NoiseFloor*100, second.Delta*100, second.NoiseFloor*100)
	if second.Decision != DecisionImproved {
		return DecisionNotConfirmed, second, reason
	}
	return DecisionImproved, second, reason
}
