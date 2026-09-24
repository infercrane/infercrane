package main

import (
	"testing"
	"time"
)

func TestSummarizeRequiresContinuousPassingWindow(t *testing.T) {
	start := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	receipts := make([]probeReceipt, 0, 4)
	for index, ttft := range []float64{300, 400, 500, 600} {
		receipts = append(receipts, probeReceipt{
			SchemaVersion: "infercrane.dev/openrouter-provider-soak/v1", StartedAtUnixMS: start.Add(time.Duration(index) * time.Minute).UnixMilli(),
			Status: 200, TTFTMS: ttft, PromptTokens: 10, CompletionTokens: 4, SawDone: true, SawFinishReason: true, Passed: true,
		})
	}
	evaluatedAt := receiptsTime(receipts[len(receipts)-1])
	report := summarize(receipts, start, evaluatedAt, 3*time.Minute, 4, 0, time.Second, 90*time.Second)
	if !report.Eligible || report.Failed != 0 || report.TTFTP95MS != 600 || report.MaxGapSec != 60 {
		t.Fatalf("unexpected report: %+v", report)
	}

	receipts[2].Passed = false
	report = summarize(receipts, start, evaluatedAt, 3*time.Minute, 4, 0, time.Second, 90*time.Second)
	if report.Eligible || report.Failed != 1 {
		t.Fatalf("failed probe did not block promotion: %+v", report)
	}
}

func TestSummarizeRejectsUnobservedWindowEdges(t *testing.T) {
	requestedSince := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	first := requestedSince.Add(4 * time.Minute)
	receipts := []probeReceipt{
		{SchemaVersion: "infercrane.dev/openrouter-provider-soak/v1", StartedAtUnixMS: first.UnixMilli(), Status: 200, TTFTMS: 100, PromptTokens: 1, CompletionTokens: 1, SawDone: true, SawFinishReason: true, Passed: true},
		{SchemaVersion: "infercrane.dev/openrouter-provider-soak/v1", StartedAtUnixMS: first.Add(time.Minute).UnixMilli(), Status: 200, TTFTMS: 100, PromptTokens: 1, CompletionTokens: 1, SawDone: true, SawFinishReason: true, Passed: true},
	}

	report := summarize(receipts, requestedSince, first.Add(10*time.Minute), time.Minute, 2, 0, time.Second, 3*time.Minute)
	if report.Eligible || report.MaxGapSec != 9*60 {
		t.Fatalf("unobserved window edges did not block promotion: %+v", report)
	}
}

func receiptsTime(receipt probeReceipt) time.Time {
	return time.UnixMilli(receipt.StartedAtUnixMS).UTC()
}

func TestPercentileUsesNearestRank(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5}
	if got := percentile(values, 0.95); got != 5 {
		t.Fatalf("p95 = %v, want 5", got)
	}
}
