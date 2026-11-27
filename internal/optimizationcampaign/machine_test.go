package optimizationcampaign

import (
	"encoding/json"
	"fmt"
	"github.com/infercrane/infercrane/internal/lab"
	"math"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// The crossover run of the map on one H100 (23 November): short chat at 48, 64
// and 96 at once, three servers per setup. Each row is a server's GPU busy
// share and output tokens per second in one cell. In every cell the slowest
// server of a setup had the least busy GPU.
var crossoverServers = []struct {
	setup, load string
	server      int
	busy        float64
	output      float64
}{
	{"plain", "48", 1, 84.3, 2161}, {"plain", "48", 2, 86.4, 2199}, {"plain", "48", 3, 95.7, 2470},
	{"one-draft", "48", 1, 90.6, 3140}, {"one-draft", "48", 2, 82.2, 2912}, {"one-draft", "48", 3, 96.1, 3316},
	{"three-drafts", "48", 1, 90.7, 3083}, {"three-drafts", "48", 2, 94.0, 3310}, {"three-drafts", "48", 3, 98.3, 3391},
	{"plain", "64", 1, 89.8, 2806}, {"plain", "64", 2, 85.6, 2594}, {"plain", "64", 3, 95.7, 2979},
	{"one-draft", "64", 1, 95.7, 3727}, {"one-draft", "64", 2, 88.6, 3544}, {"one-draft", "64", 3, 97.1, 3821},
	{"three-drafts", "64", 1, 91.7, 3425}, {"three-drafts", "64", 2, 95.2, 3565}, {"three-drafts", "64", 3, 98.1, 3613},
	{"plain", "96", 1, 95.6, 3585}, {"plain", "96", 2, 88.0, 3307}, {"plain", "96", 3, 97.3, 3590},
	{"one-draft", "96", 1, 95.8, 4407}, {"one-draft", "96", 2, 89.8, 4060}, {"one-draft", "96", 3, 98.0, 4425},
	{"three-drafts", "96", 1, 90.8, 3358}, {"three-drafts", "96", 2, 97.0, 3574}, {"three-drafts", "96", 3, 98.8, 3590},
}

func TestTheSlowServerOfEachSetupIsFlagged(t *testing.T) {
	var servers []ServerBusy
	slowest := map[string]string{}
	slowestOutput := map[string]float64{}
	for _, row := range crossoverServers {
		name := fmt.Sprintf("server %d", row.server)
		servers = append(servers, ServerBusy{Setup: row.setup, Load: row.load, Server: name, GPUBusyPercent: row.busy})
		key := row.setup + "/" + row.load
		if output, seen := slowestOutput[key]; !seen || row.output < output {
			slowest[key], slowestOutput[key] = name, row.output
		}
	}
	flagged := map[string]bool{}
	for _, server := range LessBusyServers(servers) {
		flagged[server.Setup+"/"+server.Load+"/"+server.Server] = true
	}
	for key, server := range slowest {
		if !flagged[key+"/"+server] {
			t.Errorf("%s: the slowest server, %s, was not flagged (flagged: %v)", key, server, flagged)
		}
	}
	// A server within a few points of the busiest is not flagged.
	if flagged["three-drafts/48/server 2"] || flagged["one-draft/64/server 1"] {
		t.Fatalf("a server near the busiest was flagged: %v", flagged)
	}
	if got := LessBusyServers([]ServerBusy{{Setup: "a", Load: "1", Server: "only", GPUBusyPercent: 50}}); len(got) != 0 {
		t.Fatalf("a lone server was flagged: %v", got)
	}
}

// A copy of the control that lands 8% from it is a machine difference the
// fixed 0.75% allowance does not cover. A 6% gain judged against 0.75% is a
// gain; against 8% it is not.
func TestACopyThatDiffersBy8PercentWidensTheInterval(t *testing.T) {
	gain := MetricComparison{HigherIsBetter: true, Delta: 0.06, NoiseFloor: 0.03, Decision: DecisionImproved, Method: ComparisonPaired, Pairs: 4, Comparisons: 2, Confidence: 0.975, MachineDifference: machineDifference}
	widened := WidenForMachines(gain, -0.08)
	if widened.Decision != DecisionNoMeasurableDifference || widened.NoiseFloor < 0.15 || widened.MachineDifference != 0.08 {
		t.Fatalf("widened=%+v", widened)
	}
	// A difference inside the allowance changes nothing.
	if same := WidenForMachines(gain, 0.005); same != gain {
		t.Fatalf("narrow copy changed the comparison: %+v", same)
	}
	// A large gain survives a wide machine allowance.
	large := gain
	large.Delta = 0.35
	if widened := WidenForMachines(large, 0.08); widened.Decision != DecisionImproved {
		t.Fatalf("large gain lost: %+v", widened)
	}
}

func TestACopyDiffersOnlyByItsSeed(t *testing.T) {
	draft := func(args ...string) optimizer.DeploymentDraft {
		var d optimizer.DeploymentDraft
		d.Model.ID, d.Runtime.Engine, d.Runtime.Version, d.Resources.GPU, d.Runtime.Args = "Qwen/Qwen3.8-27B-FP8", "vllm", "0.34.0", "H100", args
		return d
	}
	control := draft("--max-num-seqs", "512")
	if !copyOfControl(control, draft("--max-num-seqs", "512", "--seed", "1")) || !copyOfControl(control, draft("--seed=1", "--max-num-seqs", "512")) {
		t.Fatal("a seed-only copy was not recognised")
	}
	if copyOfControl(control, draft("--max-num-seqs", "512")) || copyOfControl(control, draft("--max-num-seqs", "256", "--seed", "1")) {
		t.Fatal("the control itself or a changed setup counted as a copy")
	}
}

func TestMachineWarningsNameALessBusyServer(t *testing.T) {
	busy := func(percent float64) *benchmark.ServerMetrics {
		return &benchmark.ServerMetrics{Collected: true, Metrics: map[string]benchmark.ServerMetricSummary{gpuBusyMetric: {Type: "gauge", Stats: map[string]float64{"avg": percent}}}}
	}
	measured := &MeasuredEvidence{Runs: []RunSummary{{Arm: "control", ServerMetrics: busy(97)}, {Arm: "candidate", ServerMetrics: busy(96)}, {Arm: "control", ServerMetrics: busy(97)}},
		Confirmation: &ConfirmationEvidence{Runs: []RunSummary{{Arm: "control", ServerMetrics: busy(85)}, {Arm: "candidate", ServerMetrics: busy(97)}, {Arm: "control", ServerMetrics: busy(86)}}}}
	warnings := MachineWarnings(measured)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "window on fresh servers") || !strings.Contains(warnings[0], "explain part of a gain") {
		t.Fatalf("warnings=%v", warnings)
	}
	// Without the gauge nothing is known, and nothing is said.
	if got := MachineWarnings(&MeasuredEvidence{Runs: []RunSummary{{Arm: "control"}, {Arm: "candidate"}}}); len(got) != 0 {
		t.Fatalf("warned without readings: %v", got)
	}
}

