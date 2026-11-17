package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestPlainThemeAddsNoANSIAndKeepsOldStatusWords(t *testing.T) {
	plain := plainTheme
	for _, value := range []string{plain.title("t"), plain.muted("m"), plain.section("Evidence"), plain.tone(toneGood, "ok"), plain.delta("+4.1%", toneGood), plain.card("body", 40, toneBad)} {
		if strings.Contains(value, "\x1b[") {
			t.Fatalf("plain theme styled %q", value)
		}
	}
	// terminalStatus printed the capitalized state when not styled; badge keeps that.
	for state, want := range map[string]string{"serving": "SERVING", "awaiting_approval": "AWAITING_APPROVAL", "": ""} {
		if got := plain.badge(state); got != want {
			t.Fatalf("plain badge(%q) = %q, want %q", state, got, want)
		}
	}
	if got := plain.chip("noise ±5.1%"); got != "[noise ±5.1%]" {
		t.Fatalf("plain chip = %q", got)
	}
}

func TestStyledThemePairsEveryColorWithItsWord(t *testing.T) {
	for _, background := range []string{"dark", "light", ""} {
		styled := styledTheme(80, background)
		for state, want := range map[string]string{"serving": "● SERVING", "running": "● RUNNING", "waiting": "● WAITING", "failed": "● FAILED", "something_new": "● SOMETHING NEW"} {
			got := styled.badge(state)
			if !strings.Contains(got, "\x1b[") || ansi.Strip(got) != want {
				t.Fatalf("%s badge(%q) = %q, want styled %q", background, state, got, want)
			}
		}
		better, worse, unknown := styled.delta("−12.0%", toneGood), styled.delta("+14.1%", toneBad), styled.delta("unknown", toneUnknown)
		if better == worse || ansi.Strip(better) != "−12.0%" || ansi.Strip(worse) != "+14.1%" || ansi.Strip(unknown) != "unknown" {
			t.Fatalf("%s deltas: better %q worse %q unknown %q", background, better, worse, unknown)
		}
		if chip := ansi.Strip(styled.chip("noise ±5.1%")); chip != " noise ±5.1% " {
			t.Fatalf("%s chip = %q", background, chip)
		}
	}
}

func TestStatusToneCoversEveryResourceState(t *testing.T) {
	for state, want := range map[string]string{
		"serving": toneGood, "qualified": toneGood, "succeeded": toneGood, "PASS": toneGood,
		"running": toneInfo, "provisioning": toneInfo, "measuring": toneInfo,
		"waiting": toneWatch, "awaiting_approval": toneWatch, "inconclusive": toneWatch, "WATCH": toneWatch,
		"failed": toneBad, "rejected": toneBad, "cancelled": toneBad, "BLOCK": toneBad,
		"unknown": toneUnknown, "cleaned": toneUnknown, "": toneUnknown,
	} {
		if got := statusTone(state); got != want {
			t.Fatalf("statusTone(%q) = %q, want %q", state, got, want)
		}
	}
}

func TestOutputIsPlainForScriptsAndNoColor(t *testing.T) {
	if outputTheme("json").styled {
		t.Fatal("--output json was styled")
	}
	// Tests run with stdout redirected, so human output is plain too.
	if outputTheme("human").styled {
		t.Fatal("redirected stdout was styled")
	}
	t.Setenv("NO_COLOR", "1")
	if styleAllowed(os.Stdout) {
		t.Fatal("NO_COLOR allowed styling")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if styleAllowed(os.Stdout) {
		t.Fatal("TERM=dumb allowed styling")
	}
}

func TestTerminalBackgroundReadsColorFGBGWithoutQueryingTheTerminal(t *testing.T) {
	for value, want := range map[string]string{"": "", "15;0": "dark", "0;15": "light", "0;7": "light", "15;default;0": "dark", "garbage": ""} {
		t.Setenv("COLORFGBG", value)
		if got := terminalBackground(); got != want {
			t.Fatalf("COLORFGBG=%q background = %q, want %q", value, got, want)
		}
	}
}

func TestWrapWordsKeepsEveryCharacter(t *testing.T) {
	text := "sha256:e3eadf5ddf0ffe8a7a28da882a1a6f6d2d90efab0d0abf9a458a6d6e2b90d011 (83ccd598) Qwen/Qwen3-0.6B"
	for _, width := range []int{12, 30, 80} {
		lines := wrapWords(text, width)
		for _, line := range lines {
			if ansi.StringWidth(line) > width {
				t.Fatalf("width %d: line %q is too wide", width, line)
			}
		}
		if strings.ReplaceAll(strings.Join(lines, ""), " ", "") != strings.ReplaceAll(text, " ", "") {
			t.Fatalf("width %d dropped characters: %q", width, lines)
		}
	}
	if got := wrapWords("meta-llama/Llama-3.1-8B-Instruct", 20); !strings.HasSuffix(got[0], "/") && !strings.HasSuffix(got[0], "-") {
		t.Fatalf("model name broke away from a seam: %q", got)
	}
}

func TestWrapFlagsBreaksAtFlagBoundaries(t *testing.T) {
	got := wrapFlags(`--kv-cache-dtype fp8 --max-num-batched-tokens 2048 --temperature -1`, 40)
	want := []string{"--kv-cache-dtype fp8", "--max-num-batched-tokens 2048", "--temperature -1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("wrapFlags = %q, want %q", got, want)
	}
	long := wrapFlags(`--speculative-config {"method": "ngram", "num_speculative_tokens": 4, "prompt_lookup_max": 4}`, 40)
	if len(long) < 2 || !strings.HasPrefix(long[1], "  ") || !strings.HasPrefix(long[0], "--speculative-config") {
		t.Fatalf("long flag value did not continue indented: %q", long)
	}
}

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for offset, want := range map[time.Duration]string{-10 * time.Second: "just now", -4 * time.Minute: "4m ago", -2 * time.Hour: "2h ago", -72 * time.Hour: "3d ago", 43 * time.Minute: "in 43m", 20 * time.Second: "in under a minute"} {
		if got := relativeTime(now, now.Add(offset)); got != want {
			t.Fatalf("relativeTime(%s) = %q, want %q", offset, got, want)
		}
	}
	if got := relativeTime(now, time.Time{}); got != "unknown" {
		t.Fatalf("zero time = %q, want unknown", got)
	}
}

func TestTableAlignsStyledCellsByDisplayWidth(t *testing.T) {
	styled := styledTheme(60, "dark")
	out := styled.table(0, []column{{header: "Name"}, {header: "State"}, {header: "Created", right: true}}, [][]string{{"qwen-prod", "● SERVING", "2m ago"}, {"llama", "● FAILED", "26h ago"}}, func(_, col int, text string) string {
		if col == 1 {
			return styled.tone(toneGood, text)
		}
		return text
	})
	lines := strings.Split(strings.TrimSuffix(ansi.Strip(out), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("table has %d lines:\n%s", len(lines), ansi.Strip(out))
	}
	if strings.Index(lines[2], "●") != strings.Index(lines[3], "●") || ansi.StringWidth(lines[2]) != ansi.StringWidth(lines[3]) {
		t.Fatalf("columns are not aligned:\n%s", ansi.Strip(out))
	}
}
