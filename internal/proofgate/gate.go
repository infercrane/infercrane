// Package proofgate turns the signed report of one optimization run into a
// merge verdict for a change to a model, an engine setting, or a serving
// setup: PASS, WATCH, BLOCK, or UNKNOWN, and the check conclusion a CI system
// applies to it. It reads the report only, so anyone holding the saved report
// can recompute the verdict. It never estimates: a measurement the report
// does not carry leaves the verdict UNKNOWN, and UNKNOWN never passes.
package proofgate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

// SchemaVersion identifies the gate result document.
const SchemaVersion = "infercrane.proof-gate/v1"

// Verdicts. They use Release Guard's words for the same ideas.
const (
	Pass    = releaseguard.QualityPass
	Watch   = releaseguard.QualityWatch
	Block   = releaseguard.QualityBlock
	Unknown = releaseguard.QualityUnknown
)

// Conclusions a CI check can take.
const (
	ConclusionSuccess = "success"
	ConclusionNeutral = "neutral"
	ConclusionFailure = "failure"
)

// Claims about a measured gain, as the technique contract states them: a
// gain is shown only when it held on fresh servers too.
const (
	ClaimShown    = "shown"
	ClaimNotShown = "not shown"
)

// Policy is what the gate's caller chose.
type Policy struct {
	// Setup is the report's ID of the setup the change introduced. Empty
	// selects the only setup that is not the current one.
	Setup string
	// OnWatch is the conclusion for WATCH: neutral (default), failure, or
	// success.
	OnWatch string
	// RequireSignature makes an unsigned report UNKNOWN.
	RequireSignature bool
}

