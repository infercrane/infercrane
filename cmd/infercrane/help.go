package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Help reads like a short menu: a few everyday sections first, and every
// other command one flag away (`infercrane help --all`). It renders with the
// same theme as command output, so it is styled for a person at a terminal
// and plain for scripts, pipes, NO_COLOR, and coding agents.

const (
	rootTagline = "Run open models in your own cloud at the lowest cost that still meets\nyour speed and quality targets."
	cliDocsURL  = "https://docs.infercrane.com/cli"
)

type helpSection struct {
	id, title string
	everyday  bool
}

var helpSections = []helpSection{
	{id: "start", title: "Get started", everyday: true},
	{id: "deployments", title: "Deployments", everyday: true},
	{id: "optimize", title: "Optimize", everyday: true},
	{id: "observe", title: "Observe", everyday: true},
	{id: "config", title: "Configuration", everyday: true},
	{id: "serving", title: "More serving tools"},
	{id: "evidence", title: "Evidence and measurement"},
	{id: "platform", title: "Platform administration"},
	{id: "legacy", title: "Older commands"},
}

// helpTheme styles help written to stdout. Help never prints JSON, and its
// section rules stop at 80 columns so help reads as one block on wide screens.
func helpTheme() theme {
	t := themeFor(os.Stdout, "")
	t.width = min(t.width, 80)
	return t
}

// heading is a help section title: a rule-led heading when styled, and the
// conventional "Title:" when plain.
func (t theme) heading(text string) string {
	if !t.styled {
		return text + ":"
	}
	return t.section(text)
}

// command marks a command the reader can type.
func (t theme) command(text string) string { return t.tone(toneInfo, text) }

// rootOptions are the options every command accepts, in the words help uses.
var rootOptions = [][2]string{
	{"--context NAME", "Use another saved context for one command"},
	{"--output json", "Print JSON for scripts and agents"},
	{"--no-color", "Turn off colors"},
	{"-h, --help", "Show help for any command"},
	{"-v, --version", "Show the CLI version"},
}

func renderRootHelp(w io.Writer, t theme, root *cobra.Command, all bool) {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n\n", t.title("Usage:"), "infercrane [OPTIONS] COMMAND [ARGS]...")
	b.WriteString(rootTagline + "\n")
	if line := accountLine(t); line != "" {
		b.WriteString("\n" + line + "\n")
	}

	bySection := map[string][]*cobra.Command{}
	nameWidth, hidden := 0, 0
	for _, spec := range publicCommandSpecs {
		command, _, err := root.Find([]string{strings.Fields(spec.use)[0]})
		if err != nil || command == root {
			continue
		}
		section, ok := sectionFor(spec.section)
		if !ok {
			continue
		}
		if !section.everyday && !all {
			hidden++
			continue
		}
		bySection[spec.section] = append(bySection[spec.section], command)
		nameWidth = max(nameWidth, lipgloss.Width(command.Name()))
	}
	for _, option := range rootOptions {
		nameWidth = max(nameWidth, lipgloss.Width(option[0]))
	}
	row := func(name, description string) string {
		return "  " + name + strings.Repeat(" ", nameWidth-lipgloss.Width(name)+3) + t.muted(description) + "\n"
	}
	for _, section := range helpSections {
		commands := bySection[section.id]
		if len(commands) == 0 {
			continue
		}
		b.WriteString("\n" + t.heading(section.title) + "\n")
		for _, command := range commands {
			b.WriteString(row(command.Name(), command.Short))
		}
	}
	b.WriteString("\n" + t.heading("Options") + "\n")
	for _, option := range rootOptions {
		b.WriteString(row(option[0], option[1]))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "Run %s for details on one command.\n", t.command("infercrane COMMAND --help"))
	if hidden > 0 {
		fmt.Fprintf(&b, "Run %s to see %d more commands for operators.\n", t.command("infercrane help --all"), hidden)
	}
	fmt.Fprintf(&b, "Run %s to turn on tab completion.\n", t.command("infercrane completion bash|zsh|fish"))
	fmt.Fprintf(&b, "Docs: %s\n", cliDocsURL)
	_, _ = io.WriteString(w, b.String())
}

func sectionFor(id string) (helpSection, bool) {
	for _, section := range helpSections {
		if section.id == id {
			return section, true
		}
	}
	return helpSection{}, false
}