func TestTheGateWidensEveryOtherCandidateByTheCopysDifference(t *testing.T) {
	spec := func(args ...string) string {
		var d optimizer.DeploymentDraft
		d.Name, d.Model.ID, d.Runtime.Engine, d.Runtime.Version, d.Resources.GPU, d.Resources.GPUCount, d.Runtime.Args = "chat", "Qwen/Qwen3.8-27B-FP8", "vllm", "0.34.0", "H100", 1, args
		encoded, _ := json.Marshal(d)
		return string(encoded)
	}
	control := domain.OptimizationCandidateRun{ID: "control", DeploymentSpecJSON: spec("--max-num-seqs", "512")}
	campaign := domain.OptimizationCampaign{Candidates: []domain.OptimizationCandidateRun{control,
		{ID: "copy", DeploymentSpecJSON: spec("--max-num-seqs", "512", "--seed", "1")},
		{ID: "one-draft", DeploymentSpecJSON: spec("--max-num-seqs", "512", "--speculative-config", `{"method":"mtp","num_speculative_tokens":1}`)}}}
	comparison := func(delta float64, decision string) *MetricComparison {
		return &MetricComparison{HigherIsBetter: true, Delta: delta, NoiseFloor: 0.03, Decision: decision, Method: ComparisonPaired, Pairs: 4, Comparisons: 2, Confidence: 0.975, MachineDifference: machineDifference}
	}
	assessments := map[string]ControlAssessment{
		"copy":      {Basis: basisInterleaved, Decision: DecisionImproved, Comparison: comparison(0.08, DecisionImproved)},
		"one-draft": {Basis: basisInterleaved, Decision: DecisionImproved, Comparison: comparison(0.06, DecisionImproved)},
	}
	widenForMachines(campaign, control, true, assessments, 0, "")
	got := assessments["one-draft"]
	if got.Decision != DecisionNoMeasurableDifference || !strings.Contains(got.Reason, "a copy of the current setup measured 8.0% from it") {
		t.Fatalf("one-draft=%+v", got)
	}
	if assessments["copy"].Comparison.MachineDifference != machineDifference {
		t.Fatalf("the copy widened itself: %+v", assessments["copy"])
	}
}

