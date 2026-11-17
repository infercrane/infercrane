package optimizationreport

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

// The renderers lead with outcomes in plain words. Mechanics appear only as
// evidence under the outcomes, and every unknown says why.

var stateWords = map[string]string{
	"proposed": "queued", "provisioning": "starting a server", "ready": "server ready",
	"measuring": "measuring against your current setup", "validating": "checking answer quality",
	"ranked": "measured and ranked", "guarding": "running the safety check", "guard_passed": "passed the safety check",
	"qualified": "winner, ready to activate", "rejected": "rejected", "inconclusive": "no clear result",
	"promoted": "live", "observed": "live", "failed": "failed", "cancelled": "cancelled", "cleaned": "server removed",
}

var reasonWords = map[string]string{
	"measured_no_measurable_difference":      "no real difference from your current setup",
	"measured_candidate_regressed":           "worse than your current setup",
	"measured_comparison_unavailable":        "not compared reliably with your current setup",
	"measured_candidate_superseded":          "another setup measured better",
	"measured_candidate_outside_constraints": "missed your speed or cost limits",
	"measured_ranking_inconclusive":          "results were not comparable",
	"quality_gate_rejected":                  "failed the answer-quality check",
	"release_guard_rejected":                 "failed the safety check",
	"release_guard_inconclusive":             "safety check had too little evidence",
	"approval_expired_before_activation":     "approval expired before activation",
}

var decisionWords = map[string]string{
	DecisionImproved: "better", DecisionRegressed: "worse", DecisionNoDifference: "no real difference",
	DecisionUnchanged: "unchanged", DecisionUnknown: "unknown",
}

var metricWords = map[string]string{
	sourceOutputTPS: "output throughput", sourceTTFTP95: "p95 time to first token",
}

// View is the formatted report shared by the text and HTML renderers and the
// CLI's styled terminal rendering. Every value is already formatted, and every
// missing value already reads "unknown", so a renderer only lays it out.
type View struct {
	Title, Subtitle, Banner, Headline string
	Kind                              string
	Rows                              []ViewRow
	Measurement                       []string
	Candidates                        []ViewCandidate
	SignalsNote                       string
	Signals                           []ViewSignal
	Winner, Baseline                  []ViewField
	WinnerTitle, BaselineTitle        string
	Evidence                          []ViewField
	Unknown                           []ViewField
	Digest                            string
}

// ViewRow is one outcome: the current setup against the winner. Tone is good,
// bad, neutral, watch, or unknown.
type ViewRow struct {
	Label, Baseline, Winner, Change, Decision, Noise, Reason string
	Tone                                                     string
	// Lead is an outcome card's large value and LeadNote the line under it.
	Lead, LeadNote string
}

// lead fills the card values. A card leads with the chosen setup; when the
// campaign chose none it leads with the current setup, so measured values
// stay visible instead of a large "unknown".
func (row *ViewRow) lead(chosen bool) {
	if chosen {
		row.Lead, row.LeadNote = row.Winner, "current setup: "+row.Baseline
		return
	}
	row.Lead, row.LeadNote = row.Baseline, "current setup"
}

// ViewCandidate is one measured setup. Measured joins the known measurements;
// TokensPerSecond, TTFTP95, and CostPerMillion hold them one by one and read
// "unknown" when the setup did not record them. State is the raw lifecycle
// state behind Status.
type ViewCandidate struct {
	Label, Role, Status, Measured, Versus, Tone string
	State                                       string
	TokensPerSecond, TTFTP95, CostPerMillion    string
}

// ViewSignal is one engine signal for both setups.
type ViewSignal struct {
	Name, Stat, Baseline, Winner string
}

// ViewField is one labelled value.
type ViewField struct {
	Name, Value string
}

