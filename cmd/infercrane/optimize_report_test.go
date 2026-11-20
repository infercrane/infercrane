package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/passport"
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
		Quality:    optimizationreport.Quality{Verdict: "UNKNOWN", Basis: optimizationreport.BasisUnknown, Evals: []optimizationreport.QualityEval{}, Reason: "no signed quality evidence was found for the winner"},
		Noise:      optimizationreport.Noise{Basis: optimizationreport.BasisUnknown, ObjectiveDecision: optimizationreport.DecisionUnknown, Reason: "the control was not measured alongside the winner"},
		Monthly:    optimizationreport.Monthly{Basis: optimizationreport.BasisUnknown, HoursPerMonth: optimizationreport.HoursPerMonth, Decision: optimizationreport.DecisionUnknown, Reason: "the campaign did not replay your traffic, so there is no traffic volume to project"},
		Workload:   optimizationreport.Workload{Source: "synthetic_profile"},
		Candidates: []optimizationreport.CandidateResult{},
		Bottleneck: optimizationreport.Bottleneck{State: "unknown", Source: optimizationreport.BasisEngineMetrics, GPUTelemetry: "not_collected", Signals: []optimizationreport.EngineSignal{}},
		Appendix:   optimizationreport.Appendix{Evidence: []optimizationreport.EvidenceDigest{}},
		Unknown:    []string{"metrics.ttft_p95_ms", "quality.verdict", "noise", "monthly", "bottleneck"},
	}
}

// signedCLIReport is cliReport as the control plane serves it: with its real
// report digest, signed by key.
func signedCLIReport(t *testing.T, key ed25519.PrivateKey) []byte {
	t.Helper()
	report := cliReport()
	unsigned, _ := json.Marshal(report)
	sum := sha256.Sum256(unsigned)
	report.ReportDigest = "sha256:" + hex.EncodeToString(sum[:])
	document, err := optimizationreport.Sign(report, key)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(document)
	return body
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
	serveReport(t, signedCLIReport(t, fixtureSigningKey))
	output, err := captureStdout(t, func() error { return optimizeReportCommand(t.Context(), []string{"campaign-1"}) })
	if err != nil {
		t.Fatal(err)
	}
	banner, headline, cost, unknown := strings.Index(output, "!! CPU run: proves the pipeline, not GPU speed !!"), strings.Index(output, "Roll out “custom image, --parallel 4”"), strings.Index(output, "Cost per 1M output tokens"), strings.Index(output, "Unknown, never estimated")
	if banner < 0 || headline < banner || cost < headline || unknown < cost {
		t.Fatalf("report text is missing the banner, the outcome, or the unknowns, or is out of order:\n%s", output)
	}
	signed := "Signed by key " + passport.KeyID(fixtureSigningKey.Public().(ed25519.PublicKey)) + " — verify with infercrane optimize report verify FILE"
	for _, want := range []string{"−23.1%", "better  noise ±4.0%", "└ ttft_p95_ms was not comparable", "Monthly cost: the campaign did not replay your traffic", signed} {
		if !strings.Contains(output, want) {
			t.Fatalf("report text lacks %q:\n%s", want, output)
		}
	}
}

func TestOptimizeReportJSONIsTheSignedControlPlaneReport(t *testing.T) {
	body := signedCLIReport(t, fixtureSigningKey)
	serveReport(t, body)
	output, err := captureStdout(t, func() error {
		return optimizeReportCommand(t.Context(), []string{"campaign-1", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var printed struct {
		Report    map[string]any                `json:"report"`
		Signature *optimizationreport.Signature `json:"signature"`
	}
	if err = json.Unmarshal([]byte(output), &printed); err != nil {
		t.Fatalf("json output is not JSON: %v\n%s", err, output)
	}
	var sent optimizationreport.Document
	_ = json.Unmarshal(body, &sent)
	if printed.Report["report_digest"] != sent.Report.ReportDigest || printed.Report["simulated_accelerator"] != true || printed.Report["banner"] != optimizationreport.CPUBanner {
		t.Fatalf("json output changed the report: %v", printed.Report)
	}
	if printed.Signature == nil || *printed.Signature != *sent.Signature {
		t.Fatalf("json output dropped or changed the signature: %+v", printed.Signature)
	}
	metrics := printed.Report["metrics"].([]any)
	if ttft := metrics[1].(map[string]any); ttft["change_percent"] != nil || ttft["decision"] != "unknown" {
		t.Fatalf("an unknown change was not kept as null: %v", ttft)
	}
}

func TestOptimizeReportVerifyChecksASavedReport(t *testing.T) {
	serveReport(t, signedCLIReport(t, fixtureSigningKey))
	output, err := captureStdout(t, func() error {
		return optimizeReportCommand(t.Context(), []string{"campaign-1", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	saved := filepath.Join(dir, "report.json")
	if err = os.WriteFile(saved, []byte(output), 0o644); err != nil {
		t.Fatal(err)
	}
	trusted := base64.StdEncoding.EncodeToString(fixtureSigningKey.Public().(ed25519.PublicKey))
	keyID := passport.KeyID(fixtureSigningKey.Public().(ed25519.PublicKey))
	verify := func(args ...string) (string, error) {
		return captureStdout(t, func() error { return optimizeReportCommand(t.Context(), append([]string{"verify"}, args...)) })
	}
	if printed, err := verify(saved); err != nil || !strings.Contains(printed, "VERIFIED  sha256:") || !strings.Contains(printed, "Run       campaign-1") || !strings.Contains(printed, keyID+" (compare it with your operator's key") {
		t.Fatalf("untouched report: %q err=%v", printed, err)
	}
	if printed, err := verify(saved, "--public-key", trusted); err != nil || !strings.Contains(printed, keyID+" (matches --public-key)") {
		t.Fatalf("pinned report: %q err=%v", printed, err)
	}
	_, other, _ := passport.GenerateKey()
	if _, err := verify(saved, "--public-key", base64.StdEncoding.EncodeToString(other.Public().(ed25519.PublicKey))); err == nil || !strings.Contains(err.Error(), "not the key you trust") {
		t.Fatalf("another trusted key: err=%v", err)
	}
	changed := filepath.Join(dir, "changed.json")
	if err = os.WriteFile(changed, []byte(strings.Replace(output, "Qwen/Qwen3-0.6B", "Qwen/Qwen3-0.7B", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(changed); err == nil || !strings.Contains(err.Error(), "report verification failed") {
		t.Fatalf("changed report: err=%v", err)
	}
	unsigned := filepath.Join(dir, "unsigned.json")
	document, _ := optimizationreport.Sign(cliReport(), nil)
	body, _ := json.Marshal(document)
	if err = os.WriteFile(unsigned, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(unsigned); err == nil || !strings.Contains(err.Error(), "not signed: this control plane has no signing key") {
		t.Fatalf("unsigned report: err=%v", err)
	}
	for _, args := range [][]string{{}, {"--public-key", trusted}, {saved, "extra"}, {saved, "--public-key", "not-a-key"}} {
		if _, err := verify(args...); err == nil {
			t.Fatalf("args %v verified", args)
		}
	}
	if _, err := verify(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("a missing file verified")
	}
}

func TestOptimizeReportWritesSelfContainedHTML(t *testing.T) {
	serveReport(t, signedCLIReport(t, fixtureSigningKey))
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
	if !bytes.Contains(page, []byte("Signed by key "+passport.KeyID(fixtureSigningKey.Public().(ed25519.PublicKey))+" — verify with infercrane optimize report verify FILE")) {
		t.Fatalf("HTML report does not name its signing key:\n%s", page)
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
