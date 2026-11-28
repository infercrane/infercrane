package optimizationreport

import (
	"encoding/json"
	"html"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

func orderedIn(t *testing.T, text string, parts ...string) {
	t.Helper()
	last := -1
	for _, part := range parts {
		index := strings.Index(text, part)
		if index < 0 {
			t.Fatalf("output lacks %q:\n%s", part, text)
		}
		if index < last {
			t.Fatalf("%q appears out of order:\n%s", part, text)
		}
		last = index
	}
}

func TestTextLeadsWithOutcomesThenEvidence(t *testing.T) {
	text := Text(Document{Report: Assemble(winnerFixture().evidence())})
	orderedIn(t, text,
		"Roll out “Up to 256 concurrent requests”",
		"Cost per 1M output tokens", "p95 time to first token", "Output tokens/s per user", "Output tokens/s per GPU", "Requests/s", "Answer quality · gsm8k", "Monthly cost at your traffic",
		"How it was measured", "Noise band: ±3.0% on output throughput", "Load: a replay of your traffic",
		"Every setup measured", "Bottleneck signals", "\nWinning setup\n", "\nYour current setup\n", "\nEvidence\n", "this report")
	for _, want := range []string{"−23.1%", "better  noise ±3.0%", "no real difference  noise ±8.0%", "3 of the winner, taken in turns", "PASS", "730-hour month", "--max-num-seqs 256", "1 × H100 · aws eu-central-1 elastic · 1–2 replicas"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Unknown, never estimated") {
		t.Fatalf("a complete report listed unknown fields:\n%s", text)
	}
}

func TestTextExplainsEveryUnknown(t *testing.T) {
	f := winnerFixture()
	f.arms[1].hourly = nil
	text := Text(Document{Report: Assemble(f.evidence())})
	orderedIn(t, text, "Cost per 1M output tokens", "└ winner: no sourced hourly price", "Unknown, never estimated", "Cost per 1M output tokens: winner: no sourced hourly price", "Monthly cost: cost per output token is unknown")
}

func TestHTMLIsSelfContainedAndOrdered(t *testing.T) {
	f := winnerFixture()
	f.arms[1].args = []string{"--served-model-name", "<script>alert(1)</script>"}
	page, err := HTML(Document{Report: Assemble(f.evidence())})
	if err != nil {
		t.Fatal(err)
	}
	html := string(page)
	for _, forbidden := range []string{"<script", "http://", "https://", "@import", "src=", "<link"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("HTML report is not self-contained or leaks markup: contains %q", forbidden)
		}
	}
	if !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatal("setup arguments were not escaped")
	}
	orderedIn(t, html, `<section aria-label="Outcomes" class="cards">`, "Cost per 1M output tokens", "Monthly cost at your traffic", "How it was measured", "Every setup measured", "Bottleneck signals", "Appendix: setup and evidence", "Evidence digests")

	// Without engine metrics the bottleneck section is omitted, not empty.
	measured := interleaved()
	for index := range measured.Runs {
		measured.Runs[index].ServerMetrics = nil
	}
	f.arms[1].measured = measured
	page, err = HTML(Document{Report: Assemble(f.evidence())})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "<h2>Bottleneck signals</h2>") || !strings.Contains(string(page), "Bottleneck signals: the serving engine&#39;s metrics were not scraped") {
		t.Fatal("HTML shows an empty bottleneck section or hides why it is unknown")
	}
}

// The report never names internal mechanics that are not for customers.
func TestNoOutputNamesInternalSearchMechanics(t *testing.T) {
	kept := winnerFixture()
	kept.arms[0].state, kept.arms[1].state = optimizationcampaign.CandidateQualified, optimizationcampaign.CandidateCleaned
	simulated := winnerFixture()
	for index := range simulated.arms {
		simulated.arms[index].gpu = "SIMULATED-CPU"
	}
	for name, f := range map[string]fixture{"winner": winnerFixture(), "kept": kept, "simulated": simulated} {
		report := Assemble(f.evidence())
		encoded, _ := json.Marshal(report)
		page, err := HTML(Document{Report: report})
		if err != nil {
			t.Fatal(err)
		}
		for format, output := range map[string]string{"json": string(encoded), "text": Text(Document{Report: report}), "html": string(page)} {
			if strings.Contains(strings.ToLower(output), "recipe search") {
				t.Fatalf("%s %s output names an internal mechanic", name, format)
			}
		}
	}
}

func TestFormatting(t *testing.T) {
	for value, want := range map[float64]string{0.4273: "$0.427", 2.5: "$2.50", 1240.4: "$1,240", 0: "$0"} {
		if got := usd(value); got != want {
			t.Errorf("usd(%v) = %q, want %q", value, got, want)
		}
	}
	if signedPercent(-23.08) != "−23.1%" || signedPercent(5) != "+5.0%" || signedUSD(-322.4) != "−$322" {
		t.Fatal("signed formatting changed")
	}
	if duration(7260) != "2h 1m" || duration(95) != "1m 35s" || grouped(1234567) != "1,234,567" {
		t.Fatal("duration or grouping formatting changed")
	}
}

