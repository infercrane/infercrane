package main

import (
	"fmt"
	"image/color"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
)

// The CLI's terminal look follows the dashboard's instrument design (web
// DESIGN.md): monochrome structure, thin lines, and color only for measured
// state, always next to a word that says the same thing.
//
// A theme decorates one output stream. Its zero value is plain: every helper
// returns its text unchanged and adds no ANSI, so each renderer has a single
// code path. Styling is on only for a person at a terminal: stdout is a TTY,
// NO_COLOR is unset, TERM is not dumb, and the command is not writing JSON.

type theme struct {
	styled bool
	// background is "dark", "light", or "" when unknown.
	background string
	width      int
	// profile is the color depth the terminal supports. Colors are reduced
	// to it when rendered, so styled text can be written with fmt.
	profile colorprofile.Profile
}

// plainTheme decorates nothing. Scripts, pipes, and coding agents get it.
var plainTheme = theme{width: 80}

// outputTheme is the theme for a command's stdout.
func outputTheme(output string) theme { return themeFor(os.Stdout, output) }

// themeFor is the theme for one stream of a command writing output.
func themeFor(file *os.File, output string) theme {
	if output == "json" || !styleAllowed(file) {
		return plainTheme
	}
	profile := colorprofile.Detect(file, os.Environ())
	if profile <= colorprofile.ASCII {
		return plainTheme
	}
	return theme{styled: true, background: terminalBackground(), width: terminalWidth(file), profile: profile}
}

// styledTheme is a styled true-color theme for tests and golden files.
func styledTheme(width int, background string) theme {
	return theme{styled: true, background: background, width: width, profile: colorprofile.TrueColor}
}

// styleAllowed reports whether file may carry ANSI styling.
func styleAllowed(file *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(file.Fd())
}

// terminalWidth is the width of file's terminal, bounded so text stays
// readable on ultrawide screens and usable on narrow ones.
func terminalWidth(file *os.File) int {
	width, _, err := term.GetSize(file.Fd())
	if err != nil || width <= 0 {
		return 80
	}
	return min(max(width, 40), 120)
}

// terminalBackground is "dark" or "light" when the terminal says so in
// COLORFGBG ("fg;bg"), and "" when it does not. The CLI never queries the
// terminal: a query can stall a command, or leak its late answer into the
// shell, where nothing answers it (CI, script, slow SSH).
func terminalBackground() string {
	value := os.Getenv("COLORFGBG")
	if value == "" {
		return ""
	}
	parts := strings.Split(value, ";")
	background, err := strconv.Atoi(parts[len(parts)-1])
	switch {
	case err != nil:
		return ""
	case background == 7 || background == 15:
		return "light"
	default:
		return "dark"
	}
}

// palette is the dashboard's semantic palette (DESIGN.md) for a dark
// terminal, its light-mode counterpart, and mid-tones that read on either
// when the background is unknown. Primary text keeps the terminal's own
// foreground, and the chip carries its own colors.
type palette struct {
	muted, line, chipText, chip, info, healthy, warning, failure color.Color
}

var palettes = map[string][6]string{
	//        muted      line       info       healthy    warning    failure
	"dark":  {"#a1a1aa", "#3f3f46", "#6aa8ff", "#45d6a8", "#f0b44c", "#ff6b7f"},
	"light": {"#666666", "#d4d4d8", "#1d4ed8", "#087f5b", "#9a6700", "#cf222e"},
	"":      {"#7c7c85", "#63636b", "#3b82f6", "#1e9e78", "#b7791f", "#e5484d"},
}

func (t theme) palette() palette {
	colors, ok := palettes[t.background]
	if !ok {
		colors = palettes[""]
	}
	pick := func(value string) color.Color { return t.profile.Convert(lipgloss.Color(value)) }
	return palette{
		muted: pick(colors[0]), line: pick(colors[1]), info: pick(colors[2]),
		healthy: pick(colors[3]), warning: pick(colors[4]), failure: pick(colors[5]),
		chipText: pick("#e4e4e7"), chip: pick("#3f3f46"),
	}
}