// NewView formats a report for display without adding or dropping a value.
func NewView(r Report) View {
	v := View{Title: "Optimization result", Subtitle: subtitle(r), Banner: r.Banner, Headline: r.Outcome.Headline, Kind: r.Outcome.Kind, Digest: r.ReportDigest}
	for _, metric := range r.Metrics {
		v.Rows = append(v.Rows, metricRow(metric))
	}
	v.Rows = append(v.Rows, qualityRow(r.Quality), monthlyRow(r.Monthly))
	chosen := r.Outcome.Kind == OutcomeWinnerSelected || r.Outcome.Kind == OutcomeBaselineKept
	for index := range v.Rows {
		v.Rows[index].lead(chosen)
	}
	v.Measurement = measurementLines(r)
	for _, candidate := range r.Candidates {
		v.Candidates = append(v.Candidates, candidateView(candidate))
	}
	if r.Bottleneck.State != BasisUnknown {
		v.SignalsNote = "What the serving engine reported while each setup was measured. GPU telemetry: " + strings.ReplaceAll(r.Bottleneck.GPUTelemetry, "_", " ") + "."
		for _, signal := range r.Bottleneck.Signals {
			v.Signals = append(v.Signals, ViewSignal{Name: signal.Name, Stat: signal.Stat, Baseline: plainNumber(signal.Baseline), Winner: plainNumber(signal.Winner)})
		}
	}
	if r.Appendix.Winner != nil {
		v.WinnerTitle, v.Winner = "Winning setup", setupFields(*r.Appendix.Winner)
		if r.Outcome.Kind == OutcomeBaselineKept {
			v.WinnerTitle = "Your current setup (kept)"
		}
	}
	if r.Appendix.Baseline != nil && (r.Appendix.Winner == nil || r.Appendix.Baseline.CandidateID != r.Appendix.Winner.CandidateID) {
		v.BaselineTitle, v.Baseline = "Your current setup", setupFields(*r.Appendix.Baseline)
	}
	for _, entry := range r.Appendix.Evidence {
		value := entry.Digest
		if entry.ID != "" && !strings.HasSuffix(entry.Digest, strings.TrimPrefix(entry.ID, "optlab-")) {
			value += "  (" + entry.ID + ")"
		}
		if entry.Subject != "" {
			value += "  " + entry.Subject
		}
		v.Evidence = append(v.Evidence, ViewField{strings.ReplaceAll(entry.Kind, "_", " "), value})
	}
	v.Evidence = append(v.Evidence, ViewField{"this report", r.ReportDigest})
	for _, field := range r.Unknown {
		v.Unknown = append(v.Unknown, ViewField{field, unknownReason(r, field)})
	}
	return v
}

func subtitle(r Report) string {
	parts := []string{}
	if r.ModelIdentity != "" {
		parts = append(parts, r.ModelIdentity)
	}
	if r.Objective != "" {
		parts = append(parts, "goal: "+r.Objective)
	}
	parts = append(parts, "campaign "+r.CampaignID)
	return strings.Join(parts, " · ")
}

func metricRow(metric Metric) ViewRow {
	format := formatterFor(metric.Key)
	row := ViewRow{Label: metric.Label, Baseline: orUnknown(metric.Baseline, format), Winner: orUnknown(metric.Winner, format), Decision: decisionWords[metric.Decision], Reason: metric.Reason}
	if metric.ChangePercent != nil {
		row.Change = signedPercent(*metric.ChangePercent)
	} else if metric.Decision == DecisionUnchanged {
		row.Change = "no change"
	} else {
		row.Change = "unknown"
	}
	if metric.NoiseFloorPercent != nil {
		row.Noise = fmt.Sprintf("noise ±%.1f%%", *metric.NoiseFloorPercent)
	}
	switch {
	case metric.Decision == DecisionUnknown && metric.ChangePercent != nil:
		row.Decision = "not checked against noise"
	case metric.Decision == DecisionUnknown:
		row.Decision = ""
	}
	row.Tone = tone(metric.Decision)
	return row
}

func qualityRow(quality Quality) ViewRow {
	row := ViewRow{Label: "Answer quality", Baseline: orUnknown(quality.BaselineScore, score), Winner: orUnknown(quality.WinnerScore, score), Decision: quality.Verdict, Reason: quality.Reason}
	if quality.Eval != "" {
		row.Label += " (" + quality.Eval + ")"
	}
	switch {
	case quality.Delta != nil:
		row.Change = fmt.Sprintf("%+.3f", *quality.Delta)
	case quality.Basis == BasisBaselineKept:
		row.Change = "no change"
	default:
		row.Change = "unknown"
	}
	if quality.WatchThresholdPercent != nil {
		row.Noise = fmt.Sprintf("watch above %.1f%% drop", *quality.WatchThresholdPercent)
	}
	switch quality.Verdict {
	case releaseguard.QualityPass:
		row.Tone = "good"
	case releaseguard.QualityWatch:
		row.Tone = "watch"
	case releaseguard.QualityBlock:
		row.Tone = "bad"
	default:
		row.Tone = "unknown"
	}
	return row
}

