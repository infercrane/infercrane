package store

import (
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

func guardNumber(value float64) *float64 { return &value }

func TestBenchmarkGuardMetricsRequireComparableAIPerfRuns(t *testing.T) {
	activeWorkload := `{"endpoint_type":"chat","streaming":true,"request_count":40,"concurrency":4,"random_seed":17,"server_token_count":true,"revision_selector":"active"}`
	candidateWorkload := `{"endpoint_type":"chat","streaming":true,"request_count":40,"concurrency":4,"random_seed":17,"server_token_count":true,"revision_selector":"candidate","direct_revision_validation":true}`
	rows := []domain.BenchmarkResult{
		{ID: "candidate-benchmark", RevisionID: "candidate", Tool: "aiperf", ToolVersion: "0.9.0", WorkloadJSON: candidateWorkload, RequestCount: 40, Failed: 1, TTFTP95MS: guardNumber(120), LatencyP95MS: guardNumber(220), OutputTokenThroughput: guardNumber(90)},
		{ID: "active-benchmark", RevisionID: "active", Tool: "aiperf", ToolVersion: "0.9.0", WorkloadJSON: activeWorkload, RequestCount: 40, TTFTP95MS: guardNumber(100), LatencyP95MS: guardNumber(200), OutputTokenThroughput: guardNumber(100)},
	}
	active, candidate, ok := benchmarkGuardMetrics(1, 1, rows, "active", "candidate")
	if !ok || active.EvidenceSource != "aiperf_benchmark" || active.EvidenceID != "active-benchmark" || candidate.EvidenceID != "candidate-benchmark" || candidate.ErrorRate != 0.025 || candidate.P95TTFTMS == nil || *candidate.P95TTFTMS != 120 {
		t.Fatalf("active=%+v candidate=%+v ok=%t", active, candidate, ok)
	}

	rows[0].WorkloadJSON = `{"endpoint_type":"chat","streaming":true,"request_count":40,"concurrency":8,"random_seed":17,"server_token_count":true}`
	if _, _, ok = benchmarkGuardMetrics(1, 1, rows, "active", "candidate"); ok {
		t.Fatal("different concurrency must not be compared")
	}
}
