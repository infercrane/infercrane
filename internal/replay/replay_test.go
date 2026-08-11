package replay

import (
	"encoding/json"
	"testing"
	"time"
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
