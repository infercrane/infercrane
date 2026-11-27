// Package proofreport assembles the Proof Report: the document a design
// partner's CTO reads at the end of a proof engagement. It puts the runs of
// the engagement side by side, one workload at a time and one load at a
// time, before and after: output per GPU, latency against the customer's own
// limits, cost per million output tokens where the price was recorded, the
// answer-quality verdict, and the saving, counted only when it is verified.
//
// It reads finished runs only through their campaign reports
// (internal/optimizationreport) and judges every chosen setup with the one
// shared claim rule (internal/technique): a gain is "shown" only when a fresh
// machine measured it again, its answers held (within 1 point for a setup
// that changes answers), and every field a reader needs to rerun it is
// present. Nothing is estimated: a missing input stays unknown and says why.
// The report holds no prompts, no answers, and no InferCrane price.
package proofreport

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/infercrane/infercrane/internal/bandverdict"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
	"github.com/infercrane/infercrane/internal/technique"
)

const (
	SchemaVersion    = "infercrane.dev/proof-report/v1"
	AlgorithmVersion = "proof-report-v1"

	// MaxRuns bounds one report: a proof engagement measures a few workloads
	// at a few loads each.
	MaxRuns = 16
	// MaxTextLength bounds the title and the name it is prepared for.
	MaxTextLength = 120

	// MonthsPerYear turns a verified monthly saving into a year at the same
	// captured rate. The report names it a projection.
	MonthsPerYear = 12
)

// Saving states. A saving is "verified" only when every check passed;
// "not_shown" means a setup was chosen but the saving is not proven;
// "none" means nothing changes (the current setup was kept, no setup
// qualified, or the gain is not in cost or capacity); "in_progress" means
// the run has not finished.
const (
	SavingVerified   = "verified"
	SavingNotShown   = "not_shown"
	SavingNone       = "none"
	SavingInProgress = "in_progress"
)

// Workload verdicts across its loads.
const (
	WorkloadVerified   = "verified_at_every_load"
	WorkloadPartial    = "verified_at_some_loads"
	WorkloadNotShown   = "not_shown"
	WorkloadNoSaving   = "no_saving"
	WorkloadInProgress = "in_progress"
)

// ErrInvalidRequest is a request this package refuses before reading any
// record: no runs, too many, a repeated run, or text it does not accept.
var ErrInvalidRequest = errors.New("invalid proof report request")

// ClaimRule is the shared rule, in the words every rendering prints.
const ClaimRule = "A gain counts only when it cleared the measured noise against your current setup on the same servers, a fresh machine measured it again, and the answers held: a setup that can change answers must hold within 1 point of your current setup's task score. Anything less is \"not shown\"."

// Report is the Proof Report. Every pointer is null when its input is
// missing; Unknown says what is missing and why.
type Report struct {
	SchemaVersion    string     `json:"schema_version"`
	AlgorithmVersion string     `json:"algorithm_version"`
	Title            string     `json:"title"`
	PreparedFor      string     `json:"prepared_for,omitempty"`
	Period           Period     `json:"period"`
	Finished         bool       `json:"finished"`
	Summary          Summary    `json:"summary"`
	ClaimRule        string     `json:"claim_rule"`
	Workloads        []Workload `json:"workloads"`
	Limits           []string   `json:"limits"`
	Unknown          []string   `json:"unknown"`
	// ReportDigest must stay the last field: Verify recomputes it from the
	// report's JSON bytes, where it is the last member.
	ReportDigest string `json:"report_digest"`
}

// Period is when the runs were measured: the first run's creation and the
// last change of any run, both read from the run records.
type Period struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Summary is the engagement's result in one place. MonthlySavingUSD adds the
// verified monthly saving of every workload that has one; Covers says how
// many workloads that is. AnnualSavingUSD is twelve such months.
type Summary struct {
	Headline           string   `json:"headline"`
	Workloads          int      `json:"workloads"`
	VerifiedWorkloads  int      `json:"verified_workloads"`
	MonthlySavingUSD   *float64 `json:"monthly_saving_usd"`
	AnnualSavingUSD    *float64 `json:"annual_saving_usd"`
	SavingCovers       string   `json:"saving_covers,omitempty"`
	SavingAssumption   string   `json:"saving_assumption,omitempty"`
	OutputPerGPUChange *float64 `json:"output_per_gpu_change_percent"`
}

// Workload is one model on one kind of traffic, measured at one or more
// loads. Its saving is the smallest verified saving across its loads, and
// only when every load verified one: the traffic will meet every load.
type Workload struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Traffic string `json:"traffic"`
	Verdict string `json:"verdict"`
	Summary string `json:"summary"`
	Runs    []Run  `json:"runs"`
	// Loads is the verdict per load from the band study when the workload
	// was measured at two or more loads; LoadsReason says why it is absent.
	Loads       *LoadVerdict `json:"loads,omitempty"`
	LoadsReason string       `json:"loads_reason,omitempty"`

	MonthlySavingUSD     *float64 `json:"monthly_saving_usd"`
	OutputPerGPUChange   *float64 `json:"output_per_gpu_change_percent"`
	CostPerMillionChange *float64 `json:"cost_per_million_change_percent"`
	GPUHoursChange       *float64 `json:"gpu_hours_change_percent"`
}

// LoadVerdict is the judge's verdict across the workload's loads.
type LoadVerdict struct {
	Metric  string     `json:"metric"`
	Summary string     `json:"summary"`
	Reason  string     `json:"reason"`
	Bands   []LoadBand `json:"bands"`
}

// LoadBand is one load's decision.
type LoadBand struct {
	Load     int    `json:"load"`
	Decision string `json:"decision"`
	Winner   string `json:"winner,omitempty"`
	Reason   string `json:"reason"`
}

