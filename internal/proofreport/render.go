package proofreport

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/bandverdict"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/releaseguard"
	"github.com/infercrane/infercrane/internal/technique"
)

// View is the report laid out in words, the one source of every rendering:
// Markdown, HTML and the terminal print the same strings. It formats numbers
// and nothing else; every "unknown" is the report's own.
type View struct {
	Title     string
	Subtitle  string
	Headline  string
	Facts     []Pair
	ClaimRule string
	Workloads []WorkloadView
	Limits    []string
	Unknown   []string
	Digest    string
	Signature string
}

// Pair is one labelled value.
type Pair struct {
	Name  string
	Value string
}

// WorkloadView is one workload in words.
type WorkloadView struct {
	Heading  string
	Summary  string
	Traffic  string
	Saving   []Pair
	Loads    []LoadView
	LoadNote string
	Runs     []RunView
}

// LoadView is one load's decision in words.
type LoadView struct {
	Load     string
	Decision string
	Setup    string
	Reason   string
}

// RunView is one run in words.
type RunView struct {
	Heading   string
	Headline  string
	Setups    []SetupRow
	Context   []Pair
	Numbers   []NumberRow
	Targets   []TargetRow
	Quality   string
	Claim     string
	ClaimTone string
	// Disclosures travel with the claim, word for word.
	Disclosures []string
	Saving      string
	Tone        string
	Reasons     []string
	Evidence    []Pair
	Rerun       []string
	Warnings    []string
}

// SetupRow is one property of the setup before and after.
type SetupRow struct {
	Name   string
	Before string
	After  string
}

// NumberRow is one outcome before and after.
type NumberRow struct {
	Label    string
	Before   string
	After    string
	Change   string
	Noise    string
	Decision string
	Tone     string
	Reason   string
}

// TargetRow is one of the customer's limits.
type TargetRow struct {
	Limit  string
	Before string
	After  string
}

// NewView lays out a document.
func NewView(d Document) View {
	r := d.Report
	v := View{Title: r.Title, Headline: r.Summary.Headline, ClaimRule: r.ClaimRule, Limits: r.Limits, Unknown: r.Unknown, Digest: r.ReportDigest, Signature: signatureLine(d)}
	parts := []string{}
	if r.PreparedFor != "" {
		parts = append(parts, "Prepared for "+r.PreparedFor)
	}
	switch {
	case r.Period.From != "" && r.Period.From != r.Period.To:
		parts = append(parts, "runs measured "+r.Period.From+" to "+r.Period.To)
	case r.Period.From != "":
		parts = append(parts, "runs measured "+r.Period.From)
	}
	parts = append(parts, "InferCrane Proof Report")
	v.Subtitle = strings.Join(parts, " · ")
	s := r.Summary
	v.Facts = append(v.Facts, Pair{"Workloads", strconv.Itoa(s.Workloads)}, Pair{"Saving verified", fmt.Sprintf("%d of %d workloads", s.VerifiedWorkloads, s.Workloads)})
	if s.MonthlySavingUSD != nil {
		v.Facts = append(v.Facts, Pair{"Verified saving a month", money(*s.MonthlySavingUSD) + " (" + s.SavingCovers + ")"})
	} else {
		v.Facts = append(v.Facts, Pair{"Verified saving a month", "unknown: no workload has a verified saving with a recorded price and your traffic volume"})
	}
	if s.AnnualSavingUSD != nil {
		v.Facts = append(v.Facts, Pair{"A year at that rate (projection)", money(*s.AnnualSavingUSD)})
	}
	if s.OutputPerGPUChange != nil {
		v.Facts = append(v.Facts, Pair{"Output per GPU, verified workloads", "at least " + percent(*s.OutputPerGPUChange)})
	}
	for index, workload := range r.Workloads {
		v.Workloads = append(v.Workloads, workloadView(index+1, workload))
	}
	return v
}

func workloadView(number int, w Workload) WorkloadView {
	out := WorkloadView{Heading: fmt.Sprintf("Workload %d: %s", number, w.Name), Summary: w.Summary, Traffic: w.Traffic, LoadNote: w.LoadsReason}
	if w.MonthlySavingUSD != nil {
		out.Saving = append(out.Saving, Pair{"Verified saving a month", money(*w.MonthlySavingUSD) + ", the smallest across its loads"})
	}
	if w.OutputPerGPUChange != nil {
		out.Saving = append(out.Saving, Pair{"Output per GPU", "at least " + percent(*w.OutputPerGPUChange)})
	}
	if w.CostPerMillionChange != nil {
		out.Saving = append(out.Saving, Pair{"Cost per 1M output tokens", "at least " + fewer(*w.CostPerMillionChange, "lower")})
	}
	if w.GPUHoursChange != nil {
		out.Saving = append(out.Saving, Pair{"GPU hours for the same traffic", "at least " + fewer(*w.GPUHoursChange, "fewer")})
	}
	if w.Loads != nil {
		for _, band := range w.Loads.Bands {
			out.Loads = append(out.Loads, LoadView{Load: fmt.Sprintf("%d at once", band.Load), Decision: decisionWords(band.Decision), Setup: orDash(band.Winner), Reason: band.Reason})
		}
		if words := summaryWords[w.Loads.Summary]; words != "" {
			out.LoadNote = words
		}
	}
	for _, run := range w.Runs {
		out.Runs = append(out.Runs, runView(run))
	}
	return out
}