// The answer-quality outcome is named in a few words in the table and the
// cards. The suite identity, whose version carries a digest, stays in the
// evidence: a quality evidence entry already names it, or the report adds it.
func TestAnswerQualityLabelIsShortAndTheSuiteStaysInEvidence(t *testing.T) {
	const digestSuite = "infercrane-default@1@sha256:f65db09bb9ba827c58c317236a8faf795f73b09b9380d84307b69334782611af"
	tests := []struct {
		name         string
		quality      Quality
		evidence     []EvidenceDigest
		label        string
		evidenceName string
	}{
		{name: "built-in check", quality: Quality{Source: "infercrane", Eval: digestSuite}, evidence: []EvidenceDigest{{Kind: "winner_quality", Digest: "sha256:aa", Subject: digestSuite + " signed by sha256:7018"}}, label: "Answer quality · InferCrane answer check", evidenceName: "winner quality"},
		{name: "customer eval", quality: Quality{Source: "customer", Eval: "gsm8k@3"}, label: "Answer quality · gsm8k", evidenceName: "quality eval"},
		{name: "Release Guard eval", quality: Quality{Basis: BasisReleaseGuard, Eval: "support-tickets@2@sha256:0123"}, evidence: []EvidenceDigest{{Kind: "release_guard", ID: "rg-1", Digest: "sha256:bb", Subject: "decision accept"}}, label: "Answer quality · support-tickets", evidenceName: "quality eval"},
		{name: "no eval", quality: Quality{Verdict: "UNKNOWN"}, label: "Answer quality"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.quality.Evals = []QualityEval{}
			report := Report{SchemaVersion: SchemaVersion, CampaignID: "c1", Quality: test.quality, Appendix: Appendix{Evidence: test.evidence}, ReportDigest: "sha256:cc"}
			view := NewView(Document{Report: report})
			if got := view.Rows[len(view.Rows)-2].Label; got != test.label {
				t.Fatalf("label = %q, want %q", got, test.label)
			}
			text := Text(Document{Report: report})
			table, evidence, _ := strings.Cut(text, "\nEvidence\n")
			if eval := test.quality.Eval; eval != "" {
				if strings.Contains(table, eval) {
					t.Fatalf("the suite identity is in the outcome table:\n%s", text)
				}
				if !strings.Contains(evidence, eval) || !strings.Contains(evidence, test.evidenceName) {
					t.Fatalf("evidence lacks %q under %q:\n%s", eval, test.evidenceName, text)
				}
				if strings.Count(evidence, eval) != 1 {
					t.Fatalf("evidence names the suite %d times:\n%s", strings.Count(evidence, eval), text)
				}
			}
			page, err := HTML(Document{Report: report})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(page), `<div class="label">`+html.EscapeString(test.label)+`</div>`) {
				t.Fatalf("HTML card label is not %q", test.label)
			}
		})
	}
}

// The renderers use the dashboard's words. The current setup is the baseline
// the others were measured against, so it never reads "rejected", and a
// winner waits to be rolled out, not activated.
func TestRenderersSayRunBaselineAndRollOut(t *testing.T) {
	f := winnerFixture()
	f.arms[0].state = optimizationcampaign.CandidateRejected
	report := Assemble(f.evidence())
	page, err := HTML(Document{Report: report})
	if err != nil {
		t.Fatal(err)
	}
	for format, output := range map[string]string{"text": Text(Document{Report: report}), "html": html.UnescapeString(string(page))} {
		for _, want := range []string{"· run " + report.CampaignID, "run input", "baseline · another setup measured better", "winner, ready to roll out"} {
			if !strings.Contains(output, want) {
				t.Fatalf("%s lacks %q:\n%s", format, want, output)
			}
		}
		for _, wrong := range []string{"campaign input", "campaign " + report.CampaignID, "rejected · another setup measured better", "activate"} {
			if strings.Contains(output, wrong) {
				t.Fatalf("%s says %q:\n%s", format, wrong, output)
			}
		}
	}
	// A setup that is not the current one keeps its verdict.
	f.arms[2].state = optimizationcampaign.CandidateRejected
	if text := Text(Document{Report: Assemble(f.evidence())}); !strings.Contains(text, "· rejected · ") {
		t.Fatalf("a rejected setup lost its verdict:\n%s", text)
	}
}

// A setup an agent refused says why in words, not as a code.
func TestASetupAnAgentRefusedSaysWhy(t *testing.T) {
	for code, want := range map[string]string{
		optimizationcampaign.FailureAgentSetupNotPlainServer: "failed · the agent starts plain servers only, and this setup is another kind of server",
		optimizationcampaign.FailureAgentSetupUnsupported:    "failed · the agent cannot start this setup yet",
	} {
		if words := statusWords(optimizationcampaign.CandidateFailed, code); words != want {
			t.Fatalf("%s: %q", code, words)
		}
	}
}
