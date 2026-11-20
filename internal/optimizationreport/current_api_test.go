package optimizationreport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/apibaseline"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapicatalog"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

func migrationEvidence(t *testing.T, card *apibaseline.RateCard) Evidence {
	t.Helper()
	evidence := winnerFixture().evidence()
	reference, lower, upper := 0.95, -1.5, 3.25
	payload, _ := json.Marshal(qualityevidence.MigrationVerdict{Schema: qualityevidence.MigrationVerdictSchema, CampaignID: "campaign-1", Endpoint: "support-bot", BindingID: "binding-1", CandidateDeployment: "qwen-chat-opt-winner", CandidateRevisionID: "rev-winner", Suite: "support-graded", SuiteVersion: "1@sha256:x", Evaluator: "infercrane-graded", EvaluatorVersion: "1", MarginPercent: 5, Verdict: releaseguard.QualityPass, Code: "quality_within_policy", ReferenceScore: &reference, CandidateScore: 0.94, Bootstrap: &releaseguard.BootstrapComparison{Status: "accept", IntervalLowerPercent: &lower, IntervalUpperPercent: &upper}})
	evidence.CurrentAPI = &CurrentAPIEvidence{Endpoint: "support-bot", Binding: "primary", BindingID: "binding-1", Model: "gpt-4.1-mini", RateCard: card, Now: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC),
		Verdicts: []domain.MigrationQualityVerdict{{ID: "verdict-1", CandidateRunID: "winner", BindingID: "binding-1", RevisionID: "rev-winner", Verdict: releaseguard.QualityPass, PayloadJSON: string(payload)}}}
	// The closed API's live requests: 1h window, TTFT and cached input.
	shapes := []map[string]any{
		{"operation": "chat", "arrival_ms": 10, "duration_ms": 800, "ttft_ms": 300, "input_tokens": 100, "cached_input_tokens": 50, "output_tokens": 400},
		{"operation": "chat", "arrival_ms": 20, "duration_ms": 900, "ttft_ms": 350, "input_tokens": 100, "output_tokens": 600},
	}
	encoded, _ := json.Marshal(shapes)
	evidence.Replay.ShapeJSON = string(encoded)
	return evidence
}

func rateCard() *apibaseline.RateCard {
	input, cached, output := int64(2_000_000), int64(500_000), int64(8_000_000)
	return &apibaseline.RateCard{ID: "card-1", Authority: apibaseline.AuthorityListPrice, Pricing: modelapicatalog.Pricing{Currency: "USD", InputMicrousdPerMillion: &input, CachedInputMicrousdPerMillion: &cached, OutputMicrousdPerMillion: &output}, Source: "https://openai.com/api/pricing", ValidUntil: time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)}
}

func TestReportComparesTheWinnerWithTheCurrentAPI(t *testing.T) {
	report := Assemble(migrationEvidence(t, rateCard()))
	current := report.CurrentAPI
	if current == nil || current.Model != "gpt-4.1-mini" || current.Endpoint != "support-bot" {
		t.Fatalf("current API = %+v", current)
	}
	// 150 uncached × $2 + 50 cached × $0.5 + 1000 output × $8 per 1M tokens.
	window := (150*2.0 + 50*0.5 + 1000*8.0) / 1e6
	cost := current.Cost
	if cost.Basis != BasisRateCard || !near(cost.MonthlyUSD, window*730) || !near(cost.BlendedUSDPerMillionTokens, window/1200*1e6) || *cost.CachedInputTokens != 50 || cost.Authority != apibaseline.AuthorityListPrice {
		t.Fatalf("cost = %+v", cost)
	}
	if !near(cost.WinnerMonthlyUSD, *report.Monthly.WinnerUSD) || !near(cost.MonthlyChangeUSD, *report.Monthly.WinnerUSD-window*730) || cost.MonthlyChangePercent == nil || *cost.MonthlyChangePercent > -90 {
		t.Fatalf("switch = %+v", cost)
	}
	latency := current.Latency
	if latency.Basis != BasisObservedLiveTraffic || !near(latency.TTFTP95MS, 350) || !near(latency.LatencyP95MS, 900) || latency.Requests != 2 || !near(latency.WinnerTTFTP95MS, 380) || latency.Note != CurrentAPILatencyNote {
		t.Fatalf("latency = %+v", latency)
	}
	quality := current.Quality
	if quality.Verdict != releaseguard.QualityPass || quality.VerdictID != "verdict-1" || !near(quality.CurrentAPIScore, 0.95) || !near(quality.WinnerScore, 0.94) || !near(quality.IntervalUpperPercent, 3.25) {
		t.Fatalf("quality = %+v", quality)
	}
	for _, field := range report.Unknown {
		if strings.HasPrefix(field, "current_api") {
			t.Fatalf("complete current API reported unknown %s", field)
		}
	}
	text := Text(report)
	for _, want := range []string{"Your current API", "a month at your traffic", "list price", "different window", "PASS · current API 0.950, chosen setup 0.940", "against a 5.0% margin"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text lacks %q:\n%s", want, text)
		}
	}
	if html, err := HTML(report); err != nil || !strings.Contains(string(html), "Your current API") {
		t.Fatalf("html err=%v", err)
	}
}

func TestCurrentAPIStaysUnknownWithoutEvidence(t *testing.T) {
	expired := rateCard()
	expired.ValidUntil = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		edit func(*Evidence)
		path string
		want string
	}{
		"no rate card":    {func(e *Evidence) { e.CurrentAPI.RateCard = nil }, "current_api.cost", "no rate card"},
		"expired":         {func(e *Evidence) { e.CurrentAPI.RateCard = expired }, "current_api.cost", "expired"},
		"no verdict":      {func(e *Evidence) { e.CurrentAPI.Verdicts = nil }, "current_api.quality", "no signed verdict"},
		"binding deleted": {func(e *Evidence) { e.CurrentAPI.Reason = "the current API's binding no longer exists" }, "current_api.latency", "no longer exists"},
		"missing tokens": {func(e *Evidence) {
			e.Replay.ShapeJSON = `[{"operation":"chat","arrival_ms":1,"duration_ms":5,"ttft_ms":2}]`
		}, "current_api.cost", "token counts"},
	} {
		evidence := migrationEvidence(t, rateCard())
		test.edit(&evidence)
		report := Assemble(evidence)
		assertUnknown(t, report, test.path)
		reason := unknownReason(report, test.path)
		if !strings.Contains(reason, test.want) {
			t.Fatalf("%s: reason %q", name, reason)
		}
		if test.path == "current_api.cost" && (report.CurrentAPI.Cost.MonthlyUSD != nil || report.CurrentAPI.Cost.MonthlyChangeUSD != nil) {
			t.Fatalf("%s: cost invented: %+v", name, report.CurrentAPI.Cost)
		}
	}
	// A campaign that is not a migration has no current API at all.
	if report := Assemble(winnerFixture().evidence()); report.CurrentAPI != nil {
		t.Fatalf("current API on a non-migration campaign: %+v", report.CurrentAPI)
	}
}