func decisionWords(decision string) string {
	switch decision {
	case bandverdict.DecisionWinner:
		return "Winner"
	case bandverdict.DecisionKeep:
		return "Keep current"
	case bandverdict.DecisionNoDecision:
		return "No decision"
	}
	return "Unknown"
}

// summaryWords says the band study's summary in words.
var summaryWords = map[string]string{
	bandverdict.SummaryOneSetup:   "One setup wins at every load measured.",
	bandverdict.SummaryPerBand:    "Different setups win at different loads. One deployment serves one setup unless a per-server switch serves the table below.",
	bandverdict.SummaryIncomplete: "At least one load has no verdict; its row says why.",
}

func runView(run Run) RunView {
	load := "an unrecorded load"
	if run.Load != nil {
		load = fmt.Sprintf("%d requests at once", *run.Load)
	}
	out := RunView{Heading: "At " + load + " · run " + run.CampaignID, Headline: run.Headline, Warnings: run.Warnings, Rerun: run.Rerun, Reasons: run.Saving.Reasons}
	out.Setups = setupRows(run.Before, run.After)
	source := sources[run.Workload.Source]
	if source == "" {
		source = strings.ReplaceAll(orUnknown(run.Workload.Source), "_", " ")
	}
	traffic := []string{source}
	if run.Workload.Profile != "" {
		traffic = append(traffic, "profile "+run.Workload.Profile)
	}
	if run.Workload.RequestsPerRun != nil {
		traffic = append(traffic, fmt.Sprintf("%d requests per run", *run.Workload.RequestsPerRun))
	}
	if run.Workload.Method != "" {
		traffic = append(traffic, "method "+run.Workload.Method)
	}
	out.Context = append(out.Context, Pair{"Workload", strings.Join(traffic, ", ")})
	if run.Workload.ShapeDigest != "" {
		out.Context = append(out.Context, Pair{"Traffic shape", run.Workload.ShapeDigest})
	}
	out.Context = append(out.Context, Pair{"Baseline", baselineWords(run.Baseline)})
	for _, number := range run.Numbers {
		out.Numbers = append(out.Numbers, numberRow(number))
	}
	for _, target := range run.Targets {
		out.Targets = append(out.Targets, TargetRow{Limit: targetWords(target), Before: metWords(target, target.Before, target.BeforeMet), After: metWords(target, target.After, target.AfterMet)})
	}
	out.Quality = qualityWords(run.Quality)
	out.Claim, out.ClaimTone = claimWords(run.Claim), shownTone(run.Claim.Claim)
	out.Disclosures = append([]string{}, run.Claim.Disclosures...)
	out.Saving, out.Tone = savingWords(run.Saving)
	out.Evidence = append(out.Evidence, Pair{"This run's signed report", run.Evidence.ReportDigest + " at " + run.Evidence.ReportPath}, Pair{"Check it", run.Evidence.Verify})
	for _, record := range run.Evidence.Records {
		name := strings.ReplaceAll(record.Kind, "_", " ")
		value := record.Digest
		if record.ID != "" {
			value = record.ID + " · " + value
		}
		if record.Subject != "" {
			value += " · " + record.Subject
		}
		out.Evidence = append(out.Evidence, Pair{name, value})
	}
	return out
}

