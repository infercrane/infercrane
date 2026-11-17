package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

// optimize watch is a read-only, plain-language view of one campaign: where each
// setup is, what was measured against the current setup, and what happens next.
// It only reads the control API and never approves, activates, or cancels.

var watchStateWords = map[string]string{
	"proposed": "queued", "provisioning": "starting a server", "ready": "server ready",
	"measuring": "measuring against your current setup", "validating": "checking answer quality",
	"ranked": "measured, waiting to compare", "guarding": "running the safety check", "guard_passed": "passed the safety check",
	"qualified": "winner, ready to activate", "rejected": "rejected", "inconclusive": "no clear result",
	"failed": "failed", "cancelled": "cancelled", "cleaned": "server removed",
}

var watchReasonWords = map[string]string{
	"measured_no_measurable_difference":  "no real difference from your current setup",
	"measured_candidate_regressed":       "worse than your current setup",
	"measured_comparison_unavailable":    "not compared reliably with your current setup",
	"measured_candidate_superseded":      "another setup measured better",
	"quality_gate_rejected":              "answers changed too much",
	"quality_reference_unavailable":      "no reference answers to compare with",
	"release_guard_rejected":             "failed the safety check",
	"release_guard_inconclusive":         "safety check had too little evidence",
	"approval_expired_before_activation": "approval expired before activation",
}

var watchStages = []string{"proposed", "provisioning", "ready", "measuring", "validating", "ranked", "qualified"}

var watchTerminal = map[string]bool{"qualified": true, "guard_passed": true, "rejected": true, "inconclusive": true, "failed": true, "cancelled": true, "activated": true, "promoted": true, "completed": true}

type watchCandidate struct {
	ID                  string          `json:"id"`
	ProposalCandidateID string          `json:"proposal_candidate_id"`
	Rank                int             `json:"rank"`
	State               string          `json:"state"`
	FailureCode         string          `json:"failure_code"`
	DeploymentName      string          `json:"deployment_name"`
	BenchmarkID         string          `json:"benchmark_id"`
	QualityEvidenceID   string          `json:"quality_evidence_id"`
	DeploymentSpec      json.RawMessage `json:"deployment_spec"`
	Quality             *watchQuality   `json:"quality"`
	// Control is set by control APIs that mark the campaign control; without
	// it the control is the proposal's first-ranked setup.
	Control *bool `json:"control,omitempty"`
	// Label and Measurement come from the control API. A control plane
	// released before them leaves them empty.
	Label       string     `json:"label"`
	Measurement *watchLive `json:"measurement"`
}

// watchLive is the control API's live record of a candidate's interleaved
// measurement.
type watchLive struct {
	TurnsPlanned int        `json:"turns_planned"`
	TurnsDone    int        `json:"turns_done"`
	Current      *watchRun  `json:"current"`
	Runs         []watchRun `json:"runs"`
}

type watchRun struct {
	Kind      string     `json:"kind"`
	Arm       string     `json:"arm"`
	Turn      int        `json:"turn"`
	EndedAt   *time.Time `json:"ended_at"`
	OutputTPS *float64   `json:"output_token_throughput"`
	Error     string     `json:"error"`
}

// watchProgress is where the run is as a whole. Every number comes from the
// control API; the ETA is the API's estimate from observed step durations.
type watchProgress struct {
	Stage          string   `json:"stage"`
	ElapsedSeconds *float64 `json:"elapsed_seconds"`
	SetupsTotal    int      `json:"setups_total"`
	TurnsDone      int      `json:"turns_done"`
	TurnsTotal     *int     `json:"turns_total"`
	ETA            struct {
		State            string   `json:"state"`
		RemainingSeconds *float64 `json:"remaining_seconds"`
		Reason           string   `json:"reason"`
	} `json:"eta"`
}

type watchSpend struct {
	State        string   `json:"state"`
	Label        string   `json:"label"`
	EstimatedUSD *float64 `json:"estimated_usd"`
	CapUSD       *float64 `json:"cap_usd"`
	Reason       string   `json:"reason"`
}

// watchQuality is the control API's view of a candidate's answer check.
type watchQuality struct {
	State   string `json:"state"`
	Source  string `json:"source,omitempty"`
	Role    string `json:"role,omitempty"`
	Reason  string `json:"reason"`
	Command string `json:"command,omitempty"`
}

