package optimizationevidence

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goldenManifest = "testdata/golden/manifest.json"

// TestOptimizerGoldenSuite is the fixed evaluation of the evaluator. It fails
// on any wrong decision or false win, and when the checked-in tasks stop
// covering a required situation.
func TestOptimizerGoldenSuite(t *testing.T) {
	report, err := RunGoldenSuite(goldenManifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range report.Results {
		if !result.Correct || result.FalseWin {
			t.Errorf("%s: expected %s %s, evaluator decided %s %s (false win: %t): %s", result.TaskID, result.ExpectedDecision, result.ExpectedCandidateID, result.Decision, result.CandidateID, result.FalseWin, result.Reason)
		}
	}
	if !report.Passed || report.FalseWins != 0 || report.CorrectDecisions != report.Tasks {
		t.Fatalf("golden suite failed: %d/%d correct, %d false wins", report.CorrectDecisions, report.Tasks, report.FalseWins)
	}
	covered := map[string]bool{}
	for _, result := range report.Results {
		for _, category := range result.Categories {
			covered[category] = true
		}
	}
	for _, category := range RequiredGoldenCategories {
		if !covered[category] {
			t.Errorf("no golden task covers %s", category)
		}
	}
	if report.WinRate == nil || *report.WinRate != 1 || report.MedianGainOverBaseline == nil || report.GPUHoursPerAnswer == nil {
		t.Fatalf("suite metrics are incomplete: %+v", report)
	}
	t.Logf("tasks %d, correct %d, win rate %.0f%% (%d/%d), false wins %d, median gain over baseline %.1f%% (%d known, %d unknown), measured GPU-hours per answer %.4f",
		report.Tasks, report.CorrectDecisions, *report.WinRate*100, report.CorrectPromotions, report.ExpectedPromotions, report.FalseWins,
		*report.MedianGainOverBaseline*100, report.PromotionsWithKnownGain, report.PromotionsWithUnknownGain, *report.GPUHoursPerAnswer)
}

// The suite must catch the evaluator it replaces: picking the best absolute
// score promotes a challenger whose lead is inside measurement noise.
func TestGoldenSuiteCatchesBaselineBlindEvaluator(t *testing.T) {
	blind := func(campaign Campaign) (Evaluation, error) {
		campaign.Selection.BaselineCandidateID = ""
		return Evaluate(campaign)
	}
	report, err := runGoldenSuite(goldenManifest, blind)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.FalseWins == 0 {
		t.Fatalf("a baseline-blind evaluator passed the suite: %+v", report)
	}
	for _, result := range report.Results {
		if result.TaskID == "h200-decode-saturation-lead-inside-noise" && (!result.FalseWin || result.CandidateID != "sglang-0520-nextn-k4-bounded-graphs") {
			t.Fatalf("inside-noise lead was not reported as a false win: %+v", result)
		}
	}
}

func TestGoldenSuiteScoresWrongExpectations(t *testing.T) {
	manifest := goldenFixture(t, []GoldenTask{
		{ID: "expects-noise-win", Evidence: "qwen38-public-decode-saturation-modal-2026-09-23T111756Z.json", BaselineCandidateID: "sglang-0520-nextn-k4-graphs-linear-flashinfer", ExpectedDecision: DecisionPromote, ExpectedCandidateID: "sglang-0520-nextn-k4-bounded-graphs", Categories: []string{GoldenInsideNoise}, Why: "wrong on purpose"},
		{ID: "expects-keep", Evidence: "qwen38-public-decode-saturation-modal-2026-09-23T044530Z.json", BaselineCandidateID: "sglang-0520-control", ExpectedDecision: DecisionKeepBaseline, Categories: []string{GoldenClearWin}, Why: "wrong on purpose"},
		{ID: "forbids-winner", Evidence: "qwen38-public-decode-saturation-modal-2026-09-23T111756Z.json", BaselineCandidateID: "sglang-0520-control", ExpectedDecision: DecisionPromote, ExpectedCandidateID: "sglang-0520-nextn-k4-graphs-linear-flashinfer", MustNotWin: []string{"sglang-0520-nextn-k4-bounded-graphs"}, Categories: []string{GoldenClearWin}, Why: "wrong on purpose"},
	})
	report, err := RunGoldenSuite(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.CorrectDecisions != 0 || report.FalseWins != 2 || report.MissedWins != 1 || report.ExpectedPromotions != 2 || report.CorrectPromotions != 0 || *report.WinRate != 0 {
		t.Fatalf("wrong expectations were not scored: %+v", report)
	}
	if report.MedianGainOverBaseline == nil || math.Abs(*report.MedianGainOverBaseline-0.4434) > 0.001 || report.PromotionsWithKnownGain != 2 {
		t.Fatalf("median gain over the two promotions is wrong: %+v", report)
	}
}

func TestGoldenGPUHoursStayUnknownWithoutLaneTime(t *testing.T) {
	campaign := qwenCampaign(t)
	hours := measuredGPUHours(campaign)
	if hours == nil || *hours <= 0 {
		t.Fatalf("measured GPU time missing: %v", hours)
	}
	campaign.Candidates[0].Lanes[0].GPUSecondsPerSuccessfulRequest = 0
	if measuredGPUHours(campaign) != nil {
		t.Fatal("a lane without GPU seconds produced a GPU-hour total")
	}
	campaign = qwenCampaign(t)
	campaign.Candidates[1].Lanes[2].SuccessfulRequests = 0
	if measuredGPUHours(campaign) != nil {
		t.Fatal("a lane without successful requests produced a GPU-hour total")
	}
}

func TestGoldenManifestRejectsAmbiguousTasks(t *testing.T) {
	valid := GoldenTask{ID: "a", Evidence: "e.json", BaselineCandidateID: "base", ExpectedDecision: DecisionKeepBaseline, Categories: []string{GoldenInsideNoise}, Why: "reason"}
	tests := map[string]func(*GoldenTask){
		"missing why":                 func(task *GoldenTask) { task.Why = " " },
		"unknown decision":            func(task *GoldenTask) { task.ExpectedDecision = "maybe" },
		"promote without candidate":   func(task *GoldenTask) { task.ExpectedDecision = DecisionPromote },
		"promote the baseline":        func(task *GoldenTask) { task.ExpectedDecision, task.ExpectedCandidateID = DecisionPromote, "base" },
		"keep with candidate":         func(task *GoldenTask) { task.ExpectedCandidateID = "other" },
		"must-not-win names baseline": func(task *GoldenTask) { task.MustNotWin = []string{"base"} },
		"no category":                 func(task *GoldenTask) { task.Categories = nil },
		"unknown category":            func(task *GoldenTask) { task.Categories = []string{"vibes"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			task := valid
			mutate(&task)
			if err := (GoldenManifest{SchemaVersion: GoldenManifestSchema, Tasks: []GoldenTask{task}}).Validate(); err == nil {
				t.Fatalf("invalid task accepted: %+v", task)
			}
		})
	}
	if err := (GoldenManifest{SchemaVersion: GoldenManifestSchema, Tasks: []GoldenTask{valid, valid}}).Validate(); err == nil {
		t.Fatal("duplicate task ids accepted")
	}
	if err := (GoldenManifest{SchemaVersion: GoldenManifestSchema, Tasks: []GoldenTask{valid}}).Validate(); err != nil {
		t.Fatalf("valid task rejected: %v", err)
	}
}

func TestGoldenTaskRejectsUnknownCandidatesAndConflictingBaseline(t *testing.T) {
	unknown := goldenFixture(t, []GoldenTask{{ID: "typo", Evidence: "qwen38-public-decode-saturation-modal-2026-09-23T044530Z.json", BaselineCandidateID: "sglang-0520-contrl", ExpectedDecision: DecisionRejectAll, Categories: []string{GoldenSLOGate}, Why: "typo"}})
	if _, err := RunGoldenSuite(unknown); err == nil || !strings.Contains(err.Error(), "not in the evidence") {
		t.Fatalf("unknown baseline accepted: %v", err)
	}
	directory := t.TempDir()
	campaign := qwenCampaign(t)
	campaign.Selection.BaselineCandidateID = "sglang-control"
	body, err := json.Marshal(campaign)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, "declared.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	conflict := writeGoldenManifest(t, directory, []GoldenTask{{ID: "conflict", Evidence: "declared.json", BaselineCandidateID: "vllm-control", ExpectedDecision: DecisionPromote, ExpectedCandidateID: "sglang-control", Categories: []string{GoldenBaselineFailsSLO}, Why: "conflict"}})
	if _, err = RunGoldenSuite(conflict); err == nil || !strings.Contains(err.Error(), "evidence declares baseline") {
		t.Fatalf("conflicting baseline accepted: %v", err)
	}
}

// goldenFixture writes a manifest next to copies of recorded evidence.
func goldenFixture(t *testing.T, tasks []GoldenTask) string {
	t.Helper()
	directory := t.TempDir()
	for _, task := range tasks {
		body, err := os.ReadFile(filepath.Join("..", "..", "docs", "testing", "evidence", task.Evidence))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(directory, task.Evidence), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return writeGoldenManifest(t, directory, tasks)
}

func writeGoldenManifest(t *testing.T, directory string, tasks []GoldenTask) string {
	t.Helper()
	body, err := json.Marshal(GoldenManifest{SchemaVersion: GoldenManifestSchema, Tasks: tasks})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "manifest.json")
	if err = os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
