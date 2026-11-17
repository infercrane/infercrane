package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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
	status := watchStatus(watchCandidate{State: "rejected", FailureCode: "measured_no_measurable_difference"}, watchMeasurement{}, false)
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
	for _, want := range []string{"kept your current setup", "kept · nothing to activate", "no real difference from your current setup", "Keep your current setup", "Nothing to activate: no new endpoint is published.", "infercrane optimize cancel campaign-1 removes the measurement servers now", "results appear as each setup finishes measuring"} {
		if !strings.Contains(text, want) {
			t.Fatalf("watch view lacks %q:\n%s", want, text)
		}
	}
	// A kept control is not a winner and has nothing to activate.
	for _, wrong := range []string{"winner, ready to activate", "optimize activate", "optimized endpoint"} {
		if strings.Contains(text, wrong) {
			t.Fatalf("watch view says %q for a kept control:\n%s", wrong, text)
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

	// A control activated anyway is live, but never "measured better".
	control.State, candidate.State = "promoted", "cleaned"
	snapshot.campaign.Candidates, snapshot.campaign.State = []watchCandidate{control, candidate}, "promoted"
	if text := renderOptimizeWatch(snapshot, newUIStyles(true), 100, false); !strings.Contains(text, "“custom image, default settings” is live.") || strings.Contains(text, "measured better") {
		t.Fatalf("an activated control was described as a measured win:\n%s", text)
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

func TestRenderOptimizeWatchShowsTheAnswerCheck(t *testing.T) {
	control := watchCandidate{ID: "control", Rank: 1, State: "ranked", DeploymentSpec: watchSpec("vllm"), Quality: &watchQuality{State: "passed", Source: "infercrane", Role: "reference", Reason: "This setup's answers to the 24 prompts are the reference the other setups are compared with."}}
	waiting := watchCandidate{ID: "fast", Rank: 2, State: "validating", DeploymentSpec: watchSpec("vllm", "--max-num-seqs", "256"), Quality: &watchQuality{State: "waiting_for_evidence", Reason: "Waiting for your signed evaluation of this setup.", Command: "infercrane evaluation ingest qwen-opt-1 rev-1 --result ./evaluator-result.json --key ./quality-evidence.key --file ./quality-evidence.json --attach"}}
	blocked := watchCandidate{ID: "q4", Rank: 3, State: "rejected", FailureCode: "quality_gate_rejected", DeploymentSpec: watchSpec("vllm", "--quantization", "awq"), Quality: &watchQuality{State: "blocked", Source: "infercrane", Reason: "Answers matched the reference 80% across 24 prompts. More than 10% different blocks a setup."}}
	pending := watchCandidate{ID: "later", Rank: 4, State: "measuring", DeploymentSpec: watchSpec("sglang"), Quality: &watchQuality{State: "pending", Reason: "Runs after this setup is measured."}}
	snapshot := watchSnapshot{campaign: watchCampaign{ID: "campaign-1", ModelIdentity: "Qwen/Qwen3-0.6B", State: "running", Candidates: []watchCandidate{control, waiting, blocked, pending}}, measurements: map[string]watchMeasurement{}}
	text := renderOptimizeWatch(snapshot, newUIStyles(true), 120, false)
	for _, want := range []string{
		"waiting for your signed evaluation",
		"answers changed too much",
		"Answers",
		"are the reference the other setups are compared with. (InferCrane check)",
		"run: infercrane evaluation ingest qwen-opt-1 rev-1",
		"More than 10% different blocks a setup. (InferCrane check)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("watch view lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Runs after this setup is measured") {
		t.Fatalf("a pending answer check was listed:\n%s", text)
	}
	if status := watchStatus(watchCandidate{State: "validating", Quality: &watchQuality{State: "checking"}}, watchMeasurement{}, false); status != "checking answers against your current setup" {
		t.Fatalf("checking status = %q", status)
	}
}

// The 2026-11-17 RTX 4090 run as the watch saw it: speculation was ranked in
// a window whose control drifted and kept the old "no real difference" code.
func TestRenderOptimizeWatchNamesTheMetricASetupLostOn(t *testing.T) {
	run := func(arm string, tps, ttft, latency float64) map[string]any {
		return map[string]any{"arm": arm, "requests": 121, "failed": 0, "duration_seconds": 60, "output_token_throughput": tps, "ttft_p95_ms": ttft, "latency_p95_ms": latency}
	}
	measured := map[string]any{"method": "interleaved", "control": map[string]any{"role": "invalid", "candidate_id": "control", "runs": 4, "max_adjacent_output_throughput_drift": 0.069, "drift_limit": 0.05, "reason": "control output throughput drifted 6.9% between adjacent runs, above the 5% limit"},
		"runs": []any{run("control", 250, 310, 740), run("candidate", 186, 270, 1150), run("control", 240, 320, 750), run("candidate", 190, 280, 1160), run("control", 256, 312, 745), run("candidate", 188, 275, 1149), run("control", 238, 318, 742)}}
	workload, _ := json.Marshal(map[string]any{"measured": measured})
	row, _ := json.Marshal(map[string]any{"id": "bench-ngram", "output_token_throughput": 188.0, "ttft_p95_ms": 275.0, "latency_p95_ms": 1153.0, "workload": string(workload)})
	measurement, ok := parseWatchBenchmark(row, "bench-ngram")
	if !ok || len(measurement.Regressions) != 2 {
		t.Fatalf("measurement = %+v", measurement)
	}
	ngram := watchCandidate{ID: "ngram", Rank: 2, State: "rejected", FailureCode: "measured_no_measurable_difference", DeploymentSpec: watchSpec("vllm", "--speculative-config", `{"method":"ngram"}`)}
	control := watchCandidate{ID: "control", Rank: 1, State: "qualified", DeploymentSpec: watchSpec("vllm")}
	expires := time.Date(2026, 11, 17, 4, 10, 0, 0, time.UTC)
	snapshot := watchSnapshot{campaign: watchCampaign{ID: "campaign-1", ModelIdentity: "Qwen/Qwen3-0.6B", Intent: "new_endpoint", State: "qualified", ApprovalExpiresAt: &expires, Candidates: []watchCandidate{control, ngram}}, measurements: map[string]watchMeasurement{"ngram": measurement}}
	text := renderOptimizeWatch(snapshot, newUIStyles(true), 120, false)
	for _, want := range []string{
		"rejected · throughput got worse by 24% (noise ±7.3%), latency got worse by 55% (noise ±6.9%)",
		"otherwise they are removed when the approval ends at 2026-11-17 04:10 UTC",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("watch view lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "no real difference") {
		t.Fatalf("a setup measured worse was called no real difference:\n%s", text)
	}
}

func TestRenderOptimizeWatchDecisionFollowsEvolutionIntent(t *testing.T) {
	control := watchCandidate{ID: "control", Rank: 1, State: "guard_passed", DeploymentSpec: watchSpec("vllm")}
	candidate := watchCandidate{ID: "candidate", Rank: 2, State: "rejected", FailureCode: "measured_candidate_regressed", DeploymentSpec: watchSpec("vllm", "--kv-cache-dtype", "fp8")}
	snapshot := watchSnapshot{campaign: watchCampaign{ID: "campaign-2", ModelIdentity: "Qwen/Qwen3-0.6B", Intent: "evolve_endpoint", TargetDeployment: "qwen-chat", State: "guard_passed", Candidates: []watchCandidate{control, candidate}}, measurements: map[string]watchMeasurement{}}
	text := renderOptimizeWatch(snapshot, newUIStyles(true), 100, false)
	for _, want := range []string{"kept · nothing to activate", "worse than your current setup", "Nothing to activate: qwen-chat keeps serving its current revision.", "infercrane optimize cancel campaign-2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("watch view lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "optimize activate") {
		t.Fatalf("a kept evolution control was offered for activation:\n%s", text)
	}
	// An evolution winner waits after the safety check, not qualification.
	control.State, candidate.State, candidate.FailureCode = "rejected", "guard_passed", ""
	snapshot.campaign.Candidates = []watchCandidate{control, candidate}
	if text := renderOptimizeWatch(snapshot, newUIStyles(true), 100, false); !strings.Contains(text, "optimize activate campaign-2 --candidate candidate") {
		t.Fatalf("an evolution winner was not offered for activation:\n%s", text)
	}
	candidate.State = "promoted"
	snapshot.campaign.Candidates, snapshot.campaign.State = []watchCandidate{control, candidate}, "promoted"
	if text := renderOptimizeWatch(snapshot, newUIStyles(true), 100, false); !strings.Contains(text, "is live") || strings.Contains(text, "optimize activate") {
		t.Fatalf("an activated winner was offered for activation again:\n%s", text)
	}
}