func monthlyRow(monthly Monthly) ViewRow {
	row := ViewRow{Label: "Monthly cost at your traffic", Baseline: orUnknown(monthly.BaselineUSD, usd), Winner: orUnknown(monthly.WinnerUSD, usd), Reason: monthly.Reason, Tone: tone(monthly.Decision)}
	if monthly.Decision != DecisionUnknown {
		row.Decision = decisionWords[monthly.Decision]
	}
	switch {
	case monthly.ChangeUSD != nil && monthly.ChangePercent != nil:
		row.Change = signedUSD(*monthly.ChangeUSD) + " (" + signedPercent(*monthly.ChangePercent) + ")"
	case monthly.Decision == DecisionUnchanged:
		row.Change = "no change"
	default:
		row.Change = "unknown"
	}
	if monthly.Basis == BasisReplayVolume && monthly.WinnerUSD != nil {
		row.Noise = "projection at your replayed traffic"
	}
	return row
}

func measurementLines(r Report) []string {
	lines := []string{}
	n := r.Noise
	if n.Basis == BasisInterleaved {
		line := "Noise band: "
		if n.ObjectiveNoiseFloorPercent != nil {
			line += fmt.Sprintf("±%.1f%% on %s", *n.ObjectiveNoiseFloorPercent, metricName(n.ObjectiveMetric))
		} else {
			line += "unknown on " + metricName(n.ObjectiveMetric)
		}
		if n.ControlDriftPercent != nil && n.DriftLimitPercent != nil {
			line += fmt.Sprintf("; your current setup varied %.1f%% between runs (limit %.0f%%)", *n.ControlDriftPercent, *n.DriftLimitPercent)
		}
		other := "the winner"
		if r.Outcome.Kind != OutcomeWinnerSelected {
			other = "the closest challenger"
		}
		line += fmt.Sprintf("; %d runs of your current setup and %d of %s, taken in turns in the same window.", n.ControlRuns, n.CandidateRuns, other)
		lines = append(lines, line)
		if r.Outcome.Kind != OutcomeWinnerSelected && n.ObjectiveChangePercent != nil {
			lines = append(lines, fmt.Sprintf("Closest challenger: %s %s against your current setup, %s.", metricName(n.ObjectiveMetric), signedPercent(*n.ObjectiveChangePercent), decisionWords[n.ObjectiveDecision]))
		}
	} else {
		lines = append(lines, "Noise band: unknown ("+n.Reason+").")
	}
	load := "Load: "
	switch r.Workload.Source {
	case "customer_replay":
		load += "a replay of your traffic"
	case "synthetic_profile":
		load += "a synthetic profile, not your traffic"
	default:
		load += "unknown"
	}
	details := []string{}
	if r.Workload.Profile != "" {
		details = append(details, "profile "+r.Workload.Profile)
	}
	if r.Workload.Concurrency != nil {
		details = append(details, fmt.Sprintf("%d concurrent requests", *r.Workload.Concurrency))
	}
	if r.Workload.RequestsPerRun != nil {
		details = append(details, fmt.Sprintf("%d requests per run", *r.Workload.RequestsPerRun))
	}
	if len(details) > 0 {
		load += " (" + strings.Join(details, ", ") + ")"
	}
	lines = append(lines, load+".")
	m := r.Monthly
	if m.Basis == BasisReplayVolume && m.RequestsInWindow != nil && m.OutputTokensInWindow != nil && m.WindowSeconds != nil {
		lines = append(lines, fmt.Sprintf("Monthly projection: %s requests and %s output tokens in %s of your traffic, scaled to a %.0f-hour month; %s.", grouped(int64(*m.RequestsInWindow)), grouped(*m.OutputTokensInWindow), duration(*m.WindowSeconds), m.HoursPerMonth, m.Assumption))
	}
	switch r.Quality.Basis {
	case BasisReleaseGuard:
		lines = append(lines, "Answer quality: Release Guard compared signed evals of both setups under your quality policy.")
	case BasisCampaignGate:
		lines = append(lines, "Answer quality: the winner's signed eval had to pass. No Release Guard watch band applies to this campaign.")
	}
	if r.SimulatedAccelerator {
		lines = append(lines, "Hardware: a simulated accelerator on a CPU. These numbers show the process works; they are not GPU results.")
	}
	return lines
}

