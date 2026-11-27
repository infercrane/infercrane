package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/technique"
)

func run(arm, setup string, throughput float64) map[string]any {
	return map[string]any{"arm": arm, "setup": setup, "requests": 8, "failed": 0, "duration_seconds": 10.0, "output_token_throughput": throughput, "tpot_p95_ms": 1000 / throughput}
}

// window interleaves a control with two candidates at one load: control,
// a, control, b, control, ... with the candidates at the given multiples of
// the control's speed.
func window(control string, candidates map[string]float64, names []string) []any {
	runs := []any{run("control", control, 100)}
	for range 4 {
		for _, name := range names {
			runs = append(runs, run("candidate", name, 100*candidates[name]), run("control", control, 100))
		}
	}
	return runs
}

func text(values ...string) []any {
	out := make([]any, len(values))
	for index, value := range values {
		if value == "<failed>" {
			out[index] = nil
			continue
		}
		out[index] = value
	}
	return out
}

func writeRecord(t *testing.T, windows []any) string {
	t.Helper()
	record := map[string]any{"schema": recordSchema, "plan": "l4-qwen3-1.7b", "rules": "rules.md", "windows": windows}
	dir := t.TempDir()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "measure.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestJudgeGivesTheCampaignVerdictPerWindow(t *testing.T) {
	t.Parallel()
	modelFree := map[string]any{
		"control": "none", "candidates": []string{"ngram", "suffix"},
		"loads":        []any{map[string]any{"concurrency": 1, "runs": window("none", map[string]float64{"ngram": 1.0, "suffix": 1.12}, []string{"ngram", "suffix"})}},
		"same_answers": map[string]any{"prompts": 2, "answers": map[string]any{"none": text("a", "b"), "none_again": text("a", "b"), "ngram": text("a", "b"), "suffix": text("a", "x")}},
	}
	drafts := map[string]any{
		"control": "none", "candidates": []string{"published", "trained"},
		"loads":               []any{map[string]any{"concurrency": 1, "runs": window("none", map[string]float64{"published": 1.5, "trained": 1.7}, []string{"published", "trained"})}},
		"same_answers":        map[string]any{"prompts": 2, "answers": map[string]any{"none": text("a", "b"), "none_again": text("a", "x"), "published": text("a", "b"), "trained": text("a", "<failed>")}},
		"held_out_acceptance": map[string]any{"trained": map[string]any{"drafts": 100, "mean_tokens_per_step": 2.6, "per_position": []float64{0.8, 0.6, 0.5}}},
	}
	out, err := judge(writeRecord(t, []any{modelFree, drafts}))
	if err != nil {
		t.Fatal(err)
	}
	bar := out.PassBar
	if !strings.HasPrefix(bar["suffix_vs_none_at_1"], "faster (+12.0%") || !strings.HasPrefix(bar["ngram_vs_none_at_1"], "no measurable difference") {
		t.Fatalf("model-free window: %v", bar)
	}
	if !strings.HasPrefix(bar["trained_vs_none_at_1"], "faster (+70.0%") || !strings.HasPrefix(bar["published_vs_none_at_1"], "faster (+50.0%") {
		t.Fatalf("draft window: %v", bar)
	}
	if !strings.HasPrefix(bar["trained_beats_published_at_1"], "yes (+70.0% against +50.0%") {
		t.Fatalf("trained against published = %q", bar["trained_beats_published_at_1"])
	}
	// The control's second pass matched 1 of 2; the trained draft 1 of 1 compared.
	if !strings.HasPrefix(bar["headline_trained_vs_none_at_1"], "pass") {
		t.Fatalf("headline = %q", bar["headline_trained_vs_none_at_1"])
	}
	if !strings.HasPrefix(bar["training_report"], "invalid") {
		t.Fatalf("a missing report must not pass: %q", bar["training_report"])
	}
	if out.Windows[1].HeldOutAcceptance["trained"].MeanTokensPerStep != 2.6 {
		t.Fatalf("held-out acceptance lost: %+v", out.Windows[1].HeldOutAcceptance)
	}
}

