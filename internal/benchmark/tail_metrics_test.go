package benchmark

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRecords(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "records.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func latencyRecords(count int) []string {
	lines := make([]string, 0, count)
	for index := 1; index <= count; index++ {
		lines = append(lines, fmt.Sprintf(`{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":1000000001},"metrics":{"time_to_first_token":{"value":%d,"unit":"ms"},"request_latency":{"value":%d,"unit":"ms"},"inter_token_latency":{"value":%d,"unit":"ms"}}}`, index, 10*index, index))
	}
	return lines
}

// TestParseRecordsReportsP99OnlyWithEnoughSamples keeps a slowest request
// from being presented as a p99: below 100 samples nearest-rank p99 is the
// maximum, so it stays unknown.
func TestParseRecordsReportsP99OnlyWithEnoughSamples(t *testing.T) {
	small, err := parseRecords(writeRecords(t, latencyRecords(MinP99Samples-1)))
	if err != nil {
		t.Fatal(err)
	}
	if small.TTFTP99MS != nil || small.TPOTP99MS != nil || small.LatencyP99MS != nil {
		t.Fatalf("p99 reported from %d samples: %+v", MinP99Samples-1, small)
	}
	if small.TTFTP95MS == nil {
		t.Fatal("p95 must not depend on the p99 sample rule")
	}

	large, err := parseRecords(writeRecords(t, latencyRecords(200)))
	if err != nil {
		t.Fatal(err)
	}
	// Nearest rank: ceil(0.99 * 200) = 198th smallest value.
	if large.TTFTP99MS == nil || *large.TTFTP99MS != 198 || large.TPOTP99MS == nil || *large.TPOTP99MS != 198 || large.LatencyP99MS == nil || *large.LatencyP99MS != 1980 {
		t.Fatalf("p99 from 200 samples: ttft=%v tpot=%v latency=%v", large.TTFTP99MS, large.TPOTP99MS, large.LatencyP99MS)
	}
}

func TestParseRecordsMeasuresStreamGapsOfSuccessfulRequests(t *testing.T) {
	result, err := parseRecords(writeRecords(t, []string{
		`{"metadata":{"benchmark_phase":"warmup"},"metrics":{"inter_chunk_latency":{"value":[5000],"unit":"ms"}}}`,
		`{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":1000000001},"metrics":{"time_to_first_token":{"value":10},"inter_chunk_latency":{"value":[5,7,40],"unit":"ms"}}}`,
		`{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":1000000001},"metrics":{"time_to_first_token":{"value":10},"inter_chunk_latency":{"value":[0.01,0.2],"unit":"s"}}}`,
		`{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":1000000001},"metrics":{"inter_chunk_latency":{"value":[9000],"unit":"ms"}},"error":{"code":499}}`,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.StreamGapMaxMS == nil || *result.StreamGapMaxMS != 200 {
		t.Fatalf("longest stall=%v, want 200 ms from the seconds-unit record and nothing from warmup or the failed request", result.StreamGapMaxMS)
	}
	if result.StreamGapP99MS != nil {
		t.Fatalf("stream gap p99 reported from five gaps: %v", *result.StreamGapP99MS)
	}

	gaps := make([]string, 120)
	for index := range gaps {
		gaps[index] = fmt.Sprint(index + 1)
	}
	many, err := parseRecords(writeRecords(t, []string{`{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":1000000001},"metrics":{"inter_chunk_latency":{"value":[` + strings.Join(gaps, ",") + `],"unit":"ms"}}}`}))
	if err != nil {
		t.Fatal(err)
	}
	// One request with 120 gaps is enough: the sample is gaps, not requests.
	if many.StreamGapP99MS == nil || *many.StreamGapP99MS != 119 || many.StreamGapMaxMS == nil || *many.StreamGapMaxMS != 120 {
		t.Fatalf("stream gaps p99=%v max=%v", many.StreamGapP99MS, many.StreamGapMaxMS)
	}
}

func TestParseRecordsLeavesStreamGapsUnknownForBufferedRuns(t *testing.T) {
	result, err := parseRecords(writeRecords(t, []string{`{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":1000000001},"metrics":{"time_to_first_token":{"value":10},"request_latency":{"value":30}}}`}))
	if err != nil {
		t.Fatal(err)
	}
	if result.StreamGapMaxMS != nil || result.StreamGapP99MS != nil {
		t.Fatalf("buffered run reported stream gaps: max=%v p99=%v", result.StreamGapMaxMS, result.StreamGapP99MS)
	}
}

func TestParseRecordsRejectsMalformedStreamGaps(t *testing.T) {
	for name, metric := range map[string]string{
		"scalar":   `{"value":12,"unit":"ms"}`,
		"strings":  `{"value":["12"],"unit":"ms"}`,
		"negative": `{"value":[3,-1],"unit":"ms"}`,
		"unit":     `{"value":[3],"unit":"fortnights"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseRecords(writeRecords(t, []string{`{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":1000000001},"metrics":{"inter_chunk_latency":` + metric + `}}`}))
			if err == nil || !strings.Contains(err.Error(), "inter_chunk_latency") {
				t.Fatalf("malformed stream gaps were accepted: err=%v", err)
			}
		})
	}
}

// TestServerMetricsKeepPreemptionUnderTheSummaryCap covers SGLang, which
// calls preemption "retraction" and exposes more cache and queue metrics than
// the summary holds. Alphabetical order alone would drop the retraction count.
func TestServerMetricsKeepPreemptionUnderTheSummaryCap(t *testing.T) {
	var metrics []string
	for index := range maxServerMetricSummaries + 8 {
		metrics = append(metrics, fmt.Sprintf(`"sglang:cache_metric_%02d":{"type":"gauge","series":[{"stats":{"avg":%d}}]}`, index, index))
	}
	metrics = append(metrics,
		`"sglang:num_retracted_requests":{"type":"counter","series":[{"stats":{"total":4}}]}`,
		`"vllm:num_preemptions":{"type":"counter","series":[{"stats":{"total":2}}]}`,
	)
	export := `{"metrics":{` + strings.Join(metrics, ",") + `}}`
	result, err := run(context.Background(), Config{Endpoint: "http://example", Model: "m", Requests: 1, Concurrency: 1, ServerMetrics: true}, &serverMetricsRunner{export: export})
	if err != nil {
		t.Fatal(err)
	}
	summary := result.ServerMetrics
	if summary == nil || !summary.Collected || len(summary.Metrics) != maxServerMetricSummaries {
		t.Fatalf("summary=%+v", summary)
	}
	if summary.Metrics["sglang:num_retracted_requests"].Stats["total"] != 4 || summary.Metrics["vllm:num_preemptions"].Stats["total"] != 2 {
		t.Fatalf("preemption counters were dropped under the cap: %+v", summary.Metrics)
	}
	if _, kept := summary.Metrics["sglang:cache_metric_00"]; !kept {
		t.Fatal("the remaining slots must still fill in name order")
	}
}
