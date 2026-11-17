package optimizationcampaign

import (
	"fmt"
	"math"
	"strings"
)

// FailureRegressed marks a candidate that measured worse than the campaign
// control on at least one headline outcome by more than the noise floor. It is
// never reported as no difference.
const FailureRegressed = "measured_candidate_regressed"

// FailureComparisonUnavailable marks a candidate whose objective result could
// not be compared reliably with the control, for example because the control
// drifted in its measurement window. Incomparable evidence stays unknown; it
// is never reported as no difference.
const FailureComparisonUnavailable = "measured_comparison_unavailable"

const (
	// RegressionBasisInterleaved is a regression in a valid interleaved
	// comparison, as stored with the measurement.
	RegressionBasisInterleaved = "interleaved_control"
	// RegressionBasisDriftedControl is a regression read from the runs of a
	// window the control's drift invalidated. Its noise floor covers the whole
	// control spread and the drift, so it can show a setup is worse; it never
	// shows one is better.
	RegressionBasisDriftedControl = "drifted_control_runs"
)

// Regression is one headline outcome on which a candidate measured worse than
// the control. Delta and NoiseFloor are relative to the control's mean.
type Regression struct {
	Metric     string  `json:"metric"`
	Delta      float64 `json:"delta"`
	NoiseFloor float64 `json:"noise_floor"`
	Basis      string  `json:"basis"`
}

type headlineMetric struct {
	name         string
	higherBetter bool
	value        func(RunSummary) *float64
}

// headlineMetrics are the outcomes a rejection names, in the order shown.
var headlineMetrics = []headlineMetric{
	{"output_token_throughput", true, func(r RunSummary) *float64 { return r.OutputTokenThroughput }},
	{"ttft_p95_ms", false, func(r RunSummary) *float64 { return r.TTFTP95MS }},
	{"latency_p95_ms", false, func(r RunSummary) *float64 { return r.LatencyP95MS }},
}

// MeasuredRegressions lists the headline outcomes on which a candidate was
// measurably worse than the control in its own measurement window, in display
// order. A valid interleaved comparison is used as stored. A window the
// control invalidated only by drifting still holds complete, clean runs of
// both arms; they are compared with a noise floor no smaller than the drift.
// Any other invalid or missing window yields nothing.
func MeasuredRegressions(evidence *MeasuredEvidence) []Regression {
	if evidence == nil {
		return nil
	}
	var out []Regression
	switch {
	case evidence.Control.Role == ControlRoleBracketed:
		for _, metric := range headlineMetrics {
			if comparison, ok := evidence.Comparison[metric.name]; ok && comparison.Decision == DecisionRegressed {
				out = append(out, Regression{Metric: metric.name, Delta: comparison.Delta, NoiseFloor: comparison.NoiseFloor, Basis: RegressionBasisInterleaved})
			}
		}
	case driftedControl(evidence.Control):
		for _, metric := range headlineMetrics {
			comparison, ok := compareMetric(runSamples(evidence.Runs, "control", metric.value), runSamples(evidence.Runs, "candidate", metric.value), metric.higherBetter)
			if !ok {
				continue
			}
			floor := math.Max(comparison.NoiseFloor, evidence.Control.MaxDrift)
			if math.Abs(comparison.Delta) > floor && (comparison.Delta > 0) != metric.higherBetter {
				out = append(out, Regression{Metric: metric.name, Delta: comparison.Delta, NoiseFloor: floor, Basis: RegressionBasisDriftedControl})
			}
		}
	}
	return out
}

// driftedControl reports a window invalidated only because the control's
// throughput drifted past the limit. measureInterleaved records MaxDrift on
// an invalid window only in that case, after every control run completed
// cleanly.
func driftedControl(control ControlEvidence) bool {
	return control.Role == ControlRoleInvalid && control.DriftLimit > 0 && control.MaxDrift > control.DriftLimit
}

func runSamples(runs []RunSummary, arm string, value func(RunSummary) *float64) []float64 {
	var out []float64
	for _, run := range runs {
		if run.Arm != arm {
			continue
		}
		measured := value(run)
		if measured == nil || math.IsNaN(*measured) || math.IsInf(*measured, 0) {
			return nil
		}
		out = append(out, *measured)
	}
	return out
}

// DescribeRegressions puts regressions in the words every customer surface
// uses, for example "throughput got worse by 24% (noise ±8.0%)".
func DescribeRegressions(regressions []Regression) string {
	parts := make([]string, 0, len(regressions))
	for _, regression := range regressions {
		parts = append(parts, fmt.Sprintf("%s got worse by %.0f%% (noise ±%.1f%%)", MetricWords(regression.Metric), 100*math.Abs(regression.Delta), 100*regression.NoiseFloor))
	}
	return strings.Join(parts, ", ")
}

// MetricWords is the plain name of a measured outcome.
func MetricWords(metric string) string {
	switch metric {
	case "output_token_throughput":
		return "throughput"
	case "ttft_p95_ms":
		return "first-token time"
	case "latency_p95_ms":
		return "latency"
	}
	return strings.ReplaceAll(metric, "_", " ")
}

// regressionReason is the audit form of regressions, with metric names.
func regressionReason(regressions []Regression) string {
	parts := make([]string, 0, len(regressions))
	for _, regression := range regressions {
		part := fmt.Sprintf("%s changed %+.1f%% against a %.1f%% noise floor", regression.Metric, regression.Delta*100, regression.NoiseFloor*100)
		if regression.Basis == RegressionBasisDriftedControl {
			part += " in runs whose control drifted"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}