func setupRows(before, after *Setup) []SetupRow {
	if before == nil && after == nil {
		return nil
	}
	pick := func(setup *Setup, read func(*Setup) string) string {
		if setup == nil {
			return "—"
		}
		return orUnknown(read(setup))
	}
	rows := []struct {
		name string
		read func(*Setup) string
	}{
		{"Setup", func(s *Setup) string { return s.Label }},
		{"Model", func(s *Setup) string { return s.Model }},
		{"GPU", func(s *Setup) string {
			count := "unknown count"
			if s.AcceleratorCount != nil {
				count = strconv.Itoa(*s.AcceleratorCount)
			}
			return orUnknown(s.Accelerator) + " × " + count
		}},
		{"Engine", func(s *Setup) string { return strings.TrimSpace(s.Engine + " " + s.EngineVersion) }},
		{"Image", func(s *Setup) string { return s.ImageDigest }},
		{"Flags", func(s *Setup) string {
			if len(s.Args) == 0 {
				return "engine defaults"
			}
			return strings.Join(s.Args, " ")
		}},
		{"Your hourly price", func(s *Setup) string {
			if s.HourlyUSD == nil {
				return "unknown: none recorded"
			}
			return fmt.Sprintf("$%.2f per replica, from %s", *s.HourlyUSD, orUnknown(s.PriceSource))
		}},
	}
	out := make([]SetupRow, 0, len(rows))
	for _, row := range rows {
		afterValue := pick(after, row.read)
		if after == nil {
			afterValue = "unchanged"
		}
		out = append(out, SetupRow{Name: row.name, Before: pick(before, row.read), After: afterValue})
	}
	return out
}

func baselineWords(b Baseline) string {
	if b.Basis != optimizationreport.BasisInterleaved {
		text := orUnknown(b.Label) + ", not measured side by side"
		if b.Reason != "" {
			text += ": " + b.Reason
		}
		return text
	}
	text := fmt.Sprintf("%s, measured side by side on the same traffic: %d runs of it, %d of the new setup", orUnknown(b.Label), b.ControlRuns, b.CandidateRuns)
	if b.NoiseFloorPercent != nil {
		text += fmt.Sprintf("; measured noise ±%.1f%%", *b.NoiseFloorPercent)
	}
	if b.FreshServers {
		return text + "; measured again on fresh servers"
	}
	return text + "; not measured again on fresh servers"
}

func numberRow(n Number) NumberRow {
	row := NumberRow{Label: n.Label, Before: amount(n.Before, n.Unit), After: amount(n.After, n.Unit), Change: "—", Noise: "—", Decision: strings.ReplaceAll(n.Decision, "_", " "), Tone: "unknown", Reason: n.Reason}
	if n.ChangePercent != nil {
		row.Change = percent(*n.ChangePercent)
	}
	if n.NoiseFloorPercent != nil {
		row.Noise = fmt.Sprintf("±%.1f%%", *n.NoiseFloorPercent)
	}
	switch n.Decision {
	case optimizationreport.DecisionImproved:
		row.Tone = "good"
	case optimizationreport.DecisionRegressed:
		row.Tone = "bad"
	case optimizationreport.DecisionNoDifference, optimizationreport.DecisionUnchanged:
		row.Tone = "neutral"
	}
	return row
}

func targetWords(t Target) string {
	sign := "≤"
	if !t.AtMost {
		sign = "≥"
	}
	return t.Label + " " + sign + " " + amount(&t.Limit, t.Unit)
}

func metWords(t Target, value *float64, met *bool) string {
	if met == nil {
		if value == nil {
			return "unknown"
		}
		return amount(value, t.Unit)
	}
	if *met {
		return amount(value, t.Unit) + " · met"
	}
	return amount(value, t.Unit) + " · missed"
}

func qualityWords(q Quality) string {
	verdict := map[string]string{releaseguard.QualityPass: "Pass", releaseguard.QualityWatch: "Watch", releaseguard.QualityBlock: "Block"}[q.Verdict]
	if verdict == "" {
		verdict = "Unknown"
	}
	parts := []string{verdict}
	if q.BeforeScore != nil && q.AfterScore != nil {
		score := fmt.Sprintf("score %s → %s", score(*q.BeforeScore), score(*q.AfterScore))
		if q.Delta != nil {
			score += " (" + points(*q.Delta) + ")"
		}
		parts = append(parts, score)
	} else if q.AfterScore != nil {
		parts = append(parts, "score "+score(*q.AfterScore)+", your current setup's is unknown")
	}
	if q.Eval != "" {
		on := "on " + q.Eval
		if q.Prompts != nil {
			on += " (" + grouped(float64(*q.Prompts)) + " prompts)"
		}
		parts = append(parts, on)
	}
	if q.Method != "" {
		parts = append(parts, "checked by "+q.Method)
	}
	if q.AnswerRule != "" {
		rule := "rule " + strings.ReplaceAll(q.AnswerRule, "_", " ")
		if q.Servers != nil {
			rule += fmt.Sprintf(" on %d servers each", *q.Servers)
		}
		parts = append(parts, rule)
	}
	text := strings.Join(parts, " · ")
	if q.Reason != "" {
		text += ". " + upperFirst(q.Reason)
	}
	return text + "."
}