func candidateView(candidate CandidateResult) ViewCandidate {
	result := ViewCandidate{Label: candidate.Label, Status: candidateStatus(candidate), State: candidate.State}
	result.TokensPerSecond = orUnknown(candidate.OutputTokensPerSecond, tokens)
	result.TTFTP95 = orUnknown(candidate.TTFTP95MS, ms)
	result.CostPerMillion = orUnknown(candidate.CostPerMillionOutputTokens, usd)
	switch candidate.Role {
	case "baseline":
		result.Role = "current setup"
	case "baseline_kept":
		result.Role = "current setup, kept"
	case "winner":
		result.Role = "winner"
	}
	parts := []string{}
	if candidate.OutputTokensPerSecond != nil {
		parts = append(parts, tokens(*candidate.OutputTokensPerSecond)+" tokens/s")
	}
	if candidate.TTFTP95MS != nil {
		parts = append(parts, "p95 first token "+ms(*candidate.TTFTP95MS))
	}
	if candidate.CostPerMillionOutputTokens != nil {
		parts = append(parts, usd(*candidate.CostPerMillionOutputTokens)+" per 1M output tokens")
	}
	if len(parts) == 0 {
		parts = append(parts, "not measured")
	}
	result.Measured = strings.Join(parts, " · ")
	if candidate.VersusBaseline != nil {
		comparison := candidate.VersusBaseline
		result.Versus = fmt.Sprintf("%s %s against your current setup (noise ±%.1f%%): %s", metricName(comparison.Metric), signedPercent(comparison.ChangePercent), comparison.NoiseFloorPercent, decisionWords[comparison.Decision])
		result.Tone = tone(comparison.Decision)
	} else if candidate.Reason != "" && candidate.Role != "baseline" && candidate.Role != "baseline_kept" {
		result.Versus = candidate.Reason
		result.Tone = "unknown"
	}
	return result
}

func setupFields(setup Setup) []ViewField {
	fields := []ViewField{{"Setup", setup.Label}}
	engine := setup.Engine
	if engine == "" {
		engine = "unknown"
	}
	if setup.EngineVersion != "" {
		engine += " " + setup.EngineVersion
	}
	fields = append(fields, ViewField{"Engine", engine})
	args := "default settings"
	if len(setup.Args) > 0 {
		args = strings.Join(setup.Args, " ")
	}
	fields = append(fields, ViewField{"Args", args})
	if setup.Image != "" {
		fields = append(fields, ViewField{"Image", setup.Image})
	}
	hardware := "unknown"
	if setup.Accelerator != "" {
		count := "?"
		if setup.AcceleratorCount != nil {
			count = strconv.Itoa(*setup.AcceleratorCount)
		}
		hardware = count + " × " + setup.Accelerator
		if setup.AcceleratorClass == AcceleratorSimulated {
			hardware += " (simulated accelerator on CPU, not a GPU)"
		}
	}
	location := []string{}
	for _, part := range []string{setup.Provider, setup.Region, setup.ComputeMode} {
		if part != "" {
			location = append(location, part)
		}
	}
	if len(location) > 0 {
		hardware += " · " + strings.Join(location, " ")
	}
	if setup.MaxReplicas > 0 {
		hardware += fmt.Sprintf(" · %d–%d replicas", setup.MinReplicas, setup.MaxReplicas)
	}
	fields = append(fields, ViewField{"Hardware", hardware})
	if setup.Model != "" {
		fields = append(fields, ViewField{"Model", setup.Model})
	}
	price := "unknown (no sourced price)"
	if setup.HourlyUSD != nil {
		price = usd(*setup.HourlyUSD) + " per hour per replica · " + setup.PriceSource
	}
	fields = append(fields, ViewField{"Price", price})
	if setup.RevisionID != "" {
		fields = append(fields, ViewField{"Revision", setup.DeploymentName + " " + setup.RevisionID})
	}
	return fields
}

