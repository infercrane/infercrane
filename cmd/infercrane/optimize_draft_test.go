package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/speculator"
)

const (
	draftCLIModelRevision = "70d244cc86ccca08cf5af4e1e306ecf908b1ad5e"
	draftCLICommit        = "0123456789abcdef0123456789abcdef01234567"
)

func writeFile(t *testing.T, dir, name string, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func draftCLIReport() speculator.Report {
	files := []speculator.File{{Path: "config.json", Size: 900, SHA256: strings.Repeat("1", 64)}, {Path: "model.safetensors", Size: 900_000_000, SHA256: strings.Repeat("2", 64)}}
	return speculator.Report{
		Schema: speculator.ReportSchema, Name: "oasst1-chat", BaseModel: "Qwen/Qwen3-1.7B", BaseRevision: draftCLIModelRevision, Method: speculator.MethodEAGLE3, DraftTokens: 3,
		Training: speculator.Training{Framework: "speculators", FrameworkVersion: "0.9.0", DataSource: "public_dataset", DatasetFingerprint: "sha256:" + strings.Repeat("a", 64),
			Examples: 4000, HeldOutExamples: 128, TrainingTokens: 1_600_000, HeldOutAcceptance: []float64{0.75, 0.6, 0.5}},
		Files: files, ArtifactDigest: speculator.ManifestDigest(files), WallSeconds: 1200,
	}
}

func TestOptimizeDraftRunsStandalone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := optimizeDraftCommand(ctx, args, &out); err != nil {
			t.Fatalf("optimize draft %v: %v", args, err)
		}
		return out.String()
	}

	job := writeFile(t, dir, "job.json", map[string]any{
		"name": "oasst1-chat", "base_model": "Qwen/Qwen3-1.7B", "base_revision": draftCLIModelRevision, "data_source": "prompt_set", "data_path": "chat.jsonl",
		"image": "registry.example.com/trainer@sha256:" + strings.Repeat("c", 64), "accelerator": "L4", "namespace": "inference", "data_claim": "prompts", "output_claim": "drafts",
	})
	var manifest map[string]any
	if err := json.Unmarshal([]byte(run("job", "--file", job)), &manifest); err != nil || manifest["kind"] != "Job" {
		t.Fatalf("job manifest = %v, %v", manifest, err)
	}

	report := draftCLIReport()
	report.ProducedAt = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	reportPath := writeFile(t, dir, "report.json", report)
	recordJSON := run("record", "--report", reportPath, "--repository", "acme/qwen3-1.7b-chat-draft", "--revision", draftCLICommit, "--engine", "vllm", "--engine-version", "0.34.0", "--accelerator", "L4")
	var draft speculator.Draft
	if err := json.Unmarshal([]byte(recordJSON), &draft); err != nil || draft.ArtifactDigest != report.ArtifactDigest || !draft.Matches("vllm", "0.34.0", "L4") {
		t.Fatalf("record = %s, %v", recordJSON, err)
	}
	recordPath := filepath.Join(dir, "draft.json")
	if err := os.WriteFile(recordPath, []byte(recordJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	flags := run("args", "--record", recordPath)
	if !strings.HasPrefix(flags, "--speculative-config '{") || !strings.Contains(flags, `"revision":"`+draftCLICommit+`"`) {
		t.Fatalf("args = %q", flags)
	}

	setup := writeFile(t, dir, "setup.json", map[string]any{"model": "Qwen/Qwen3-1.7B", "model_revision": draftCLIModelRevision, "engine": "vllm", "engine_version": "0.34.0", "accelerator": "L4", "gpu_count": 1, "args": []string{"--max-model-len", "2048"}})
	var candidates struct {
		Candidates []map[string]any `json:"candidates"`
		Skipped    []map[string]any `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(run("candidates", "--record", recordPath, "--setup", setup)), &candidates); err != nil || len(candidates.Candidates) != 1 {
		t.Fatalf("candidates = %+v, %v", candidates, err)
	}
	other := writeFile(t, dir, "h100.json", map[string]any{"model": "Qwen/Qwen3-1.7B", "model_revision": draftCLIModelRevision, "engine": "vllm", "engine_version": "0.34.0", "accelerator": "H100", "gpu_count": 1, "args": []string{}})
	if err := json.Unmarshal([]byte(run("candidates", "--record", recordPath, "--setup", other)), &candidates); err != nil || len(candidates.Candidates) != 0 || len(candidates.Skipped) != 1 {
		t.Fatalf("a draft pinned to L4 was proposed on H100: %+v", candidates)
	}
}

func TestOptimizeDraftRefusesBadInput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()
	unknown := writeFile(t, dir, "job.json", map[string]any{"name": "x", "shell": "rm -rf /"})
	for name, args := range map[string][]string{
		"no action":     nil,
		"unknown":       {"publish"},
		"unknown field": {"job", "--file", unknown},
		"record flags":  {"record", "--report", "missing.json"},
		"stray arg":     {"args", "--record", "x.json", "extra"},
	} {
		if err := optimizeDraftCommand(ctx, args, &bytes.Buffer{}); err == nil {
			t.Fatalf("%s: accepted %v", name, args)
		}
	}
}
