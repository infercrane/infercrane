package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/infercrane/infercrane/internal/config"
)

func isolatedClientConfig(t *testing.T) {
	t.Helper()
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("INFERCRANE_API_KEY", "")
	t.Setenv("INFERCRANE_URL", "")
	t.Setenv("INFERCRANE_CONTEXT", "")
}

func TestRootHelpShowsEverydaySectionsAndHidesOperatorTools(t *testing.T) {
	isolatedClientConfig(t)
	output, err := captureStdout(t, func() error { return run(context.Background(), nil) })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Usage: infercrane [OPTIONS] COMMAND [ARGS]...",
		"Same model. Less compute. Proven on your own traffic.",
		"Not signed in. Run infercrane login to start.",
		"Get started:", "Deployments:", "Optimize:", "Observe:", "Configuration:", "Options:",
		"  deploy ", "  optimize ", "  login ",
		"infercrane help --all",
		cliDocsURL,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("root help lacks %q:\n%s", want, output)
		}
	}
	for _, operatorOnly := range []string{"  passport ", "  burst ", "  serve ", "More serving tools"} {
		if strings.Contains(output, operatorOnly) {
			t.Errorf("root help shows operator tool %q:\n%s", operatorOnly, output)
		}
	}
	if strings.Contains(output, "\x1b[") {
		t.Errorf("help written to a pipe has ANSI escapes:\n%q", output)
	}
	everyday := 0
	for _, spec := range publicCommandSpecs {
		if section, _ := sectionFor(spec.section); section.everyday {
			everyday++
		}
	}
	if everyday > 30 {
		t.Errorf("%d everyday commands; keep the first screen short", everyday)
	}
}

