package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/optimizationreport"
)

// optimize report prints the outcome of one campaign: the current setup
// against the winning setup on cost, speed, and answer quality, then the
// measurement that proves it. It only reads the control API.

const optimizeReportUsage = "usage: infercrane optimize report CAMPAIGN [--output text|json] [--html FILE]"

func optimizeReportCommand(ctx context.Context, args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return errors.New(optimizeReportUsage)
	}
	id := args[0]
	fs := newFlagSet("optimize report")
	output := fs.String("output", "text", "text or json")
	htmlPath := fs.String("html", "", "also write a self-contained HTML page to FILE")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || (*output != "text" && *output != "json") {
		return errors.New(optimizeReportUsage)
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return err
	}
	raw, report, err := loadOptimizationReport(ctx, cfg, id)
	if err != nil {
		return err
	}
	if err = writeOptimizationReport(os.Stdout, os.Stderr, raw, report, *output, *htmlPath, outputTheme(*output)); err != nil {
		return err
	}
	if *output == "text" {
		printResourceLink(cfg, "run", id)
	}
	return nil
}

// loadOptimizationReport returns the report exactly as the control plane sent
// it, plus its decoded form for rendering.
func loadOptimizationReport(ctx context.Context, cfg config.Config, id string) (json.RawMessage, optimizationreport.Report, error) {
	var response struct {
		Report json.RawMessage `json:"report"`
	}
	if err := controlJSON(ctx, cfg, http.MethodGet, "/api/v1/optimization/campaigns/"+url.PathEscape(id)+"/report", "", nil, &response); err != nil {
		return nil, optimizationreport.Report{}, err
	}
	var report optimizationreport.Report
	if len(response.Report) == 0 || json.Unmarshal(response.Report, &report) != nil || report.SchemaVersion != optimizationreport.SchemaVersion {
		return nil, optimizationreport.Report{}, errors.New("the control plane returned an unsupported optimization report")
	}
	return response.Report, report, nil
}

func writeOptimizationReport(stdout, stderr io.Writer, raw json.RawMessage, report optimizationreport.Report, output, htmlPath string, t theme) error {
	if htmlPath != "" {
		page, err := optimizationreport.HTML(report)
		if err != nil {
			return fmt.Errorf("render HTML report: %w", err)
		}
		if err = os.WriteFile(htmlPath, page, 0o644); err != nil {
			return fmt.Errorf("write HTML report: %w", err)
		}
		fmt.Fprintf(stderr, "HTML report written to %s\n", htmlPath)
	}
	if output == "json" {
		var indented bytes.Buffer
		if err := json.Indent(&indented, raw, "", "  "); err != nil {
			return fmt.Errorf("format report JSON: %w", err)
		}
		indented.WriteByte('\n')
		_, err := stdout.Write(indented.Bytes())
		return err
	}
	if !t.styled {
		_, err := io.WriteString(stdout, optimizationreport.Text(report))
		return err
	}
	_, err := io.WriteString(stdout, renderStyledReport(optimizationreport.NewView(report), t))
	return err
}