func (t theme) render(style lipgloss.Style, text string) string {
	if !t.styled || text == "" {
		return text
	}
	return style.Render(text)
}

func (t theme) foreground(c color.Color, text string) string {
	return t.render(lipgloss.NewStyle().Foreground(c), text)
}

// title is a heading in the terminal's own color.
func (t theme) title(text string) string { return t.render(lipgloss.NewStyle().Bold(true), text) }

// muted is supporting text: labels, explanations, identifiers.
func (t theme) muted(text string) string { return t.foreground(t.palette().muted, text) }

// section is a heading followed by a thin rule to the edge of the terminal.
func (t theme) section(text string) string {
	if !t.styled {
		return text
	}
	rule := t.width - lipgloss.Width(text) - 1
	if rule < 2 {
		return t.title(text)
	}
	return t.title(text) + " " + t.foreground(t.palette().line, strings.Repeat("─", rule))
}

// Tones name what a value means. Every tone keeps its word; color only
// repeats it.
const (
	toneGood    = "good"
	toneBad     = "bad"
	toneWatch   = "watch"
	toneInfo    = "info"
	toneNeutral = "neutral"
	toneUnknown = "unknown"
)

func (t theme) toneColor(tone string) color.Color {
	p := t.palette()
	switch tone {
	case toneGood:
		return p.healthy
	case toneBad:
		return p.failure
	case toneWatch:
		return p.warning
	case toneInfo:
		return p.info
	case toneUnknown:
		return p.muted
	}
	return nil
}

// tone colors text by meaning; neutral text keeps the terminal's color.
func (t theme) tone(tone, text string) string {
	if c := t.toneColor(tone); c != nil {
		return t.foreground(c, text)
	}
	return text
}

// statusTone maps a lifecycle state from any resource to its meaning.
func statusTone(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "healthy", "ready", "active", "serving", "converged", "succeeded", "pass", "qualified", "guard_passed", "promoted", "rolled_out", "observed", "live":
		return toneGood
	case "pending", "starting", "running", "leased", "observing", "approved", "provisioning", "measuring", "validating", "ranked", "guarding", "creating", "deleting":
		return toneInfo
	case "waiting", "queued", "proposed", "awaiting_approval", "degraded", "warning", "draining", "retrying", "watch", "inconclusive":
		return toneWatch
	case "failed", "unhealthy", "error", "cancelled", "rejected", "block", "expired":
		return toneBad
	}
	return toneUnknown
}

// badge is a state label: a colored dot and the state in capitals. Plain
// output is the capitalized state alone, as the CLI has always printed it.
func (t theme) badge(state string) string {
	label := strings.ToUpper(state)
	if !t.styled {
		return label
	}
	return t.tone(statusTone(state), "● "+strings.ReplaceAll(label, "_", " "))
}

// delta is a before→after change: green when better, red when worse, dim
// when unknown.
func (t theme) delta(change, tone string) string {
	if change == "" || change == "unknown" {
		return t.tone(toneUnknown, change)
	}
	style := lipgloss.NewStyle().Bold(true)
	if c := t.toneColor(tone); c != nil {
		style = style.Foreground(c)
	}
	return t.render(style, change)
}

// chip is a small boxed reading, such as a measurement's noise band.
func (t theme) chip(text string) string {
	if text == "" {
		return ""
	}
	if !t.styled {
		return "[" + text + "]"
	}
	p := t.palette()
	return lipgloss.NewStyle().Foreground(p.chipText).Background(p.chip).Padding(0, 1).Render(text)
}

// card draws body in a rounded frame. accent colors the left edge, so a card
// with a measured result scans by meaning; it is empty for a neutral card.
func (t theme) card(body string, width int, accent string) string {
	if !t.styled {
		return body
	}
	p := t.palette()
	style := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(p.line).Padding(0, 1).Width(width)
	if c := t.toneColor(accent); c != nil && accent != toneUnknown {
		style = style.BorderLeftForeground(c)
	}
	return style.Render(body)
}