func unknownReason(r Report, field string) string {
	switch {
	case strings.HasPrefix(field, "metrics."):
		for _, metric := range r.Metrics {
			if "metrics."+metric.Key == field {
				return metric.Label + ": " + emptyAs(metric.Reason, "missing input")
			}
		}
	case strings.HasPrefix(field, "quality."):
		return "Answer quality: " + emptyAs(r.Quality.Reason, "missing input")
	case field == "noise":
		return "Noise band: " + emptyAs(r.Noise.Reason, "missing input")
	case strings.HasPrefix(field, "monthly"):
		return "Monthly cost: " + emptyAs(r.Monthly.Reason, "missing input")
	case field == "bottleneck":
		return "Bottleneck signals: " + emptyAs(r.Bottleneck.Reason, "missing input")
	}
	return "missing input"
}

// measuredLoss lists the ranking codes whose words a measured regression
// replaces. A setup that lost to the baseline and measured worse on an
// outcome says which one, including runs ranked before the gate named it.
var measuredLoss = map[string]bool{
	optimizationcampaign.FailureNoMeasurableDifference: true, optimizationcampaign.FailureRegressed: true,
	optimizationcampaign.FailureComparisonUnavailable: true, "measured_candidate_superseded": true,
}

func candidateStatus(candidate CandidateResult) string {
	if candidate.Role == "baseline_kept" && (candidate.State == optimizationcampaign.CandidateQualified || candidate.State == optimizationcampaign.CandidateGuardPassed) {
		return keptWords
	}
	if measuredLoss[candidate.FailureCode] && len(candidate.Regressions) > 0 {
		regressions := make([]optimizationcampaign.Regression, 0, len(candidate.Regressions))
		for _, regression := range candidate.Regressions {
			regressions = append(regressions, optimizationcampaign.Regression{Metric: regression.Metric, Delta: regression.ChangePercent / 100, NoiseFloor: regression.NoiseFloorPercent / 100, Basis: regression.Basis})
		}
		return statusWords(candidate.State, "") + " · " + optimizationcampaign.DescribeRegressions(regressions)
	}
	return statusWords(candidate.State, candidate.FailureCode)
}

// keptWords describe a baseline the campaign kept. It is not a winner, and
// activating it changes nothing.
const keptWords = "kept · nothing to activate"

func statusWords(state, failure string) string {
	text := stateWords[state]
	if text == "" {
		text = strings.ReplaceAll(state, "_", " ")
	}
	if failure != "" {
		reason := reasonWords[failure]
		if reason == "" {
			reason = strings.ReplaceAll(failure, "_", " ")
		}
		text += " · " + reason
	}
	return text
}

func tone(decision string) string {
	switch decision {
	case DecisionImproved:
		return "good"
	case DecisionRegressed:
		return "bad"
	case DecisionNoDifference, DecisionUnchanged:
		return "neutral"
	}
	return "unknown"
}

func metricName(metric string) string {
	if name := metricWords[metric]; name != "" {
		return name
	}
	if metric == "" {
		return "the goal metric"
	}
	return strings.ReplaceAll(metric, "_", " ")
}

func formatterFor(key string) func(float64) string {
	switch key {
	case MetricCostPerMillion:
		return usd
	case MetricTTFTP95:
		return ms
	case MetricRequestsPerSec:
		return func(value float64) string { return fmt.Sprintf("%.2f", value) }
	default:
		return tokens
	}
}

func orUnknown(value *float64, format func(float64) string) string {
	if value == nil {
		return "unknown"
	}
	return format(*value)
}

func usd(value float64) string {
	switch {
	case math.Abs(value) >= 100:
		return "$" + grouped(int64(math.Round(value)))
	case math.Abs(value) >= 1:
		return fmt.Sprintf("$%.2f", value)
	case value == 0:
		return "$0"
	default:
		return "$" + strconv.FormatFloat(value, 'g', 3, 64)
	}
}

func signedUSD(value float64) string {
	if value < 0 {
		return "−" + usd(-value)
	}
	return "+" + usd(value)
}

func signedPercent(value float64) string {
	if value < 0 {
		return fmt.Sprintf("−%.1f%%", -value)
	}
	return fmt.Sprintf("+%.1f%%", value)
}

func ms(value float64) string { return fmt.Sprintf("%.0f ms", value) }

