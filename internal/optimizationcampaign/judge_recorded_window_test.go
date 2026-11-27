package optimizationcampaign

import (
	"strings"
	"testing"
)

func recordedRun(arm string, throughput float64) RunSummary {
	return RunSummary{Arm: arm, Requests: 48, OutputTokenThroughput: &throughput, DurationSeconds: 10}
}

// recordedWindow interleaves one candidate with the control as the
// trained-draft tool records it: control, candidate, control, ...
func recordedWindow(control float64, candidate []float64) []RunSummary {
	runs := []RunSummary{recordedRun("control", control)}
	for index, value := range candidate {
		// A small drift both arms share, which pairing cancels.
		drift := 1 + 0.005*float64(index)
		runs = append(runs, recordedRun("candidate", value*drift), recordedRun("control", control*drift))
	}
	return runs
}

func TestJudgeRecordedWindowDecidesAsTheCampaignDoes(t *testing.T) {
	t.Parallel()
	better, invalid := JudgeRecordedWindow(recordedWindow(1000, []float64{1100, 1104, 1098, 1102}), 2)
	if invalid != "" {
		t.Fatal(invalid)
	}
	throughput := better["output_token_throughput"]
	if throughput.Decision != DecisionImproved || throughput.Method != ComparisonPaired || throughput.Comparisons != 2 || throughput.Pairs != 4 {
		t.Fatalf("10%% faster candidate = %+v", throughput)
	}
	if throughput.Delta < 0.09 || throughput.Delta > 0.11 {
		t.Fatalf("delta = %v", throughput.Delta)
	}
	same, invalid := JudgeRecordedWindow(recordedWindow(1000, []float64{1001, 999, 1002, 998}), 2)
	if invalid != "" || same["output_token_throughput"].Decision != DecisionNoMeasurableDifference {
		t.Fatalf("a copy of the control was decided %+v (%s)", same["output_token_throughput"], invalid)
	}
	worse, _ := JudgeRecordedWindow(recordedWindow(1000, []float64{800, 805, 798, 802}), 2)
	if worse["output_token_throughput"].Decision != DecisionRegressed {
		t.Fatalf("slower candidate = %+v", worse["output_token_throughput"])
	}
}

func TestJudgeRecordedWindowVoidsBrokenWindows(t *testing.T) {
	t.Parallel()
	drifted := recordedWindow(1000, []float64{1100, 1100, 1100, 1100})
	jump := 1300.0
	drifted[2].OutputTokenThroughput = &jump
	if _, invalid := JudgeRecordedWindow(drifted, 1); !strings.Contains(invalid, "drifted") {
		t.Fatalf("a control jump of 30%% was not voided: %q", invalid)
	}
	failed := recordedWindow(1000, []float64{1100, 1100, 1100, 1100})
	failed[3].Failed = 1
	if _, invalid := JudgeRecordedWindow(failed, 1); !strings.Contains(invalid, "failed") {
		t.Fatalf("a failed run was not voided: %q", invalid)
	}
	if _, invalid := JudgeRecordedWindow(recordedWindow(1000, []float64{1100}), 1); !strings.Contains(invalid, "fewer than two") {
		t.Fatalf("a one-pair window was judged: %q", invalid)
	}
}
