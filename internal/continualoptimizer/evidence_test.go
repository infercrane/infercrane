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

func TestMonitoringAdapterDoesNotInventMissingMeasurements(t *testing.T) {
	errorRate, ttft, output := .01, 100.0, 50.0
	snapshot := domain.EndpointMonitoringSnapshot{Summary: domain.MonitoringSummary{Requests: 100, ErrorRate: &errorRate, P95TTFTMS: &ttft, OutputTokensPerSecond: &output}}
	service := FromMonitoring(snapshot, .999, .5, 1.2, .3)
	if service.RequestCount != 100 || service.TTFTP95MS != 100 || service.OutputTokensPerSecond != 50 || service.TPOTP95MS != 0 || service.QualifiedOutputTokensPerSecond != 0 {
		t.Fatalf("service=%+v", service)
	}
}
