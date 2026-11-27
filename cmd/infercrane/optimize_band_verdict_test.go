package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/bandverdict"
	"github.com/infercrane/infercrane/internal/optimizationreport"
)

func TestOptimizeBandVerdictReadsAScreenAndNamesAWinnerPerBand(t *testing.T) {
	useful := func(value float64) bandverdict.Measure {
		return bandverdict.Measure{Output: value * 1.2, UsefulOutput: &value}
	}
	screen := bandverdict.Screen{SchemaVersion: bandverdict.SchemaV1, LimitMS: 1000, Current: "defaults", Copy: "copy",
		Rules: bandverdict.Rules{MaxCurrentSpread: 0.05, MinNoiseFloor: 0.01},
		Bands: []bandverdict.Band{
			{Kind: "short", Load: 16, CurrentRuns: []bandverdict.Measure{useful(400), useful(402)}, Setups: []bandverdict.Setup{{ID: "pair", Runs: []bandverdict.Measure{useful(480)}}, {ID: "copy", Runs: []bandverdict.Measure{useful(401)}}}},
			{Kind: "short", Load: 64, CurrentRuns: []bandverdict.Measure{useful(1000), useful(990)}, Setups: []bandverdict.Setup{{ID: "pair", Runs: []bandverdict.Measure{useful(1000)}}, {ID: "copy", Runs: []bandverdict.Measure{useful(996)}}}},
		}}
	encoded, _ := json.Marshal(screen)
	path := filepath.Join(t.TempDir(), "screen.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"band-verdict", "--file", path})
	})
	if err != nil || !strings.Contains(output, "short/16: winner") || !strings.Contains(output, "short/64: keep_current") || !strings.Contains(output, "setup_per_band") || !strings.Contains(output, "not a proof") {
		t.Fatalf("output=%q err=%v", output, err)
	}
	output, err = captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"band-verdict", "--file", path, "--output", "json"})
	})
	var verdict bandverdict.Verdict
	if err != nil || json.Unmarshal([]byte(output), &verdict) != nil || verdict.Winners["short/16"] != "pair" {
		t.Fatalf("json output=%q err=%v", output, err)
	}
	if err = optimizeCommand(context.Background(), []string{"band-verdict"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("no file: %v", err)
	}
}

func TestOptimizeBandsReadsTheStudyFromTheControlPlane(t *testing.T) {
	var asked string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RequestURI()
		study := optimizationreport.BandStudy{SchemaVersion: optimizationreport.BandStudySchema, Model: "Qwen/Qwen3-8B", Current: "setup-current",
			Campaigns: []optimizationreport.StudyCampaign{{ID: "run-16", Band: "throughput/16"}, {ID: "run-64", Band: "throughput/64"}},
			Setups:    map[string]optimizationreport.StudySetup{"setup-pair": {Label: "FP8 weights and an FP8 KV cache", Engine: "vllm", EngineVersion: "0.34.0", Args: []string{"--quantization", "fp8"}}},
			Verdict: bandverdict.Verdict{Metric: bandverdict.MetricUsefulOutput, Summary: bandverdict.SummaryPerBand, Reason: "Different setups win different bands.",
				Bands:       []bandverdict.BandVerdict{{Key: "throughput/16", Load: 16, Decision: bandverdict.DecisionWinner, Winner: "setup-pair", Reason: "selected"}, {Key: "throughput/64", Load: 64, Decision: bandverdict.DecisionKeep, Reason: "kept"}},
				SwitchBands: []bandverdict.SwitchBand{{UpTo: 16, Engine: "setup-pair"}, {Engine: "setup-current"}}}}
		_ = json.NewEncoder(w).Encode(study)
	}))
	defer server.Close()
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "test-secret")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	output, err := captureStdout(t, func() error {
		return optimizeCommand(context.Background(), []string{"bands", "run-16", "run-64"})
	})
	if err != nil || asked != "/api/v1/optimization/band-study?campaigns=run-16%2Crun-64" {
		t.Fatalf("asked=%s err=%v", asked, err)
	}
	for _, want := range []string{"Band study of Qwen/Qwen3-8B, 2 runs", "throughput/16: winner", "up to 16 requests at once: setup-pair", "above requests at once: setup-current", "setup-pair  FP8 weights and an FP8 KV cache"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output lacks %q:\n%s", want, output)
		}
	}
	if err = optimizeCommand(context.Background(), []string{"bands", "run-16"}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("one run: %v", err)
	}
}
