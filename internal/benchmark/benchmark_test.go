package benchmark

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRecordsMeasuresAIPerfMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	data := `{"metadata":{"benchmark_phase":"warmup"},"metrics":{"time_to_first_token":{"value":999}}}
{"metadata":{"benchmark_phase":"profiling","request_start_ns":1000000000,"request_end_ns":2000000000},"metrics":{"time_to_first_token":{"value":10},"request_latency":{"value":30},"inter_token_latency":{"value":2},"output_token_count":{"value":20}}}
{"metadata":{"benchmark_phase":"profiling","request_start_ns":1500000000,"request_end_ns":3000000000},"metrics":{"time_to_first_token":{"value":20},"request_latency":{"value":40},"inter_token_latency":{"value":4},"output_token_count":{"value":30}}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := parseRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Requests != 2 || result.Succeeded != 2 || result.TTFTP50MS == nil || *result.TTFTP50MS != 10 || result.TPOTP95MS == nil || *result.TPOTP95MS != 2 {
		t.Fatalf("result=%#v", result)
	}
	if result.DurationSeconds != 2 || result.OutputTokenThroughput == nil || *result.OutputTokenThroughput != 25 {
		t.Fatalf("throughput result=%#v", result)
	}
}

func TestParseRecordsIgnoresStructuredMetricsItDoesNotConsume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	data := `{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":1000000001},"metrics":{"time_to_first_token":{"value":10},"output_sequence":{"value":[1,2,3]}}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := parseRecords(path)
	if err != nil || result.Requests != 1 || result.TTFTP50MS == nil || *result.TTFTP50MS != 10 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestReproductionCommandRedactsCredential(t *testing.T) {
	command := shellCommand("aiperf", []string{"profile", "--model", "Qwen/Qwen3-8B", "--api-key", "top-secret"}, "top-secret", "INFERCRANE_API_KEY")
	if strings.Contains(command, "top-secret") || !strings.Contains(command, "${INFERCRANE_API_KEY}") {
		t.Fatalf("command=%s", command)
	}
}

func TestRunUsesIndependentServingModelAndTokenizer(t *testing.T) {
	runner := &capturingRunner{}
	_, _ = run(context.Background(), Config{Endpoint: "http://example", Model: "logical-deployment", Tokenizer: "Qwen/Qwen3-8B", Requests: 1, Concurrency: 1}, runner)
	command := strings.Join(runner.profileArgs, " ")
	if !strings.Contains(command, "--model logical-deployment") || !strings.Contains(command, "--tokenizer Qwen/Qwen3-8B") || !strings.Contains(command, "--prompt-output-tokens-mean 32") {
		t.Fatalf("args=%v", runner.profileArgs)
	}
}

type capturingRunner struct{ profileArgs []string }

func (r *capturingRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) == 1 && args[0] == "--version" {
		return []byte("test"), nil
	}
	r.profileArgs = append([]string(nil), args...)
	return nil, os.ErrNotExist
}

func TestReproductionCommandDoesNotPersistDeletedTemporaryPath(t *testing.T) {
	temporary := "/tmp/infercrane-aiperf-123"
	command := portableReproductionCommand(shellCommand("aiperf", []string{"profile", "--output-artifact-dir", temporary, "--profile-export-prefix", "infercrane"}, "", "INFERCRANE_API_KEY"), temporary)
	if strings.Contains(command, temporary) || !strings.Contains(command, "--output-artifact-dir ./infercrane-benchmark-artifacts") || !strings.Contains(command, "--profile-export-prefix infercrane") {
		t.Fatalf("command=%s", command)
	}
}

type missingRunner struct{}

func (missingRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, os.ErrNotExist
}
func TestRunReportsMissingAIPerf(t *testing.T) {
	_, err := run(context.Background(), Config{Endpoint: "http://example", Model: "m", Requests: 1, Concurrency: 1}, missingRunner{})
	if err == nil || !strings.Contains(err.Error(), "pipx install aiperf") {
		t.Fatalf("err=%v", err)
	}
}