// Run is one optimization run of the engagement: one workload at one load.
// Every number in it was measured on Before and After, on the workload and
// against the baseline it names, and Rerun measures it again.
type Run struct {
	CampaignID      string   `json:"campaign_id"`
	Load            *int     `json:"load"`
	Finished        bool     `json:"finished"`
	Outcome         string   `json:"outcome"`
	Headline        string   `json:"headline"`
	Objective       string   `json:"objective"`
	ObjectiveMetric string   `json:"objective_metric,omitempty"`
	Simulated       bool     `json:"simulated_accelerator"`
	Before          *Setup   `json:"before"`
	After           *Setup   `json:"after"`
	Workload        Traffic  `json:"workload"`
	Baseline        Baseline `json:"baseline"`
	Numbers         []Number `json:"numbers"`
	Targets         []Target `json:"targets"`
	Quality         Quality  `json:"quality"`
	Claim           Claim    `json:"claim"`
	Saving          Saving   `json:"saving"`
	Evidence        Evidence `json:"evidence"`
	Rerun           []string `json:"rerun"`
	Warnings        []string `json:"warnings"`
}

// Setup is one exact serving setup: hardware and software.
type Setup struct {
	Label            string            `json:"label"`
	Model            string            `json:"model"`
	Engine           string            `json:"engine"`
	EngineVersion    string            `json:"engine_version"`
	ImageDigest      string            `json:"image_digest,omitempty"`
	Args             []string          `json:"args"`
	Env              map[string]string `json:"env,omitempty"`
	Accelerator      string            `json:"accelerator"`
	AcceleratorCount *int              `json:"accelerator_count"`
	Provider         string            `json:"provider,omitempty"`
	Region           string            `json:"region,omitempty"`
	// HourlyUSD is the hourly price of one replica as recorded with the
	// measurement, and PriceSource where it came from: the customer's own
	// compute price, never an InferCrane price.
	HourlyUSD   *float64 `json:"hourly_usd_per_replica"`
	PriceSource string   `json:"price_source,omitempty"`
}

// Traffic is the content-free traffic every setup received.
type Traffic struct {
	Source         string `json:"source"`
	Profile        string `json:"profile,omitempty"`
	ShapeDigest    string `json:"shape_digest,omitempty"`
	RequestsPerRun *int   `json:"requests_per_run"`
	Method         string `json:"method,omitempty"`
}

// Baseline is what After was compared with and how: interleaved runs on the
// same traffic, with the measured noise and the fresh-server window.
type Baseline struct {
	Label             string   `json:"label"`
	Basis             string   `json:"basis"`
	ControlRuns       int      `json:"control_runs"`
	CandidateRuns     int      `json:"candidate_runs"`
	NoiseFloorPercent *float64 `json:"noise_floor_percent"`
	FreshServers      bool     `json:"fresh_servers"`
	Reason            string   `json:"reason,omitempty"`
}

// Number is one outcome before and after. ChangePercent is After against
// Before; Decision is judged against the measured noise floor, and is
// "unknown" when the two were not measured side by side.
type Number struct {
	Key               string   `json:"key"`
	Label             string   `json:"label"`
	Unit              string   `json:"unit"`
	HigherIsBetter    bool     `json:"higher_is_better"`
	Before            *float64 `json:"before"`
	After             *float64 `json:"after"`
	ChangePercent     *float64 `json:"change_percent"`
	NoiseFloorPercent *float64 `json:"noise_floor_percent"`
	Decision          string   `json:"decision"`
	Basis             string   `json:"basis"`
	Reason            string   `json:"reason,omitempty"`
}

// Target is one of the customer's own limits, read from the run's request,
// and whether each setup met it in its own measurement. Met is null when the
// setup's value is unknown.
type Target struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Unit        string   `json:"unit"`
	Limit       float64  `json:"limit"`
	AtMost      bool     `json:"at_most"`
	Before      *float64 `json:"before"`
	After       *float64 `json:"after"`
	BeforeMet   *bool    `json:"before_met"`
	AfterMet    *bool    `json:"after_met"`
	Explanation string   `json:"explanation,omitempty"`
}

// Quality is the answer-quality verdict of After against Before.
type Quality struct {
	Verdict       string   `json:"verdict"`
	Basis         string   `json:"basis"`
	Method        string   `json:"method,omitempty"`
	Eval          string   `json:"eval,omitempty"`
	Prompts       *int     `json:"prompts"`
	BeforeScore   *float64 `json:"before_score"`
	AfterScore    *float64 `json:"after_score"`
	Delta         *float64 `json:"delta"`
	AnswerRule    string   `json:"answer_rule,omitempty"`
	AnswerVerdict string   `json:"answer_verdict,omitempty"`
	Servers       *int     `json:"servers_each"`
	Reason        string   `json:"reason,omitempty"`
}

// Claim is the chosen setup's claim under the shared rule (ClaimRule), the
// technique that made it when one did, and what a "shown" claim still lacks.
type Claim struct {
	Claim     string   `json:"claim"`
	Technique string   `json:"technique,omitempty"`
	Metric    string   `json:"metric,omitempty"`
	Change    *float64 `json:"change_percent"`
	Noise     *float64 `json:"noise_floor_percent"`
	Missing   []string `json:"missing"`
}

// Saving is what the change saves at this load, stated only when verified.
// Reasons says why a chosen setup's saving is not verified.
type Saving struct {
	State                    string   `json:"state"`
	Reasons                  []string `json:"reasons"`
	OutputPerGPUChange       *float64 `json:"output_per_gpu_change_percent"`
	CostPerMillionChange     *float64 `json:"cost_per_million_change_percent"`
	GPUHoursChange           *float64 `json:"gpu_hours_change_percent"`
	MonthlyOutputTokens      *float64 `json:"monthly_output_tokens"`
	MonthlyGPUHoursBefore    *float64 `json:"monthly_gpu_hours_before"`
	MonthlyGPUHoursAfter     *float64 `json:"monthly_gpu_hours_after"`
	MonthlyCostBeforeUSD     *float64 `json:"monthly_cost_before_usd"`
	MonthlyCostAfterUSD      *float64 `json:"monthly_cost_after_usd"`
	MonthlySavingUSD         *float64 `json:"monthly_saving_usd"`
	AnnualSavingUSD          *float64 `json:"annual_saving_usd"`
	Assumption               string   `json:"assumption,omitempty"`
	MonthlyUnavailableReason string   `json:"monthly_unavailable_reason,omitempty"`
}

