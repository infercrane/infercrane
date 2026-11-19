package replay

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestBuildStoresShapeWithoutContentAndIsDeterministic(t *testing.T) {
	start := time.Unix(1000, 0).UTC()
	in, out := 10, 5
	observations := []Observation{{StartedAt: start.Add(time.Second), DurationMS: 2000, InputTokens: &in, OutputTokens: &out, Operation: "chat", SessionIDHash: "session-hash", SharedPrefixHash: "prefix-hash"}, {StartedAt: start.Add(2 * time.Second), DurationMS: 1000, InputTokens: &in, OutputTokens: &out, Operation: "chat", SessionIDHash: "session-hash"}}
	first, err := Build("dep", "prod", "rev", start, start.Add(time.Minute), observations)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Build("dep", "prod", "rev", start, start.Add(time.Minute), observations)
	var summary Summary
	if json.Unmarshal([]byte(first.SummaryJSON), &summary) != nil || summary.PeakConcurrency != 2 || summary.Sessions != 1 || summary.ContentStored || first.ShapeDigest != second.ShapeDigest {
		t.Fatalf("trace=%#v summary=%#v", first, summary)
	}
	if string(first.ShapeJSON) == "" || contains(string(first.ShapeJSON), "prompt") {
		t.Fatal("content field entered replay")
	}
}
func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}

func TestTraceLatencyAndTokenVolumeReadTheShape(t *testing.T) {
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	ttft := func(v float64) *float64 { return &v }
	tokens := func(v int) *int { return &v }
	trace, err := Build("", "", "", start, start.Add(time.Hour), []Observation{
		{StartedAt: start, DurationMS: 100, TTFTMS: ttft(20), InputTokens: tokens(100), CachedInputTokens: tokens(40), OutputTokens: tokens(10), Operation: "chat"},
		{StartedAt: start.Add(time.Minute), DurationMS: 300, TTFTMS: ttft(60), InputTokens: tokens(200), OutputTokens: tokens(30), Operation: "chat"},
		{StartedAt: start.Add(2 * time.Minute), DurationMS: 200, InputTokens: tokens(50), OutputTokens: tokens(5), Operation: "chat"},
		{StartedAt: start.Add(3 * time.Minute), DurationMS: 5, Operation: "embeddings"},
	})
	if err != nil {
		t.Fatal(err)
	}
	latency, err := TraceLatency(trace)
	if err != nil || latency.Requests != 3 || latency.TTFTRequests != 2 || *latency.LatencyP50MS != 200 || *latency.LatencyP95MS != 300 || *latency.TTFTP95MS != 60 {
		t.Fatalf("latency=%+v err=%v", latency, err)
	}
	volume, err := TraceTokenVolume(trace)
	if err != nil || volume.Requests != 3 || volume.InputTokens != 350 || volume.CachedInputTokens != 40 || volume.CachedReported != 1 || volume.OutputTokens != 45 || volume.KeptSpanSeconds != 180 {
		t.Fatalf("volume=%+v err=%v", volume, err)
	}
	if empty, _ := TraceLatency(domain.ReplayTrace{ShapeJSON: "[]"}); empty.LatencyP95MS != nil || empty.TTFTP95MS != nil {
		t.Fatalf("empty latency=%+v", empty)
	}
}
