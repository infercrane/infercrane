package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image/color"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/domain"
)

const uiRefreshInterval = 2 * time.Second

type uiSnapshot struct {
	deployments []deploymentSummary
	view        deploymentView
	events      []cliEvent
	benchmarks  []uiBenchmark
	scaling     []uiScalingDecision
	selected    string
	err         error
	sequence    int
}

type uiSnapshotMsg uiSnapshot
type uiTickMsg time.Time
type uiActionResultMsg struct {
	operationID string
	err         error
}

type uiBenchmark struct {
	ID                    string    `json:"id"`
	RevisionID            string    `json:"revision_id"`
	Tool                  string    `json:"tool"`
	Provider              string    `json:"provider"`
	GPU                   string    `json:"gpu"`
	ComputeMode           string    `json:"compute_mode"`
	ReproductionCommand   string    `json:"reproduction_command"`
	RequestCount          int       `json:"request_count"`
	Succeeded             int       `json:"succeeded"`
	Failed                int       `json:"failed"`
	TTFTP95MS             *float64  `json:"ttft_p95_ms"`
	LatencyP95MS          *float64  `json:"latency_p95_ms"`
	OutputTokenThroughput *float64  `json:"output_token_throughput"`
	CreatedAt             time.Time `json:"created_at"`
}

type uiScalingDecision struct {
	Action      string          `json:"action"`
	OldReplicas int             `json:"old_replicas"`
	NewReplicas int             `json:"new_replicas"`
	Reason      string          `json:"reason"`
	Signals     json.RawMessage `json:"signals"`
	CreatedAt   time.Time       `json:"created_at"`
}

type uiAction struct {
	name, label, detail, method, path string
	body                              any
}

var uiTabs = []string{"Overview", "Operations", "Rollout", "Performance", "Infrastructure", "Scaling", "Events"}

type uiModel struct {
	ctx          context.Context
	cfg          config.Config
	deployments  []deploymentSummary
	view         deploymentView
	events       []cliEvent
	benchmarks   []uiBenchmark
	scaling      []uiScalingDecision
	selected     int
	width        int
	height       int
	dark         bool
	loading      bool
	help         bool
	explain      bool
	tab          int
	eventCursor  int
	palette      bool
	paletteIndex int
	confirm      *uiAction
	actionBusy   bool
	readOnly     bool
	lastUpdated  time.Time
	errorMessage string
	notice       string
	requestSeq   int
}

func uiCommand(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	readOnly := fs.Bool("read-only", false, "disable all control actions")
	if err := fs.Parse(args); err != nil || len(fs.Args()) != 0 {
		return errors.New("usage: infercrane ui [--read-only]")
	}
	stdin, err := os.Stdin.Stat()
	if err != nil || stdin.Mode()&os.ModeCharDevice == 0 {
		return errors.New("infercrane ui requires an interactive terminal; use infercrane deployments, status, events, or --output json")
	}
	stdout, err := os.Stdout.Stat()
	if err != nil || stdout.Mode()&os.ModeCharDevice == 0 {
		return errors.New("infercrane ui requires an interactive terminal and cannot be redirected; use --output json commands for automation")
	}
	program := tea.NewProgram(uiModel{ctx: ctx, cfg: cfg, loading: true, dark: true, readOnly: *readOnly}, tea.WithContext(ctx))
	_, err = program.Run()
	if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (m uiModel) Init() tea.Cmd {
	return tea.Batch(m.load("", 0), uiTick(), tea.RequestBackgroundColor)
}

func (m uiModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
	case tea.KeyPressMsg:
		if m.confirm != nil {
			switch msg.String() {
			case "y", "enter":
				m.actionBusy = true
				action := *m.confirm
				m.confirm = nil
				return m, m.runAction(action)
			case "n", "esc", "q":
				m.confirm = nil
			}
			return m, nil
		}
		if m.palette {
			actions := m.availableActions()
			switch msg.String() {
			case "esc", "ctrl+k", "q":
				m.palette = false
			case "up", "k":
				if m.paletteIndex > 0 {
					m.paletteIndex--
				}
			case "down", "j":
				if m.paletteIndex+1 < len(actions) {
					m.paletteIndex++
				}
			case "enter":
				if len(actions) > 0 {
					action := actions[m.paletteIndex]
					m.palette = false
					if action.method == "COPY" {
						m.notice = "Copied command"
						return m, tea.SetClipboard(action.path)
					}
					m.confirm = &action
				}
			}
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.help = !m.help
		case "ctrl+k", ":":
			if m.actionBusy {
				m.notice = "Wait for the current action to be persisted"
				return m, nil
			}
			m.palette, m.paletteIndex = true, 0
		case "tab", "right", "l":
			m.tab = (m.tab + 1) % len(uiTabs)
		case "shift+tab", "left", "h":
			m.tab = (m.tab + len(uiTabs) - 1) % len(uiTabs)
		case "1", "2", "3", "4", "5", "6", "7":
			m.tab = int(msg.String()[0] - '1')
		case "e":
			m.explain = !m.explain
		case "up", "k":
			if m.tab == len(uiTabs)-1 && m.eventCursor > 0 {
				m.eventCursor--
				return m, nil
			}
			if m.selected > 0 {
				m.selected--
				m.loading = true
				m.requestSeq++
				return m, m.load(m.deployments[m.selected].Name, m.requestSeq)
			}
		case "down", "j":
			if m.tab == len(uiTabs)-1 && m.eventCursor+1 < len(m.events) {
				m.eventCursor++
				return m, nil
			}
			if m.selected+1 < len(m.deployments) {
				m.selected++
				m.loading = true
				m.requestSeq++
				return m, m.load(m.deployments[m.selected].Name, m.requestSeq)
			}
		case "r":
			if m.loading {
				return m, nil
			}
			m.loading = true
			m.requestSeq++
			return m, m.load(m.selectedName(), m.requestSeq)
		case "c":
			value := m.copyValue()
			if value != "" {
				m.notice = "Copied: " + value
				return m, tea.SetClipboard(value)
			}
		}
	case uiTickMsg:
		if m.loading {
			return m, uiTick()
		}
		m.loading = true
		m.requestSeq++
		return m, tea.Batch(m.load(m.selectedName(), m.requestSeq), uiTick())
	case uiSnapshotMsg:
		if msg.sequence < m.requestSeq {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			m.errorMessage = msg.err.Error()
			return m, nil
		}
		m.errorMessage = ""
		m.deployments, m.view, m.events, m.benchmarks, m.scaling = msg.deployments, msg.view, msg.events, msg.benchmarks, msg.scaling
		if m.eventCursor >= len(m.events) {
			m.eventCursor = max(0, len(m.events)-1)
		}
		m.lastUpdated = time.Now()
		for index, deployment := range m.deployments {
			if deployment.Name == msg.selected {
				m.selected = index
				break
			}
		}
	case uiActionResultMsg:
		m.actionBusy = false
		if msg.err != nil {
			m.errorMessage = msg.err.Error()
			m.notice = "Action failed"
			return m, nil
		}
		m.notice = "Durable operation queued · " + shortID(msg.operationID)
		m.loading = true
		m.requestSeq++
		return m, m.load(m.selectedName(), m.requestSeq)
	}
	return m, nil
}