// Evidence links a run to its records: the run's own signed report, by
// digest, where to read it, and the records it rests on.
type Evidence struct {
	ReportDigest string                              `json:"report_digest"`
	ReportPath   string                              `json:"report_path"`
	Verify       string                              `json:"verify"`
	Records      []optimizationreport.EvidenceDigest `json:"records"`
}

// RunEvidence is one run as Assemble reads it.
type RunEvidence struct {
	Campaign domain.OptimizationCampaign
	Report   optimizationreport.Report
	// Result and Claim are the chosen setup's measurement under the shared
	// rule (optimizationreport.SetupResult); Result is nil when no setup was
	// chosen or it has no measurement.
	Result *technique.Result
	Claim  string
	// Answers is the chosen setup's deciding quality record, when found.
	Answers *domain.QualityEvidence
}

// Input is everything Assemble reads.
type Input struct {
	Title       string
	PreparedFor string
	Runs        []RunEvidence
}

// CheckText accepts a title or a name: printable, one line, at most
// MaxTextLength characters.
func CheckText(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if len([]rune(value)) > MaxTextLength {
		return "", fmt.Errorf("%w: %s is longer than %d characters", ErrInvalidRequest, name, MaxTextLength)
	}
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return "", fmt.Errorf("%w: %s must be one line of printable text", ErrInvalidRequest, name)
		}
	}
	return value, nil
}

// Assemble builds the report. It does no I/O, and the same input always gives
// the same report and digest.
func Assemble(in Input) Report {
	report := Report{SchemaVersion: SchemaVersion, AlgorithmVersion: AlgorithmVersion, Title: in.Title, PreparedFor: in.PreparedFor, ClaimRule: ClaimRule, Finished: len(in.Runs) > 0, Workloads: []Workload{}, Limits: []string{}, Unknown: []string{}}
	if report.Title == "" {
		report.Title = "Proof Report"
	}
	groups := map[string]int{}
	var studies [][]RunEvidence
	for _, evidence := range in.Runs {
		key := workloadKey(evidence)
		index, found := groups[key]
		if !found {
			index = len(report.Workloads)
			groups[key] = index
			report.Workloads = append(report.Workloads, newWorkload(evidence))
			studies = append(studies, nil)
		}
		report.Workloads[index].Runs = append(report.Workloads[index].Runs, assembleRun(evidence))
		studies[index] = append(studies[index], evidence)
		report.Finished = report.Finished && evidence.Report.Finished
		report.Period = widen(report.Period, evidence.Campaign)
	}
	for index := range report.Workloads {
		workload := &report.Workloads[index]
		slices.SortStableFunc(workload.Runs, func(a, b Run) int { return cmp.Compare(loadOf(a), loadOf(b)) })
		loads(workload, studies[index])
		rollUp(workload)
	}
	report.Summary = summarize(report.Workloads)
	report.Limits = limits(report)
	report.Unknown = unknown(report)
	report.ReportDigest = digest(report)
	return report
}

func loadOf(run Run) int {
	if run.Load == nil {
		return 0
	}
	return *run.Load
}

// workloadKey groups runs of one model on one traffic: the model, the kind of
// traffic and its shape, and the GPU the current setup ran on.
func workloadKey(evidence RunEvidence) string {
	proposal := proposalOf(evidence.Campaign)
	gpu := ""
	if setup := evidence.Report.Appendix.Baseline; setup != nil {
		gpu = setup.Accelerator
	}
	return strings.Join([]string{evidence.Report.ModelIdentity, kindOf(evidence.Report.Workload), proposal.Input.WorkloadFingerprint, gpu}, "\x00")
}

func kindOf(workload optimizationreport.Workload) string {
	if workload.Profile != "" {
		return workload.Profile
	}
	return workload.Source
}

func proposalOf(campaign domain.OptimizationCampaign) optimizer.Proposal {
	var proposal optimizer.Proposal
	_ = json.Unmarshal([]byte(campaign.ProposalJSON), &proposal)
	return proposal
}

func newWorkload(evidence RunEvidence) Workload {
	report := evidence.Report
	proposal := proposalOf(evidence.Campaign)
	workload := Workload{Model: report.ModelIdentity, Runs: []Run{}}
	kind := kindOf(report.Workload)
	workload.Name = report.ModelIdentity
	if kind != "" && kind != optimizationreport.BasisUnknown {
		workload.Name += " · " + strings.ReplaceAll(kind, "_", " ")
	}
	switch {
	case proposal.Input.WorkloadSource == optimizer.WorkloadSourceCustomerObserved && strings.HasPrefix(proposal.Input.WorkloadFingerprint, "sha256:"):
		workload.Traffic = "your captured traffic, replayed by shape (lengths and order, no text): " + proposal.Input.WorkloadFingerprint
	case proposal.Input.WorkloadSource == optimizer.WorkloadSourceCustomerObserved:
		workload.Traffic = "your captured traffic, without a shape digest"
	case kind != "" && kind != optimizationreport.BasisUnknown:
		workload.Traffic = "the " + strings.ReplaceAll(kind, "_", " ") + " profile, not your own traffic"
	default:
		workload.Traffic = "unknown"
	}
	return workload
}