func TestHelpAllListsEveryCommandOnce(t *testing.T) {
	isolatedClientConfig(t)
	output, err := captureStdout(t, func() error { return run(context.Background(), []string{"help", "--all"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range publicCommandSpecs {
		name := strings.Fields(spec.use)[0]
		if count := strings.Count(output, "\n  "+name+" "); count != 1 {
			t.Errorf("help --all lists %q %d times", name, count)
		}
	}
	if strings.Contains(output, "help --all to see") {
		t.Errorf("help --all still points at itself:\n%s", output)
	}
}

func TestEveryCommandHasAKnownSectionAndShortDescription(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range publicCommandSpecs {
		name := strings.Fields(spec.use)[0]
		if seen[name] {
			t.Errorf("%s is listed twice", name)
		}
		seen[name] = true
		if _, ok := sectionFor(spec.section); !ok {
			t.Errorf("%s has unknown section %q", name, spec.section)
		}
		if words := len(strings.Fields(spec.short)); words > 9 || spec.short == "" {
			t.Errorf("%s description has %d words; keep it under 10: %q", name, words, spec.short)
		}
		if strings.HasSuffix(spec.short, ".") {
			t.Errorf("%s description ends with a period", name)
		}
	}
}

func TestHelpCommandShowsOneCommand(t *testing.T) {
	output, err := captureStdout(t, func() error { return run(context.Background(), []string{"help", "deploy"}) })
	if err != nil || !strings.Contains(output, "Usage:") || !strings.Contains(output, "infercrane deploy MODEL") || !strings.Contains(output, "Examples:") {
		t.Fatalf("help deploy err=%v:\n%s", err, output)
	}
}

func TestRootHelpShowsTheSelectedContextWithoutReadingSecrets(t *testing.T) {
	isolatedClientConfig(t)
	t.Setenv("INFERCRANE_URL", "https://control.example.test")
	t.Setenv("INFERCRANE_API_KEY", "test-key")
	output, err := captureStdout(t, func() error { return run(context.Background(), []string{"--help"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "● Using control.example.test") || strings.Contains(output, "test-key") || strings.Contains(output, "Not signed in") {
		t.Fatalf("account line:\n%s", output)
	}
}

func TestUnknownCommandSuggestsTheClosestCommand(t *testing.T) {
	cases := map[string]string{"deplyo": "deploy", "stauts": "status", "optimise": "optimize", "ps": "deployments", "cost": "finops"}
	for typed, want := range cases {
		err := run(context.Background(), []string{typed})
		var unknown *UnknownCommandError
		if !errors.As(err, &unknown) || len(unknown.Suggestions) != 1 || unknown.Suggestions[0] != want {
			t.Errorf("%s: err=%v, want one suggestion %q", typed, err, want)
			continue
		}
		var out bytes.Buffer
		writeCLIError(&out, []string{typed}, err)
		text := out.String()
		if !strings.HasPrefix(text, "Error: unknown command \""+typed+"\"\n") || !strings.Contains(text, "Did you mean infercrane "+want+"?") {
			t.Errorf("%s rendered:\n%s", typed, text)
		}
	}
	if command, _, err := newRootCommand(context.Background()).Find([]string{"dashboard"}); err != nil || command.Name() != "open" {
		t.Errorf("dashboard alias resolves to %v, %v", command, err)
	}
	if err := run(context.Background(), []string{"help", "nope"}); !errors.As(err, new(*UnknownCommandError)) {
		t.Errorf("help nope err=%v", err)
	}
}

func TestHumanErrorsSayWhatToDoNext(t *testing.T) {
	var out bytes.Buffer
	writeCLIError(&out, []string{"status", "qwen"}, config.ErrNotSignedIn)
	if text := out.String(); !strings.Contains(text, "Error: you're not signed in") || !strings.Contains(text, "Run infercrane login") || !strings.Contains(text, "INFERCRANE_API_KEY") {
		t.Fatalf("not signed in:\n%s", text)
	}

	out.Reset()
	writeCLIError(&out, []string{"status", "qwen"}, &ControlError{Code: "provider_unavailable", Message: "RunPod is unavailable", Retryable: true, Remediation: "Retry with the same idempotency key."})
	want := "Error: RunPod is unavailable\n  Next: Retry with the same idempotency key.\n  Code: provider_unavailable · safe to retry\n"
	if out.String() != want {
		t.Fatalf("control error:\n%q\nwant\n%q", out.String(), want)
	}

	out.Reset()
	renderHumanError(&out, styledTheme(80, "dark"), &UnknownCommandError{Name: "deplyo", Suggestions: []string{"deploy"}})
	styled := out.String()
	if !strings.Contains(styled, "\x1b[") || ansi.Strip(styled) != "✗ Error: unknown command \"deplyo\"\n  Did you mean infercrane deploy?\n  Run infercrane help to see the commands.\n" {
		t.Fatalf("styled error:\n%q", ansi.Strip(styled))
	}
}

func TestStyledRootHelpUsesRulesAndKeepsWords(t *testing.T) {
	isolatedClientConfig(t)
	root := newRootCommand(context.Background())
	var out bytes.Buffer
	renderRootHelp(&out, styledTheme(80, "dark"), root, false)
	plain := ansi.Strip(out.String())
	if !strings.Contains(out.String(), "\x1b[") || !strings.Contains(plain, "Get started ─") || !strings.Contains(plain, "○ Not signed in") {
		t.Fatalf("styled help:\n%s", plain)
	}
	for _, line := range strings.Split(plain, "\n") {
		if width := ansi.StringWidth(line); width > 80 {
			t.Errorf("line is %d columns wide at an 80-column terminal: %q", width, line)
		}
	}
}

func TestCommandHelpFitsAnEightyColumnTerminal(t *testing.T) {
	for _, name := range []string{"deploy", "optimize", "profile", "telemetry", "workload"} {
		output, err := captureStdout(t, func() error { return run(context.Background(), []string{name, "--help"}) })
		if err != nil {
			t.Fatalf("%s --help: %v", name, err)
		}
		inOptions := false
		for _, line := range strings.Split(output, "\n") {
			if strings.HasPrefix(line, "Options:") {
				inOptions = true
			}
			if strings.HasPrefix(line, "Docs:") {
				inOptions = false
			}
			if inOptions && ansi.StringWidth(line) > 80 {
				t.Errorf("%s --help option line is %d columns: %q", name, ansi.StringWidth(line), line)
			}
		}
	}
}

func TestEveryCurrentCommandHasExamplesAndListsItsActions(t *testing.T) {
	root := newRootCommand(context.Background())
	for _, spec := range publicCommandSpecs {
		name := strings.Fields(spec.use)[0]
		if _, legacy := legacyReplacements[name]; legacy {
			continue
		}
		command, _, err := root.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(command.Example) == "" {
			t.Errorf("%s help has no example", name)
		}
		if strings.Contains(spec.use, "ACTION") && len(commandActions[name]) == 0 && commandLong[name] == "" {
			t.Errorf("%s takes an ACTION but help does not list them", name)
		}
	}
}

func TestOlderCommandsAreListedLastAndNameTheirReplacement(t *testing.T) {
	isolatedClientConfig(t)
	everyday, err := captureStdout(t, func() error { return run(context.Background(), nil) })
	if err != nil {
		t.Fatal(err)
	}
	all, err := captureStdout(t, func() error { return run(context.Background(), []string{"help", "--all"}) })
	if err != nil {
		t.Fatal(err)
	}
	older := all[strings.Index(all, "Older commands:"):]
	for name, replacement := range legacyReplacements {
		if strings.Contains(everyday, "\n  "+name+" ") {
			t.Errorf("older command %s is on the first screen", name)
		}
		if !strings.Contains(older, "\n  "+name+" ") {
			t.Errorf("older command %s is not under Older commands", name)
		}
		help, err := captureStdout(t, func() error { return run(context.Background(), []string{name, "--help"}) })
		if err != nil || !strings.Contains(help, "older command") || (replacement != "" && !strings.Contains(help, replacement)) {
			t.Errorf("%s help does not name its replacement %q:\n%s", name, replacement, help)
		}
	}
}

func TestHumanWindowAndTracebackDiagnostics(t *testing.T) {
	for window, want := range map[time.Duration]string{720 * time.Hour: "30d", 36 * time.Hour: "36h", 90 * time.Minute: "90m", 90 * time.Second: "1m30s"} {
		if got := humanWindow(window); got != want {
			t.Errorf("humanWindow(%s) = %q, want %q", window, got, want)
		}
	}
	if outputIsJSON([]string{"qwen", "--output", "json"}) != true || outputIsJSON([]string{"qwen"}) != false {
		t.Error("outputIsJSON misread --output")
	}
}