// Without GPU telemetry, a server is named from the measured runs alone: the
// time per output token at about the same number of running requests.
func TestServerSpeedWarningsNameASlowServerAndASlowRun(t *testing.T) {
	run := func(arm string, tpot, running float64) RunSummary {
		return RunSummary{Arm: arm, TPOTP95MS: &tpot, ServerMetrics: &benchmark.ServerMetrics{Collected: true, Metrics: map[string]benchmark.ServerMetricSummary{runningMetric: {Type: "gauge", Stats: map[string]float64{"avg": running}}}}}
	}
	window := func(control []float64, controlRunning []float64, candidate, candidateRunning float64) []RunSummary {
		var runs []RunSummary
		for index, tpot := range control {
			runs = append(runs, run("control", tpot, controlRunning[index]))
			if index < len(control)-1 {
				runs = append(runs, run("candidate", candidate, candidateRunning))
			}
		}
		return runs
	}
	measured := &MeasuredEvidence{
		Runs: window([]float64{20, 20.2, 20.1, 20, 20.3}, []float64{50, 50, 50, 50, 50}, 28, 90),
		// The candidate's fresh server is slower per token at the same load;
		// the control's fresh server is slow in its first run only.
		Confirmation: &ConfirmationEvidence{Runs: window([]float64{34, 20.1, 20, 20.2, 20}, []float64{40, 50, 50, 50, 50}, 33, 91)},
	}
	warnings := ServerSpeedWarnings("Two draft tokens", measured)
	if len(warnings) != 2 {
		t.Fatalf("warnings = %q", warnings)
	}
	if !strings.Contains(warnings[0], "“Two draft tokens” took 18% longer per output token on its fresh server") || !strings.Contains(warnings[0], "0.75%") {
		t.Fatalf("the candidate's slow server: %q", warnings[0])
	}
	if !strings.Contains(warnings[1], "the current setup on its fresh server took 70% longer per output token in its run 1 of 5") || !strings.Contains(warnings[1], "40 requests running against 50") {
		t.Fatalf("the control's slow run: %q", warnings[1])
	}

	// More requests running make every step slower: no machine is named.
	busier := &MeasuredEvidence{Runs: window([]float64{20, 20, 20}, []float64{50, 50, 50}, 28, 60), Confirmation: &ConfirmationEvidence{Runs: window([]float64{20, 20, 20}, []float64{50, 50, 50}, 33, 90)}}
	if got := ServerSpeedWarnings("x", busier); len(got) != 0 {
		t.Fatalf("named a server whose load differed: %q", got)
	}
	// Without the engine's running gauge nothing is compared across servers.
	tpot := func(value float64) *float64 { return &value }
	bare := &MeasuredEvidence{Runs: []RunSummary{{Arm: "candidate", TPOTP95MS: tpot(28)}, {Arm: "candidate", TPOTP95MS: tpot(28)}, {Arm: "candidate", TPOTP95MS: tpot(28)}},
		Confirmation: &ConfirmationEvidence{Runs: []RunSummary{{Arm: "candidate", TPOTP95MS: tpot(40)}, {Arm: "candidate", TPOTP95MS: tpot(40)}, {Arm: "candidate", TPOTP95MS: tpot(40)}}}}
	if got := ServerSpeedWarnings("x", bare); len(got) != 0 {
		t.Fatalf("compared servers without their load: %q", got)
	}
}

