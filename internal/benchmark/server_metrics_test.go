package benchmark

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// serverMetricsRunner writes the AIPerf record export and, optionally, a
// server-metrics export shaped like AIPerf's ServerMetricsExportData.
type serverMetricsRunner struct {
	export      string
	profileArgs []string
}

func (r *serverMetricsRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if len(args) == 1 && args[0] == "--version" {
		return []byte("aiperf " + AIPerfVersion), nil
	}
	r.profileArgs = append([]string(nil), args...)
	if _, err := (exportingRunner{}).Run(ctx, name, args...); err != nil {
		return nil, err
	}
	if r.export == "" {
		return nil, nil
	}
	directory := ""
	for index := range args {
		if args[index] == "--artifact-dir" && index+1 < len(args) {
			directory = args[index+1]
		}
	}
	return nil, os.WriteFile(filepath.Join(directory, serverMetricsExportName), []byte(r.export), 0o600)
}

func TestRunKeepsServerMetricsOffUnlessRequested(t *testing.T) {
	runner := &serverMetricsRunner{}
	result, err := run(context.Background(), Config{Endpoint: "http://example", Model: "m", Requests: 1, Concurrency: 1}, runner)
	if err != nil {
		t.Fatal(err)
	}
	command := strings.Join(runner.profileArgs, " ")
	if !strings.Contains(command, "--no-server-metrics") || !strings.Contains(command, "--no-gpu-telemetry") || result.ServerMetrics != nil {
		t.Fatalf("server metrics were collected without a request: args=%v result=%+v", runner.profileArgs, result.ServerMetrics)
	}
}

func TestRunSummarizesRequestedServerMetrics(t *testing.T) {
	export := `{"schema_version":"1.1","summary":{},"metrics":{
		"vllm:kv_cache_usage_perc":{"type":"gauge","unit":"ratio","series":[{"endpoint_url":"http://example/metrics","stats":{"avg":0.41,"max":0.93,"p99":0.9,"std":0.1}}]},
		"vllm:num_preemptions":{"type":"counter","series":[{"stats":{"total":3,"rate":0.2}}]},
		"vllm:prefix_cache_hits":{"type":"counter","series":[{"stats":{"total":1200}},{"stats":{"total":7}}]},
		"vllm:request_success":{"type":"counter","series":[{"stats":{"total":100}}]},
		"sglang:spec_accept_length":{"type":"gauge","series":[{"stats":{"avg":2.6}}]},
		"vllm:num_requests_waiting":{"type":"gauge","series":[]}
	}}`
	runner := &serverMetricsRunner{export: export}
	result, err := run(context.Background(), Config{Endpoint: "http://example", Model: "m", Requests: 1, Concurrency: 1, ServerMetrics: true}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(runner.profileArgs, " "), "--no-server-metrics") {
		t.Fatalf("requested server metrics were disabled: args=%v", runner.profileArgs)
	}
	metrics := result.ServerMetrics
	if metrics == nil || !metrics.Collected || len(metrics.Metrics) != 4 {
		t.Fatalf("server metrics were not summarized: %+v", metrics)
	}
	if usage := metrics.Metrics["vllm:kv_cache_usage_perc"]; usage.Stats["max"] != 0.93 || usage.Stats["avg"] != 0.41 || usage.Unit != "ratio" {
		t.Fatalf("cache usage summary=%+v", usage)
	}
	if _, kept := metrics.Metrics["vllm:kv_cache_usage_perc"].Stats["std"]; kept {
		t.Fatal("an unlisted statistic was retained")
	}
	if hits := metrics.Metrics["vllm:prefix_cache_hits"]; hits.Series != 2 || hits.Stats["total"] != 1200 {
		t.Fatalf("multi-series counter summary=%+v", hits)
	}
	if _, kept := metrics.Metrics["vllm:request_success"]; kept {
		t.Fatal("a metric outside the cache, queue, preemption, and speculation set was retained")
	}
	if metrics.Metrics["sglang:spec_accept_length"].Stats["avg"] != 2.6 {
		t.Fatalf("speculative acceptance was dropped: %+v", metrics.Metrics)
	}
}

func TestRunReportsMissingOrUnrecognizedServerMetricsAsUnknown(t *testing.T) {
	for name, export := range map[string]string{"missing": "", "unrecognized": `[1,2,3]`, "irrelevant": `{"metrics":{"process_cpu_seconds":{"series":[{"stats":{"total":4}}]}}}`} {
		t.Run(name, func(t *testing.T) {
			result, err := run(context.Background(), Config{Endpoint: "http://example", Model: "m", Requests: 1, Concurrency: 1, ServerMetrics: true}, &serverMetricsRunner{export: export})
			if err != nil {
				t.Fatal(err)
			}
			if result.ServerMetrics == nil || result.ServerMetrics.Collected || result.ServerMetrics.Reason == "" || len(result.ServerMetrics.Metrics) != 0 {
				t.Fatalf("missing server metrics were not reported as unknown: %+v", result.ServerMetrics)
			}
		})
	}
}

func TestConfidence95RefusesUnsupportedSampleCounts(t *testing.T) {
	if _, ok := Confidence95([]float64{1}); ok {
		t.Fatal("one sample produced an interval")
	}
	if _, ok := Confidence95(make([]float64, 11)); ok {
		t.Fatal("eleven samples used an absent t critical value")
	}
	interval, ok := Confidence95([]float64{10, 12, 14})
	if !ok || interval.Samples != 3 || interval.Mean != 12 || interval.Lower95 >= 12 || interval.Upper95 <= 12 {
		t.Fatalf("interval=%+v ok=%t", interval, ok)
	}
}
