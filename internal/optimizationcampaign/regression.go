package optimizationcampaign

import (
	"fmt"
	"math"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
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
// throughput drifted past the limit. The measurement records MaxDrift on
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

const (
	// FailureErrorRateRegression marks a candidate that failed more of its
	// requests than the control. Latency percentiles leave failed requests
	// out, so failing slow requests can pass for a speed gain. Such a
	// candidate is never selected, whatever it gained.
	FailureErrorRateRegression = "measured_error_rate_regression"
	// FailureOutputLengthRegression marks a candidate whose answers were
	// shorter than the control's. Shorter answers finish sooner and need
	// fewer tokens, so they can pass for a speed or cost gain. Such a
	// candidate is never selected, whatever it gained.
	FailureOutputLengthRegression = "measured_output_length_regression"

	// GuardMetricErrorRate is the share of requests that failed.
	GuardMetricErrorRate = "error_rate"
	// GuardMetricOutputTokens is the mean server-counted output tokens of a
	// successful request.
	GuardMetricOutputTokens = "output_tokens_per_request"

	// GuardBasisSameWindow compares with the control runs measured in turns
	// with the candidate, in its own window.
	GuardBasisSameWindow = "same_window"
	// GuardBasisSeparateRuns compares with the control's own measurement,
	// taken at another time on the same workload.
	GuardBasisSeparateRuns = "separate_runs"

	// errorRateMargin is how many more of its requests, as a share, a
	// candidate may fail than the control. When the control failed none, a
	// candidate that fails this share or more is held back too.
	errorRateMargin = 0.01
	// maxOutputLengthDrop is how much shorter than the control's, relative
	// to it, a candidate's mean answer may be.
	maxOutputLengthDrop = 0.05
	// guardTolerance keeps a value exactly at a threshold from flipping on
	// floating-point rounding.
	guardTolerance = 1e-9
)

// ArmOutcome pools one arm's runs: the requests it sent, how many failed,
// and the mean output tokens of a successful request from the server's own
// counts. OutputTokensPerRequest is nil when any run with a successful
// request did not record it, including runs measured before it was recorded.
type ArmOutcome struct {
	Requests               int      `json:"requests"`
	Failed                 int      `json:"failed"`
	OutputTokensPerRequest *float64 `json:"output_tokens_per_request"`
}

// ErrorRate is the share of requests that failed, or nil without requests.
func (o ArmOutcome) ErrorRate() *float64 {
	if o.Requests <= 0 {
		return nil
	}
	rate := float64(o.Failed) / float64(o.Requests)
	return &rate
}

// Guard is a measured result that keeps a candidate from being selected,
// whatever it gained: it failed more requests than the control, or it
// answered shorter. Control and Candidate are the two values compared, as a
// share for error_rate and in tokens for output_tokens_per_request.
type Guard struct {
	Code      string  `json:"code"`
	Metric    string  `json:"metric"`
	Control   float64 `json:"control"`
	Candidate float64 `json:"candidate"`
	Reason    string  `json:"reason"`
}

// GuardCheck compares a candidate with the control on failed requests and
// answer length. Guards lists what fired; an empty list with an unknown
// answer length on either side has checked failed requests only.
type GuardCheck struct {
	Basis     string     `json:"basis"`
	Control   ArmOutcome `json:"control"`
	Candidate ArmOutcome `json:"candidate"`
	Guards    []Guard    `json:"guards,omitempty"`
}

// CheckGuards compares a candidate's measurement with the control's. The
// control's side comes from its runs in the candidate's own window when the
// measurement holds any, and otherwise from the control's own measurement.
// It reports false when either side has no measured requests. The caller
// must already have established that both rows measured the same workload.
func CheckGuards(candidate, control domain.BenchmarkResult, controlID string) (GuardCheck, bool) {
	candidateSide, ok := OwnOutcome(candidate)
	if !ok {
		return GuardCheck{}, false
	}
	check := GuardCheck{Basis: GuardBasisSeparateRuns, Candidate: candidateSide}
	if measured := decodeRankingWorkload(candidate.WorkloadJSON).Measured; measured != nil && controlID != "" && measured.Control.CandidateID == controlID {
		if side, found := runOutcome(measured.Runs, ArmControl); found {
			check.Basis, check.Control = GuardBasisSameWindow, side
		}
	}
	if check.Basis == GuardBasisSeparateRuns {
		side, found := OwnOutcome(control)
		if !found {
			return GuardCheck{}, false
		}
		check.Control = side
	}
	check.Guards = guards(check.Candidate, check.Control)
	return check, true
}

// OwnOutcome is a setup's own measurement: the runs it was measured in as
// the candidate arm or, for evidence stored without runs, the pooled counts
// of its benchmark row with the answer length unknown.
func OwnOutcome(row domain.BenchmarkResult) (ArmOutcome, bool) {
	if measured := decodeRankingWorkload(row.WorkloadJSON).Measured; measured != nil {
		if side, found := runOutcome(measured.Runs, ArmCandidate); found {
			return side, true
		}
	}
	if row.RequestCount <= 0 || row.Failed < 0 || row.Failed > row.RequestCount {
		return ArmOutcome{}, false
	}
	return ArmOutcome{Requests: row.RequestCount, Failed: row.Failed}, true
}

// runOutcome pools one arm's runs. The answer length is weighted by each
// run's successful requests.
func runOutcome(runs []RunSummary, arm string) (ArmOutcome, bool) {
	var out ArmOutcome
	weighted, succeeded, lengthKnown := 0.0, 0, true
	for _, run := range runs {
		if run.Arm != arm {
			continue
		}
		out.Requests += run.Requests
		out.Failed += run.Failed
		ok := run.Requests - run.Failed
		if ok <= 0 {
			continue
		}
		length := run.OutputTokensPerRequest
		if length == nil || math.IsNaN(*length) || math.IsInf(*length, 0) || *length < 0 {
			lengthKnown = false
			continue
		}
		weighted += *length * float64(ok)
		succeeded += ok
	}
	if out.Requests <= 0 || out.Failed < 0 || out.Failed > out.Requests {
		return ArmOutcome{}, false
	}
	if lengthKnown && succeeded > 0 {
		mean := weighted / float64(succeeded)
		out.OutputTokensPerRequest = &mean
	}
	return out, true
}

// guards applies the two rules. A candidate is held back when its share of
// failed requests exceeds the control's by more than one percentage point,
// or reaches 1% while the control failed none; or when its mean answer is
// more than 5% shorter than the control's. An unknown value fires nothing.
func guards(candidate, control ArmOutcome) []Guard {
	var out []Guard
	if candidateRate, controlRate := candidate.ErrorRate(), control.ErrorRate(); candidateRate != nil && controlRate != nil {
		if *candidateRate-*controlRate > errorRateMargin+guardTolerance || control.Failed == 0 && *candidateRate >= errorRateMargin-guardTolerance {
			out = append(out, Guard{Code: FailureErrorRateRegression, Metric: GuardMetricErrorRate, Control: *controlRate, Candidate: *candidateRate,
				Reason: fmt.Sprintf("failed %.1f%% of requests; the current setup failed %.1f%%", 100**candidateRate, 100**controlRate)})
		}
	}
	if candidateLength, controlLength := candidate.OutputTokensPerRequest, control.OutputTokensPerRequest; candidateLength != nil && controlLength != nil && *controlLength > 0 {
		if change := *candidateLength / *controlLength - 1; change < -maxOutputLengthDrop-guardTolerance {
			out = append(out, Guard{Code: FailureOutputLengthRegression, Metric: GuardMetricOutputTokens, Control: *controlLength, Candidate: *candidateLength,
				Reason: fmt.Sprintf("answered %.0f%% shorter than the current setup; a speed or cost gain may come from shorter answers", -100*change)})
		}
	}
	return out
}

// DescribeGuards puts fired guards in the words every customer surface uses,
// for example "failed 3.1% of requests; the current setup failed 0.0%".
func DescribeGuards(guards []Guard) string {
	parts := make([]string, 0, len(guards))
	for _, guard := range guards {
		parts = append(parts, guard.Reason)
	}
	return strings.Join(parts, " · ")
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