// Signature is what the caller established about the report's signature
// before deciding: whether one was present and verified, and by which key.
type Signature struct {
	Present        bool   `json:"present"`
	Verified       bool   `json:"verified"`
	Pinned         bool   `json:"pinned"`
	KeyID          string `json:"key_id,omitempty"`
	UnsignedReason string `json:"unsigned_reason,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Setup names one side of the comparison.
type Setup struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	State       string `json:"state,omitempty"`
	FailureCode string `json:"failure_code,omitempty"`
}

// Speed is the measured change on the metric the run ranked on, against the
// noise measured in the same window.
type Speed struct {
	Metric            string   `json:"metric"`
	ChangePercent     *float64 `json:"change_percent"`
	NoiseFloorPercent *float64 `json:"noise_floor_percent"`
	Decision          string   `json:"decision"`
	Basis             string   `json:"basis"`
	// Confirmation is the same comparison measured again on fresh servers.
	Confirmation *optimizationreport.Comparison `json:"confirmation"`
	// Claim is "shown" only for a gain that held on fresh servers.
	Claim string `json:"claim"`
	// Regressions are the outcomes on which the setup measured worse beyond
	// the noise; Guards are why it cannot be chosen whatever it gained.
	Regressions []optimizationreport.Comparison `json:"regressions"`
	Guards      []optimizationreport.Guard      `json:"guards"`
}

// Result is the gate's decision and everything a comment needs to show it.
type Result struct {
	SchemaVersion string `json:"schema_version"`
	Verdict       string `json:"verdict"`
	Conclusion    string `json:"conclusion"`
	OnWatch       string `json:"on_watch"`
	Headline      string `json:"headline"`
	// Reasons say why the verdict is what it is, most decisive first.
	Reasons []string `json:"reasons"`

	CampaignID    string                          `json:"campaign_id"`
	CampaignState string                          `json:"campaign_state"`
	Finished      bool                            `json:"finished"`
	Objective     string                          `json:"objective"`
	Model         string                          `json:"model"`
	Baseline      *Setup                          `json:"baseline"`
	Setup         *Setup                          `json:"setup"`
	Speed         Speed                           `json:"speed"`
	Answers       *optimizationreport.AnswerCheck `json:"answers"`
	Simulated     bool                            `json:"simulated_accelerator"`
	Banner        string                          `json:"banner,omitempty"`

	ReportDigest string    `json:"report_digest"`
	Signature    Signature `json:"signature"`
	EvidenceURL  string    `json:"evidence_url,omitempty"`
}

// NormalizeOnWatch validates the WATCH conclusion, defaulting to neutral.
func NormalizeOnWatch(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "neutral":
		return ConclusionNeutral, nil
	case "fail", "failure":
		return ConclusionFailure, nil
	case "pass", "success":
		return ConclusionSuccess, nil
	}
	return "", fmt.Errorf("on-watch must be neutral, fail, or pass, not %q", value)
}

// Decide applies the gate to a report document whose signature the caller
// has already checked. BLOCK outranks UNKNOWN, which outranks WATCH: a
// measured regression is decisive even when something else is missing.
func Decide(document optimizationreport.Document, signature Signature, policy Policy) Result {
	report := document.Report
	onWatch, err := NormalizeOnWatch(policy.OnWatch)
	if err != nil {
		onWatch = ConclusionFailure
	}
	result := Result{
		SchemaVersion: SchemaVersion, OnWatch: onWatch, Reasons: []string{},
		CampaignID: report.CampaignID, CampaignState: report.CampaignState, Finished: report.Finished,
		Objective: report.Objective, Model: report.ModelIdentity, Simulated: report.SimulatedAccelerator, Banner: report.Banner,
		ReportDigest: report.ReportDigest, Signature: signature,
		Speed: Speed{Metric: report.ObjectiveMetric, Decision: optimizationreport.DecisionUnknown, Basis: optimizationreport.BasisUnknown, Claim: ClaimNotShown, Regressions: []optimizationreport.Comparison{}, Guards: []optimizationreport.Guard{}},
	}
	var blocks, unknowns, watches []string
	if err != nil {
		unknowns = append(unknowns, err.Error())
	}
	if report.SchemaVersion != optimizationreport.SchemaVersion {
		unknowns = append(unknowns, fmt.Sprintf("the report schema %q is not %s", report.SchemaVersion, optimizationreport.SchemaVersion))
	}
	switch {
	case signature.Error != "":
		unknowns = append(unknowns, "the report's signature did not verify: "+signature.Error)
	case !signature.Present && policy.RequireSignature:
		unknowns = append(unknowns, "the report is not signed: "+optimizationreport.UnsignedText(signature.UnsignedReason))
	}
	if !report.Finished {
		unknowns = append(unknowns, "the run has not finished (state "+fallback(report.CampaignState, "unknown")+")")
	}

	baseline, setup, selectErr := selectSetups(report, policy.Setup)
	if baseline != nil {
		result.Baseline = &Setup{ID: baseline.ID, Label: baseline.Label, State: baseline.State}
	}
	if selectErr != "" {
		unknowns = append(unknowns, selectErr)
	}
	if setup != nil {
		result.Setup = &Setup{ID: setup.ID, Label: setup.Label, State: setup.State, FailureCode: setup.FailureCode}
		b, u, w := judgeSetup(*setup, &result)
		blocks, unknowns, watches = append(blocks, b...), append(unknowns, u...), append(watches, w...)
	}

	switch {
	case len(blocks) > 0:
		result.Verdict, result.Conclusion = Block, ConclusionFailure
		result.Reasons = append(append(result.Reasons, blocks...), unknowns...)
	case len(unknowns) > 0:
		result.Verdict, result.Conclusion = Unknown, ConclusionFailure
		result.Reasons = append(result.Reasons, unknowns...)
	case len(watches) > 0:
		result.Verdict, result.Conclusion = Watch, onWatch
		result.Reasons = append(result.Reasons, watches...)
	default:
		result.Verdict, result.Conclusion = Pass, ConclusionSuccess
		result.Reasons = append(result.Reasons, passReasons(result)...)
	}
	result.Headline = headline(result)
	return result
}

// selectSetups finds the current setup and the setup under test.
func selectSetups(report optimizationreport.Report, requested string) (*optimizationreport.CandidateResult, *optimizationreport.CandidateResult, string) {
	var baseline *optimizationreport.CandidateResult
	var others []*optimizationreport.CandidateResult
	for index := range report.Candidates {
		candidate := &report.Candidates[index]
		if candidate.Role == "baseline" || candidate.Role == "baseline_kept" {
			baseline = candidate
			continue
		}
		others = append(others, candidate)
	}
	if baseline == nil {
		return nil, nil, "the report names no current setup to compare with"
	}
	requested = strings.TrimSpace(requested)
	if requested != "" {
		for _, candidate := range others {
			if candidate.ID == requested {
				return baseline, candidate, ""
			}
		}
		if requested == baseline.ID {
			return baseline, nil, "setup " + requested + " is the current setup, not a change to it"
		}
		return baseline, nil, "the report has no setup " + requested
	}
	switch len(others) {
	case 0:
		return baseline, nil, "the report has no setup besides the current one"
	case 1:
		return baseline, others[0], ""
	}
	ids := make([]string, 0, len(others))
	for _, candidate := range others {
		ids = append(ids, candidate.ID)
	}
	return baseline, nil, "the run measured several setups (" + strings.Join(ids, ", ") + "); name the one this change introduced"
}

// Failure codes that end a setup the gate blocks: it measured worse, failed
// more requests or answered shorter, missed a limit the run required, its
// answers failed, or Release Guard rejected it.
var blockingFailures = []string{
	optimizationcampaign.FailureRegressed, optimizationcampaign.FailureErrorRateRegression, optimizationcampaign.FailureOutputLengthRegression,
	"measured_candidate_outside_constraints", optimizationcampaign.FailureQualityRejected, "release_guard_rejected",
}

// Failure codes that leave the comparison readable: the setup was measured
// in full and simply not chosen.
var readableFailures = []string{optimizationcampaign.FailureNotConfirmed, optimizationcampaign.FailureNoMeasurableDifference, "measured_candidate_superseded"}

func judgeSetup(setup optimizationreport.CandidateResult, result *Result) (blocks, unknowns, watches []string) {
	speed := &result.Speed
	speed.Regressions = append(speed.Regressions, setup.Regressions...)
	speed.Guards = append(speed.Guards, setup.Guards...)
	speed.Confirmation = setup.Confirmation
	if comparison := setup.VersusBaseline; comparison != nil {
		change, floor := comparison.ChangePercent, comparison.NoiseFloorPercent
		speed.Metric, speed.ChangePercent, speed.NoiseFloorPercent = comparison.Metric, &change, &floor
		speed.Decision, speed.Basis = comparison.Decision, optimizationreport.BasisInterleaved
	}
	for _, regression := range setup.Regressions {
		blocks = append(blocks, fmt.Sprintf("%s changed %+.1f%% against a %.1f%% noise floor: worse beyond the measured noise", metricLabel(regression.Metric), regression.ChangePercent, regression.NoiseFloorPercent))
	}
	if speed.Decision == optimizationreport.DecisionRegressed && len(setup.Regressions) == 0 {
		blocks = append(blocks, fmt.Sprintf("%s changed %+.1f%% against a %.1f%% noise floor: worse beyond the measured noise", metricLabel(speed.Metric), *speed.ChangePercent, *speed.NoiseFloorPercent))
	}
	for _, guard := range setup.Guards {
		blocks = append(blocks, guard.Reason)
	}

	switch speed.Decision {
	case optimizationreport.DecisionImproved:
		switch {
		case speed.Confirmation == nil:
			speed.Claim = ClaimNotShown
		case speed.Confirmation.Decision == optimizationreport.DecisionImproved:
			speed.Claim = ClaimShown
		case speed.Confirmation.Decision == optimizationreport.DecisionRegressed:
			unknowns = append(unknowns, fmt.Sprintf("%s measured better (%+.1f%%), then worse on fresh servers (%+.1f%%, noise ±%.1f%%): the windows disagree", metricLabel(speed.Metric), *speed.ChangePercent, speed.Confirmation.ChangePercent, speed.Confirmation.NoiseFloorPercent))
		}
	case optimizationreport.DecisionNoDifference, optimizationreport.DecisionRegressed:
	default:
		reason := setup.Reason
		if reason == "" {
			reason = "it was not measured alongside the current setup"
		}
		unknowns = append(unknowns, "the speed change is unknown: "+reason)
	}
	if setup.FailureCode != "" && !slices.Contains(blockingFailures, setup.FailureCode) && !slices.Contains(readableFailures, setup.FailureCode) {
		unknowns = append(unknowns, "the run stopped this setup before it could be judged ("+setup.FailureCode+")")
	}

	result.Answers = setup.AnswerCheck
	switch answers := setup.AnswerCheck; {
	case answers == nil:
		unknowns = append(unknowns, "the answer check has not run for this setup")
	case answers.Verdict == Block:
		blocks = append(blocks, "answer check blocked: "+answers.Reason)
	case answers.Verdict == Watch:
		watches = append(watches, "answer check in the watch band: "+answers.Reason)
	case answers.Verdict == Pass && answers.Reference:
		unknowns = append(unknowns, "this setup was the answer reference, so its answers were not compared with the current setup")
	case answers.Verdict == Pass:
	default:
		unknowns = append(unknowns, "the answer check is unknown: "+fallback(answers.Reason, "no verdict was recorded"))
	}
	if slices.Contains(blockingFailures, setup.FailureCode) && len(blocks) == 0 {
		blocks = append(blocks, "the run rejected this setup ("+setup.FailureCode+")")
	}
	return blocks, unknowns, watches
}

func passReasons(result Result) []string {
	reasons := []string{}
	speed := result.Speed
	switch {
	case speed.Decision == optimizationreport.DecisionImproved && speed.Claim == ClaimShown:
		reasons = append(reasons, fmt.Sprintf("%s improved %+.1f%% (noise ±%.1f%%), then %+.1f%% (noise ±%.1f%%) on fresh servers", metricLabel(speed.Metric), *speed.ChangePercent, *speed.NoiseFloorPercent, speed.Confirmation.ChangePercent, speed.Confirmation.NoiseFloorPercent))
	case speed.Decision == optimizationreport.DecisionImproved:
		reasons = append(reasons, fmt.Sprintf("%s changed %+.1f%% (noise ±%.1f%%); no regression, and the gain is not claimed until it holds on fresh servers", metricLabel(speed.Metric), *speed.ChangePercent, *speed.NoiseFloorPercent))
	default:
		reasons = append(reasons, fmt.Sprintf("%s changed %+.1f%%, inside the %.1f%% noise floor: no measurable difference", metricLabel(speed.Metric), *speed.ChangePercent, *speed.NoiseFloorPercent))
	}
	if result.Answers != nil {
		reasons = append(reasons, "answer check passed: "+result.Answers.Reason)
	}
	return reasons
}

func headline(result Result) string {
	switch result.Verdict {
	case Pass:
		if result.Speed.Claim == ClaimShown {
			return "No regression. The change measured better, and the gain held on fresh servers."
		}
		return "No regression beyond the measured noise, and the answers held."
	case Watch:
		return "No speed regression, but the answers moved into the watch band. Review them before merging."
	case Block:
		return "Regression: this change measured worse than the current setup."
	}
	return "Not proven: the run does not show whether this change regresses."
}

// metricLabel names a report metric in words.
func metricLabel(metric string) string {
	switch metric {
	case "output_token_throughput":
		return "Output tokens/s"
	case "request_throughput":
		return "Requests/s"
	case optimizationreport.MetricTTFTP95:
		return "p95 time to first token"
	case "tpot_p95_ms":
		return "p95 time per output token"
	case "latency_p95_ms":
		return "p95 request latency"
	case optimizationreport.MetricCostPerMillion:
		return "Cost per 1M output tokens"
	case optimizationreport.MetricTokensPerUser:
		return "Output tokens/s per user"
	case optimizationreport.MetricTokensPerAccel:
		return "Output tokens/s per GPU"
	case optimizationreport.MetricRequestsPerSec:
		return "Requests/s"
	case "":
		return "The ranked metric"
	}
	return metric
}

func fallback(value, otherwise string) string {
	if strings.TrimSpace(value) == "" {
		return otherwise
	}
	return value
}