func widen(period Period, campaign domain.OptimizationCampaign) Period {
	if !campaign.CreatedAt.IsZero() {
		from := campaign.CreatedAt.UTC().Format("2006-01-02")
		if period.From == "" || from < period.From {
			period.From = from
		}
	}
	last := campaign.UpdatedAt
	if last.IsZero() {
		last = campaign.CreatedAt
	}
	if !last.IsZero() {
		to := last.UTC().Format("2006-01-02")
		if to > period.To {
			period.To = to
		}
	}
	return period
}

func assembleRun(evidence RunEvidence) Run {
	report := evidence.Report
	proposal := proposalOf(evidence.Campaign)
	run := Run{CampaignID: report.CampaignID, Load: report.Workload.Concurrency, Finished: report.Finished, Outcome: report.Outcome.Kind, Headline: report.Outcome.Headline,
		Objective: report.Objective, ObjectiveMetric: report.ObjectiveMetric, Simulated: report.SimulatedAccelerator, Warnings: append([]string{}, report.Warnings...)}
	if report.Banner != "" {
		run.Warnings = append([]string{report.Banner}, run.Warnings...)
	}
	run.Before = setupOf(report.Appendix.Baseline)
	if report.Outcome.Kind == optimizationreport.OutcomeWinnerSelected {
		run.After = setupOf(report.Appendix.Winner)
	}
	shape := ""
	if strings.HasPrefix(proposal.Input.WorkloadFingerprint, "sha256:") {
		shape = proposal.Input.WorkloadFingerprint
	}
	run.Workload = Traffic{Source: report.Workload.Source, Profile: report.Workload.Profile, ShapeDigest: shape, RequestsPerRun: report.Workload.RequestsPerRun, Method: report.Workload.Method}
	run.Baseline = baseline(report, evidence.Result)
	run.Numbers = numbers(report)
	run.Targets = targets(proposal, report)
	run.Quality = quality(report, evidence)
	run.Claim = claim(evidence)
	run.Saving = saving(report, run)
	run.Evidence = Evidence{ReportDigest: report.ReportDigest, ReportPath: "/api/v1/optimization/campaigns/" + report.CampaignID + "/report",
		Verify:  "infercrane optimize report " + report.CampaignID + " --output json > run.json && " + strings.Replace(optimizationreport.VerifyCommand, "FILE", "run.json", 1),
		Records: append([]optimizationreport.EvidenceDigest{}, report.Appendix.Evidence...)}
	run.Rerun = rerun(proposal)
	return run
}

func setupOf(setup *optimizationreport.Setup) *Setup {
	if setup == nil {
		return nil
	}
	out := &Setup{Label: setup.Label, Model: setup.Model, Engine: setup.Engine, EngineVersion: setup.EngineVersion, Args: append([]string{}, setup.Args...), Accelerator: setup.Accelerator,
		AcceleratorCount: setup.AcceleratorCount, Provider: setup.Provider, Region: setup.Region, HourlyUSD: setup.HourlyUSD, PriceSource: setup.PriceSource}
	if len(setup.Env) > 0 {
		out.Env = setup.Env
	}
	if _, digest, found := strings.Cut(setup.Image, "@"); found && strings.HasPrefix(digest, "sha256:") {
		out.ImageDigest = digest
	}
	return out
}

func baseline(report optimizationreport.Report, result *technique.Result) Baseline {
	noise := report.Noise
	out := Baseline{Label: report.Outcome.BaselineLabel, Basis: noise.Basis, ControlRuns: noise.ControlRuns, CandidateRuns: noise.CandidateRuns, NoiseFloorPercent: noise.ObjectiveNoiseFloorPercent, Reason: noise.Reason}
	out.FreshServers = result != nil && strings.Contains(result.Baseline.Basis, "fresh-servers")
	return out
}

// shown are the campaign report's outcomes the Proof Report carries, in its
// order and words: capacity first, then cost, then latency.
var shown = []struct{ key, label string }{
	{optimizationreport.MetricTokensPerAccel, "Output per GPU"},
	{optimizationreport.MetricUsefulOutput, "Output within your limits"},
	{optimizationreport.MetricCostPerMillion, "Cost per 1M output tokens"},
	{optimizationreport.MetricTTFTP95, "Time to first token, p95"},
	{optimizationreport.MetricTokensPerUser, "Output speed per user"},
}

func numbers(report optimizationreport.Report) []Number {
	out := []Number{}
	for _, want := range shown {
		for _, metric := range report.Metrics {
			if metric.Key != want.key {
				continue
			}
			label := want.label
			if report.SimulatedAccelerator && metric.Key == optimizationreport.MetricTokensPerAccel {
				label = "Output per simulated accelerator"
			}
			out = append(out, Number{Key: metric.Key, Label: label, Unit: metric.Unit, HigherIsBetter: metric.HigherIsBetter, Before: metric.Baseline, After: metric.Winner,
				ChangePercent: metric.ChangePercent, NoiseFloorPercent: metric.NoiseFloorPercent, Decision: metric.Decision, Basis: metric.Basis, Reason: metric.Reason})
		}
	}
	return out
}

func metricOf(report optimizationreport.Report, key string) *optimizationreport.Metric {
	for index := range report.Metrics {
		if report.Metrics[index].Key == key {
			return &report.Metrics[index]
		}
	}
	return nil
}