// column describes one table column. The one wrap column takes the width the
// others leave and wraps; the rest size to their widest cell.
type column struct {
	header string
	right  bool
	wrap   bool
}

// table lays out rows of plain text in aligned columns. paint styles one
// laid-out line of a cell, so wrapping never splits an escape sequence.
func (t theme) table(indent int, columns []column, rows [][]string, paint func(row, col int, text string) string) string {
	return t.tableWithDetail(indent, columns, rows, paint, nil)
}

// tableWithDetail is table with detail lines, already laid out, printed
// under each row.
func (t theme) tableWithDetail(indent int, columns []column, rows [][]string, paint func(row, col int, text string) string, detail func(row int) string) string {
	const gap = 2
	widths := make([]int, len(columns))
	for index, col := range columns {
		widths[index] = lipgloss.Width(col.header)
	}
	for _, row := range rows {
		for index := range columns {
			if index < len(row) && !columns[index].wrap {
				widths[index] = max(widths[index], lipgloss.Width(row[index]))
			}
		}
	}
	for index, col := range columns {
		if !col.wrap {
			continue
		}
		used := indent + gap*(len(columns)-1)
		for other := range columns {
			if other != index {
				used += widths[other]
			}
		}
		widest := lipgloss.Width(col.header)
		for _, row := range rows {
			if index < len(row) {
				widest = max(widest, lipgloss.Width(row[index]))
			}
		}
		widths[index] = min(widest, max(12, t.width-used))
	}
	var out strings.Builder
	emit := func(cells [][]string, rowIndex int) {
		height := 1
		for _, lines := range cells {
			height = max(height, len(lines))
		}
		for line := range height {
			var b strings.Builder
			b.WriteString(strings.Repeat(" ", indent))
			for index, lines := range cells {
				text := ""
				if line < len(lines) {
					text = lines[line]
				}
				fill := strings.Repeat(" ", max(0, widths[index]-lipgloss.Width(text)))
				if rowIndex >= 0 && paint != nil && text != "" {
					text = paint(rowIndex, index, text)
				} else if rowIndex < 0 {
					text = t.muted(text)
				}
				if columns[index].right {
					b.WriteString(fill + text)
				} else {
					b.WriteString(text + fill)
				}
				if index < len(cells)-1 {
					b.WriteString(strings.Repeat(" ", gap))
				}
			}
			out.WriteString(strings.TrimRight(b.String(), " ") + "\n")
		}
	}
	header := make([][]string, len(columns))
	total := gap * (len(columns) - 1)
	for index, col := range columns {
		header[index] = []string{strings.ToUpper(col.header)}
		total += widths[index]
	}
	emit(header, -1)
	if t.styled {
		out.WriteString(strings.Repeat(" ", indent) + t.foreground(t.palette().line, strings.Repeat("─", total)) + "\n")
	}
	for rowIndex, row := range rows {
		cells := make([][]string, len(columns))
		for index := range columns {
			text := ""
			if index < len(row) {
				text = row[index]
			}
			if columns[index].wrap {
				cells[index] = wrapWords(text, widths[index])
			} else {
				cells[index] = []string{text}
			}
		}
		emit(cells, rowIndex)
		if detail != nil {
			out.WriteString(detail(rowIndex))
		}
	}
	return out.String()
}

// definitions lays out name/value pairs with the names in a muted column. A
// value with a word too long for its column, such as a digest, moves below
// its name instead of breaking.
func (t theme) definitions(indent, nameWidth int, names, values []string, paint func(index int, text string) string) string {
	var out strings.Builder
	pad := strings.Repeat(" ", indent)
	valueWidth := max(20, t.width-indent-nameWidth-2)
	for index, name := range names {
		value := values[index]
		lines := wrapWords(value, valueWidth)
		stacked := longestWord(value) > valueWidth
		if stacked {
			lines = wrapWords(value, max(20, t.width-indent-2))
		}
		for line, text := range lines {
			if paint != nil {
				text = paint(index, text)
			}
			switch {
			case stacked && line == 0:
				out.WriteString(pad + t.muted(name) + "\n" + pad + "  " + text + "\n")
			case stacked:
				out.WriteString(pad + "  " + text + "\n")
			case line == 0:
				out.WriteString(pad + t.muted(padRight(name, nameWidth)) + "  " + text + "\n")
			default:
				out.WriteString(pad + strings.Repeat(" ", nameWidth+2) + text + "\n")
			}
		}
	}
	return out.String()
}

