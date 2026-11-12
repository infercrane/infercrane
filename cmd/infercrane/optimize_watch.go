package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
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
)

// optimize watch is a read-only, plain-language view of one campaign: where each
// setup is, what was measured against the current setup, and what happens next.
// It only reads the control API and never approves, activates, or cancels.

var watchStateWords = map[string]string{
	"proposed": "queued", "provisioning": "starting a server", "ready": "server ready",
	"measuring": "measuring against your current setup", "validating": "checking answer quality",
	"ranked": "measured and ranked", "guarding": "running the safety check", "guard_passed": "passed the safety check",
	"qualified": "winner, ready to activate", "rejected": "rejected", "inconclusive": "no clear result",
	"failed": "failed", "cancelled": "cancelled", "cleaned": "server removed",
}

var watchReasonWords = map[string]string{
	"measured_no_measurable_difference":  "no real difference from your current setup",
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
}

type watchCampaign struct {
	ID            string           `json:"id"`
	ModelIdentity string           `json:"model_identity"`
	Objective     string           `json:"objective"`
	Intent        string           `json:"intent"`
	State         string           `json:"state"`
	Candidates    []watchCandidate `json:"candidates"`
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
	fs := flag.NewFlagSet("optimize watch", flag.ContinueOnError)
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

// watchLabel describes a candidate by what it changes, in plain words.
func watchLabel(candidate watchCandidate) string {
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

func watchStatus(candidate watchCandidate) string {
	text := watchStateWords[candidate.State]
	if text == "" {
		text = strings.ReplaceAll(candidate.State, "_", " ")
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
	candidates := append([]watchCandidate(nil), campaign.Candidates...)
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Rank < candidates[j].Rank })
	var b strings.Builder
	title := "Optimization · " + campaign.ModelIdentity
	b.WriteString(paint(s.title, title) + "  " + paint(s.subtle, watchStatus(watchCandidate{State: campaign.State})) + "\n")
	b.WriteString(paint(s.subtle, "Each setup is measured against your current setup on the same traffic, then checked for answer quality.") + "\n\n")
	b.WriteString(paint(s.title, "Progress") + "\n")
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
		b.WriteString("  " + track.String() + "  " + truncateText(watchLabel(candidate), 40) + "  " + paint(s.subtle, watchStatus(candidate)) + "\n")
	}
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
		case i == 0 && verdict == "":
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
	if watchTerminal[campaign.State] {
		b.WriteString("\n" + paint(s.title, "Decision") + "\n")
		var winner *watchCandidate
		for i := range candidates {
			if candidates[i].State == "qualified" {
				winner = &candidates[i]
				break
			}
		}
		switch {
		case campaign.State == "cancelled":
			b.WriteString("  Cancelled. Nothing changed in production.\n")
		case winner != nil && len(candidates) > 0 && winner.ID != candidates[0].ID:
			b.WriteString("  " + paint(s.healthyText, "Roll out “"+watchLabel(*winner)+"”: it measured better on your traffic.") + "\n")
			b.WriteString("  " + paint(s.subtle, "infercrane optimize activate "+campaign.ID+" --candidate "+winner.ID) + "\n")
		case winner != nil:
			b.WriteString("  Keep your current setup: nothing beat it by more than the measurement noise.\n")
			b.WriteString("  " + paint(s.subtle, "infercrane optimize activate "+campaign.ID+" --candidate "+winner.ID+" publishes it as the optimized endpoint") + "\n")
		default:
			b.WriteString("  No setup qualified. Nothing changes in production.\n")
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