var watchQualityWords = map[string]string{
	"checking":             "checking answers against your current setup",
	"waiting_for_eval_set": "waiting for your eval set",
	"waiting_for_evidence": "waiting for your signed evaluation",
}

type watchCampaign struct {
	ID                 string           `json:"id"`
	ModelIdentity      string           `json:"model_identity"`
	Objective          string           `json:"objective"`
	Intent             string           `json:"intent"`
	TargetDeployment   string           `json:"target_deployment"`
	State              string           `json:"state"`
	ApprovalExpiresAt  *time.Time       `json:"approval_expires_at"`
	ApprovedMaxCostUSD *float64         `json:"approved_max_cost_usd"`
	Candidates         []watchCandidate `json:"candidates"`
	Progress           *watchProgress   `json:"progress"`
	Spend              *watchSpend      `json:"spend"`
	Warnings           []string         `json:"warnings"`
}

type watchComparison struct {
	HigherIsBetter bool    `json:"higher_is_better"`
	Delta          float64 `json:"delta"`
	NoiseFloor     float64 `json:"noise_floor"`
	Decision       string  `json:"decision"`
}

type watchMeasurement struct {
	OutputTPS     *float64
	TTFTP95MS     *float64
	LatencyP95MS  *float64
	CostPerMillon *float64
	Comparison    map[string]watchComparison
	// Regressions are the outcomes measured worse than the control in this
	// setup's own window, including a window the control's drift invalidated.
	Regressions []optimizationcampaign.Regression
}

type watchSnapshot struct {
	campaign     watchCampaign
	measurements map[string]watchMeasurement
	err          error
	at           time.Time
}

type watchSnapshotMsg watchSnapshot

type watchModel struct {
	ctx      context.Context
	cfg      config.Config
	id       string
	snapshot watchSnapshot
	width    int
	dark     bool
	loading  bool
}

func optimizeWatchCommand(ctx context.Context, args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: infercrane optimize watch CAMPAIGN [--once]")
	}
	id := args[0]
	fs := newFlagSet("optimize watch")
	once := fs.Bool("once", false, "print one plain-text snapshot and exit")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return errors.New("usage: infercrane optimize watch CAMPAIGN [--once]")
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	interactive := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	if *once || !interactive {
		snapshot := loadWatchSnapshot(ctx, cfg, id)
		if snapshot.err != nil {
			return snapshot.err
		}
		fmt.Print(renderOptimizeWatch(snapshot, newUIStyles(true), 100, false))
		return nil
	}
	program := tea.NewProgram(watchModel{ctx: ctx, cfg: cfg, id: id, dark: true, loading: true, width: 100}, tea.WithContext(ctx))
	_, err = program.Run()
	if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (m watchModel) Init() tea.Cmd {
	return tea.Batch(m.load(), uiTick(), tea.RequestBackgroundColor)
}

func (m watchModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	case watchSnapshotMsg:
		m.snapshot, m.loading = watchSnapshot(msg), false
	case uiTickMsg:
		return m, tea.Batch(m.load(), uiTick())
	}
	return m, nil
}

func (m watchModel) View() tea.View {
	content := "Loading campaign " + m.id + "…"
	if !m.loading {
		content = renderOptimizeWatch(m.snapshot, newUIStyles(m.dark), m.width, true) + "\n" + newUIStyles(m.dark).subtle.Render("q to quit · read-only · refreshes every 2 s")
	}
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "InferCrane · optimization"
	return view
}

func (m watchModel) load() tea.Cmd {
	return func() tea.Msg { return watchSnapshotMsg(loadWatchSnapshot(m.ctx, m.cfg, m.id)) }
}