// A setup measured on two pairs of servers puts a number on how far machines
// differ, and that number is the allowance in every decision: two windows that
// disagree widely keep the control; two that agree still win.
func TestTheMachineAllowanceComesFromASetupMeasuredTwice(t *testing.T) {
	first := MetricComparison{ControlMean: 220, CandidateMean: 140, Delta: -80.0 / 220, NoiseFloor: 0.04, Decision: DecisionImproved}
	for name, test := range map[string]struct {
		second   MetricComparison
		decision string
	}{
		"windows disagree": {second: MetricComparison{ControlMean: 222, CandidateMean: 200, Delta: -0.10, NoiseFloor: 0.05, Decision: DecisionImproved, Method: ComparisonPaired, Confidence: 0.95, MachineDifference: machineDifference}, decision: RankSupersede},
		"windows agree":    {second: MetricComparison{ControlMean: 222, CandidateMean: 151, Delta: -0.32, NoiseFloor: 0.05, Decision: DecisionImproved, Method: ComparisonPaired, Confidence: 0.95, MachineDifference: machineDifference}, decision: RankSelect},
	} {
		t.Run(name, func(t *testing.T) {
			campaign, benchmarks := rankingFixture(t, "interactive", nil)
			control := ControlEvidence{Role: ControlRoleBracketed, CandidateID: "candidate-a", Runs: 5}
			benchmarks[1].WorkloadJSON = measuredWorkload(t, `{"profile":"interactive","request_count":100,"random_seed":17}`, MeasuredEvidence{
				Method: MeasurementMethod, Control: control, Comparison: map[string]MetricComparison{"ttft_p95_ms": first},
				Confirmation: &ConfirmationEvidence{Control: control, Comparison: map[string]MetricComparison{"ttft_p95_ms": test.second}, FreshCandidate: true, FreshControl: true},
			}, nil)
			result, err := RankMeasuredCampaign(campaign, benchmarks)
			if err != nil {
				t.Fatal(err)
			}
			if result.Decisions["candidate-b"] != test.decision || !strings.Contains(result.Reasons["candidate-b"], "on fresh servers than on its first ones") {
				t.Fatalf("decisions=%v reasons=%v", result.Decisions, result.Reasons)
			}
		})
	}
}

func TestMachineSwingReadsOnlyValidSecondWindows(t *testing.T) {
	invalid := ControlEvidence{Role: ControlRoleInvalid, CandidateID: "control"}
	bracketed := ControlEvidence{Role: ControlRoleBracketed, CandidateID: "control"}
	workload := func(confirmation *ConfirmationEvidence) string {
		encoded, _ := json.Marshal(map[string]any{"measured": MeasuredEvidence{Method: MeasurementMethod, Control: bracketed, Comparison: map[string]MetricComparison{"output_token_throughput": {Delta: 0.20}}, Confirmation: confirmation}})
		return string(encoded)
	}
	rows := []lab.Candidate{{EvidenceID: "e1"}, {EvidenceID: "e2"}}
	ids := map[string]string{"e1": "valid", "e2": "invalid"}
	benchmarks := map[string]domain.BenchmarkResult{
		"e1": {WorkloadJSON: workload(&ConfirmationEvidence{Control: bracketed, Comparison: map[string]MetricComparison{"output_token_throughput": {Delta: 0.04}}})},
		"e2": {WorkloadJSON: workload(&ConfirmationEvidence{Control: invalid})},
	}
	swing, from := machineSwing(rows, ids, benchmarks, "control", "output_token_throughput")
	want := math.Expm1(math.Abs(math.Log1p(0.20)-math.Log1p(0.04)) / math.Sqrt2)
	if from != "valid" || math.Abs(swing-want) > 1e-12 {
		t.Fatalf("swing=%g from=%s, want %g from valid", swing, from, want)
	}
}