// accountLine says which control plane this CLI talks to and whether it has
// a credential. It reads only the saved configuration and the environment:
// help never contacts the control plane or the OS keychain, so it cannot say
// whether a saved key is still valid, and it does not claim to.
func accountLine(t theme) string {
	settings, err := config.ClientConfiguration()
	if err != nil {
		return ""
	}
	name := os.Getenv("INFERCRANE_CONTEXT")
	if name == "" {
		name = settings.Current
	}
	selected, known := settings.Contexts[name]
	controlURL := selected.URL
	if value := os.Getenv("INFERCRANE_URL"); value != "" {
		controlURL = value
	}
	hasKey := os.Getenv("INFERCRANE_API_KEY") != "" || selected.APIKey != "" || selected.CredentialStore != ""
	if !hasKey {
		return t.tone(toneUnknown, "○") + " Not signed in. Run " + t.command("infercrane login") + " to start."
	}
	host := controlURL
	if parsed, parseErr := url.Parse(controlURL); parseErr == nil && parsed.Host != "" {
		host = parsed.Host
	}
	if host == "" {
		host = "127.0.0.1:8080"
	}
	label := "Using " + host
	if known && name != "" {
		label += t.muted(" · context " + name)
	}
	return t.tone(toneGood, "●") + " " + label
}

// usageTemplate is Cobra's command help with the root help's headings. The
// template functions read the help theme when help is rendered.
const usageTemplate = `{{heading "Usage"}}{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]{{end}}{{with actions .}}

{{heading "Actions"}}
{{.}}{{end}}{{if gt (len .Aliases) 0}}

{{heading "Aliases"}}
  {{.NameAndAliases}}{{end}}{{if .HasExample}}

{{heading "Examples"}}
{{examples .Example}}{{end}}{{if .HasAvailableSubCommands}}

{{heading "Commands"}}{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{muted .Short}}{{end}}{{end}}{{end}}{{if .HasAvailableLocalFlags}}

{{heading "Options"}}
{{flagUsages .LocalFlags | trimTrailingWhitespaces}}{{end}}{{if .HasAvailableInheritedFlags}}

{{heading "Global options"}}
{{flagUsages .InheritedFlags | trimTrailingWhitespaces}}{{end}}

Docs: ` + cliDocsURL + `
`

func init() {
	cobra.AddTemplateFuncs(map[string]any{
		"heading": func(text string) string { return helpTheme().heading(text) },
		"muted":   func(text string) string { return helpTheme().muted(text) },
		"actions": func(cmd *cobra.Command) string { return renderActions(helpTheme(), cmd.Name()) },
		// Option descriptions wrap to the help width, so a long one continues
		// under its own column instead of breaking mid-word at the edge.
		"flagUsages": func(flags *pflag.FlagSet) string { return flags.FlagUsagesWrapped(helpTheme().width) },
		"examples": func(text string) string {
			t := helpTheme()
			lines := strings.Split(text, "\n")
			for index, line := range lines {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					lines[index] = t.muted(line)
				}
			}
			return strings.Join(lines, "\n")
		},
	})
}

// newHelpCommand replaces Cobra's help command so `infercrane help --all`
// lists every command and `infercrane help COMMAND` shows one.
func newHelpCommand(root *cobra.Command) *cobra.Command {
	var all bool
	command := &cobra.Command{
		Use:   "help [COMMAND]",
		Short: "Show help for a command",
		Args:  cobra.ArbitraryArgs,
		ValidArgsFunction: func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			var names []string
			for _, child := range root.Commands() {
				if child.IsAvailableCommand() && strings.HasPrefix(child.Name(), toComplete) {
					names = append(names, child.Name())
				}
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				renderRootHelp(cmd.OutOrStdout(), helpTheme(), root, all)
				return nil
			}
			target, rest, err := root.Find(args)
			if err != nil || target == root || len(rest) > 0 && target.HasAvailableSubCommands() {
				return newUnknownCommandError(root, args[0])
			}
			return target.Help()
		},
	}
	command.Flags().BoolVar(&all, "all", false, "list every command, including operator tools")
	return command
}

// UnknownCommandError is a command name the CLI does not have. It carries
// the closest real commands so the message can offer them.
type UnknownCommandError struct {
	Name        string
	Suggestions []string
}

func (e *UnknownCommandError) Error() string {
	message := fmt.Sprintf("unknown command %q", e.Name)
	if len(e.Suggestions) > 0 {
		message += "; did you mean " + strings.Join(e.Suggestions, " or ") + "?"
	}
	return message
}