func loadWatchSnapshot(ctx context.Context, cfg config.Config, id string) watchSnapshot {
	snapshot := watchSnapshot{measurements: map[string]watchMeasurement{}, at: time.Now()}
	var response struct {
		Campaign watchCampaign `json:"campaign"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/optimization/campaigns/"+url.PathEscape(id), "", nil, &response); err != nil {
		snapshot.err = err
		return snapshot
	}
	snapshot.campaign = response.Campaign
	for _, candidate := range response.Campaign.Candidates {
		if candidate.BenchmarkID == "" || candidate.DeploymentName == "" {
			continue
		}
		var rows struct {
			Data []json.RawMessage `json:"data"`
		}
		if controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(candidate.DeploymentName)+"/benchmarks", "", nil, &rows) != nil {
			continue
		}
		for _, raw := range rows.Data {
			if measurement, ok := parseWatchBenchmark(raw, candidate.BenchmarkID); ok {
				snapshot.measurements[candidate.ID] = measurement
				break
			}
		}
	}
	return snapshot
}

// parseWatchBenchmark reads one benchmark row and its measured comparison with
// the control. Missing values stay nil; nothing is estimated.
func parseWatchBenchmark(raw json.RawMessage, benchmarkID string) (watchMeasurement, bool) {
	var row struct {
		ID           string          `json:"id"`
		OutputTPS    *float64        `json:"output_token_throughput"`
		TTFTP95MS    *float64        `json:"ttft_p95_ms"`
		LatencyP95MS *float64        `json:"latency_p95_ms"`
		Workload     json.RawMessage `json:"workload"`
		Cost         json.RawMessage `json:"cost_metadata"`
	}
	if json.Unmarshal(raw, &row) != nil || row.ID != benchmarkID {
		return watchMeasurement{}, false
	}
	measurement := watchMeasurement{OutputTPS: row.OutputTPS, TTFTP95MS: row.TTFTP95MS, LatencyP95MS: row.LatencyP95MS}
	var workload struct {
		Measured *struct {
			Comparison map[string]watchComparison `json:"comparison"`
		} `json:"measured"`
	}
	if json.Unmarshal(unwrapJSONString(row.Workload), &workload) == nil && workload.Measured != nil {
		measurement.Comparison = workload.Measured.Comparison
	}
	var evidence struct {
		Measured *optimizationcampaign.MeasuredEvidence `json:"measured"`
	}
	if json.Unmarshal(unwrapJSONString(row.Workload), &evidence) == nil {
		measurement.Regressions = optimizationcampaign.MeasuredRegressions(evidence.Measured)
	}
	var cost struct {
		Available bool     `json:"available"`
		Hourly    *float64 `json:"hourly"`
	}
	if json.Unmarshal(unwrapJSONString(row.Cost), &cost) == nil && cost.Available && cost.Hourly != nil && row.OutputTPS != nil && *row.OutputTPS > 0 {
		value := *cost.Hourly / (*row.OutputTPS * 3600) * 1_000_000
		measurement.CostPerMillon = &value
	}
	return measurement, true
}

// unwrapJSONString accepts a JSON document or a JSON string that contains one.
func unwrapJSONString(raw json.RawMessage) []byte {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []byte(text)
	}
	return raw
}

// watchLabel describes a candidate by what it changes, in plain words: the
// control API's name for it, else its engine and flags from an older control
// plane that names no setup.
func watchLabel(candidate watchCandidate) string {
	if strings.TrimSpace(candidate.Label) != "" {
		return candidate.Label
	}
	var spec struct {
		Runtime struct {
			Engine string   `json:"engine"`
			Args   []string `json:"args"`
		} `json:"runtime"`
	}
	if json.Unmarshal(unwrapJSONString(candidate.DeploymentSpec), &spec) != nil || spec.Runtime.Engine == "" {
		return "setup " + shortID(candidate.ID)
	}
	engine := spec.Runtime.Engine
	if engine == "custom-oci" {
		engine = "custom image"
	}
	if len(spec.Runtime.Args) == 0 {
		return engine + ", default settings"
	}
	return engine + ", " + strings.Join(spec.Runtime.Args, " ")
}

// watchMeasuredLoss lists the ranking codes a measured regression explains
// better: a setup that lost and measured worse says on which outcome.
var watchMeasuredLoss = map[string]bool{
	optimizationcampaign.FailureNoMeasurableDifference: true, optimizationcampaign.FailureRegressed: true,
	optimizationcampaign.FailureComparisonUnavailable: true, "measured_candidate_superseded": true,
}

// watchKeptWords describe a control the campaign kept. Activating it would
// change nothing, so it is never called a winner.
const watchKeptWords = "kept · nothing to activate"

// watchStatus describes one setup. kept is true for a control the campaign
// kept; measurement may be empty.
func watchStatus(candidate watchCandidate, measurement watchMeasurement, kept bool) string {
	if kept && (candidate.State == "qualified" || candidate.State == "guard_passed") {
		return watchKeptWords
	}
	text := watchStateWords[candidate.State]
	if candidate.State == "validating" && candidate.Quality != nil && watchQualityWords[candidate.Quality.State] != "" {
		text = watchQualityWords[candidate.Quality.State]
	}
	if candidate.State == "measuring" && candidate.Measurement != nil {
		text = "measuring · " + watchTurn(*candidate.Measurement)
	}
	if text == "" {
		text = strings.ReplaceAll(candidate.State, "_", " ")
	}
	if watchMeasuredLoss[candidate.FailureCode] && len(measurement.Regressions) > 0 {
		return text + " · " + optimizationcampaign.DescribeRegressions(measurement.Regressions)
	}
	if candidate.FailureCode != "" {
		reason := watchReasonWords[candidate.FailureCode]
		if reason == "" {
			reason = strings.ReplaceAll(candidate.FailureCode, "_", " ")
		}
		text += " · " + reason
	}
	return text
}

// watchVerdict summarizes a candidate's measured comparison with the control.
func watchVerdict(measurement watchMeasurement) (string, string) {
	for _, metric := range []string{"output_token_throughput", "ttft_p95_ms", "latency_p95_ms"} {
		comparison, ok := measurement.Comparison[metric]
		if !ok {
			continue
		}
		size := fmt.Sprintf("%.0f%%", 100*abs(comparison.Delta))
		noise := fmt.Sprintf("noise ±%.1f%%", 100*comparison.NoiseFloor)
		name := watchMetricName(metric)
		switch comparison.Decision {
		case "improved":
			return "better", fmt.Sprintf("%s improved %s (%s)", name, size, noise)
		case "regressed":
			return "worse", fmt.Sprintf("%s got worse by %s (%s)", name, size, noise)
		case "no_measurable_difference":
			return "same", fmt.Sprintf("no real difference in %s (%s)", name, noise)
		}
	}
	if len(measurement.Regressions) > 0 {
		return "worse", optimizationcampaign.DescribeRegressions(measurement.Regressions)
	}
	return "", ""
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func renderOptimizeWatch(snapshot watchSnapshot, s uiStyles, width int, styled bool) string {
	paint := func(style lipgloss.Style, text string) string {
		if styled {
			return style.Render(text)
		}
		return text
	}
	if width < 60 {
		width = 60
	}
	campaign := snapshot.campaign
	candidates := watchOrder(campaign.Candidates)
	control, winner := watchControl(candidates), watchWinner(candidates)
	kept := control != nil && winner != nil && winner.ID == control.ID
	var b strings.Builder
	title := "Optimization · " + campaign.ModelIdentity
	headline := watchStageWord(campaign)
	if kept && watchDecided(campaign) {
		headline = "kept your current setup"
	}
	b.WriteString(paint(s.title, title) + "  " + paint(s.subtle, headline) + "\n")
	b.WriteString(paint(s.subtle, "Each setup is measured against your current setup on the same traffic, then checked for answer quality.") + "\n")
	if line := watchRunLine(campaign, candidates); line != "" {
		b.WriteString("  " + line + "\n")
	}
	if line := watchSpendLine(campaign); line != "" {
		b.WriteString("  " + paint(s.subtle, line) + "\n")
	}
	if len(campaign.Warnings) > 0 {
		b.WriteString("\n" + paint(s.title, "Proposal warnings") + "\n")
		for _, warning := range campaign.Warnings {
			b.WriteString("  - " + warning + "\n")
		}
	}
	b.WriteString("\n" + paint(s.title, "Progress") + "\n")
	for _, candidate := range candidates {
		index := -1
		for i, stage := range watchStages {
			if stage == candidate.State {
				index = i
			}
		}
		failed := candidate.State == "rejected" || candidate.State == "failed" || candidate.State == "inconclusive" || candidate.State == "cancelled"
		var track strings.Builder
		for i := range watchStages {
			mark := "■"
			switch {
			case failed && i >= max(index, 0):
				mark = paint(s.failureText, "■")
			case i < index || (index == len(watchStages)-1 && i == index):
				mark = paint(s.healthyText, "■")
			case i == index:
				mark = paint(s.warningText, "■")
			default:
				mark = paint(s.subtle, "□")
			}
			track.WriteString(mark)
		}
		isKept := kept && candidate.ID == control.ID
		name := watchLabel(candidate)
		if control != nil && candidate.ID == control.ID && !strings.Contains(strings.ToLower(name), "current setup") {
			name += " · current setup"
		}
		b.WriteString("  " + track.String() + "  " + padText(truncateText(name, 44), 44) + "  " + paint(s.subtle, watchStatus(candidate, snapshot.measurements[candidate.ID], isKept)) + "\n")
	}
	b.WriteString(renderWatchTurns(candidates, s, paint))
	b.WriteString("\n" + paint(s.title, "Measured") + "\n")
	measuredAny := false
	for i, candidate := range candidates {
		measurement, ok := snapshot.measurements[candidate.ID]
		if !ok {
			continue
		}
		measuredAny = true
		line := fmt.Sprintf("  %-40s %s tokens/s · p95 first token %s · p95 latency %s", truncateText(watchLabel(candidate), 40), metric(measurement.OutputTPS, ""), metric(measurement.TTFTP95MS, " ms"), metric(measurement.LatencyP95MS, " ms"))
		if measurement.CostPerMillon != nil {
			line += fmt.Sprintf(" · $%.2f per 1M tokens", *measurement.CostPerMillon)
		}
		b.WriteString(line + "\n")
		kind, verdict := watchVerdict(measurement)
		switch {
		case (control != nil && candidate.ID == control.ID || control == nil && i == 0) && verdict == "":
			b.WriteString("    " + paint(s.subtle, "your current setup") + "\n")
		case kind == "better":
			b.WriteString("    " + paint(s.healthyText, verdict) + "\n")
		case kind == "worse":
			b.WriteString("    " + paint(s.failureText, verdict) + "\n")
		case verdict != "":
			b.WriteString("    " + paint(s.subtle, verdict) + "\n")
		}
	}
	if !measuredAny {
		b.WriteString("  " + paint(s.subtle, "results appear as each setup finishes measuring") + "\n")
	}
	b.WriteString(renderWatchAnswers(candidates, s, paint))
	if watchDecided(campaign) {
		b.WriteString("\n" + paint(s.title, "Decision") + "\n")
		switch {
		case campaign.State == "cancelled":
			b.WriteString("  Cancelled. Nothing changed in production.\n")
		case winner != nil && (winner.State == "promoted" || winner.State == "observed"):
			live := "“" + watchLabel(*winner) + "” is live"
			if !kept {
				live += ": it measured better on your traffic"
			}
			b.WriteString("  " + paint(s.healthyText, live+".") + "\n")
		case winner != nil && !kept:
			b.WriteString("  " + paint(s.healthyText, "Roll out “"+watchLabel(*winner)+"”: it measured better on your traffic.") + "\n")
			b.WriteString("  " + paint(s.subtle, "infercrane optimize activate "+campaign.ID+" --candidate "+winner.ID) + "\n")
		case winner != nil:
			b.WriteString(renderWatchKept(campaign, s, paint))
		default:
			b.WriteString("  No setup qualified. Nothing changes in production.\n")
		}
	}
	return b.String()
}

// renderWatchKept states a keep decision and its one next step. The kept
// control's server stays up until the approval ends, so the step removes it;
// activating the control would change nothing.
func renderWatchKept(campaign watchCampaign, s uiStyles, paint func(lipgloss.Style, string) string) string {
	var b strings.Builder
	b.WriteString("  Keep your current setup: nothing beat it by more than the measurement noise.\n")
	if campaign.Intent == optimizationcampaign.IntentEvolveEndpoint {
		target := campaign.TargetDeployment
		if target == "" {
			target = "your endpoint"
		}
		b.WriteString("  Nothing to activate: " + target + " keeps serving its current revision.\n")
	} else {
		b.WriteString("  Nothing to activate: no new endpoint is published.\n")
	}
	cleanup := "infercrane optimize cancel " + campaign.ID + " removes the measurement servers now"
	if campaign.ApprovalExpiresAt != nil {
		cleanup += "; otherwise they are removed when the approval ends at " + campaign.ApprovalExpiresAt.UTC().Format("2006-01-02 15:04 UTC")
	}
	b.WriteString("  " + paint(s.subtle, cleanup) + "\n")
	return b.String()
}

// watchControl is the campaign control: the setup the API marks, else the
// proposal's first-ranked setup, which is the proposal's starting point.
// candidates must be sorted by rank.
func watchControl(candidates []watchCandidate) *watchCandidate {
	for index := range candidates {
		if candidates[index].Control != nil && *candidates[index].Control {
			return &candidates[index]
		}
	}
	for index := range candidates {
		if candidates[index].Control == nil {
			return &candidates[index]
		}
	}
	return nil
}

// watchWinner is the setup the campaign chose: live, or waiting for
// activation after qualification (new endpoint) or the safety check
// (evolution).
func watchWinner(candidates []watchCandidate) *watchCandidate {
	var waiting *watchCandidate
	for index := range candidates {
		switch candidates[index].State {
		case "promoted", "observed":
			return &candidates[index]
		case "qualified", "guard_passed":
			if waiting == nil {
				waiting = &candidates[index]
			}
		}
	}
	return waiting
}

// watchOrder lists setups in the order the run measures them: the current
// setup first, then planning rank.
func watchOrder(candidates []watchCandidate) []watchCandidate {
	ordered := append([]watchCandidate(nil), candidates...)
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := watchMarked(ordered[i]), watchMarked(ordered[j])
		if left != right {
			return left
		}
		return ordered[i].Rank < ordered[j].Rank
	})
	return ordered
}

// watchMarked reports whether the control API marked the setup as the control.
func watchMarked(candidate watchCandidate) bool {
	return candidate.Control != nil && *candidate.Control
}

// padText right-pads to width runes so status words line up.
func padText(value string, width int) string {
	return value + strings.Repeat(" ", max(0, width-len([]rune(value))))
}

// watchDecided reads the run's stage. The aggregate campaign state reads
// "ranked" as soon as one setup is ranked, so it is used only when the
// control plane reports no progress.
func watchDecided(campaign watchCampaign) bool {
	if campaign.Progress != nil {
		return campaign.Progress.Stage == "decided" || campaign.Progress.Stage == "cancelled"
	}
	return watchTerminal[campaign.State]
}

var watchStageWords = map[string]string{
	"awaiting_approval": "waiting for approval", "running": "running", "comparing": "comparing results",
	"decided": "decided", "cancelled": "cancelled",
}

func watchStageWord(campaign watchCampaign) string {
	if campaign.Progress != nil && watchStageWords[campaign.Progress.Stage] != "" {
		return watchStageWords[campaign.Progress.Stage]
	}
	return watchStatus(watchCandidate{State: campaign.State}, watchMeasurement{}, false)
}

// watchTurn says which measured turn is running, as the control API counts it.
func watchTurn(live watchLive) string {
	if live.Current != nil && live.Current.Kind == "warmup" {
		return "warming up"
	}
	turn := live.TurnsDone
	if live.Current != nil {
		turn++
	}
	if live.TurnsPlanned <= 0 {
		return fmt.Sprintf("turn %d", max(turn, 1))
	}
	return fmt.Sprintf("turn %d of %d", min(max(turn, 1), live.TurnsPlanned), live.TurnsPlanned)
}

// watchRunLine is one line on where the run is: which setup, which turn, time
// elapsed, and the API's estimate of time left. Unknown values are said so.
func watchRunLine(campaign watchCampaign, ordered []watchCandidate) string {
	progress := campaign.Progress
	if progress == nil {
		return ""
	}
	var parts []string
	switch progress.Stage {
	case "awaiting_approval":
		return "Waiting for approval. Nothing runs until a spend cap is approved."
	case "running":
		for index, candidate := range ordered {
			if candidate.State == "provisioning" || candidate.State == "ready" || candidate.State == "measuring" || candidate.State == "validating" {
				parts = append(parts, fmt.Sprintf("Setup %d of %d", index+1, len(ordered)))
				if candidate.State == "measuring" && candidate.Measurement != nil {
					parts = append(parts, watchTurn(*candidate.Measurement))
				}
				break
			}
		}
	case "comparing":
		parts = append(parts, "Comparing the measured setups")
	case "decided":
		if progress.ElapsedSeconds != nil {
			return "Decided after " + watchDuration(*progress.ElapsedSeconds)
		}
		return "Decided"
	case "cancelled":
		return "Cancelled"
	}
	if progress.ElapsedSeconds != nil {
		parts = append(parts, watchDuration(*progress.ElapsedSeconds)+" elapsed")
	}
	if progress.Stage == "running" {
		if progress.ETA.State == "estimated" && progress.ETA.RemainingSeconds != nil {
			parts = append(parts, "about "+watchDuration(*progress.ETA.RemainingSeconds)+" left (estimate from this run's measured steps)")
		} else if progress.ETA.Reason != "" {
			parts = append(parts, "time left unknown, "+progress.ETA.Reason)
		}
	}
	return strings.Join(parts, " · ")
}

// watchSpendLine shows spend so far against the cap. The control API
// estimates it from the quoted rate; unknown stays unknown.
func watchSpendLine(campaign watchCampaign) string {
	spend := campaign.Spend
	capText := ""
	if campaign.ApprovedMaxCostUSD != nil {
		capText = fmt.Sprintf("$%.2f cap", *campaign.ApprovedMaxCostUSD)
	}
	switch {
	case spend == nil && capText == "":
		return ""
	case spend == nil:
		return "Spend so far not reported · " + capText
	case spend.State == "estimated" && spend.EstimatedUSD != nil:
		line := fmt.Sprintf("Spend so far about $%.2f", *spend.EstimatedUSD)
		if capText != "" {
			line += " of the " + capText
		}
		return line + " · " + spend.Label
	default:
		line := "Spend so far unknown"
		if spend.Reason != "" {
			line += " · " + spend.Reason
		}
		if capText != "" {
			line += " · " + capText
		}
		return line
	}
}

func watchDuration(seconds float64) string {
	minutes := int(seconds/60 + 0.5)
	switch {
	case seconds < 60:
		return "under a minute"
	case minutes < 60:
		return fmt.Sprintf("%dm", minutes)
	default:
		return fmt.Sprintf("%dh %02dm", minutes/60, minutes%60)
	}
}

// renderWatchTurns draws the measuring setup's turns as they finish: C is the
// current setup and X the setup being tested, each with its measured output
// tokens per second. Nothing is drawn for a turn before it ends.
func renderWatchTurns(candidates []watchCandidate, s uiStyles, paint func(lipgloss.Style, string) string) string {
	for _, candidate := range candidates {
		live := candidate.Measurement
		if candidate.State != "measuring" || live == nil {
			continue
		}
		var cells []string
		for _, run := range live.Runs {
			if run.Kind != "turn" {
				continue
			}
			arm := "X"
			if run.Arm == "control" {
				arm = "C"
			}
			switch {
			case run.EndedAt == nil:
				cells = append(cells, arm+" "+paint(s.warningText, "running"))
			case run.Error != "":
				cells = append(cells, arm+" "+paint(s.failureText, "failed"))
			case run.OutputTPS != nil:
				cells = append(cells, fmt.Sprintf("%s %.0f", arm, *run.OutputTPS))
			default:
				cells = append(cells, arm+" —")
			}
		}
		for len(cells) < live.TurnsPlanned {
			cells = append(cells, paint(s.subtle, "·"))
		}
		var b strings.Builder
		b.WriteString("\n" + paint(s.title, "Now measuring") + "  " + truncateText(watchLabel(candidate), 48) + " · " + watchTurn(*live) + "\n")
		b.WriteString("  " + strings.Join(cells, "  ") + "\n")
		b.WriteString("  " + paint(s.subtle, "C your current setup · X this setup · output tokens/s per turn") + "\n")
		return b.String()
	}
	return ""
}

// renderWatchAnswers shows each setup's answer check as the control API
// reports it, with the exact command when the check waits on the customer.
func renderWatchAnswers(candidates []watchCandidate, s uiStyles, paint func(lipgloss.Style, string) string) string {
	var b strings.Builder
	for _, candidate := range candidates {
		quality := candidate.Quality
		if quality == nil || quality.State == "" || quality.State == "pending" || quality.State == "not_run" {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("\n" + paint(s.title, "Answers") + "\n")
		}
		source := ""
		switch quality.Source {
		case "infercrane":
			source = " (InferCrane check)"
		case "customer":
			source = " (your evaluation)"
		}
		style := s.subtle
		switch quality.State {
		case "passed":
			style = s.healthyText
		case "watch", "waiting_for_eval_set", "waiting_for_evidence":
			style = s.warningText
		case "blocked", "inconclusive":
			style = s.failureText
		}
		b.WriteString(fmt.Sprintf("  %-40s %s\n", truncateText(watchLabel(candidate), 40), paint(style, quality.Reason+source)))
		if quality.Command != "" {
			b.WriteString("    " + paint(s.subtle, "run: "+quality.Command) + "\n")
		}
	}
	return b.String()
}

// watchMetricName gives the plain word shown for a measured metric.
func watchMetricName(metric string) string {
	switch metric {
	case "output_token_throughput":
		return "throughput"
	case "ttft_p95_ms":
		return "first-token time"
	default:
		return "latency"
	}
}
