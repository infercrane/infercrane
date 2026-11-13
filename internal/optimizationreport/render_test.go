package optimizationreport

import (
	"encoding/json"
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
	text := Text(Assemble(winnerFixture().evidence()))
	orderedIn(t, text,
		"Roll out “vllm, --max-num-seqs 256”",
		"Cost per 1M output tokens", "p95 time to first token", "Output tokens/s per user", "Output tokens/s per GPU", "Requests/s", "Answer quality (gsm8k@1)", "Monthly cost at your traffic",
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
	text := Text(Assemble(f.evidence()))
	orderedIn(t, text, "Cost per 1M output tokens", "└ winner: no sourced hourly price", "Unknown, never estimated", "Cost per 1M output tokens: winner: no sourced hourly price", "Monthly cost: cost per output token is unknown")
}

func TestHTMLIsSelfContainedAndOrdered(t *testing.T) {
	f := winnerFixture()
	f.arms[1].args = []string{"--served-model-name", "<script>alert(1)</script>"}
	page, err := HTML(Assemble(f.evidence()))
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
	page, err = HTML(Assemble(f.evidence()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(page), "<h2>Bottleneck signals</h2>") || !strings.Contains(string(page), "Bottleneck signals: the serving engine reported no") {
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
		page, err := HTML(report)
		if err != nil {
			t.Fatal(err)
		}
		for format, output := range map[string]string{"json": string(encoded), "text": Text(report), "html": string(page)} {
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