// targets reads the customer's own limits from the run's request and checks
// each setup's own measurement against them.
func targets(proposal optimizer.Proposal, report optimizationreport.Report) []Target {
	var before, after *optimizationreport.CandidateResult
	for index := range report.Candidates {
		candidate := &report.Candidates[index]
		if candidate.ID == report.Outcome.BaselineCandidateID {
			before = candidate
		}
		if report.Outcome.Kind == optimizationreport.OutcomeWinnerSelected && candidate.ID == report.Outcome.WinnerCandidateID {
			after = candidate
		}
	}
	input := proposal.Input
	out := []Target{}
	add := func(key, label, unit string, limit *float64, atMost bool, value func(*optimizationreport.CandidateResult) *float64) {
		if limit == nil {
			return
		}
		target := Target{Key: key, Label: label, Unit: unit, Limit: *limit, AtMost: atMost}
		if before != nil {
			target.Before = value(before)
			target.BeforeMet = meets(target.Before, *limit, atMost)
		}
		if after != nil {
			target.After = value(after)
			target.AfterMet = meets(target.After, *limit, atMost)
		}
		if target.Before == nil || (report.Outcome.Kind == optimizationreport.OutcomeWinnerSelected && target.After == nil) {
			target.Explanation = "a setup's own measurement did not record it"
		}
		out = append(out, target)
	}
	add("ttft_p95_ms", "Time to first token, p95", "ms", input.MaxTTFTP95MS, true, func(c *optimizationreport.CandidateResult) *float64 { return c.TTFTP95MS })
	add("tpot_p95_ms", "Time per output token, p95", "ms", input.MaxTPOTP95MS, true, func(c *optimizationreport.CandidateResult) *float64 { return c.TPOTP95MS })
	add("error_rate", "Failed requests", "ratio", input.MaxErrorRate, true, func(c *optimizationreport.CandidateResult) *float64 {
		if c.ErrorRatePercent == nil {
			return nil
		}
		return ptr(*c.ErrorRatePercent / 100)
	})
	add("output_tokens_per_second", "Output throughput", "tokens_per_second", input.MinOutputTokensSecond, false, func(c *optimizationreport.CandidateResult) *float64 { return c.OutputTokensPerSecond })
	return out
}

func meets(value *float64, limit float64, atMost bool) *bool {
	if value == nil {
		return nil
	}
	met := *value <= limit
	if !atMost {
		met = *value >= limit
	}
	return &met
}

func quality(report optimizationreport.Report, evidence RunEvidence) Quality {
	q := report.Quality
	out := Quality{Verdict: q.Verdict, Basis: q.Basis, Eval: q.Eval, BeforeScore: q.BaselineScore, AfterScore: q.WinnerScore, Delta: q.Delta, Reason: q.Reason}
	if evidence.Result != nil {
		out.Method = evidence.Result.Correctness.Method
	}
	if row := evidence.Answers; row != nil {
		if row.SampleCount > 0 {
			prompts := row.SampleCount
			out.Prompts = &prompts
		}
		var distribution qualityevidence.Distribution
		if row.DistributionJSON != "" && json.Unmarshal([]byte(row.DistributionJSON), &distribution) == nil {
			out.AnswerRule, out.AnswerVerdict = distribution.TaskRule, distribution.TaskVerdict
			if distribution.TaskServers > 0 {
				servers := distribution.TaskServers
				out.Servers = &servers
			}
		}
	}
	return out
}

// missingWords names each field a "shown" claim needs in a reader's words.
var missingWords = map[string]string{
	"evidence." + technique.EvidenceFreshMachineConfirmation: "a fresh machine's confirmation of the gain",
	"software.image_digest":                                  "the exact image digest",
	"reproduction.image_digest":                              "the exact image digest",
	"workload.shape_digest":                                  "a digest of your traffic's shape",
	"reproduction.traffic_shape":                             "a digest of your traffic's shape",
	"reproduction.commit":                                    "the build commit that measured it",
	"reproduction.command":                                   "the serving command",
	"correctness.method":                                     "an answer check",
	"correctness.verdict":                                    "answers shown to hold",
	"hardware.accelerator":                                   "the GPU",
	"hardware.count":                                         "the GPU count",
	"software.engine":                                        "the engine",
	"software.engine_version":                                "the engine version",
	"workload.kind":                                          "the kind of traffic",
	"workload.load":                                          "the load",
	"precision.weights":                                      "the weights' precision",
	"precision.kv_cache":                                     "the KV cache's precision",
	"baseline.setup":                                         "the baseline",
	"baseline.basis":                                         "how it was compared",
	"measured":                                               "a measurement",
}

func words(field string) string {
	if text, found := missingWords[field]; found {
		return text
	}
	if name, found := strings.CutPrefix(field, "evidence."); found {
		return "the technique's own evidence (" + name + ")"
	}
	return field
}

func claim(evidence RunEvidence) Claim {
	out := Claim{Claim: technique.ClaimNotShown, Missing: []string{}}
	result := evidence.Result
	if result == nil {
		if evidence.Report.Outcome.Kind == optimizationreport.OutcomeWinnerSelected {
			out.Missing = append(out.Missing, words("measured"))
		}
		return out
	}
	if evidence.Claim == technique.ClaimShown {
		out.Claim = technique.ClaimShown
	}
	if result.Technique != "" {
		out.Technique = result.Technique + "@" + result.TechniqueVersion
	}
	out.Metric = result.Measured.Metric
	if result.Measured.Decision != technique.DecisionUnknown {
		out.Change, out.Noise = ptr(result.Measured.Change*100), ptr(result.Measured.NoiseFloor*100)
	}
	seen := map[string]bool{}
	add := func(text string) {
		if !seen[text] {
			seen[text] = true
			out.Missing = append(out.Missing, text)
		}
	}
	for _, field := range result.Unknown() {
		add(words(field))
	}
	// The rule's other two conditions, so the list says everything a "not
	// shown" claim lacks, not only absent fields.
	if result.Correctness.Verdict != "passed" {
		add(words("correctness.verdict"))
	}
	// Without the fresh machine's window the change is unknown, which the
	// missing confirmation already says.
	if !seen[words("evidence."+technique.EvidenceFreshMachineConfirmation)] && (result.Measured.Decision != technique.DecisionImproved || result.Measured.NoiseFloor <= 0 || result.Measured.Change <= result.Measured.NoiseFloor) {
		add("a gain beyond the measured noise in both windows")
	}
	return out
}