// renderStyledReport lays out the same view as optimizationreport.Text as
// terminal cards: the verdict first, one card per outcome, then aligned tables
// of how it was measured and every setup. It formats nothing itself, so every
// value and every "unknown" is the report's own.
func renderStyledReport(v optimizationreport.View, t theme) string {
	t.width = min(t.width, 110)
	var b strings.Builder
	b.WriteString(t.title(v.Title) + "\n")
	for _, line := range wrapWords(v.Subtitle, t.width) {
		b.WriteString(t.muted(line) + "\n")
	}
	b.WriteString("\n")
	if v.Banner != "" {
		b.WriteString(t.render(lipgloss.NewStyle().Bold(true).Foreground(t.toneColor(toneWatch)), "▲ "+v.Banner) + "\n\n")
	}
	kind, accent := outcomeBadge(v.Kind)
	headline := []string{t.tone(accent, "● "+kind)}
	for _, line := range wrapWords(v.Headline, t.width-4) {
		headline = append(headline, t.title(line))
	}
	b.WriteString(t.card(strings.Join(headline, "\n"), t.width, accent) + "\n\n")
	b.WriteString(renderOutcomeCards(v, t))
	b.WriteString("\n" + t.section("How it was measured") + "\n")
	names, values := make([]string, 0, len(v.Measurement)), make([]string, 0, len(v.Measurement))
	nameWidth := 0
	for _, line := range v.Measurement {
		name, value, ok := strings.Cut(line, ": ")
		if !ok {
			name, value = "", line
		}
		names, values = append(names, name), append(values, value)
		nameWidth = max(nameWidth, lipgloss.Width(name))
	}
	b.WriteString(t.definitions(2, nameWidth, names, values, nil))
	if len(v.Candidates) > 0 {
		b.WriteString("\n" + t.section("Every setup measured") + "\n")
		b.WriteString(renderSetupTable(v.Candidates, t))
	}
	if len(v.Signals) > 0 {
		b.WriteString("\n" + t.section("Bottleneck signals") + "\n")
		for _, line := range wrapWords(v.SignalsNote, t.width-2) {
			b.WriteString("  " + t.muted(line) + "\n")
		}
		rows := make([][]string, 0, len(v.Signals))
		for _, signal := range v.Signals {
			rows = append(rows, []string{signal.Name, signal.Stat, signal.Baseline, signal.Winner})
		}
		b.WriteString(t.table(2, []column{{header: "Signal", wrap: true}, {header: "Stat"}, {header: "Current", right: true}, {header: "Winner", right: true}}, rows, func(_, _ int, text string) string {
			if text == "unknown" {
				return t.muted(text)
			}
			return text
		}))
	}
	for _, block := range []struct {
		title  string
		fields []optimizationreport.ViewField
	}{{v.WinnerTitle, v.Winner}, {v.BaselineTitle, v.Baseline}} {
		if len(block.fields) == 0 {
			continue
		}
		b.WriteString("\n" + t.section(block.title) + "\n")
		b.WriteString(renderSetupFields(block.fields, t))
	}
	b.WriteString("\n" + t.section("Evidence") + "\n")
	evidenceNames, evidenceValues := make([]string, 0, len(v.Evidence)), make([]string, 0, len(v.Evidence))
	for _, field := range v.Evidence {
		evidenceNames, evidenceValues = append(evidenceNames, field.Name), append(evidenceValues, field.Value)
	}
	b.WriteString(t.definitions(2, 18, evidenceNames, evidenceValues, nil))
	if len(v.Unknown) > 0 {
		b.WriteString("\n" + t.section("Unknown, never estimated") + "\n")
		for _, field := range v.Unknown {
			for index, line := range wrapWords(field.Value, t.width-4) {
				prefix := "    "
				if index == 0 {
					prefix = "  " + t.tone(toneUnknown, "○") + " "
				}
				b.WriteString(prefix + t.muted(line) + "\n")
			}
		}
	}
	return b.String()
}

// outcomeBadge names the report's outcome kind and the tone of its card.
func outcomeBadge(kind string) (string, string) {
	switch kind {
	case optimizationreport.OutcomeWinnerSelected:
		return "WINNER FOUND", toneGood
	case optimizationreport.OutcomeBaselineKept:
		return "CURRENT SETUP KEPT", toneNeutral
	case optimizationreport.OutcomeNoWinner:
		return "NO WINNER", toneWatch
	case optimizationreport.OutcomeInProgress:
		return "IN PROGRESS", toneInfo
	case optimizationreport.OutcomeCancelled:
		return "CANCELLED", toneBad
	}
	return strings.ToUpper(strings.ReplaceAll(emptyAs(kind, "unknown"), "_", " ")), toneUnknown
}