func newUnknownCommandError(root *cobra.Command, name string) error {
	// A word people habitually type for a command ("ps", "cost") names
	// exactly that command; edit distance is only for typos.
	for _, command := range root.Commands() {
		if slices.Contains(command.SuggestFor, name) {
			return &UnknownCommandError{Name: name, Suggestions: []string{command.Name()}}
		}
	}
	var suggestions []string
	for _, suggestion := range root.SuggestionsFor(name) {
		if !slices.Contains(suggestions, suggestion) {
			suggestions = append(suggestions, suggestion)
		}
	}
	return &UnknownCommandError{Name: name, Suggestions: suggestions}
}

// rootArgs reports an unknown command with suggestions. Cobra checks a
// command's own Args only when it has no subcommand to run, so this sees
// exactly the names that matched nothing.
func rootArgs(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return nil
	}
	return newUnknownCommandError(cmd.Root(), args[0])
}

// renderHumanError writes one failure for a person or a script. The first
// line says what happened; the lines after it say what to do next.
func renderHumanError(w io.Writer, t theme, err error) {
	message, hints := humanError(err)
	head := t.render(lipgloss.NewStyle().Bold(true).Foreground(t.palette().failure), "Error")
	if t.styled {
		head = t.tone(toneBad, "✗") + " " + head
	}
	fmt.Fprintf(w, "%s: %s\n", head, message)
	for _, hint := range hints {
		fmt.Fprintf(w, "  %s\n", highlightCommands(t, hint))
	}
}

// humanError splits an error into a message and next steps.
func humanError(err error) (string, []string) {
	var unknown *UnknownCommandError
	if errors.As(err, &unknown) {
		hints := []string{}
		if len(unknown.Suggestions) > 0 {
			hints = append(hints, "Did you mean `infercrane "+strings.Join(unknown.Suggestions, "` or `infercrane ")+"`?")
		}
		hints = append(hints, "Run `infercrane help` to see the commands.")
		return fmt.Sprintf("unknown command %q", unknown.Name), hints
	}
	if errors.Is(err, config.ErrNotSignedIn) {
		return "you're not signed in", []string{"Run `infercrane login` to sign in through your browser,", "or set INFERCRANE_API_KEY for scripts and CI."}
	}
	var controlErr *ControlError
	if errors.As(err, &controlErr) {
		message := controlErr.Message
		if message == "" {
			message = controlErr.Code
		}
		hints := []string{}
		if controlErr.Remediation != "" {
			hints = append(hints, "Next: "+controlErr.Remediation)
		}
		if controlErr.Code != "" {
			code := "Code: " + controlErr.Code
			if controlErr.Retryable {
				code += " · safe to retry"
			}
			hints = append(hints, code)
		}
		return message, hints
	}
	return err.Error(), nil
}

// highlightCommands colors `backticked` commands in a hint and drops the
// backticks, so plain output reads as prose and styled output points at what
// to type.
func highlightCommands(t theme, text string) string {
	parts := strings.Split(text, "`")
	if len(parts)%2 == 0 {
		return text
	}
	var b strings.Builder
	for index, part := range parts {
		if index%2 == 1 {
			b.WriteString(t.command(part))
			continue
		}
		b.WriteString(part)
	}
	return b.String()
}

// renderActions lists a command's actions as two aligned columns.
func renderActions(t theme, name string) string {
	actions := commandActions[name]
	if len(actions) == 0 {
		return ""
	}
	width := 0
	for _, action := range actions {
		width = max(width, lipgloss.Width(action[0]))
	}
	lines := make([]string, 0, len(actions))
	for _, action := range actions {
		lines = append(lines, "  "+action[0]+strings.Repeat(" ", width-lipgloss.Width(action[0])+3)+t.muted(action[1]))
	}
	return strings.Join(lines, "\n")
}

// legacyLong is the help for an older command: what it does and what to use.
func legacyLong(short, replacement string) string {
	if replacement == "" {
		return short + ".\n\nThis is an older command. It still works, and nothing replaces it yet."
	}
	return short + ".\n\nThis is an older command. It still works; use " + replacement + " instead."
}

// noteLegacyCommand tells a person at a terminal that a command is older and
// what replaces it. Scripts, pipes, and JSON output never see the note.
func noteLegacyCommand(name string, args []string) {
	replacement, legacy := legacyReplacements[name]
	if !legacy || replacement == "" || outputIsJSON(args) || !styleAllowed(os.Stderr) {
		return
	}
	t := themeFor(os.Stderr, "")
	fmt.Fprintln(os.Stderr, t.muted("Note: infercrane "+name+" is an older command; use "+replacement+"."))
}

func outputIsJSON(args []string) bool {
	for index, arg := range args {
		if arg == "--output=json" || arg == "-output=json" || ((arg == "--output" || arg == "-output") && index+1 < len(args) && args[index+1] == "json") {
			return true
		}
	}
	return false
}