// saving decides whether the run's saving is verified, and states it only
// then. Every reason it is not is listed, not only the first.
func saving(report optimizationreport.Report, run Run) Saving {
	out := Saving{State: SavingNotShown, Reasons: []string{}}
	switch {
	case !report.Finished:
		out.State = SavingInProgress
		out.Reasons = append(out.Reasons, "the run has not finished")
		return out
	case report.Outcome.Kind != optimizationreport.OutcomeWinnerSelected:
		out.State = SavingNone
		out.Reasons = append(out.Reasons, report.Outcome.Headline)
		return out
	}
	perGPU := metricOf(report, optimizationreport.MetricTokensPerAccel)
	cost := metricOf(report, optimizationreport.MetricCostPerMillion)
	capacity := perGPU != nil && perGPU.Decision == optimizationreport.DecisionImproved && perGPU.ChangePercent != nil
	cheaper := cost != nil && cost.Decision == optimizationreport.DecisionImproved && cost.ChangePercent != nil
	if !capacity && !cheaper {
		// A gain in latency alone serves the same traffic on the same GPUs.
		out.State = SavingNone
		out.Reasons = append(out.Reasons, "neither output per GPU nor cost per output token improved beyond the measured noise; the change is not a saving")
		return out
	}
	if report.SimulatedAccelerator {
		out.Reasons = append(out.Reasons, "it ran on CPUs or a simulated accelerator, which proves the pipeline, not GPU speed")
	}
	if run.Claim.Claim != technique.ClaimShown {
		reason := "its gain is not shown under the claim rule"
		if len(run.Claim.Missing) > 0 {
			reason += ": it lacks " + strings.Join(run.Claim.Missing, ", ")
		}
		out.Reasons = append(out.Reasons, reason)
	}
	if report.Quality.Verdict != releaseguard.QualityPass {
		verdict := strings.ToLower(report.Quality.Verdict)
		if verdict == "" {
			verdict = "unknown"
		}
		out.Reasons = append(out.Reasons, "the answer-quality verdict is "+verdict+", not pass")
	}
	for _, candidate := range report.Candidates {
		if candidate.ID == report.Outcome.WinnerCandidateID && len(candidate.Guards) > 0 {
			out.Reasons = append(out.Reasons, "it failed more requests or answered shorter than your current setup")
		}
	}
	if len(out.Reasons) > 0 {
		return out
	}
	out.State = SavingVerified
	if capacity {
		out.OutputPerGPUChange = perGPU.ChangePercent
		out.GPUHoursChange = ptr((1/(1+*perGPU.ChangePercent/100) - 1) * 100)
	}
	if cheaper {
		out.CostPerMillionChange = cost.ChangePercent
	}
	monthly := report.Monthly
	if monthly.Basis != optimizationreport.BasisReplayVolume || monthly.MonthlyOutputTokens == nil {
		out.MonthlyUnavailableReason = cmp.Or(monthly.Reason, "the run did not replay your traffic, so there is no volume to project")
		return out
	}
	out.MonthlyOutputTokens, out.Assumption = monthly.MonthlyOutputTokens, monthly.Assumption
	if capacity && finitePositive(perGPU.Baseline) && finitePositive(perGPU.Winner) {
		out.MonthlyGPUHoursBefore = ptr(*monthly.MonthlyOutputTokens / (*perGPU.Baseline * 3600))
		out.MonthlyGPUHoursAfter = ptr(*monthly.MonthlyOutputTokens / (*perGPU.Winner * 3600))
	}
	switch {
	case cheaper && monthly.BaselineUSD != nil && monthly.WinnerUSD != nil:
		out.MonthlyCostBeforeUSD, out.MonthlyCostAfterUSD = monthly.BaselineUSD, monthly.WinnerUSD
		out.MonthlySavingUSD = ptr(*monthly.BaselineUSD - *monthly.WinnerUSD)
		out.AnnualSavingUSD = ptr(*out.MonthlySavingUSD * MonthsPerYear)
	case !cheaper && cost != nil && cost.Reason != "":
		out.MonthlyUnavailableReason = "cost per output token is unknown: " + cost.Reason
	default:
		out.MonthlyUnavailableReason = cmp.Or(monthly.Reason, "cost per output token did not improve beyond the measured noise")
	}
	return out
}

// loads reads a workload measured at two or more loads as the judge's band
// study: what wins at each load.
func loads(workload *Workload, runs []RunEvidence) {
	if len(runs) < 2 {
		return
	}
	reports, campaigns := make([]optimizationreport.Report, 0, len(runs)), make([]domain.OptimizationCampaign, 0, len(runs))
	for _, run := range runs {
		reports, campaigns = append(reports, run.Report), append(campaigns, run.Campaign)
	}
	study, err := optimizationreport.BandStudyOf(reports, campaigns)
	if err != nil {
		workload.LoadsReason = "no verdict across loads: " + strings.TrimPrefix(err.Error(), optimizationreport.ErrStudyMismatch.Error()+": ")
		return
	}
	verdict := &LoadVerdict{Metric: study.Verdict.Metric, Summary: study.Verdict.Summary, Reason: study.Verdict.Reason, Bands: []LoadBand{}}
	for _, band := range study.Verdict.Bands {
		entry := LoadBand{Load: band.Load, Decision: band.Decision, Reason: band.Reason}
		if setup, found := study.Setups[band.Winner]; found {
			entry.Winner = setup.Label
		}
		// The judge chose the setup at this load; whether its saving counts is
		// this report's claim rule, said in the report's own words.
		for _, run := range workload.Runs {
			if run.Load == nil || *run.Load != band.Load || band.Decision != bandverdict.DecisionWinner {
				continue
			}
			switch run.Saving.State {
			case SavingVerified:
				entry.Reason = "The judge chose it at this load, and its saving is verified."
			case SavingNotShown:
				entry.Reason = "The judge chose it at this load, but its saving is not shown: " + strings.Join(run.Saving.Reasons, "; ") + "."
			case SavingNone:
				entry.Reason = "The judge chose it at this load, but it saves nothing: " + strings.Join(run.Saving.Reasons, "; ") + "."
			}
		}
		verdict.Bands = append(verdict.Bands, entry)
	}
	workload.Loads = verdict
}