// renderOutcomeCards draws one card per outcome: current → winner, then the
// change, the decision, and the noise band it was checked against.
func renderOutcomeCards(v optimizationreport.View, t theme) string {
	columns := 1
	if t.width >= 76 {
		columns = 2
	}
	if t.width >= 108 {
		columns = 3
	}
	cardWidth := (t.width - (columns - 1)) / columns
	inner := cardWidth - 4
	cards := make([]string, 0, len(v.Rows))
	tones := make([]string, 0, len(v.Rows))
	for _, row := range v.Rows {
		var lines []string
		labels := []string{row.Label}
		if head, tail, ok := strings.Cut(row.Label, " · "); ok && lipgloss.Width(row.Label) > inner {
			labels = []string{head, tail}
		}
		for _, label := range labels {
			for _, line := range wrapWords(label, inner) {
				lines = append(lines, t.muted(line))
			}
		}
		current, winner := row.Baseline, row.Winner
		paintValue := func(text string, strong bool) string {
			if text == "unknown" {
				return t.tone(toneUnknown, text)
			}
			if strong {
				return t.title(text)
			}
			return text
		}
		if lipgloss.Width(current+" → "+winner) <= inner {
			lines = append(lines, paintValue(current, false)+t.muted(" → ")+paintValue(winner, true))
		} else {
			lines = append(lines, paintValue(current, false), t.muted("→ ")+paintValue(winner, true))
		}
		verdict := t.delta(row.Change, row.Tone)
		if row.Change == "unknown" {
			verdict = t.muted("change ") + verdict
		}
		if row.Decision != "" {
			decision := t.tone(row.Tone, row.Decision)
			if row.Decision == strings.ToUpper(row.Decision) {
				decision = t.tone(row.Tone, "● "+row.Decision)
			}
			verdict += "  " + decision
		}
		if row.Noise != "" {
			if chip := t.chip(row.Noise); lipgloss.Width(verdict)+2+lipgloss.Width(chip) <= inner {
				verdict += "  " + chip
			} else {
				lines = append(lines, verdict)
				verdict = ""
				for _, line := range wrapWords(row.Noise, inner) {
					lines = append(lines, t.muted(line))
				}
			}
		}
		if verdict != "" {
			lines = append(lines, verdict)
		}
		if row.Reason != "" && row.Tone == toneUnknown {
			for index, line := range wrapWords(row.Reason, inner-2) {
				prefix := "  "
				if index == 0 {
					prefix = "└ "
				}
				lines = append(lines, t.muted(prefix+line))
			}
		}
		cards = append(cards, strings.Join(lines, "\n"))
		tones = append(tones, row.Tone)
	}
	var b strings.Builder
	for start := 0; start < len(cards); start += columns {
		end := min(start+columns, len(cards))
		height := 0
		for _, card := range cards[start:end] {
			height = max(height, lipgloss.Height(card))
		}
		rendered := make([]string, 0, columns*2)
		for index, card := range cards[start:end] {
			if gap := height - lipgloss.Height(card); gap > 0 {
				card += strings.Repeat("\n", gap)
			}
			if index > 0 {
				rendered = append(rendered, " ")
			}
			rendered = append(rendered, t.card(card, cardWidth, tones[start+index]))
		}
		b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, rendered...) + "\n")
	}
	return b.String()
}