func claimWords(c Claim) string {
	text := "Claim " + c.Claim
	if c.Technique != "" {
		text += " · technique " + c.Technique
	}
	if c.Change != nil && c.Noise != nil && c.Metric != "" {
		text += fmt.Sprintf(" · %s %s against ±%.1f%% noise, the smaller of both windows", strings.ReplaceAll(c.Metric, "_", " "), percent(*c.Change), *c.Noise)
	}
	if len(c.Missing) > 0 {
		text += " · lacks " + strings.Join(c.Missing, ", ")
	}
	return text + "."
}

func savingWords(s Saving) (string, string) {
	switch s.State {
	case SavingInProgress:
		return "Still measuring.", "unknown"
	case SavingNone:
		return "No saving at this load.", "neutral"
	case SavingNotShown:
		return "Saving not shown.", "watch"
	}
	parts := []string{}
	if s.OutputPerGPUChange != nil {
		text := "output per GPU " + percent(*s.OutputPerGPUChange)
		if s.GPUHoursChange != nil {
			text += ", so the same traffic needs " + fewer(*s.GPUHoursChange, "fewer") + " GPU hours"
		}
		parts = append(parts, text)
	}
	if s.CostPerMillionChange != nil {
		parts = append(parts, "cost per 1M output tokens "+fewer(*s.CostPerMillionChange, "lower"))
	}
	if s.MonthlySavingUSD != nil && s.MonthlyCostBeforeUSD != nil && s.MonthlyCostAfterUSD != nil {
		parts = append(parts, fmt.Sprintf("%s a month at your captured traffic (%s → %s)", money(*s.MonthlySavingUSD), money(*s.MonthlyCostBeforeUSD), money(*s.MonthlyCostAfterUSD)))
	}
	if s.AnnualSavingUSD != nil {
		parts = append(parts, money(*s.AnnualSavingUSD)+" a year at that rate (projection)")
	}
	if s.MonthlyGPUHoursBefore != nil && s.MonthlyGPUHoursAfter != nil {
		parts = append(parts, fmt.Sprintf("%s → %s GPU hours a month", grouped(*s.MonthlyGPUHoursBefore), grouped(*s.MonthlyGPUHoursAfter)))
	}
	text := "Saving verified: " + strings.Join(parts, "; ") + "."
	if s.MonthlyUnavailableReason != "" {
		text += " Monthly amount unknown: " + s.MonthlyUnavailableReason + "."
	}
	if s.Assumption != "" {
		text += " Basis: " + s.Assumption + "."
	}
	return text, "good"
}

// amount formats a value in its unit; nil is "unknown".
func amount(value *float64, unit string) string {
	if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return "unknown"
	}
	v := *value
	switch unit {
	case "usd":
		return fmt.Sprintf("$%.4g", v)
	case "ms":
		return grouped(v) + " ms"
	case "tokens_per_second":
		return grouped(v) + " tokens/s"
	case "requests_per_second":
		return fmt.Sprintf("%.3g requests/s", v)
	case "ratio":
		return fmt.Sprintf("%.2f%%", v*100)
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}

// grouped is a whole number with thousands separators, or one decimal below 100.
func grouped(v float64) string {
	if math.Abs(v) < 100 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	whole := strconv.FormatInt(int64(math.Round(math.Abs(v))), 10)
	var out []byte
	for index := range whole {
		if index > 0 && (len(whole)-index)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, whole[index])
	}
	if v < 0 {
		return "-" + string(out)
	}
	return string(out)
}

func money(v float64) string {
	if math.Abs(v) < 100 {
		return fmt.Sprintf("$%.2f", v)
	}
	if v < 0 {
		return "-$" + grouped(-v)
	}
	return "$" + grouped(v)
}

func percent(v float64) string {
	return fmt.Sprintf("%+.1f%%", v)
}

// fewer says a negative change as a smaller amount: -23.1 is "23.1% fewer".
func fewer(v float64, word string) string {
	if v > 0 {
		return percent(v)
	}
	return fmt.Sprintf("%.1f%% %s", -v, word)
}

// sources names where the traffic came from.
var sources = map[string]string{"customer_replay": "your traffic, replayed", "synthetic_profile": "a synthetic profile, not your traffic"}

// score shows a 0 to 1 score as a percentage; anything else as it is.
func score(v float64) string {
	if v >= 0 && v <= 1 {
		return fmt.Sprintf("%.1f%%", v*100)
	}
	return strconv.FormatFloat(v, 'g', 4, 64)
}

func points(delta float64) string {
	if math.Abs(delta) <= 1 {
		return fmt.Sprintf("%+.1f points", delta*100)
	}
	return fmt.Sprintf("%+.3g", delta)
}

func orUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func upperFirst(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

// shownTone maps the claim to the tone renderings use.
func shownTone(claim string) string {
	if claim == technique.ClaimShown {
		return "good"
	}
	return "watch"
}