func (m uiModel) View() tea.View {
	content := m.render()
	view := tea.NewView(content)
	view.AltScreen = true
	view.WindowTitle = "InferCrane"
	return view
}

func (m uiModel) load(selected string, sequence int) tea.Cmd {
	return func() tea.Msg {
		snapshot := loadUISnapshot(m.ctx, m.cfg, selected)
		snapshot.sequence = sequence
		return uiSnapshotMsg(snapshot)
	}
}

func uiTick() tea.Cmd {
	return tea.Tick(uiRefreshInterval, func(stamp time.Time) tea.Msg { return uiTickMsg(stamp) })
}

func loadUISnapshot(ctx context.Context, cfg config.Config, selected string) uiSnapshot {
	var response struct {
		Data []deploymentSummary `json:"data"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments", "", nil, &response); err != nil {
		return uiSnapshot{err: err}
	}
	selectedExists := false
	for _, deployment := range response.Data {
		if deployment.Name == selected {
			selectedExists = true
			break
		}
	}
	if !selectedExists {
		selected = ""
		if len(response.Data) > 0 {
			selected = response.Data[0].Name
		}
	}
	snapshot := uiSnapshot{deployments: response.Data, selected: selected}
	if selected == "" {
		return snapshot
	}
	type viewResult struct {
		value deploymentView
		err   error
	}
	type eventsResult struct {
		value []cliEvent
		err   error
	}
	type benchmarksResult struct {
		value []uiBenchmark
		err   error
	}
	type scalingResult struct {
		value []uiScalingDecision
		err   error
	}
	viewCh, eventsCh := make(chan viewResult, 1), make(chan eventsResult, 1)
	benchmarksCh, scalingCh := make(chan benchmarksResult, 1), make(chan scalingResult, 1)
	go func() { value, err := fetchDeployment(ctx, cfg, selected); viewCh <- viewResult{value, err} }()
	go func() { value, err := deploymentEvents(ctx, cfg, selected); eventsCh <- eventsResult{value, err} }()
	go func() {
		var response struct {
			Data []uiBenchmark `json:"data"`
		}
		err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(selected)+"/benchmarks?limit=10", "", nil, &response)
		benchmarksCh <- benchmarksResult{response.Data, err}
	}()
	go func() {
		var response struct {
			Data []uiScalingDecision `json:"data"`
		}
		err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/deployments/"+url.PathEscape(selected)+"/scaling-decisions?limit=20", "", nil, &response)
		scalingCh <- scalingResult{response.Data, err}
	}()
	view, events, benchmarks, scaling := <-viewCh, <-eventsCh, <-benchmarksCh, <-scalingCh
	for _, err := range []error{view.err, events.err, benchmarks.err, scaling.err} {
		if err != nil {
			snapshot.err = err
			return snapshot
		}
	}
	snapshot.view, snapshot.events, snapshot.benchmarks, snapshot.scaling = view.value, events.value, benchmarks.value, scaling.value
	return snapshot
}

func (m uiModel) runAction(action uiAction) tea.Cmd {
	return func() tea.Msg {
		var response struct {
			Operation domain.Operation `json:"operation"`
		}
		key := fmt.Sprintf("ui-%s-%s-%d", action.name, m.selectedName(), time.Now().UnixNano())
		err := controlJSON(m.ctx, m.cfg, action.method, action.path, key, action.body, &response)
		return uiActionResultMsg{operationID: response.Operation.ID, err: err}
	}
}

func (m uiModel) availableActions() []uiAction {
	name := m.selectedName()
	if name == "" {
		return nil
	}
	escaped := url.PathEscape(name)
	actions := []uiAction{
		{name: "request", label: "Send test request", detail: "Copy a safe request command", method: "COPY", path: "infercrane request " + name + " --stream"},
		{name: "benchmark", label: "Benchmark configuration", detail: "Copy reproducible benchmark command", method: "COPY", path: "infercrane benchmark " + name + " --requests 100 --concurrency 10"},
		{name: "scale", label: "Change capacity", detail: "Open semantic plan in the CLI", method: "COPY", path: "infercrane plan " + m.view.Deployment.Model + " --name " + name + " --min " + fmt.Sprint(m.view.Deployment.MinReplicas) + " --max " + fmt.Sprint(m.view.Deployment.MaxReplicas)},
	}
	if m.readOnly {
		return actions
	}
	if m.view.ActiveOperation != nil {
		actions = append([]uiAction{{name: "cancel", label: "Cancel active operation", detail: "Requests cooperative cancellation; infrastructure cleanup remains durable", method: http.MethodPost, path: "/api/v1/operations/" + url.PathEscape(m.view.ActiveOperation.ID) + "/cancel", body: struct{}{}}}, actions...)
	}
	candidate := m.view.Deployment.CandidateRevisionID
	if candidate != "" {
		actions = append(actions, uiAction{name: "evaluate", label: "Evaluate Release Guard", detail: "May send measured inference traffic and incur provider cost", method: http.MethodPost, path: "/api/v1/deployments/" + escaped + "/rollouts/guard/evaluate", body: struct{}{}})
		if currentGuardAccepts(m.view) {
			actions = append(actions, uiAction{name: "promote", label: "Promote accepted candidate", detail: "Routes the accepted immutable revision and safely drains old capacity", method: http.MethodPost, path: "/api/v1/deployments/" + escaped + "/rollouts/" + url.PathEscape(candidate) + "/promote", body: struct{}{}})
		}
	}
	return actions
}

func currentGuardAccepts(view deploymentView) bool {
	if len(view.ReleaseGuardEvaluations) == 0 || view.Deployment.CandidateRevisionID == "" {
		return false
	}
	g := view.ReleaseGuardEvaluations[0]
	decision := strings.ToLower(g.Decision)
	return g.CandidateRevisionID == view.Deployment.CandidateRevisionID && (decision == "promote" || decision == "pass" || decision == "accept")
}

func (m uiModel) selectedName() string {
	if m.selected >= 0 && m.selected < len(m.deployments) {
		return m.deployments[m.selected].Name
	}
	return ""
}

func (m uiModel) copyValue() string {
	if m.view.ActiveOperation != nil && m.view.ActiveOperation.ID != "" {
		return "infercrane operation watch " + m.view.ActiveOperation.ID
	}
	if m.view.Deployment.Name != "" {
		return strings.TrimRight(m.cfg.ControlURL, "/") + "/v1"
	}
	return ""
}

type uiStyles struct {
	accent, healthy, warning, failure, muted, text, border                color.Color
	title, label, subtle, selected, healthyText, warningText, failureText lipgloss.Style
}

func newUIStyles(dark bool) uiStyles {
	choose := lipgloss.LightDark(dark)
	styles := uiStyles{
		accent:  choose(lipgloss.Color("#1D4ED8"), lipgloss.Color("#60A5FA")),
		healthy: choose(lipgloss.Color("#087F5B"), lipgloss.Color("#44D7B6")),
		warning: choose(lipgloss.Color("#9A6700"), lipgloss.Color("#F5B942")),
		failure: choose(lipgloss.Color("#CF222E"), lipgloss.Color("#FF5667")),
		muted:   choose(lipgloss.Color("#626A73"), lipgloss.Color("#9CA3AD")),
		text:    choose(lipgloss.Color("#16181B"), lipgloss.Color("#F3F1EA")),
		border:  choose(lipgloss.Color("#D0D7DE"), lipgloss.Color("#30363D")),
	}
	styles.title = lipgloss.NewStyle().Foreground(styles.text).Bold(true)
	styles.label = lipgloss.NewStyle().Foreground(styles.muted).Width(14)
	styles.subtle = lipgloss.NewStyle().Foreground(styles.muted)
	styles.selected = lipgloss.NewStyle().Foreground(styles.accent).Bold(true)
	styles.healthyText = lipgloss.NewStyle().Foreground(styles.healthy)
	styles.warningText = lipgloss.NewStyle().Foreground(styles.warning)
	styles.failureText = lipgloss.NewStyle().Foreground(styles.failure)
	return styles
}

func (m uiModel) render() string {
	s := newUIStyles(m.dark)
	width := m.width
	if width < 40 {
		width = 80
	}
	mode := "operations workspace"
	if m.readOnly {
		mode += " · read-only"
	}
	header := s.title.Render("InferCrane") + "  " + s.subtle.Render(mode)
	connection := s.healthyText.Render("● connected")
	if m.errorMessage != "" {
		connection = s.failureText.Render("● reconnecting")
	} else if m.actionBusy {
		connection = s.warningText.Render("◐ queuing action")
	} else if m.loading {
		connection = s.warningText.Render("◌ refreshing")
	}
	header = header + strings.Repeat(" ", max(1, width-lipgloss.Width(header)-lipgloss.Width(connection)-1)) + connection
	separator := lipgloss.NewStyle().Foreground(s.border).Render(strings.Repeat("─", width))

	var body string
	if len(m.deployments) == 0 && m.errorMessage == "" {
		body = "\n" + s.title.Render("No deployments") + "\n\n" + s.subtle.Render("Create one with: infercrane deploy MODEL")
	} else if width >= 160 {
		leftWidth := 27
		mainWidth := min(92, width-leftWidth-50)
		contextWidth := width - leftWidth - mainWidth - 6
		body = lipgloss.JoinHorizontal(lipgloss.Top,
			m.renderDeployments(s, leftWidth), "   ",
			m.renderDetails(s, mainWidth), "   ",
			m.renderContext(s, contextWidth),
		)
	} else if width >= 100 {
		leftWidth := min(32, width/3)
		left := m.renderDeployments(s, leftWidth)
		right := m.renderDetails(s, width-leftWidth-3)
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, "   ", right)
	} else {
		body = m.renderCompact(s, width)
	}

	footer := "←/→ views  1–7 jump  j/k move  ctrl+k actions  r refresh  c copy  ? help  q quit"
	if width < 105 {
		footer = "←/→ views  1–7 jump  j/k move  ctrl+k actions  ? help  q quit"
	}
	if m.help {
		safety := "Actions show impact, require confirmation, and queue idempotent durable operations."
		if m.readOnly {
			safety = "Read-only mode: API mutations are disabled; copyable CLI workflows remain available."
		}
		footer = safety + "\n" + footer
	}
	if m.notice != "" {
		footer = m.notice + "\n" + footer
	}
	if m.errorMessage != "" {
		footer = s.failureText.Render("Connection: "+truncateText(m.errorMessage, max(20, width-12))) + "\n" + footer
	}
	if !m.lastUpdated.IsZero() {
		footer += "  · updated " + m.lastUpdated.Format("15:04:05")
	}
	footerLines := strings.Count(footer, "\n") + 1
	availableBodyHeight := m.height - 6 - footerLines
	if availableBodyHeight > lipgloss.Height(body) {
		body += strings.Repeat("\n", availableBodyHeight-lipgloss.Height(body))
	}
	result := header + "\n" + separator + "\n" + m.renderTabs(s, width) + "\n" + separator + "\n" + body + "\n" + separator + "\n" + s.subtle.Render(footer)
	if m.palette {
		return m.renderPalette(s, result)
	}
	if m.confirm != nil {
		return m.renderConfirmation(s, result)
	}
	return result
}

func (m uiModel) renderTabs(s uiStyles, width int) string {
	labels := uiTabs
	if width < 118 {
		labels = []string{"Home", "Ops", "Rollout", "Perf", "Infra", "Scale", "Events"}
	}
	if width < 72 {
		labels = []string{"H", "O", "R", "P", "I", "S", "E"}
	}
	parts := make([]string, 0, len(labels))
	for i := range uiTabs {
		label := labels[i]
		style := s.subtle
		if i == m.tab {
			style = s.selected
		}
		item := fmt.Sprintf(" %d %s ", i+1, label)
		if width < 72 {
			item = fmt.Sprintf("%d%s", i+1, label)
		}
		parts = append(parts, style.Render(item))
	}
	return strings.Join(parts, " ")
}

func (m uiModel) renderContext(s uiStyles, width int) string {
	lines := []string{s.title.Render("WORKSPACE"), ""}
	switch m.tab {
	case 1:
		lines = append(lines, s.subtle.Render("Durable execution and recovery."), "", uiRow(s, "Active", boolLabel(m.view.ActiveOperation != nil)))
	case 2:
		lines = append(lines, s.subtle.Render("Revision safety and promotion evidence."), "", uiRow(s, "Candidate", shortID(m.view.Deployment.CandidateRevisionID)), uiRow(s, "Guard", latestGuardDecision(m.view)))
	case 3:
		lines = append(lines, s.subtle.Render("Measured behavior; no prompt content stored."), "", uiRow(s, "Benchmarks", fmt.Sprint(len(m.benchmarks))), uiRow(s, "Cold starts", fmt.Sprint(m.view.ColdStartStats.ColdStarts)))
	case 4:
		provider, compute := deploymentInfrastructure(m.view)
		lines = append(lines, s.subtle.Render("Raw placement and artifact identity."), "", uiRow(s, "Provider", provider), uiRow(s, "Compute", compute))
	case 5:
		lines = append(lines, s.subtle.Render("Persisted capacity decisions."), "", uiRow(s, "Bounds", fmt.Sprintf("%d–%d", m.view.Deployment.MinReplicas, m.view.Deployment.MaxReplicas)), uiRow(s, "Decisions", fmt.Sprint(len(m.scaling))))
	case 6:
		lines = append(lines, s.subtle.Render("Full durable history; select an event for payload."), "", uiRow(s, "Events", fmt.Sprint(len(m.events))))
	default:
		lines = append(lines, s.subtle.Render("Live health and deterministic explanation."), "", uiRow(s, "Serving", strings.ToUpper(m.view.LifecycleStatus.ServingState)), uiRow(s, "Converged", strings.ToUpper(m.view.LifecycleStatus.ConvergenceState)))
	}
	lines = append(lines, "", s.title.Render("QUICK ACTIONS"), s.selected.Render("Ctrl-K")+"  applicable actions", "c       copy endpoint/resume", "r       refresh now", "?       safety and help", "", s.title.Render("DURABILITY"), s.subtle.Render("You can close this terminal. Control-plane operations continue and resume from persisted state."))
	return lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
}

func boolLabel(value bool) string {
	if value {
		return "yes"
	}
	return "none"
}

func latestGuardDecision(view deploymentView) string {
	if len(view.ReleaseGuardEvaluations) == 0 {
		return "not evaluated"
	}
	g := view.ReleaseGuardEvaluations[0]
	if g.CandidateRevisionID != view.Deployment.CandidateRevisionID {
		return "historical " + strings.ToUpper(g.Decision)
	}
	return strings.ToUpper(g.Decision)
}

func (m uiModel) renderDeployments(s uiStyles, width int) string {
	lines := []string{s.title.Render("DEPLOYMENTS"), ""}
	for index, deployment := range m.deployments {
		marker := "  "
		nameStyle := lipgloss.NewStyle().Foreground(s.text)
		if index == m.selected {
			marker, nameStyle = "› ", s.selected
		}
		status := statusDot(deployment.ObservedState, s)
		nameWidth := max(8, width-13)
		lines = append(lines, marker+nameStyle.Render(fmt.Sprintf("%-*s", nameWidth, truncateText(deployment.Name, nameWidth)))+" "+status)
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(lines, "\n"))
}

func (m uiModel) renderDetails(s uiStyles, width int) string {
	if m.view.Deployment.Name == "" {
		return s.subtle.Render("Select a deployment")
	}
	var content string
	switch m.tab {
	case 1:
		content = m.renderOperationView(s, width)
	case 2:
		content = m.renderRolloutView(s, width)
	case 3:
		content = m.renderPerformance(s, width)
	case 4:
		content = m.renderInfrastructure(s, width)
	case 5:
		content = m.renderScaling(s, width)
	case 6:
		content = m.renderEventView(s, width)
	default:
		content = strings.Join([]string{m.renderOverview(s), "", m.renderExplanation(s, width), "", m.renderOperation(s), "", m.renderGuard(s)}, "\n")
	}
	return lipgloss.NewStyle().Width(width).Render(content)
}

func (m uiModel) renderCompact(s uiStyles, width int) string {
	list := m.renderDeployments(s, width)
	if m.view.Deployment.Name == "" {
		return list
	}
	return list + "\n\n" + m.renderDetails(s, width)
}

func (m uiModel) renderOverview(s uiStyles) string {
	d, lifecycle := m.view.Deployment, m.view.LifecycleStatus
	provider, compute := deploymentInfrastructure(m.view)
	endpoint := strings.TrimRight(m.cfg.ControlURL, "/") + "/v1"
	traffic := fmt.Sprintf("%.2f req/s · %.2f%% errors", m.view.RequestStats.RequestsPerSecond, m.view.RequestStats.ErrorRate*100)
	if m.view.RequestStats.P95TTFTMS != nil {
		traffic += fmt.Sprintf(" · %.0fms TTFT p95", *m.view.RequestStats.P95TTFTMS)
	}
	return strings.Join([]string{
		s.title.Render(d.Name) + "  " + statusDot(lifecycle.ServingState, s),
		uiRow(s, "Model", d.Model),
		uiRow(s, "Endpoint", endpoint),
		uiRow(s, "Revision", shortID(d.ActiveRevisionID)),
		uiRow(s, "Provider", provider),
		uiRow(s, "Compute", compute),
		uiRow(s, "Runtime", d.Runtime),
		uiRow(s, "Replicas", fmt.Sprintf("%d/%d ready · %d provisioning · %d draining", lifecycle.ReadyReplicas, lifecycle.DesiredReplicas, lifecycle.ProvisioningReplicas, lifecycle.DrainingReplicas)),
		uiRow(s, "Traffic", traffic),
	}, "\n")
}

func deploymentInfrastructure(view deploymentView) (string, string) {
	provider, compute := "existing targets", "elastic"
	for _, revision := range view.Revisions {
		if revision.ID != view.Deployment.ActiveRevisionID {
			continue
		}
		var metadata struct {
			Cloud       string `json:"cloud"`
			ComputeMode string `json:"compute_mode"`
		}
		if json.Unmarshal(revision.Spec, &metadata) == nil {
			provider = emptyAs(metadata.Cloud, provider)
			compute = emptyAs(metadata.ComputeMode, compute)
		}
		break
	}
	return provider, compute
}

func (m uiModel) renderOperation(s uiStyles) string {
	lines := []string{s.title.Render("ACTIVE OPERATION")}
	operation := m.view.ActiveOperation
	if operation == nil {
		return strings.Join(append(lines, s.subtle.Render("None · deployment is converged")), "\n")
	}
	lines = append(lines,
		uiRow(s, "Phase", operationPhase(*operation)),
		uiRow(s, "Operation", shortID(operation.ID)),
		uiRow(s, "Progress", fmt.Sprintf("%d%% · check %d/%d", operation.Progress, operation.Attempt, operation.MaxAttempts)),
		uiRow(s, "Reason", operation.Message),
	)
	if operation.NextAttemptAt != nil {
		lines = append(lines, uiRow(s, "Next check", operation.NextAttemptAt.Format("15:04:05")))
	}
	return strings.Join(lines, "\n")
}

func (m uiModel) renderGuard(s uiStyles) string {
	lines := []string{s.title.Render("RELEASE GUARD")}
	if len(m.view.ReleaseGuardEvaluations) == 0 {
		return strings.Join(append(lines, s.subtle.Render("No persisted evaluation")), "\n")
	}
	guard := m.view.ReleaseGuardEvaluations[0]
	evaluationState := "CURRENT"
	if m.view.Deployment.CandidateRevisionID == "" || m.view.Deployment.CandidateRevisionID != guard.CandidateRevisionID {
		evaluationState = "HISTORICAL"
	}
	decision := strings.ToUpper(guard.Decision)
	decisionStyle := s.warningText
	if guard.Decision == "promote" || guard.Decision == "pass" {
		decisionStyle = s.healthyText
	} else if guard.Decision == "reject" || guard.Decision == "fail" {
		decisionStyle = s.failureText
	}
	return strings.Join(append(lines,
		uiRow(s, "Evaluation", evaluationState),
		uiRow(s, "Active", shortID(guard.ActiveRevisionID)),
		uiRow(s, "Candidate", shortID(guard.CandidateRevisionID)),
		s.label.Render("Decision")+decisionStyle.Render(decision),
	), "\n")
}

func (m uiModel) renderExplanation(s uiStyles, width int) string {
	reason := "Deployment is converged and no persisted blocker is active."
	if m.view.ActiveOperation != nil {
		reason = fmt.Sprintf("%s is blocking convergence: %s", operationPhase(*m.view.ActiveOperation), m.view.ActiveOperation.Message)
	} else if m.view.LifecycleStatus.ServingState == "unhealthy" || m.view.LifecycleStatus.ServingState == "degraded" {
		reason = fmt.Sprintf("Serving is %s with %d ready replica(s), %d provisioning, and %d unhealthy target(s).", m.view.LifecycleStatus.ServingState, m.view.LifecycleStatus.ReadyReplicas, m.view.LifecycleStatus.ProvisioningReplicas, m.view.LifecycleStatus.UnhealthyTargets)
	} else if len(m.view.ReleaseGuardEvaluations) > 0 {
		guard := m.view.ReleaseGuardEvaluations[0]
		if guard.Decision != "" {
			reason = "Latest Release Guard decision is " + strings.ToUpper(guard.Decision) + "."
			if len(guard.Reasons) > 0 && string(guard.Reasons) != "null" && string(guard.Reasons) != "[]" {
				reason += " Persisted reasons: " + string(guard.Reasons)
			}
		}
	}
	return s.title.Render("EXPLANATION") + "\n" + truncateText(reason, max(20, width))
}

func (m uiModel) renderEvents(s uiStyles, limit, width int) string {
	lines := []string{s.title.Render("EVENTS")}
	if len(m.events) == 0 {
		return strings.Join(append(lines, s.subtle.Render("No persisted events")), "\n")
	}
	for index, event := range m.events {
		if index >= limit {
			break
		}
		line := uiEventTime(event.CreatedAt, time.Now()) + "  " + fmt.Sprintf("%-20s", truncateText(event.Type, 20)) + " " + event.Summary
		lines = append(lines, truncateText(line, max(20, width)))
	}
	return strings.Join(lines, "\n")
}

func (m uiModel) renderOperationView(s uiStyles, width int) string {
	content := m.renderOperation(s)
	if m.view.ActiveOperation == nil {
		return content + "\n\n" + s.title.Render("RECOVERY") + "\n" + s.subtle.Render("No recovery action is needed. Durable state is converged.")
	}
	op := m.view.ActiveOperation
	return content + "\n\n" + s.title.Render("RECOVERY") + "\n" +
		uiRow(s, "Retryable", fmt.Sprint(op.Retryable)) + "\n" +
		uiRow(s, "Cancel", fmt.Sprint(op.CancelRequested)) + "\n" +
		uiRow(s, "Resume", "infercrane operation watch "+op.ID) + "\n\n" +
		s.subtle.Render("Closing this terminal never stops the durable operation. Use ctrl+k for applicable controls.")
}

func (m uiModel) renderRolloutView(s uiStyles, width int) string {
	lines := []string{s.title.Render("IMMUTABLE REVISIONS")}
	for _, revision := range m.view.Revisions {
		marker := "  "
		if revision.ID == m.view.Deployment.ActiveRevisionID {
			marker = "● "
		}
		if revision.ID == m.view.Deployment.CandidateRevisionID {
			marker = "◐ "
		}
		lines = append(lines, fmt.Sprintf("%srev-%d  %-11s %s", marker, revision.Number, strings.ToUpper(revision.Status), shortID(revision.ID)))
		if revision.Reason != "" {
			lines = append(lines, "   "+s.subtle.Render(truncateText(revision.Reason, width-3)))
		}
	}
	lines = append(lines, "", m.renderGuard(s), "", s.title.Render("POLICY"),
		uiRow(s, "Minimum evidence", fmt.Sprintf("%d requests", m.view.ReleaseGuardPolicy.MinimumRequests)),
		uiRow(s, "TTFT regression", fmt.Sprintf("≤ %.1f%%", m.view.ReleaseGuardPolicy.MaxTTFTRegressionPercent)),
		uiRow(s, "Latency regression", fmt.Sprintf("≤ %.1f%%", m.view.ReleaseGuardPolicy.MaxLatencyRegressionPercent)))
	return strings.Join(lines, "\n")
}

func metric(value *float64, unit string) string {
	if value == nil {
		return "not measured"
	}
	return fmt.Sprintf("%.1f%s", *value, unit)
}

func (m uiModel) renderPerformance(s uiStyles, width int) string {
	r, c := m.view.RequestStats, m.view.ColdStartStats
	lines := []string{s.title.Render("LIVE REQUESTS"),
		uiRow(s, "Request rate", fmt.Sprintf("%.2f req/s", r.RequestsPerSecond)), uiRow(s, "Errors", fmt.Sprintf("%.2f%%", r.ErrorRate*100)),
		uiRow(s, "TTFT p50 / p95", metric(r.P50TTFTMS, "ms")+" / "+metric(r.P95TTFTMS, "ms")),
		uiRow(s, "Latency p50/p95", metric(r.P50LatencyMS, "ms")+" / "+metric(r.P95LatencyMS, "ms")),
		uiRow(s, "Output", fmt.Sprintf("%.1f tok/s", r.OutputTokensPerSecond)), "", s.title.Render("COLD STARTS"),
		uiRow(s, "Evidence", fmt.Sprintf("%d classified · %d cold · %d warm", c.ClassifiedRequests, c.ColdStarts, c.WarmRequests)),
		uiRow(s, "Cold TTFT p95", metric(c.ColdTTFTP95MS, "ms")), uiRow(s, "Warm TTFT p95", metric(c.WarmTTFTP95MS, "ms")),
		uiRow(s, "Time ready p95", metric(c.TimeToReadyP95MS, "ms")), uiRow(s, "Bottleneck", emptyAs(c.BottleneckCode, "not established")),
		"", s.title.Render("BENCHMARK HISTORY")}
	if len(m.benchmarks) == 0 {
		lines = append(lines, s.subtle.Render("No persisted benchmark. ctrl+k copies a reproducible command."))
	}
	for _, b := range m.benchmarks {
		lines = append(lines, fmt.Sprintf("%s  %d/%d succeeded  TTFT p95 %s  output %s", uiEventTime(b.CreatedAt, time.Now()), b.Succeeded, b.RequestCount, metric(b.TTFTP95MS, "ms"), metric(b.OutputTokenThroughput, " tok/s")))
	}
	return strings.Join(lines, "\n")
}

func (m uiModel) renderInfrastructure(s uiStyles, width int) string {
	provider, compute := deploymentInfrastructure(m.view)
	lines := []string{s.title.Render("PLACEMENT"), uiRow(s, "Provider adapter", provider), uiRow(s, "Compute mode", compute), uiRow(s, "Routing", m.view.Deployment.RoutingStrategy), "", s.title.Render("TARGETS")}
	if len(m.view.Targets) == 0 {
		lines = append(lines, s.subtle.Render("No published targets"))
	}
	for _, t := range m.view.Targets {
		lines = append(lines, fmt.Sprintf("%s  %-16s %-10s %s", statusDot(t.Health, s), truncateText(t.Name, 16), emptyAs(t.Provider, "external"), truncateText(t.URL, max(12, width-46))))
	}
	lines = append(lines, "", s.title.Render("REPLICAS"))
	if len(m.view.Replicas) == 0 {
		lines = append(lines, s.subtle.Render("Existing-target deployment · no provider-managed replicas"))
	}
	for _, r := range m.view.Replicas {
		lines = append(lines, fmt.Sprintf("%s  ordinal %-2d %-12s resource %s", statusDot(r.Health, s), r.Ordinal, r.LifecycleState, shortID(r.ProviderResourceID)))
	}
	lines = append(lines, "", s.title.Render("MODEL ARTIFACT"))
	if len(m.view.ModelArtifacts) == 0 {
		lines = append(lines, s.subtle.Render("No immutable artifact identity persisted"))
	} else {
		a := m.view.ModelArtifacts[0]
		lines = append(lines, uiRow(s, "Repository", a.Repository), uiRow(s, "Immutable", shortID(a.ImmutableRevision)), uiRow(s, "Cache", emptyAs(a.CacheState, "unknown")))
	}
	return strings.Join(lines, "\n")
}

func (m uiModel) renderScaling(s uiStyles, width int) string {
	d := m.view.Deployment
	lines := []string{s.title.Render("AUTOSCALING"), uiRow(s, "Enabled", fmt.Sprint(d.AutoscalingEnabled)), uiRow(s, "Bounds", fmt.Sprintf("%d → %d replicas", d.MinReplicas, d.MaxReplicas)), uiRow(s, "Current", fmt.Sprintf("%d ready · %d provisioning", m.view.LifecycleStatus.ReadyReplicas, m.view.LifecycleStatus.ProvisioningReplicas)), "", s.title.Render("PERSISTED DECISIONS")}
	if len(m.scaling) == 0 {
		lines = append(lines, s.subtle.Render("No scaling decisions have been persisted."))
	}
	for _, decision := range m.scaling {
		lines = append(lines, fmt.Sprintf("%s  %-10s %d → %d", uiEventTime(decision.CreatedAt, time.Now()), strings.ToUpper(decision.Action), decision.OldReplicas, decision.NewReplicas), "   "+s.subtle.Render(truncateText(decision.Reason, max(20, width-3))))
	}
	return strings.Join(lines, "\n")
}

func (m uiModel) renderEventView(s uiStyles, width int) string {
	lines := []string{s.title.Render("DURABLE EVENT TIMELINE")}
	if len(m.events) == 0 {
		return strings.Join(append(lines, s.subtle.Render("No persisted events")), "\n")
	}
	for i, event := range m.events {
		marker := "  "
		style := lipgloss.NewStyle().Foreground(s.text)
		if i == m.eventCursor {
			marker = "› "
			style = s.selected
		}
		lines = append(lines, marker+style.Render(uiEventTime(event.CreatedAt, time.Now())+"  "+event.Type)+"  "+truncateText(event.Summary, max(15, width-35)))
	}
	event := m.events[m.eventCursor]
	lines = append(lines, "", s.title.Render("SELECTED EVENT"), uiRow(s, "Time", event.CreatedAt.Format(time.RFC3339)), uiRow(s, "Type", event.Type), uiRow(s, "Summary", event.Summary))
	if len(event.Payload) > 0 && string(event.Payload) != "null" {
		lines = append(lines, uiRow(s, "Payload", truncateText(string(event.Payload), max(20, width-14))))
	}
	return strings.Join(lines, "\n")
}

func (m uiModel) renderPalette(s uiStyles, background string) string {
	actions := m.availableActions()
	lines := []string{s.title.Render("ACTIONS"), s.subtle.Render("Only actions valid for the selected deployment are shown."), ""}
	for i, a := range actions {
		marker := "  "
		style := lipgloss.NewStyle().Foreground(s.text)
		if i == m.paletteIndex {
			marker = "› "
			style = s.selected
		}
		lines = append(lines, marker+style.Render(a.label), "   "+s.subtle.Render(a.detail))
	}
	if len(actions) == 0 {
		lines = append(lines, s.subtle.Render("No applicable actions"))
	}
	lines = append(lines, "", s.subtle.Render("enter select · esc close"))
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(s.border).Padding(1, 2).Width(min(72, max(36, m.width-8))).Render(strings.Join(lines, "\n"))
	return background + "\n" + box
}

func (m uiModel) renderConfirmation(s uiStyles, background string) string {
	a := m.confirm
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(s.warning).Padding(1, 2).Width(min(72, max(36, m.width-8))).Render(strings.Join([]string{s.warningText.Render("CONFIRM " + strings.ToUpper(a.label)), "", a.detail, "", s.subtle.Render("A durable operation will be persisted. Closing the terminal will not cancel it."), "", s.title.Render("y / enter confirm") + "  " + s.subtle.Render("n / esc cancel")}, "\n"))
	return background + "\n" + box
}

func uiEventTime(stamp, now time.Time) string {
	if stamp.IsZero() {
		return "--:--:--"
	}
	localStamp, localNow := stamp.In(now.Location()), now
	if localStamp.Year() == localNow.Year() && localStamp.YearDay() == localNow.YearDay() {
		return localStamp.Format("15:04:05")
	}
	return localStamp.Format("01-02 15:04")
}

func uiRow(styles uiStyles, label, value string) string {
	return styles.label.Render(label) + value
}

func statusDot(status string, styles uiStyles) string {
	upper := strings.ToUpper(emptyAs(status, "unknown"))
	switch strings.ToLower(status) {
	case "healthy", "ready", "active", "serving", "converged", "succeeded", "pass":
		return styles.healthyText.Render("● " + upper)
	case "failed", "unhealthy", "error", "cancelled", "reject":
		return styles.failureText.Render("● " + upper)
	case "degraded", "warning", "draining", "converging", "waiting":
		return styles.warningText.Render("● " + upper)
	default:
		return styles.subtle.Render("○ " + upper)
	}
}

func shortID(value string) string {
	if value == "" {
		return "none"
	}
	if len(value) <= 16 {
		return value
	}
	return value[:13] + "…"
}

func truncateText(value string, width int) string {
	if width < 1 {
		return ""
	}
	runes := []rune(strings.ReplaceAll(value, "\n", " "))
	if len(runes) <= width {
		return string(runes)
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}