// renderSetupTable lists every measured setup with its numbers in aligned
// columns. Runtime flags go one per line under the setup, so long flag
// strings wrap at flag boundaries. Below 64 columns the numbers move under
// the setup instead.
func renderSetupTable(candidates []optimizationreport.ViewCandidate, t theme) string {
	const indent, gap = "  ", 2
	headers := []string{"TOKENS/S", "P95 TTFT", "$/1M OUT"}
	widths := []int{len(headers[0]), len(headers[1]), len(headers[2])}
	numbers := t.width >= 64
	for _, candidate := range candidates {
		for index, value := range []string{candidate.TokensPerSecond, candidate.TTFTP95, candidate.CostPerMillion} {
			widths[index] = max(widths[index], lipgloss.Width(value))
		}
	}
	setupWidth := t.width - len(indent)
	if numbers {
		setupWidth -= widths[0] + widths[1] + widths[2] + 3*gap
	}
	numberCells := func(values []string, paint func(string) string) string {
		if !numbers {
			return ""
		}
		var cells strings.Builder
		for index, value := range values {
			cells.WriteString(strings.Repeat(" ", gap+widths[index]-lipgloss.Width(value)) + paint(value))
		}
		return cells.String()
	}
	var b strings.Builder
	b.WriteString(indent + t.muted(strings.TrimRight(padRight("SETUP", setupWidth)+numberCells(headers, func(text string) string { return text }), " ")) + "\n")
	if t.styled {
		b.WriteString(indent + t.foreground(t.palette().line, strings.Repeat("─", t.width-len(indent))) + "\n")
	}
	detail := t.width - 6
	for _, candidate := range candidates {
		engine, args := splitSetupLabel(candidate.Label)
		if args == "" || !isFlagWord(strings.Fields(args)[0]) {
			engine, args = candidate.Label, ""
		}
		role := strings.ToUpper(candidate.Role)
		heads := wrapWords(engine, setupWidth)
		roleLine := ""
		if role != "" {
			if last := heads[len(heads)-1]; lipgloss.Width(last)+2+lipgloss.Width(role) <= setupWidth {
				heads[len(heads)-1] = last + "  " + role
			} else {
				roleLine = role
			}
		}
		for index, head := range heads {
			painted := t.title(head)
			if role != "" && roleLine == "" && index == len(heads)-1 {
				painted = t.title(strings.TrimSuffix(head, "  "+role)) + "  " + t.tone(toneInfo, role)
			}
			line := indent + painted
			if index == 0 && numbers {
				line += strings.Repeat(" ", setupWidth-lipgloss.Width(head)) + numberCells([]string{candidate.TokensPerSecond, candidate.TTFTP95, candidate.CostPerMillion}, func(text string) string {
					if text == "unknown" {
						return t.muted(text)
					}
					return text
				})
			}
			b.WriteString(strings.TrimRight(line, " ") + "\n")
		}
		if roleLine != "" {
			b.WriteString(indent + t.tone(toneInfo, roleLine) + "\n")
		}
		for _, flag := range wrapFlags(args, detail) {
			b.WriteString("    " + t.muted(flag) + "\n")
		}
		if !numbers {
			for _, measured := range wrapWords(candidate.Measured, detail) {
				b.WriteString("    " + measured + "\n")
			}
		}
		for index, status := range wrapWords(candidate.Status, detail-2) {
			prefix := "      "
			if index == 0 {
				prefix = "    " + t.tone(statusTone(candidate.State), "●") + " "
			}
			b.WriteString(prefix + status + "\n")
		}
		for _, versus := range wrapWords(candidate.Versus, detail) {
			if versus != "" {
				b.WriteString("    " + t.tone(candidate.Tone, versus) + "\n")
			}
		}
	}
	return b.String()
}

// splitSetupLabel separates a setup label such as "vllm, --kv-cache-dtype fp8"
// into the engine and its arguments.
func splitSetupLabel(label string) (string, string) {
	engine, args, ok := strings.Cut(label, ", ")
	if !ok {
		return label, ""
	}
	return engine, args
}

// renderSetupFields lists one exact setup; its runtime arguments go one flag
// per line.
func renderSetupFields(fields []optimizationreport.ViewField, t theme) string {
	var b strings.Builder
	for _, field := range fields {
		if field.Name == "Args" && field.Value != "default settings" {
			for index, flag := range wrapFlags(field.Value, t.width-14) {
				name := ""
				if index == 0 {
					name = field.Name
				}
				b.WriteString("  " + t.muted(padRight(name, 9)) + "  " + flag + "\n")
			}
			continue
		}
		b.WriteString(t.definitions(2, 9, []string{field.Name}, []string{field.Value}, nil))
	}
	return b.String()
}