// rollUp gives a workload its verdict and its saving: the smallest verified
// saving across its loads, and only when every load verified one.
func rollUp(workload *Workload) {
	verified, inProgress, chosen := 0, 0, 0
	var verifiedLoads, otherLoads []string
	for _, run := range workload.Runs {
		load := "an unrecorded load"
		if run.Load != nil {
			load = strconv.Itoa(*run.Load) + " at once"
		}
		switch run.Saving.State {
		case SavingVerified:
			verified++
			verifiedLoads = append(verifiedLoads, load)
		case SavingInProgress:
			inProgress++
			otherLoads = append(otherLoads, load)
		case SavingNotShown:
			chosen++
			otherLoads = append(otherLoads, load)
		default:
			otherLoads = append(otherLoads, load)
		}
	}
	switch {
	case inProgress > 0:
		workload.Verdict, workload.Summary = WorkloadInProgress, "Still measuring: "+strings.Join(otherLoads, ", ")+"."
	case verified == len(workload.Runs):
		workload.Verdict, workload.Summary = WorkloadVerified, "Saving verified at every load measured ("+strings.Join(verifiedLoads, ", ")+")."
		workload.MonthlySavingUSD = smallest(workload.Runs, func(s Saving) *float64 { return s.MonthlySavingUSD })
		workload.OutputPerGPUChange = smallest(workload.Runs, func(s Saving) *float64 { return s.OutputPerGPUChange })
		workload.CostPerMillionChange = largest(workload.Runs, func(s Saving) *float64 { return s.CostPerMillionChange })
		workload.GPUHoursChange = largest(workload.Runs, func(s Saving) *float64 { return s.GPUHoursChange })
	case verified > 0:
		workload.Verdict, workload.Summary = WorkloadPartial, "Saving verified at "+strings.Join(verifiedLoads, ", ")+"; not at "+strings.Join(otherLoads, ", ")+", so no saving is stated for the workload."
	case chosen > 0:
		workload.Verdict, workload.Summary = WorkloadNotShown, "A setup was chosen, but its saving is not shown: see each load's reasons."
	default:
		workload.Verdict, workload.Summary = WorkloadNoSaving, "No saving: your current setup stays."
	}
}

// smallest is the least of a value every run has, or nil when one lacks it.
// Changes are signed so that "least" is the most cautious: a smaller output
// gain, a smaller saving.
func smallest(runs []Run, value func(Saving) *float64) *float64 {
	var out *float64
	for _, run := range runs {
		current := value(run.Saving)
		if current == nil {
			return nil
		}
		if out == nil || *current < *out {
			out = ptr(*current)
		}
	}
	return out
}

// largest is the greatest of a value every run has: for a cost or GPU-hours
// change, where a saving is negative, the greatest is the smallest saving.
func largest(runs []Run, value func(Saving) *float64) *float64 {
	var out *float64
	for _, run := range runs {
		current := value(run.Saving)
		if current == nil {
			return nil
		}
		if out == nil || *current > *out {
			out = ptr(*current)
		}
	}
	return out
}

func summarize(workloads []Workload) Summary {
	summary := Summary{Workloads: len(workloads)}
	counted := 0
	var assumption string
	for _, workload := range workloads {
		if workload.Verdict == WorkloadVerified {
			summary.VerifiedWorkloads++
		}
		if workload.MonthlySavingUSD != nil {
			counted++
			summary.MonthlySavingUSD = ptr(value(summary.MonthlySavingUSD) + *workload.MonthlySavingUSD)
			for _, run := range workload.Runs {
				assumption = cmp.Or(assumption, run.Saving.Assumption)
			}
		}
		if workload.OutputPerGPUChange != nil && (summary.OutputPerGPUChange == nil || *workload.OutputPerGPUChange < *summary.OutputPerGPUChange) {
			summary.OutputPerGPUChange = ptr(*workload.OutputPerGPUChange)
		}
	}
	if summary.MonthlySavingUSD != nil {
		summary.AnnualSavingUSD = ptr(*summary.MonthlySavingUSD * MonthsPerYear)
		summary.SavingCovers = fmt.Sprintf("%d of %d workloads", counted, len(workloads))
		summary.SavingAssumption = assumption + "; a year is twelve months at the captured rate"
	}
	summary.Headline = headline(summary, workloads)
	return summary
}

func headline(summary Summary, workloads []Workload) string {
	switch {
	case len(workloads) == 0:
		return "No runs were read."
	case summary.VerifiedWorkloads == 0:
		for _, workload := range workloads {
			if workload.Verdict == WorkloadInProgress {
				return "Still measuring: no saving is verified yet."
			}
		}
		return "No saving is verified: your current setup stays where nothing was shown."
	}
	text := fmt.Sprintf("Saving verified on %d of %d workloads", summary.VerifiedWorkloads, summary.Workloads)
	if summary.MonthlySavingUSD != nil {
		text += fmt.Sprintf(": %s a month at your captured traffic (%s)", money(*summary.MonthlySavingUSD), summary.SavingCovers)
	}
	if summary.OutputPerGPUChange != nil {
		text += fmt.Sprintf(", with at least %s output per GPU", percent(*summary.OutputPerGPUChange))
	}
	return text + "."
}

