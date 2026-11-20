package continualoptimizer

import (
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/workloadprofile"
)

func TestPublicPriorAdapterPreservesScreeningBoundary(t *testing.T) {
	document, err := workloadprofile.PublicChutesPrior()
	if err != nil {
		t.Fatal(err)
	}
	row, err := FromPublicProfile(document, document.Profiles[0])
	if err != nil || row.Source != SourcePublicPrior || row.Digest != workloadprofile.PublicPriorDigest || row.RequestCount < 1 {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestReplayAdapterDerivesContentFreeReuseRatios(t *testing.T) {
	start := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	trace := domain.ReplayTrace{
		SchemaVersion: "infercrane.replay/v1", ShapeDigest: strings.Repeat("a", 64), RequestCount: 4,
		WindowStart: start, WindowEnd: start.Add(time.Minute),
		SummaryJSON: `{"requests":4,"input_tokens_mean":1000,"output_tokens_mean":200,"peak_concurrency":2,"content_stored":false}`,
		ShapeJSON:   `[{"streaming":true,"session_id_hash":"s","shared_prefix_hash":"p"},{"streaming":true,"session_id_hash":"s","shared_prefix_hash":"p","tool_pause_ms":20},{"streaming":false,"session_id_hash":"x"},{"streaming":false}]`,
	}
	row, err := FromReplayTrace(trace)
	if err != nil || row.Source != SourceCustomerObserved || row.RequestsPerSecond != float64(4)/60 || row.SharedPrefixRatio != .5 || row.SessionReuseRatio != .5 || row.ToolPauseRequestRatio != .25 || row.StreamingRequestRatio != .5 {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestReplayAdapterRejectsContentOrCountMismatch(t *testing.T) {
	start := time.Now().UTC().Add(-time.Minute)
	trace := domain.ReplayTrace{SchemaVersion: "infercrane.replay/v1", ShapeDigest: strings.Repeat("a", 64), RequestCount: 1, WindowStart: start, WindowEnd: start.Add(time.Minute), SummaryJSON: `{"requests":1,"peak_concurrency":1,"content_stored":true}`, ShapeJSON: `[{}]`}
	if _, err := FromReplayTrace(trace); err == nil {
		t.Fatal("content-bearing trace must be rejected")
	}
}

func TestMonitoringAdaptersDoNotInventMissingMeasurements(t *testing.T) {
	start := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	errorRate, ttft, output := .01, 100.0, 50.0
	snapshot := domain.EndpointMonitoringSnapshot{Endpoint: "chat", WindowStart: start, WindowEnd: start.Add(7 * 24 * time.Hour), Summary: domain.MonitoringSummary{
		Requests: 1000, Streaming: 250, InputTokens: 400_000, InputTokenSamples: 800, OutputTokens: 160_000, OutputTokenSamples: 800,
		ErrorRate: &errorRate, P95TTFTMS: &ttft, OutputTokensPerSecond: &output,
	}}
	workload, err := WorkloadFromMonitoring(snapshot)
	if err != nil || workload.Source != SourceCustomerObserved || workload.InputTokensMean != 500 || workload.OutputTokensMean != 200 || workload.StreamingRequestRatio != .25 || workload.PeakConcurrency != 0 || !strings.HasPrefix(workload.Digest, "sha256:") {
		t.Fatalf("workload=%+v err=%v", workload, err)
	}
	service := ServiceFromMonitoring(snapshot, CostEvidence{})
	if service.RequestCount != 1000 || service.TTFTP95MS != 100 || service.OutputTokensPerSecond != 50 || service.TPOTP95MS != 0 || service.QualifiedOutputTokensPerSecond != 0 || service.Availability != 0 || service.ContributionMargin != nil || service.CostPerMillionOutputUSD != 0 {
		t.Fatalf("service=%+v", service)
	}
	// 1000 requests x 200 output tokens over 168 hours, 2 replicas at $2/h.
	service = ServiceFromMonitoring(snapshot, CostEvidence{HourlyUSDPerReplica: 2, AverageReplicas: 2})
	want := 4 / (200_000.0 / 168 / 1e6)
	if diff := service.CostPerMillionOutputUSD - want; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("cost=%f want=%f", service.CostPerMillionOutputUSD, want)
	}
	if _, err = WorkloadFromMonitoring(domain.EndpointMonitoringSnapshot{WindowStart: start, WindowEnd: start.Add(time.Hour)}); err == nil {
		t.Fatal("a window without requests is not workload evidence")
	}
}

func TestAverageReplicasWeighsLifetimesInsideTheWindow(t *testing.T) {
	start := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	end := start.Add(10 * time.Hour)
	deleted := start.Add(5 * time.Hour)
	spans := []domain.ReplicaSpan{
		{ReplicaID: "a", CreatedAt: start.Add(-time.Hour)},                      // whole window
		{ReplicaID: "b", CreatedAt: start.Add(-time.Hour), DeletedAt: &deleted}, // half
		{ReplicaID: "c", CreatedAt: end.Add(time.Hour)},                         // after
	}
	if got := AverageReplicas(spans, start, end); got != 1.5 {
		t.Fatalf("average=%f", got)
	}
}