// wrapWords wraps plain text at spaces to width. A word longer than width is
// broken; nothing is dropped.
func wrapWords(text string, width int) []string {
	if width <= 0 || lipgloss.Width(text) <= width {
		return []string{text}
	}
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		for lipgloss.Width(word) > width {
			if line != "" {
				lines = append(lines, line)
				line = ""
			}
			head, tail := splitAtWidth(word, width)
			lines = append(lines, head)
			word = tail
		}
		switch {
		case line == "":
			line = word
		case lipgloss.Width(line)+1+lipgloss.Width(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" || len(lines) == 0 {
		lines = append(lines, line)
	}
	return lines
}

// splitAtWidth cuts a word that cannot fit, after its last "/" or "-" within
// width when it has one, so paths and model names break at a seam.
func splitAtWidth(word string, width int) (string, string) {
	runes := []rune(word)
	used, cut, seam := 0, len(runes), 0
	for index, r := range runes {
		cell := lipgloss.Width(string(r))
		if used+cell > width {
			cut = index
			break
		}
		used += cell
		if (r == '/' || r == '-') && index > 0 {
			seam = index + 1
		}
	}
	if cut == len(runes) {
		return word, ""
	}
	if seam > 0 && seam >= cut/2 {
		cut = seam
	}
	return string(runes[:cut]), string(runes[cut:])
}

func longestWord(text string) int {
	longest := 0
	for _, word := range strings.Fields(text) {
		longest = max(longest, lipgloss.Width(word))
	}
	return longest
}

func padRight(text string, width int) string {
	return text + strings.Repeat(" ", max(0, width-lipgloss.Width(text)))
}

// flagGroups splits runtime arguments into one group per flag with its
// values, so long flag strings wrap at flag boundaries.
func flagGroups(args string) []string {
	var groups []string
	for _, word := range strings.Fields(args) {
		if len(groups) == 0 || isFlagWord(word) {
			groups = append(groups, word)
			continue
		}
		groups[len(groups)-1] += " " + word
	}
	return groups
}

// isFlagWord is a --long or -s flag, not a negative number value.
func isFlagWord(word string) bool {
	return strings.HasPrefix(word, "--") || (len(word) > 1 && word[0] == '-' && unicode.IsLetter(rune(word[1])))
}

// wrapFlags lays out runtime arguments one flag per line; a flag whose value
// is still too long continues on indented lines.
func wrapFlags(args string, width int) []string {
	var lines []string
	for _, group := range flagGroups(args) {
		wrapped := wrapWords(group, width)
		lines = append(lines, wrapped[0])
		if len(wrapped) > 1 {
			for _, rest := range wrapWords(strings.Join(wrapped[1:], " "), width-2) {
				lines = append(lines, "  "+rest)
			}
		}
	}
	return lines
}

// relativeTime says how long ago, or how soon, a moment is.
func relativeTime(now, moment time.Time) string {
	if moment.IsZero() {
		return "unknown"
	}
	elapsed := now.Sub(moment)
	suffix := " ago"
	if elapsed < 0 {
		elapsed, suffix = -elapsed, ""
	}
	var amount string
	switch {
	case elapsed < 45*time.Second:
		if suffix == "" {
			return "in under a minute"
		}
		return "just now"
	case elapsed < time.Hour:
		amount = fmt.Sprintf("%dm", int(elapsed.Round(time.Minute)/time.Minute))
	case elapsed < 48*time.Hour:
		amount = fmt.Sprintf("%dh", int(elapsed.Round(time.Hour)/time.Hour))
	default:
		amount = fmt.Sprintf("%dd", int(elapsed.Round(24*time.Hour)/(24*time.Hour)))
	}
	if suffix == "" {
		return "in " + amount
	}
	return amount + suffix
}