func tokens(value float64) string {
	switch {
	case value >= 100:
		return fmt.Sprintf("%.0f", value)
	case value >= 10:
		return fmt.Sprintf("%.1f", value)
	default:
		return fmt.Sprintf("%.2f", value)
	}
}

func score(value float64) string { return strconv.FormatFloat(value, 'f', 3, 64) }

func plainNumber(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatFloat(*value, 'g', 4, 64)
}

func grouped(value int64) string {
	text := strconv.FormatInt(value, 10)
	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")
	var out strings.Builder
	for index, digit := range text {
		if index > 0 && (len(text)-index)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	if negative {
		return "-" + out.String()
	}
	return out.String()
}

func duration(seconds float64) string {
	total := int64(math.Round(seconds))
	hours, minutes, secs := total/3600, total%3600/60, total%60
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	case minutes > 0:
		return fmt.Sprintf("%dm %ds", minutes, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

func emptyAs(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// Text renders the report as plain text for terminals and logs.
func Text(r Report) string {
	v := NewView(r)
	var b strings.Builder
	b.WriteString(v.Title + "\n" + v.Subtitle + "\n\n")
	if v.Banner != "" {
		b.WriteString("!! " + v.Banner + " !!\n\n")
	}
	b.WriteString(v.Headline + "\n\n")
	labelWidth := 0
	for _, row := range v.Rows {
		labelWidth = max(labelWidth, len([]rune(row.Label)))
	}
	columns := func(label, baseline, winner, change, rest string) string {
		return fmt.Sprintf("%s  %-14s %-14s %-19s %s", pad(label, labelWidth), baseline, winner, change, rest)
	}
	b.WriteString(strings.TrimRight(columns("", "Current setup", winnerColumn(v.Kind), "Change", ""), " ") + "\n")
	for _, row := range v.Rows {
		rest := strings.TrimSpace(row.Decision + "  " + row.Noise)
		b.WriteString(strings.TrimRight(columns(row.Label, row.Baseline, row.Winner, row.Change, rest), " ") + "\n")
		if row.Reason != "" && row.Tone == "unknown" {
			b.WriteString(pad("", labelWidth) + "  └ " + row.Reason + "\n")
		}
	}
	b.WriteString("\nHow it was measured\n")
	for _, line := range v.Measurement {
		b.WriteString("  " + line + "\n")
	}
	if len(v.Candidates) > 0 {
		b.WriteString("\nEvery setup measured\n")
		for _, candidate := range v.Candidates {
			heading := candidate.Label
			if candidate.Role != "" {
				heading += " [" + candidate.Role + "]"
			}
			b.WriteString("  " + heading + " · " + candidate.Status + "\n")
			b.WriteString("    " + candidate.Measured + "\n")
			if candidate.Versus != "" {
				b.WriteString("    " + candidate.Versus + "\n")
			}
		}
	}
	if len(v.Signals) > 0 {
		b.WriteString("\nBottleneck signals\n  " + v.SignalsNote + "\n")
		for _, signal := range v.Signals {
			b.WriteString(fmt.Sprintf("  %-44s %-5s current %-10s winner %s\n", signal.Name, signal.Stat, signal.Baseline, signal.Winner))
		}
	}
	for _, block := range []struct {
		title  string
		fields []ViewField
	}{{v.WinnerTitle, v.Winner}, {v.BaselineTitle, v.Baseline}} {
		if len(block.fields) == 0 {
			continue
		}
		b.WriteString("\n" + block.title + "\n")
		for _, field := range block.fields {
			b.WriteString(fmt.Sprintf("  %-9s %s\n", field.Name, field.Value))
		}
	}
	b.WriteString("\nEvidence\n")
	for _, field := range v.Evidence {
		b.WriteString(fmt.Sprintf("  %-18s %s\n", field.Name, field.Value))
	}
	if len(v.Unknown) > 0 {
		b.WriteString("\nUnknown, never estimated\n")
		for _, field := range v.Unknown {
			b.WriteString("  " + field.Value + "\n")
		}
	}
	return b.String()
}

func winnerColumn(kind string) string {
	if kind == OutcomeBaselineKept {
		return "Kept"
	}
	return "Winning setup"
}

func pad(text string, width int) string {
	if gap := width - len([]rune(text)); gap > 0 {
		return text + strings.Repeat(" ", gap)
	}
	return text
}
