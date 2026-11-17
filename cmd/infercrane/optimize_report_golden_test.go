package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/infercrane/infercrane/internal/optimizationreport"
)

var updateGolden = flag.Bool("update", false, "rewrite the CLI golden files from the recorded fixtures")

// The fixture is the report of a real RTX 4090 campaign (83ccd598) in which no
// setup qualified. Its plain rendering is byte for byte the text that run
// printed.
func loadReportFixture(t *testing.T) (json.RawMessage, optimizationreport.Report) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "optimize-report-rtx4090.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report optimizationreport.Report
	if err = json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	return raw, report
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the golden file; run go test ./cmd/infercrane -run Golden -update and review the change\n--- got ---\n%s", name, got)
	}
}

func TestOptimizeReportPlainGoldenIsTheRealRunText(t *testing.T) {
	raw, report := loadReportFixture(t)
	var stdout, stderr bytes.Buffer
	if err := writeOptimizationReport(&stdout, &stderr, raw, report, "text", "", plainTheme); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "\x1b[") {
		t.Fatal("plain report contains ANSI escape sequences")
	}
	checkGolden(t, "optimize-report-rtx4090.plain.golden", stdout.Bytes())
}

func TestOptimizeReportStyledGolden(t *testing.T) {
	raw, report := loadReportFixture(t)
	for _, test := range []struct {
		name  string
		width int
	}{{"optimize-report-rtx4090.styled-80", 80}, {"optimize-report-rtx4090.styled-120", 120}, {"optimize-report-rtx4090.styled-60", 60}} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := writeOptimizationReport(&stdout, &stderr, raw, report, "text", "", styledTheme(test.width, "dark")); err != nil {
				t.Fatal(err)
			}
			styled := stdout.String()
			if !strings.Contains(styled, "\x1b[") {
				t.Fatal("styled report has no styling")
			}
			text := ansi.Strip(styled)
			for number, line := range strings.Split(text, "\n") {
				if width := ansi.StringWidth(line); width > test.width {
					t.Fatalf("line %d is %d columns wide, over %d: %q", number+1, width, test.width, line)
				}
			}
			if test.width == 80 {
				checkGolden(t, test.name+".ansi.golden", stdout.Bytes())
			}
			checkGolden(t, test.name+".golden", []byte(text))
		})
	}
}

// TestOptimizeReportStyledKeepsEveryValue proves the cards add no claim and
// drop no value: every value the plain view formats, including every
// "unknown" and its reason, appears in the styled rendering.
func TestOptimizeReportStyledKeepsEveryValue(t *testing.T) {
	_, report := loadReportFixture(t)
	view := optimizationreport.NewView(report)
	for _, width := range []int{60, 80, 120} {
		text := collapseSpace(ansi.Strip(renderStyledReport(view, styledTheme(width, "light"))))
		want := []string{view.Title, view.Subtitle, view.Headline}
		for _, row := range view.Rows {
			want = append(want, row.Label, row.Baseline, row.Winner, row.Change, row.Decision, row.Noise, row.Reason)
		}
		for _, line := range view.Measurement {
			_, value, _ := strings.Cut(line, ": ")
			want = append(want, value)
		}
		for _, candidate := range view.Candidates {
			want = append(want, candidate.Status, candidate.Versus, candidate.TokensPerSecond, candidate.TTFTP95, candidate.CostPerMillion)
			_, args := splitSetupLabel(candidate.Label)
			want = append(want, args)
		}
		for _, field := range append(append(view.Baseline, view.Winner...), view.Evidence...) {
			want = append(want, field.Name, field.Value)
		}
		for _, field := range view.Unknown {
			want = append(want, field.Value)
		}
		for _, value := range want {
			if value = collapseSpace(value); value != "" && !strings.Contains(text, value) {
				t.Errorf("width %d: styled report is missing %q", width, value)
			}
		}
		if got, want := strings.Count(text, "unknown"), strings.Count(collapseSpace(optimizationreport.Text(report)), "unknown"); got < want {
			t.Errorf("width %d: styled report shows %d unknowns, plain shows %d", width, got, want)
		}
	}
}

// collapseSpace drops frames and every space, so a value wrapped or broken
// across lines still matches itself.
func collapseSpace(text string) string {
	text = strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ", "└", " ").Replace(text)
	return strings.Join(strings.Fields(text), "")
}
