package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"net/http"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/infercrane/infercrane/internal/config"
)

const uiRefreshInterval = 2 * time.Second

type uiSnapshot struct {
	deployments []deploymentSummary
	view        deploymentView
	events      []cliEvent
	selected    string
	err         error
	sequence    int
}

type uiSnapshotMsg uiSnapshot
type uiTickMsg time.Time

type uiModel struct {
	ctx          context.Context
	cfg          config.Config
	deployments  []deploymentSummary
	view         deploymentView
	events       []cliEvent
	selected     int
	width        int
	height       int
	dark         bool
	loading      bool
	help         bool
	explain      bool
	lastUpdated  time.Time
	errorMessage string
	notice       string
	requestSeq   int
}

func uiCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: infercrane ui")
	}
	stdin, err := os.Stdin.Stat()
	if err != nil || stdin.Mode()&os.ModeCharDevice == 0 {
		return errors.New("infercrane ui requires an interactive terminal; use infercrane deployments, status, events, or --output json")
	}
	stdout, err := os.Stdout.Stat()
	if err != nil || stdout.Mode()&os.ModeCharDevice == 0 {
		return errors.New("infercrane ui requires an interactive terminal and cannot be redirected; use --output json commands for automation")
	}
	program := tea.NewProgram(uiModel{ctx: ctx, cfg: cfg, loading: true, dark: true}, tea.WithContext(ctx))
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
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "?":
			m.help = !m.help
		case "e":
			m.explain = !m.explain
		case "up", "k":
			if m.selected > 0 {
				m.selected--
				m.loading = true
				m.requestSeq++
				return m, m.load(m.deployments[m.selected].Name, m.requestSeq)
			}
		case "down", "j":
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
		m.deployments, m.view, m.events = msg.deployments, msg.view, msg.events
		m.lastUpdated = time.Now()
		for index, deployment := range m.deployments {
			if deployment.Name == msg.selected {
				m.selected = index
				break
			}
		}
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
	var err error
	snapshot.view, err = fetchDeployment(ctx, cfg, selected)
	if err != nil {
		snapshot.err = err
		return snapshot
	}
	snapshot.events, snapshot.err = deploymentEvents(ctx, cfg, selected)
	return snapshot
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
	header := s.title.Render("InferCrane") + "  " + s.subtle.Render("operations console · read-only")
	connection := s.healthyText.Render("● connected")
	if m.errorMessage != "" {
		connection = s.failureText.Render("● reconnecting")
	} else if m.loading {
		connection = s.warningText.Render("◌ refreshing")
	}
	header = header + strings.Repeat(" ", max(1, width-lipgloss.Width(header)-lipgloss.Width(connection)-1)) + connection
	separator := lipgloss.NewStyle().Foreground(s.border).Render(strings.Repeat("─", width))

	var body string
	if len(m.deployments) == 0 && m.errorMessage == "" {
		body = "\n" + s.title.Render("No deployments") + "\n\n" + s.subtle.Render("Create one with: infercrane deploy MODEL")
	} else if width >= 100 {
		leftWidth := min(32, width/3)
		left := m.renderDeployments(s, leftWidth)
		right := m.renderDetails(s, width-leftWidth-3)
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, "   ", right)
	} else {
		body = m.renderCompact(s, width)
	}

	footer := "j/k move  r refresh  e explain  c copy endpoint/resume  ? help  q quit"
	if m.help {
		footer = "Read-only: no key creates, scales, promotes, rejects, cancels, or deletes resources.\n" + footer
	}
	if m.notice != "" {
		footer = m.notice + "\n" + footer
	}
	if m.errorMessage != "" {
		footer = s.failureText.Render("Connection: "+truncateText(m.errorMessage, max(20, width-12))) + "\n" + footer
	}
	if !m.lastUpdated.IsZero() {
		footer += s.subtle.Render("  · updated " + m.lastUpdated.Format("15:04:05"))
	}
	return header + "\n" + separator + "\n" + body + "\n" + separator + "\n" + s.subtle.Render(footer)
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
	sections := []string{
		m.renderOverview(s),
		"",
		m.renderOperation(s),
		"",
		m.renderGuard(s),
	}
	if m.explain {
		sections = append(sections, "", m.renderExplanation(s, width))
	}
	eventLimit := min(6, max(2, m.height-27))
	sections = append(sections, "", m.renderEvents(s, eventLimit, width))
	return lipgloss.NewStyle().Width(width).Render(strings.Join(sections, "\n"))
}

func (m uiModel) renderCompact(s uiStyles, width int) string {
	list := m.renderDeployments(s, width)
	if m.view.Deployment.Name == "" {
		return list
	}
	detail := list + "\n\n" + m.renderOverview(s) + "\n\n" + m.renderOperation(s) + "\n\n" + m.renderGuard(s)
	if m.explain {
		detail += "\n\n" + m.renderExplanation(s, width)
	}
	return detail + "\n\n" + m.renderEvents(s, 4, width)
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