func TestJudgeHoldsTheHeadlineToAnswerQuality(t *testing.T) {
	t.Parallel()
	items, answers := []any{}, map[string][]any{"none": {}, "trained": {}}
	for index := range 200 {
		items = append(items, map[string]any{"id": "gsm8k:test:" + string(rune('a'+index%26)), "kind": "number", "gold": "4"})
		answers["none"] = append(answers["none"], "Answer: 4")
		wrong := "Answer: 4"
		if index%4 == 0 {
			wrong = "Answer: 5"
		}
		answers["trained"] = append(answers["trained"], wrong)
	}
	drafts := map[string]any{
		"control": "none", "candidates": []string{"trained"},
		"loads":        []any{map[string]any{"concurrency": 1, "runs": window("none", map[string]float64{"trained": 1.7}, []string{"trained"})}},
		"same_answers": map[string]any{"prompts": 1, "answers": map[string]any{"none": text("a"), "none_again": text("a"), "trained": text("a")}},
		"tasks":        map[string]any{"items": items, "answers": answers},
	}
	out, err := judge(writeRecord(t, []any{drafts}))
	if err != nil {
		t.Fatal(err)
	}
	if out.PassBar["quality_trained_vs_none"] != "worse" || out.PassBar["headline_trained_vs_none_at_1"] != "not shown" {
		t.Fatalf("bar = %v", out.PassBar)
	}
}

func TestJudgeHoldsTheHeadlineToIdenticalAnswersWithoutATaskSet(t *testing.T) {
	t.Parallel()
	drafts := map[string]any{
		"control": "none", "candidates": []string{"trained"},
		"loads": []any{map[string]any{"concurrency": 1, "runs": window("none", map[string]float64{"trained": 1.7}, []string{"trained"})}},
		"same_answers": map[string]any{"prompts": 4, "answers": map[string]any{
			"none": text("a", "b", "c", "d"), "none_again": text("a", "b", "c", "d"), "trained": text("a", "x", "y", "z"),
		}},
	}
	out, err := judge(writeRecord(t, []any{drafts}))
	if err != nil {
		t.Fatal(err)
	}
	if out.PassBar["headline_trained_vs_none_at_1"] != "not shown" {
		t.Fatalf("a draft whose greedy answers changed 3 of 4 passed: %v", out.PassBar)
	}
}

