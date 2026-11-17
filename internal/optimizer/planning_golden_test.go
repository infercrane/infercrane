package optimizer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const planningGoldenPath = "../optimizationevidence/testdata/golden/planning.json"

func TestPlanningGoldenSuitePasses(t *testing.T) {
	report, err := RunPlanningGolden(context.Background(), planningGoldenPath, catalogSource(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range report.Results {
		if !result.Correct {
			t.Errorf("%s: %s", result.TaskID, strings.Join(result.Problems, "; "))
		}
	}
	if !report.Passed || report.Correct != report.Tasks || report.RulesVersion != DiagnosisRulesVersion {
		t.Fatalf("planning golden suite failed: %d of %d correct", report.Correct, report.Tasks)
	}
}

func TestPlanningGoldenSuiteCoversEveryRequiredSituation(t *testing.T) {
	manifest, err := LoadPlanningGoldenManifest(planningGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, task := range manifest.Tasks {
		for _, category := range task.Categories {
			covered[category] = true
		}
	}
	for _, category := range RequiredPlanningCategories {
		if !covered[category] {
			t.Errorf("no planning golden task covers %q", category)
		}
	}
}

func TestPlanningGoldenDetectsAReorderedOrForbiddenPlan(t *testing.T) {
	manifest, err := LoadPlanningGoldenManifest(planningGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var task PlanningGoldenTask
	for _, candidate := range manifest.Tasks {
		if candidate.ID == "h100-kv-preemption-fp8-kv-first" {
			task = candidate
		}
	}
	proposal, err := catalogSource(t).Propose(context.Background(), task.Request)
	if err != nil {
		t.Fatal(err)
	}
	if result := scorePlanningTask(task, proposal); !result.Correct {
		t.Fatalf("baseline task should pass: %v", result.Problems)
	}
	swapped := proposal
	swapped.Candidates = append([]Candidate{}, proposal.Candidates...)
	swapped.Candidates[1], swapped.Candidates[2] = swapped.Candidates[2], swapped.Candidates[1]
	if result := scorePlanningTask(task, swapped); result.Correct {
		t.Fatal("a reordered plan passed the golden task")
	}
	forbidden := task
	forbidden.MustNotPropose = []string{TechniqueKVSequenceCap}
	forbidden.ExpectedOrder = []string{GoldenOrderControl, TechniqueFP8KVCache}
	if result := scorePlanningTask(forbidden, proposal); result.Correct {
		t.Fatal("a forbidden technique passed the golden task")
	}
	args := task
	args.MustNotUseArgs = []string{"--kv-cache-dtype"}
	if result := scorePlanningTask(args, proposal); result.Correct {
		t.Fatal("a forbidden argument passed the golden task")
	}
	needed := task
	needed.ExpectedNeededEvidence = []string{"engine_metrics"}
	if result := scorePlanningTask(needed, proposal); result.Correct {
		t.Fatal("a plan that asked for no evidence passed a task expecting engine metrics")
	}
}

func TestPlanningGoldenManifestRejectsMalformedTasks(t *testing.T) {
	directory := t.TempDir()
	for name, body := range map[string]string{
		"schema":     `{"schema_version":"other","tasks":[]}`,
		"empty":      `{"schema_version":"` + PlanningGoldenSchema + `","tasks":[]}`,
		"unknown":    `{"schema_version":"` + PlanningGoldenSchema + `","tasks":[],"extra":1}`,
		"no-diag":    `{"schema_version":"` + PlanningGoldenSchema + `","tasks":[{"id":"a","request":{"model_identity":"m"},"expected_state":"applied","expected_order":["control"],"categories":["kv_pressure"],"why":"w"}]}`,
		"technique":  `{"schema_version":"` + PlanningGoldenSchema + `","tasks":[{"id":"a","request":{"model_identity":"m","diagnosis":{}},"expected_state":"applied","expected_order":["control","made-up"],"categories":["kv_pressure"],"why":"w"}]}`,
		"category":   `{"schema_version":"` + PlanningGoldenSchema + `","tasks":[{"id":"a","request":{"model_identity":"m","diagnosis":{}},"expected_state":"applied","expected_order":["control"],"categories":["nope"],"why":"w"}]}`,
		"no-control": `{"schema_version":"` + PlanningGoldenSchema + `","tasks":[{"id":"a","request":{"model_identity":"m","diagnosis":{}},"expected_state":"applied","expected_order":["fp8-kv-cache"],"categories":["kv_pressure"],"why":"w"}]}`,
	} {
		path := filepath.Join(directory, name+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPlanningGoldenManifest(path); err == nil {
			t.Errorf("%s: malformed manifest accepted", name)
		}
	}
	body, err := os.ReadFile(planningGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !IsPlanningGoldenManifest(body) || IsPlanningGoldenManifest([]byte(`{"schema_version":"infercrane.dev/optimizer-golden-suite/v1"}`)) {
		t.Fatal("planning manifest detection is wrong")
	}
}
