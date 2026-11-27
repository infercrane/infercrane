package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResultsImportListAndRankWithALocalStore(t *testing.T) {
	store := filepath.Join(t.TempDir(), "results.jsonl")
	gym := "../../internal/resultstore/testdata/synthetic/map-hybrid.json"
	importArgs := []string{"results", "import", gym, "--store", store, "--architecture", "hybrid", "--params-b", "27", "--checkpoint", "fp8", "--recorded-at", "2001-01-02T03:00:00Z"}
	output, err := captureStdout(t, func() error { return run(context.Background(), importArgs) })
	if err != nil || !strings.Contains(output, "Added 110 of 110 measured results") || !strings.Contains(output, "0 make a claim") {
		t.Fatalf("import: %v\n%s", err, output)
	}
	output, err = captureStdout(t, func() error { return run(context.Background(), importArgs) })
	if err != nil || !strings.Contains(output, "Added 0 of 110 measured results") {
		t.Fatalf("a second import: %v\n%s", err, output)
	}
	output, err = captureStdout(t, func() error {
		return run(context.Background(), []string{"results", "list", "--store", store, "--kind", "short", "--min-load", "128", "--metric", "output_tokens_per_second"})
	})
	if err != nil || strings.Count(output, "short/128") != 4 || !strings.Contains(output, "one-draft") || !strings.Contains(output, "not shown") {
		t.Fatalf("list: %v\n%s", err, output)
	}
	query := filepath.Join(t.TempDir(), "query.json")
	if err = os.WriteFile(query, []byte(`{"model": {"id": "org/hybrid", "architecture": "hybrid", "params_b": 27, "checkpoint": "fp8", "features": ["native_mtp"]},
		"accelerator": "H100", "engine_version": "0.34.0", "kind": "short", "load": 112, "metric": "output_tokens_per_second", "current": {"engine": "vllm"},
		"candidates": [{"name": "three", "knobs": {"engine": "vllm", "speculation": "mtp", "drafts": 3}}, {"name": "one", "knobs": {"engine": "vllm", "speculation": "mtp", "drafts": 1}},
		{"name": "eagle", "knobs": {"engine": "vllm", "speculation": "eagle", "drafts": 2}}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = captureStdout(t, func() error {
		return run(context.Background(), []string{"results", "rank", "--store", store, "--query", query})
	})
	if err != nil || !strings.Contains(output, "1. one (") || !strings.Contains(output, "3. eagle") || !strings.Contains(output, "unknown: no similar measured cell") {
		t.Fatalf("rank: %v\n%s", err, output)
	}
	if err = run(context.Background(), []string{"results", "import", "--store", store}); err == nil {
		t.Fatal("import without a file ran")
	}
	// A benchmark runner's run directory, judged by a technique manifest.
	output, err = captureStdout(t, func() error {
		return run(context.Background(), []string{"results", "import", "../../internal/resultstore/testdata/synthetic/bench-run", "--store", store})
	})
	if err != nil || !strings.Contains(output, "10 measured results, 0 make a claim") || !strings.Contains(output, "4 not shown: dry run: stand-in servers, makes no claim") || !strings.Contains(output, "4 not shown: no technique manifest") || !strings.Contains(output, "6 not shown: baseline") {
		t.Fatalf("run import: %v\n%s", err, output)
	}
}
