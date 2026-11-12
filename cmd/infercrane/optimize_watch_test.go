package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func watchSpec(engine string, args ...string) json.RawMessage {
	encoded, _ := json.Marshal(map[string]any{"runtime": map[string]any{"engine": engine, "args": args}})
	return encoded
}

func TestParseWatchBenchmarkReadsComparisonAndCostWithoutEstimating(t *testing.T) {
	workload, _ := json.Marshal(map[string]any{"measured": map[string]any{"comparison": map[string]any{"output_token_throughput": map[string]any{"higher_is_better": true, "delta": 0.21, "noise_floor": 0.03, "decision": "improved"}}}})
	row, _ := json.Marshal(map[string]any{"id": "bench-1", "output_token_throughput": 100.0, "ttft_p95_ms": 250.0, "workload": string(workload), "cost_metadata": map[string]any{"available": true, "hourly": 3.6}})
	measurement, ok := parseWatchBenchmark(row, "bench-1")
	if !ok || measurement.OutputTPS == nil || *measurement.OutputTPS != 100 || measurement.LatencyP95MS != nil {
		t.Fatalf("benchmark row was not read exactly: %+v ok=%v", measurement, ok)
	}
	if measurement.CostPerMillon == nil || *measurement.CostPerMillon != 10 {
		t.Fatalf("cost per million tokens = %v, want 10 (3.6 $/h at 100 tok/s)", measurement.CostPerMillon)
	}
	if measurement.Comparison["output_token_throughput"].Decision != "improved" {
		t.Fatalf("measured comparison was not read: %+v", measurement.Comparison)
	}
	if _, ok := parseWatchBenchmark(row, "other"); ok {
		t.Fatal("a benchmark row for another candidate was accepted")
	}
	unpriced, _ := json.Marshal(map[string]any{"id": "bench-2", "output_token_throughput": 100.0, "cost_metadata": map[string]any{"available": false}})
	if measurement, _ := parseWatchBenchmark(unpriced, "bench-2"); measurement.CostPerMillon != nil {
		t.Fatal("cost was estimated without sourced price evidence")
	}
}

func TestWatchLabelAndStatusUsePlainWords(t *testing.T) {
	if got := watchLabel(watchCandidate{DeploymentSpec: watchSpec("custom-oci")}); got != "custom image, default settings" {
		t.Fatalf("label = %q", got)
	}
	if got := watchLabel(watchCandidate{DeploymentSpec: watchSpec("vllm", "--max-num-seqs", "256")}); got != "vllm, --max-num-seqs 256" {
		t.Fatalf("label = %q", got)
	}
	if got := watchLabel(watchCandidate{ID: "0123456789abcdef", DeploymentSpec: json.RawMessage(`{}`)}); !strings.HasPrefix(got, "setup ") {
		t.Fatalf("label without a spec = %q", got)
	}
	status := watchStatus(watchCandidate{State: "rejected", FailureCode: "measured_no_measurable_difference"})
	if status != "rejected · no real difference from your current setup" {
		t.Fatalf("status = %q", status)
	}
}

func TestWatchVerdictFollowsMeasuredDecision(t *testing.T) {
	cases := map[string]string{"improved": "better", "regressed": "worse", "no_measurable_difference": "same"}
	for decision, want := range cases {
		kind, text := watchVerdict(watchMeasurement{Comparison: map[string]watchComparison{"ttft_p95_ms": {Delta: -0.12, NoiseFloor: 0.04, Decision: decision}}})
		if kind != want || !strings.Contains(text, "first-token time") || !strings.Contains(text, "noise ±4.0%") {
			t.Fatalf("decision %s gave %q %q", decision, kind, text)
		}
	}
	if kind, _ := watchVerdict(watchMeasurement{}); kind != "" {
		t.Fatal("a verdict was invented without a measured comparison")
	}
}

func TestRenderOptimizeWatchStatesTheDecision(t *testing.T) {
	control := watchCandidate{ID: "control", Rank: 1, State: "qualified", DeploymentSpec: watchSpec("custom-oci")}
	candidate := watchCandidate{ID: "candidate", Rank: 2, State: "rejected", FailureCode: "measured_no_measurable_difference", DeploymentSpec: watchSpec("custom-oci", "--parallel", "8")}
	snapshot := watchSnapshot{campaign: watchCampaign{ID: "campaign-1", ModelIdentity: "Qwen/Qwen3-0.6B", State: "qualified", Candidates: []watchCandidate{candidate, control}}, measurements: map[string]watchMeasurement{}}
	text := renderOptimizeWatch(snapshot, newUIStyles(true), 100, false)
	for _, want := range []string{"winner, ready to activate", "no real difference from your current setup", "Keep your current setup", "results appear as each setup finishes measuring"} {
		if !strings.Contains(text, want) {
			t.Fatalf("watch view lacks %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "default settings") > strings.Index(text, "--parallel 8") {
		t.Fatalf("candidates were not ordered by rank:\n%s", text)
	}

	control.State, candidate.State, candidate.FailureCode = "rejected", "qualified", ""
	snapshot.campaign.Candidates = []watchCandidate{control, candidate}
	if text := renderOptimizeWatch(snapshot, newUIStyles(true), 100, false); !strings.Contains(text, "Roll out “custom image, --parallel 8”") || !strings.Contains(text, "optimize activate campaign-1 --candidate candidate") {
		t.Fatalf("a measured winner was not offered for activation:\n%s", text)
	}

	snapshot.campaign.State = "measuring"
	if text := renderOptimizeWatch(snapshot, newUIStyles(true), 100, false); strings.Contains(text, "Decision") {
		t.Fatalf("a decision was shown before the campaign finished:\n%s", text)
	}
	snapshot.campaign.State = "cancelled"
	if text := renderOptimizeWatch(snapshot, newUIStyles(true), 100, false); !strings.Contains(text, "Nothing changed in production") {
		t.Fatalf("cancelled campaign text:\n%s", text)
	}
}

func TestOptimizeWatchRequiresCampaign(t *testing.T) {
	if err := optimizeWatchCommand(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "optimize watch CAMPAIGN") {
		t.Fatalf("missing campaign error = %v", err)
	}
	if err := optimizeWatchCommand(t.Context(), []string{"--once"}); err == nil {
		t.Fatal("a flag was accepted as the campaign ID")
	}
}
