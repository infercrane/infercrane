package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func environment(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestRunRejectsUnknownOperationsAndIncompleteTargets(t *testing.T) {
	env := environment(nil)
	for _, args := range [][]string{{}, {"explode"}, {"profile"}, {"qualify"}} {
		if err := run(context.Background(), args, env, strings.NewReader("{}"), &bytes.Buffer{}); err == nil {
			t.Fatalf("args %v accepted", args)
		}
	}
	for _, operation := range []string{"profile", "timeline", "qualify"} {
		if err := run(context.Background(), []string{operation}, env, strings.NewReader(`{"surprise":1}`), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "decode") {
			t.Fatalf("%s accepted an unknown field: %v", operation, err)
		}
	}
}

func TestGenerateFailsClosedWithoutPricedConfiguration(t *testing.T) {
	err := run(context.Background(), []string{"generate"}, environment(nil), strings.NewReader("{}"), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "INFERCRANE_KERNEL_GPU_HOURLY_USD") {
		t.Fatalf("err=%v", err)
	}
	if err = run(context.Background(), []string{"generate"}, environment(nil), strings.NewReader(`{"surprise":1}`), &bytes.Buffer{}); err == nil {
		t.Fatal("unknown request field accepted")
	}
}

func TestLiteratureDefaultsToOffAndValidatesSource(t *testing.T) {
	for _, value := range []string{"", "off"} {
		source, err := literatureSource(value)
		if err != nil || source != nil {
			t.Fatalf("%q: source=%v err=%v", value, source, err)
		}
	}
	if source, err := literatureSource("arxiv"); err != nil || source == nil {
		t.Fatalf("arxiv: %v %v", source, err)
	}
	if _, err := literatureSource("relative/file.json"); err == nil {
		t.Fatal("relative literature path accepted")
	}
}

func TestReadTokenRequiresOwnerOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(" secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readToken(path); err == nil {
		t.Fatal("group-readable token accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	token, err := readToken(path)
	if err != nil || token != "secret" {
		t.Fatalf("token=%q err=%v", token, err)
	}
}

func TestProfileWiringReachesTheHardwareCheck(t *testing.T) {
	directory := t.TempDir()
	token := filepath.Join(directory, "worker.token")
	if err := os.WriteFile(token, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := environment(map[string]string{
		"INFERCRANE_KERNEL_RUNTIMES_FILE": "../../tools/kernel-lab/target-runtimes.json", "INFERCRANE_KERNEL_MODEL_CACHE": directory, "INFERCRANE_KERNEL_WORK_DIR": directory,
		"INFERCRANE_KERNEL_PACK_DIR": directory, "INFERCRANE_KERNEL_REPLAY_DIR": directory, "INFERCRANE_KERNEL_GPU_HOURLY_USD": "4.59",
		"INFERCRANE_KERNEL_EVALUATOR": directory + "/evaluate", "INFERCRANE_ACCELERATOR_WORKER_URL": "http://127.0.0.1:1", "INFERCRANE_ACCELERATOR_WORKER_TOKEN_FILE": token, "PATH": "/nonexistent",
	})
	// The file path is relative to this package, so resolve it the way an operator must.
	absolute, err := filepath.Abs("../../tools/kernel-lab/target-runtimes.json")
	if err != nil {
		t.Fatal(err)
	}
	wrapped := func(name string) string {
		if name == "INFERCRANE_KERNEL_RUNTIMES_FILE" {
			return absolute
		}
		return env(name)
	}
	digest := "sha256:" + strings.Repeat("1", 64)
	request := `{"input_digest":"` + digest + `","model":{"repository":"acme/model","revision":"` + strings.Repeat("a", 40) + `"},"runtime":{"name":"vllm","version":"0.22.1","image_digest":"` + digest + `"},"hardware":{"vendor":"nvidia","accelerator":"H200"},"topology":{"mode":"aggregated","nodes":1,"accelerators":1,"tensor_parallel":1,"pipeline_parallel":1,"expert_parallel":1},"workload":{"digest":"` + digest + `","modality":"text","phase":"decode","batch_size":1,"concurrency":1,"input_tokens":1,"output_tokens":1,"quality_suite":"q","replay":"r"}}`
	if err = run(context.Background(), []string{"profile"}, wrapped, strings.NewReader(request), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "identify GPU") {
		t.Fatalf("expected the configured adapter to reach the GPU check: %v", err)
	}
	broken := func(name string) string {
		if name == "INFERCRANE_KERNEL_WORK_DIR" {
			return "relative"
		}
		return wrapped(name)
	}
	if err = run(context.Background(), []string{"profile"}, broken, strings.NewReader(request), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "INFERCRANE_KERNEL_WORK_DIR") {
		t.Fatalf("relative work directory accepted: %v", err)
	}
	// The reviewed catalog has no Nsight Systems configuration, so a timeline
	// request fails closed before any GPU or container work.
	if err = run(context.Background(), []string{"timeline"}, wrapped, strings.NewReader(request), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "opt-in") {
		t.Fatalf("timeline without a reviewed configuration: %v", err)
	}
	for _, seconds := range []string{"2", "600", "twenty", "1.5"} {
		window := func(name string) string {
			if name == "INFERCRANE_KERNEL_TIMELINE_SECONDS" {
				return seconds
			}
			return wrapped(name)
		}
		if err = run(context.Background(), []string{"timeline"}, window, strings.NewReader(request), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "INFERCRANE_KERNEL_TIMELINE_SECONDS") {
			t.Fatalf("timeline window %q accepted: %v", seconds, err)
		}
	}
}
