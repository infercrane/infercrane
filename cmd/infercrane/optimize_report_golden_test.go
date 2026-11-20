package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/infercrane/infercrane/internal/optimizationreport"
)

var updateGolden = flag.Bool("update", false, "rewrite the CLI golden files from the recorded fixtures")

// The fixtures are reports of a real RTX 4090 campaign (83ccd598). The first is
// how it read before cleanup kept its decision (no setup qualified); its plain
// rendering is byte for byte the text that run printed, plus the line saying it
// is not signed. The second, "-kept", is the same run as the control plane
// reports it now: the current setup kept, with InferCrane's built-in answer
// check, and signed by a fixed test key.
var reportFixtures = []string{"optimize-report-rtx4090", "optimize-report-rtx4090-kept"}

// fixtureSigningKey is a fixed test key, so the golden files name a stable key ID.
var fixtureSigningKey = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))

func loadReportFixture(t *testing.T, name string) (json.RawMessage, optimizationreport.Document) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var report optimizationreport.Report
	if err = json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	var key ed25519.PrivateKey
	if strings.HasSuffix(name, "-kept") {
		key = fixtureSigningKey
	}
	document, err := optimizationreport.Sign(report, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return raw, document
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

func TestOptimizeReportPlainGolden(t *testing.T) {
	for _, name := range reportFixtures {
		t.Run(name, func(t *testing.T) {
			raw, report := loadReportFixture(t, name)
			var stdout, stderr bytes.Buffer
			if err := writeOptimizationReport(&stdout, &stderr, raw, report, "text", "", plainTheme); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stdout.String(), "\x1b[") {
				t.Fatal("plain report contains ANSI escape sequences")
			}
			checkGolden(t, name+".plain.golden", stdout.Bytes())
		})
	}
}

func TestOptimizeReportStyledGolden(t *testing.T) {
	for _, name := range reportFixtures {
		raw, report := loadReportFixture(t, name)
		for _, width := range []int{60, 80, 120} {
			golden := fmt.Sprintf("%s.styled-%d", name, width)
			t.Run(golden, func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				if err := writeOptimizationReport(&stdout, &stderr, raw, report, "text", "", styledTheme(width, "dark")); err != nil {
					t.Fatal(err)
				}
				styled := stdout.String()
				if !strings.Contains(styled, "\x1b[") {
					t.Fatal("styled report has no styling")
				}
				text := ansi.Strip(styled)
				for number, line := range strings.Split(text, "\n") {
					if lineWidth := ansi.StringWidth(line); lineWidth > width {
						t.Fatalf("line %d is %d columns wide, over %d: %q", number+1, lineWidth, width, line)
					}
				}
				if width == 80 {
					checkGolden(t, golden+".ansi.golden", stdout.Bytes())
				}
				checkGolden(t, golden+".golden", []byte(text))
			})
		}
	}
}

// The answer-quality outcome is named in a few words; the suite identity,
// whose version carries a digest, stays in the evidence. A long label used to
// widen the plain table's first column to over 100 characters.
func TestOptimizeReportNamesAnswerQualityShortlyAndKeepsTheSuiteInEvidence(t *testing.T) {
	_, report := loadReportFixture(t, "optimize-report-rtx4090-kept")
	suite := report.Report.Quality.Eval
	plain := optimizationreport.Text(report)
	if !strings.Contains(plain, "\nAnswer quality · InferCrane answer check  ") {
		t.Fatalf("plain report lacks the short answer-quality label:\n%s", plain)
	}
	for _, width := range []int{60, 80, 120} {
		styled := cardsInReadingOrder(ansi.Strip(renderStyledReport(optimizationreport.NewView(report), styledTheme(width, "dark"))))
		if !strings.Contains(collapseSpace(styled), "AnswerqualityInferCraneanswercheck") && !strings.Contains(collapseSpace(styled), "Answerquality·InferCraneanswercheck") {
			t.Fatalf("styled report at %d columns lacks the short answer-quality label:\n%s", width, styled)
		}
	}
	styled := ansi.Strip(renderStyledReport(optimizationreport.NewView(report), styledTheme(80, "dark")))
	for name, text := range map[string]string{"plain": plain, "styled": styled} {
		label, evidence, found := strings.Cut(text, "Evidence")
		if !found {
			t.Fatalf("%s report has no evidence section", name)
		}
		if strings.Contains(label, suite) {
			t.Fatalf("%s report shows the suite identity before the evidence:\n%s", name, text)
		}
		if !strings.Contains(collapseSpace(evidence), collapseSpace(suite)) {
			t.Fatalf("%s report lost the suite identity from its evidence:\n%s", name, text)
		}
	}
	header := strings.Split(plain, "\n")[5]
	if column := strings.Index(header, "Current setup"); column < 0 || column > 50 {
		t.Fatalf("plain outcome table's first column is %d wide:\n%s", column, plain)
	}
}

// TestOptimizeReportStyledKeepsEveryValue proves the cards add no claim and
// drop no value: every value the plain view formats, including every
// "unknown" and its reason, appears in the styled rendering.
func TestOptimizeReportStyledKeepsEveryValue(t *testing.T) {
	for _, name := range reportFixtures {
		_, report := loadReportFixture(t, name)
		assertStyledReportKeepsEveryValue(t, report)
	}
}

func assertStyledReportKeepsEveryValue(t *testing.T, report optimizationreport.Document) {
	t.Helper()
	view := optimizationreport.NewView(report)
	for _, width := range []int{60, 80, 120} {
		text := collapseSpace(cardsInReadingOrder(ansi.Strip(renderStyledReport(view, styledTheme(width, "light")))))
		want := []string{view.Title, view.Subtitle, view.Headline, view.Signature}
		for _, row := range view.Rows {
			// A card may break a long label at its " · " seam.
			head, tail, _ := strings.Cut(row.Label, " · ")
			want = append(want, head, tail, row.Baseline, row.Winner, row.Change, row.Decision, row.Noise, row.Reason)
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

// cardsInReadingOrder rewrites rows of side-by-side cards as one card after
// another, so text wrapped inside a card reads in order.
func cardsInReadingOrder(text string) string {
	var out []string
	var starts []int
	var cards [][]string
	for _, line := range strings.Split(text, "\n") {
		runes := []rune(line)
		switch {
		case strings.HasPrefix(line, "╭") && strings.Contains(line, "╮ ╭"):
			starts = nil
			for index, r := range runes {
				if r == '╭' {
					starts = append(starts, index)
				}
			}
			cards = make([][]string, len(starts))
		case starts != nil && strings.HasPrefix(line, "╰"):
			for _, card := range cards {
				out = append(out, card...)
			}
			starts, cards = nil, nil
		case starts != nil:
			for index, start := range starts {
				end := len(runes)
				if index+1 < len(starts) {
					end = min(end, starts[index+1])
				}
				if start < end {
					cards[index] = append(cards[index], string(runes[start:end]))
				}
			}
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// collapseSpace drops frames and every space, so a value wrapped or broken
// across lines still matches itself.
func collapseSpace(text string) string {
	text = strings.NewReplacer("│", " ", "╭", " ", "╮", " ", "╰", " ", "╯", " ", "─", " ", "└", " ").Replace(text)
	return strings.Join(strings.Fields(text), "")
}
