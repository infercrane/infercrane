package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizationreport"
)

// cliReport is a finished CPU campaign as the control plane returns it.
func cliReport() optimizationreport.Report {
	baseline, winner, change, floor := 0.0347, 0.0267, -23.1, 4.0
	ttftBase, ttftWin := 900.0, 700.0
	return optimizationreport.Report{
		SchemaVersion: optimizationreport.SchemaVersion, AlgorithmVersion: optimizationreport.AlgorithmVersion, CampaignID: "campaign-1", ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive", CampaignState: "qualified", Finished: true,
		SimulatedAccelerator: true, Banner: optimizationreport.CPUBanner,
		Outcome: optimizationreport.Outcome{Kind: optimizationreport.OutcomeWinnerSelected, Headline: "Roll out “custom image, --parallel 4”: it measured better than your current setup on the same traffic.", WinnerCandidateID: "winner", WinnerLabel: "custom image, --parallel 4"},
		Metrics: []optimizationreport.Metric{
			{Key: optimizationreport.MetricCostPerMillion, Label: "Cost per 1M output tokens", Baseline: &baseline, Winner: &winner, ChangePercent: &change, NoiseFloorPercent: &floor, Decision: optimizationreport.DecisionImproved, Basis: optimizationreport.BasisInterleaved},
			{Key: optimizationreport.MetricTTFTP95, Label: "p95 time to first token", Baseline: &ttftBase, Winner: &ttftWin, Decision: optimizationreport.DecisionUnknown, Basis: optimizationreport.BasisUnknown, Reason: "ttft_p95_ms was not comparable between the interleaved runs"},
		},
		Quality:      optimizationreport.Quality{Verdict: "UNKNOWN", Basis: optimizationreport.BasisUnknown, Evals: []optimizationreport.QualityEval{}, Reason: "no signed quality evidence was found for the winner"},
		Noise:        optimizationreport.Noise{Basis: optimizationreport.BasisUnknown, ObjectiveDecision: optimizationreport.DecisionUnknown, Reason: "the control was not measured alongside the winner"},
		Monthly:      optimizationreport.Monthly{Basis: optimizationreport.BasisUnknown, HoursPerMonth: optimizationreport.HoursPerMonth, Decision: optimizationreport.DecisionUnknown, Reason: "the campaign did not replay your traffic, so there is no traffic volume to project"},
		Workload:     optimizationreport.Workload{Source: "synthetic_profile"},
		Candidates:   []optimizationreport.CandidateResult{},
		Bottleneck:   optimizationreport.Bottleneck{State: "unknown", Source: optimizationreport.BasisEngineMetrics, GPUTelemetry: "not_collected", Signals: []optimizationreport.EngineSignal{}},
		Appendix:     optimizationreport.Appendix{Evidence: []optimizationreport.EvidenceDigest{}},
		Unknown:      []string{"metrics.ttft_p95_ms", "quality.verdict", "noise", "monthly", "bottleneck"},
		ReportDigest: "sha256:" + strings.Repeat("f", 64),
	}
}

func serveReport(t *testing.T, body []byte) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/optimization/campaigns/campaign-1/report" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "test-secret")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
}

func TestOptimizeReportPrintsOutcomesWithBannerAndUnknowns(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"report": cliReport()})
	serveReport(t, body)
	output, err := captureStdout(t, func() error { return optimizeReportCommand(t.Context(), []string{"campaign-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	banner, headline, cost, unknown := strings.Index(output, "!! CPU run: proves the pipeline, not GPU speed !!"), strings.Index(output, "Roll out “custom image, --parallel 4”"), strings.Index(output, "Cost per 1M output tokens"), strings.Index(output, "Unknown, never estimated")
	if banner < 0 || headline < banner || cost < headline || unknown < cost {
		t.Fatalf("report text is missing the banner, the outcome, or the unknowns, or is out of order:\n%s", output)
	}
	for _, want := range []string{"−23.1%", "better  noise ±4.0%", "└ ttft_p95_ms was not comparable", "Monthly cost: the campaign did not replay your traffic"} {
		if !strings.Contains(output, want) {
			t.Fatalf("report text lacks %q:\n%s", want, output)
		}
	}
}

func TestOptimizeReportJSONIsTheControlPlaneReport(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"report": cliReport()})
	serveReport(t, body)
	output, err := captureStdout(t, func() error {
		return optimizeReportCommand(t.Context(), []string{"campaign-1", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var printed, sent map[string]any
	if err = json.Unmarshal([]byte(output), &printed); err != nil {
		t.Fatalf("json output is not JSON: %v\n%s", err, output)
	}
	var envelope struct {
		Report json.RawMessage `json:"report"`
	}
	_ = json.Unmarshal(body, &envelope)
	_ = json.Unmarshal(envelope.Report, &sent)
	if printed["report_digest"] != sent["report_digest"] || printed["simulated_accelerator"] != true || printed["banner"] != optimizationreport.CPUBanner {
		t.Fatalf("json output changed the report: %v", printed)
	}
	metrics := printed["metrics"].([]any)
	if ttft := metrics[1].(map[string]any); ttft["change_percent"] != nil || ttft["decision"] != "unknown" {
		t.Fatalf("an unknown change was not kept as null: %v", ttft)
	}
}

func TestOptimizeReportWritesSelfContainedHTML(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"report": cliReport()})
	serveReport(t, body)
	path := filepath.Join(t.TempDir(), "result.html")
	output, err := captureStdout(t, func() error {
		return optimizeReportCommand(t.Context(), []string{"campaign-1", "--output", "json", "--html", path})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(output)) {
		t.Fatalf("--html polluted the JSON output:\n%s", output)
	}
	page, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(page, []byte("<!doctype html>")) || !bytes.Contains(page, []byte(optimizationreport.CPUBanner)) || bytes.Contains(page, []byte("<script")) || bytes.Contains(page, []byte("https://")) {
		t.Fatalf("HTML report is not a self-contained page with the CPU banner:\n%s", page)
	}
	if bytes.Index(page, []byte("Cost per 1M output tokens")) > bytes.Index(page, []byte("How it was measured")) {
		t.Fatal("HTML report does not lead with outcome cards")
	}
}

func TestOptimizeReportRejectsBadArgumentsAndUnsupportedReports(t *testing.T) {
	for _, args := range [][]string{{}, {"--output", "json"}, {"campaign-1", "--output", "yaml"}, {"campaign-1", "extra"}} {
		if err := optimizeReportCommand(t.Context(), args); err == nil || !strings.Contains(err.Error(), "usage: infercrane optimize report") {
			t.Fatalf("args %v: err=%v", args, err)
		}
	}
	serveReport(t, []byte(`{"report":{"schema_version":"something-else/v9"}}`))
	if err := optimizeReportCommand(t.Context(), []string{"campaign-1"}); err == nil || !strings.Contains(err.Error(), "unsupported optimization report") {
		t.Fatalf("an unknown report schema was rendered: %v", err)
	}
}