// limits states what the report does not cover, plainly.
func limits(report Report) []string {
	out := []string{
		"Every number was measured on the hardware, engine version and traffic named beside it. It says nothing about other GPUs, engines, versions, models or traffic.",
		"Prompts and answers were not stored: traffic is replayed by its shape, and answers are compared by their check's score.",
	}
	add := func(line string) {
		if !slices.Contains(out, line) {
			out = append(out, line)
		}
	}
	monthly, annual := false, false
	for _, workload := range report.Workloads {
		var loadsMeasured []string
		for _, run := range workload.Runs {
			if run.Load != nil {
				loadsMeasured = append(loadsMeasured, strconv.Itoa(*run.Load))
			}
			if run.Simulated {
				add(fmt.Sprintf("Run %s ran on CPUs or a simulated accelerator: it proves the pipeline, not GPU speed, and no saving from it counts.", run.CampaignID))
			}
			if run.Saving.MonthlySavingUSD != nil {
				monthly = true
				annual = annual || run.Saving.AnnualSavingUSD != nil
			}
			if run.Quality.Eval != "" {
				prompts := ""
				if run.Quality.Prompts != nil {
					prompts = " (" + grouped(float64(*run.Quality.Prompts)) + " prompts)"
				}
				add(fmt.Sprintf("%s: answer quality was checked on %s%s only; other tasks are not measured.", workload.Name, run.Quality.Eval, prompts))
			}
		}
		if len(loadsMeasured) > 0 {
			add(fmt.Sprintf("%s was measured at %s requests at once; other loads are not covered.", workload.Name, strings.Join(loadsMeasured, ", ")))
		}
		if !strings.HasPrefix(workload.Traffic, "your captured traffic") {
			add(fmt.Sprintf("%s was measured on %s.", workload.Name, workload.Traffic))
		}
	}
	add("Cost uses the hourly price recorded with each measurement, which is your compute's price; where none was recorded, cost is unknown, never estimated.")
	if monthly {
		add("Monthly figures are cost per output token times your captured output-token volume. They assume servers stay busy, so idle capacity is not included.")
	}
	if annual {
		add("A year is twelve months at the captured rate; growth and seasonality are not included.")
	}
	return out
}

// unknown lists, in words, every number the report could not state.
func unknown(report Report) []string {
	out := []string{}
	for _, workload := range report.Workloads {
		for _, run := range workload.Runs {
			where := workload.Name
			if run.Load != nil {
				where += fmt.Sprintf(" at %d at once", *run.Load)
			}
			for _, number := range run.Numbers {
				if number.Decision == optimizationreport.DecisionUnknown && run.Outcome == optimizationreport.OutcomeWinnerSelected {
					out = append(out, fmt.Sprintf("%s: %s: %s", where, number.Label, cmp.Or(number.Reason, "not measured side by side")))
				}
			}
			if run.Outcome == optimizationreport.OutcomeWinnerSelected && run.Quality.Verdict == releaseguard.QualityUnknown {
				out = append(out, fmt.Sprintf("%s: answer quality: %s", where, cmp.Or(run.Quality.Reason, "no verdict")))
			}
			for _, target := range run.Targets {
				if target.Explanation != "" {
					out = append(out, fmt.Sprintf("%s: %s against your limit: %s", where, target.Label, target.Explanation))
				}
			}
			if run.Saving.State == SavingVerified && run.Saving.MonthlyUnavailableReason != "" {
				out = append(out, fmt.Sprintf("%s: monthly saving: %s", where, run.Saving.MonthlyUnavailableReason))
			}
		}
		if workload.LoadsReason != "" {
			out = append(out, workload.Name+": "+workload.LoadsReason)
		}
	}
	return out
}

// rerun is the commands that measure the run again from the same request
// (the same model, GPU, objective, traffic and limits) and save the new run's
// signed report to compare with this one. NEW_RUN is the ID the first prints.
func rerun(proposal optimizer.Proposal) []string {
	input := proposal.Input
	if input.ModelIdentity == "" || input.GPU == "" {
		return []string{}
	}
	model := input.ModelIdentity
	parts := []string{"infercrane optimize create", quote(model)}
	flag := func(name, value string) {
		if strings.TrimSpace(value) != "" {
			parts = append(parts, "--"+name, quote(value))
		}
	}
	flag("model-revision", input.ModelRevision)
	flag("provider", cmp.Or(input.ProviderAdapter, input.Provider))
	flag("region", input.Region)
	flag("gpu", input.GPU)
	if input.GPUCount > 1 {
		flag("gpu-count", strconv.Itoa(input.GPUCount))
	}
	flag("objective", input.Objective)
	flag("profile", input.WorkloadProfile)
	flag("workload-fingerprint", input.WorkloadFingerprint)
	number := func(name string, value *float64) {
		if value != nil {
			flag(name, strconv.FormatFloat(*value, 'f', -1, 64))
		}
	}
	number("target-concurrency", input.TargetConcurrency)
	number("max-ttft-p95-ms", input.MaxTTFTP95MS)
	number("max-tpot-p95-ms", input.MaxTPOTP95MS)
	number("max-error-rate", input.MaxErrorRate)
	number("min-output-tokens-second", input.MinOutputTokensSecond)
	if input.MaxCandidates > 0 {
		flag("max-candidates", strconv.Itoa(input.MaxCandidates))
	}
	return []string{strings.Join(parts, " "), "infercrane optimize watch NEW_RUN", "infercrane optimize report NEW_RUN --output json > new-run.json"}
}

// quote leaves plain tokens alone and single-quotes anything a shell would
// read otherwise.
func quote(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:@=+,", r))
	}) < 0 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func digest(report Report) string {
	report.ReportDigest = ""
	encoded, err := json.Marshal(report)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func finitePositive(value *float64) bool {
	return value != nil && *value > 0 && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func value(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func ptr(v float64) *float64 { return &v }