func TestJudgeRefusesAnotherSchema(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "measure.json"), []byte(`{"schema":"infercrane.trained-draft-measure/v1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := judge(dir); err == nil {
		t.Fatal("another schema was judged")
	}
}

// The trained draft's contract record is shown only with the draft-acceptance
// evidence its manifest requires: the sha256 of acceptance.json, attached
// where vLLM's counters gave the draft an acceptance. A dirty harness tree
// leaves the commit unknown.
func TestContractRecordCarriesTheDraftAcceptanceEvidence(t *testing.T) {
	t.Parallel()
	drafts := map[string]any{
		"control": "none", "candidates": []string{"published", "trained"},
		"loads":        []any{map[string]any{"concurrency": 1, "requests_per_run": 8, "runs": window("none", map[string]float64{"published": 1.4, "trained": 1.6}, []string{"published", "trained"})}},
		"same_answers": map[string]any{"prompts": 2, "answers": map[string]any{"none": text("a", "b"), "none_again": text("a", "b"), "published": text("a", "b"), "trained": text("a", "b")}},
		"acceptance":   map[string]any{"trained": map[string]any{"1": map[string]any{"drafts": 900, "mean_tokens_per_step": 2.31, "per_position": []float64{0.7, 0.5, 0.4}}}},
	}
	write := func(dir, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	complete := func(dirty bool) string {
		t.Helper()
		dir := t.TempDir()
		encoded, err := json.Marshal(map[string]any{
			"schema": recordSchema, "plan": "l4-qwen3-1.7b", "rules": "rules.md", "windows": []any{drafts},
			"engine": "vllm 0.34.0", "image": "vllm/vllm-openai@sha256:" + strings.Repeat("a", 64), "gpu": "L4",
			"commit": strings.Repeat("b", 40), "dirty": dirty, "precision": map[string]any{"weights": "bf16", "kv_cache": "bf16"},
			"command": "TRAINED_DRAFT_PLAN=l4-qwen3-1.7b modal run tools/trained-draft/trained_draft.py --stage measure --run-id r --budget-usd 0.94",
		})
		if err != nil {
			t.Fatal(err)
		}
		write(dir, "measure.json", string(encoded))
		write(dir, shapeFile, `{"schema":"shape"}`)
		write(dir, acceptanceFile, `{"schema":"acceptance"}`)
		return dir
	}
	dir := complete(false)
	out, err := judge(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 {
		t.Fatalf("results = %+v", out.Results)
	}
	// One machine, one run: complete, and still not shown until a fresh
	// machine confirms it (technique.EvidenceFreshMachineConfirmation).
	shown := out.Results[0]
	if shown.Claim != "not shown" || !slices.Equal(shown.Unknown, []string{"evidence." + technique.EvidenceFreshMachineConfirmation}) || shown.Baseline.Setup != "none" || shown.Workload.Requests != 8 {
		t.Fatalf("complete record: claim %s, unknown %v, %+v", shown.Claim, shown.Unknown, shown.Result)
	}
	if len(shown.Evidence) != 1 || shown.Evidence[0].Name != "draft-acceptance" || shown.Evidence[0].Digest != fileDigest(filepath.Join(dir, acceptanceFile)) || !strings.Contains(shown.Evidence[0].Summary, "2.31 tokens") {
		t.Fatalf("evidence = %+v", shown.Evidence)
	}
	if err := shown.Validate(); err != nil {
		t.Fatalf("the contract refused the record: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, acceptanceFile)); err != nil {
		t.Fatal(err)
	}
	out, err = judge(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.Results[0].Claim != "not shown" || !strings.Contains(strings.Join(out.Results[0].Unknown, ","), "evidence.draft-acceptance") {
		t.Fatalf("without the acceptance file: claim %s, unknown %v", out.Results[0].Claim, out.Results[0].Unknown)
	}
	out, err = judge(complete(true))
	if err != nil {
		t.Fatal(err)
	}
	if out.Results[0].Claim != "not shown" || !strings.Contains(strings.Join(out.Results[0].Unknown, ","), "reproduction.commit") {
		t.Fatalf("a dirty tree: claim %s, unknown %v", out.Results[0].Claim, out.Results[0].Unknown)
	}
}

// The rung's bar reads one line per check, so a partial pass reads as
// partial: here every check passes except training ran (no report), and
// climb does not pass; a failed request fails validity alone.
func TestRungBarStatesEachCheckOnItsOwnLine(t *testing.T) {
	t.Parallel()
	drafts := func(failed int) map[string]any {
		runs := window("none", map[string]float64{"published": 1.4, "trained": 1.6}, []string{"published", "trained"})
		runs[1].(map[string]any)["failed"] = failed
		return map[string]any{
			"control": "none", "candidates": []string{"published", "trained"},
			"loads":        []any{map[string]any{"concurrency": 8, "runs": window("none", map[string]float64{"published": 1.1, "trained": 1.0}, []string{"published", "trained"})}, map[string]any{"concurrency": 1, "runs": runs}},
			"same_answers": map[string]any{"prompts": 2, "answers": map[string]any{"none": text("a", "b"), "none_again": text("a", "b"), "published": text("a", "b"), "trained": text("a", "b")}},
			"held_out_acceptance": map[string]any{
				"trained":   map[string]any{"drafts": 500, "mean_tokens_per_step": 1.95, "per_position": []float64{0.5, 0.5, 0.5}},
				"published": map[string]any{"drafts": 520, "mean_tokens_per_step": 1.70, "per_position": []float64{0.4, 0.5, 0.5}},
			},
		}
	}
	out, err := judge(writeRecord(t, []any{drafts(0)}))
	if err != nil {
		t.Fatal(err)
	}
	bar := out.PassBar
	for key, prefix := range map[string]string{
		"bar_validity": "pass", "bar_acceptance_moved": "pass (+0.250", "bar_faster": "pass: the trained draft against none at 1 at once: faster", "bar_answers_held": "pass: no sign of changed answers (identical greedy answers 2 of 2",
		"bar_training_ran": "fail (training report", "bar_climb": "not passed",
	} {
		if !strings.HasPrefix(bar[key], prefix) {
			t.Fatalf("%s = %q, want prefix %q", key, bar[key], prefix)
		}
	}
	out, err = judge(writeRecord(t, []any{drafts(1)}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.PassBar["bar_validity"], "fail (1 failed requests") || !strings.HasPrefix(out.PassBar["bar_acceptance_moved"], "pass") {
		t.Fatalf("a failed request: %v", out.PassBar)
	}
}

// A confirm window on another GPU that measured the gain again is the
// fresh-machine confirmation: the record is shown at the smaller change and
// the larger floor. The same GPU, a confirm of another measure record, or a
// fresh machine that measured no gain leave it "not shown", and the bar says
// why.
func TestAFreshMachineConfirmsTheGainOrTheRecordStaysNotShown(t *testing.T) {
	t.Parallel()
	write := func(dir, name string, value any) {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(confirmGPU string, again float64, sameMeasure bool) string {
		t.Helper()
		dir := t.TempDir()
		drafts := map[string]any{
			"control": "none", "candidates": []string{"published", "trained"},
			"loads":        []any{map[string]any{"concurrency": 1, "requests_per_run": 8, "runs": window("none", map[string]float64{"published": 1.4, "trained": 1.6}, []string{"published", "trained"})}},
			"same_answers": map[string]any{"prompts": 2, "answers": map[string]any{"none": text("a", "b"), "none_again": text("a", "b"), "published": text("a", "b"), "trained": text("a", "b")}},
			"acceptance":   map[string]any{"trained": map[string]any{"1": map[string]any{"drafts": 900, "mean_tokens_per_step": 2.31, "per_position": []float64{0.7, 0.5, 0.4}}}},
		}
		write(dir, "measure.json", map[string]any{
			"schema": recordSchema, "plan": "l4-qwen3-8b", "rules": "rules.md", "windows": []any{drafts},
			"engine": "vllm 0.34.0", "image": "vllm/vllm-openai@sha256:" + strings.Repeat("a", 64), "gpu": "L4",
			"commit": strings.Repeat("b", 40), "precision": map[string]any{"weights": "fp8", "kv_cache": "bf16"},
			"command": "TRAINED_DRAFT_PLAN=l4-qwen3-8b modal run tools/trained-draft/trained_draft.py::launch --stage measure --run-id r --budget-usd 2.5",
			"machine": map[string]any{"task_id": "ta-1", "gpu_uuid": "GPU-aaaa"},
		})
		write(dir, shapeFile, map[string]any{"schema": "shape"})
		write(dir, acceptanceFile, map[string]any{"schema": "acceptance"})
		confirms := fileDigest(filepath.Join(dir, "measure.json"))
		if !sameMeasure {
			confirms = "sha256:" + strings.Repeat("0", 64)
		}
		write(dir, confirmFile, map[string]any{
			"schema": confirmSchema, "plan": "l4-qwen3-8b", "confirms": confirms, "machine": map[string]any{"task_id": "ta-2", "gpu_uuid": confirmGPU},
			"windows": []any{map[string]any{"control": "none", "candidates": []string{"trained"},
				"loads": []any{map[string]any{"concurrency": 1, "requests_per_run": 8, "runs": window("none", map[string]float64{"trained": again}, []string{"trained"})}}}},
		})
		return dir
	}
	out, err := judge(setup("GPU-bbbb", 1.5, true))
	if err != nil {
		t.Fatal(err)
	}
	record := out.Results[0]
	names := []string{}
	for _, item := range record.Evidence {
		names = append(names, item.Name)
	}
	if record.Claim != "shown" || len(record.Unknown) != 0 || record.Baseline.Basis != "same-machine+fresh-servers" || !slices.Contains(names, technique.EvidenceFreshMachineConfirmation) {
		t.Fatalf("confirmed record: claim %s, unknown %v, basis %s, evidence %v", record.Claim, record.Unknown, record.Baseline.Basis, names)
	}
	// +60% first, +50% again: the claim counts the smaller change.
	if record.Measured.Change < 0.49 || record.Measured.Change > 0.51 || !strings.HasPrefix(out.PassBar["fresh_machine_at_1"], "confirmed") {
		t.Fatalf("change %.3f, bar %q", record.Measured.Change, out.PassBar["fresh_machine_at_1"])
	}
	for name, dir := range map[string]string{
		"the same GPU":             setup("GPU-aaaa", 1.5, true),
		"another measure record":   setup("GPU-bbbb", 1.5, false),
		"no gain on the fresh GPU": setup("GPU-bbbb", 1.0, true),
	} {
		out, err := judge(dir)
		if err != nil {
			t.Fatal(err)
		}
		if out.Results[0].Claim != "not shown" || !slices.Contains(out.Results[0].Unknown, "evidence."+technique.EvidenceFreshMachineConfirmation) || !strings.HasPrefix(out.PassBar["fresh_machine_at_1"], "not confirmed") {
			t.Fatalf("%s: claim %s, unknown %v, bar %q", name, out.Results[0].Claim, out.Results[0].Unknown, out.PassBar["fresh_machine_at_1"])
		}
	}
}

// A plan that reads answers by the held rule pools every server pass: the
// same answers on three servers each read "held within 1 point"; one pass
// whose interval reaches below -1 point reads "not shown to hold" and the
// answers line says unknown, never "no measurable difference".
func TestTaskPassesArePooledAndReadByTheHeldRule(t *testing.T) {
	t.Parallel()
	items := []any{}
	right := func(n int, wrong map[int]bool) []any {
		out := []any{}
		for index := range n {
			answer := "Answer: 4"
			if wrong[index] {
				answer = "Answer: 5"
			}
			out = append(out, answer)
		}
		return out
	}
	for index := range 500 {
		items = append(items, map[string]any{"id": fmt.Sprintf("gsm8k:test:%d", index), "kind": "number", "gold": "4"})
	}
	drafts := func(trained []any, passes []any) map[string]any {
		return map[string]any{
			"control": "none", "candidates": []string{"trained"},
			"loads":        []any{map[string]any{"concurrency": 1, "runs": window("none", map[string]float64{"trained": 1.6}, []string{"trained"})}},
			"same_answers": map[string]any{"prompts": 1, "answers": map[string]any{"none": text("a"), "none_again": text("a"), "trained": text("b")}},
			"tasks":        map[string]any{"items": items, "rule": "held_within_1_point", "answers": map[string]any{"none": right(500, nil), "trained": trained}, "passes": passes},
		}
	}
	same := map[string]any{"none": right(500, nil), "trained": right(500, nil)}
	out, err := judge(writeRecord(t, []any{drafts(right(500, nil), []any{same, same})}))
	if err != nil {
		t.Fatal(err)
	}
	held := out.Windows[0].Tasks["trained"]
	if held.Verdict != "held" || held.ServersPerSide != 3 || held.Rule != "held_within_1_point" || !strings.HasPrefix(out.PassBar["bar_answers_held"], "pass (task-score: held within 1 point") {
		t.Fatalf("pooled = %+v, bar %q", held, out.PassBar["bar_answers_held"])
	}
	// One pass: six items lost, three gained: the interval spans zero and reaches below -1 point.
	mixed := map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true}
	reference := right(500, map[int]bool{10: true, 11: true, 12: true})
	record := drafts(right(500, mixed), nil)
	record["tasks"].(map[string]any)["answers"].(map[string]any)["none"] = reference
	out, err = judge(writeRecord(t, []any{record}))
	if err != nil {
		t.Fatal(err)
	}
	if verdict := out.Windows[0].Tasks["trained"].Verdict; verdict != "not_shown_held" || !strings.HasPrefix(out.PassBar["bar_answers_held"], "unknown (task-score: not shown to hold within 1 point") || !strings.HasPrefix(out.PassBar["quality_trained_vs_none"], "unknown") {
		t.Fatalf("one mixed pass: verdict %s, bar %q, quality %q", verdict, out.PassBar["bar_answers_held"], out.PassBar["quality_trained_vs_none"])
	}
}
