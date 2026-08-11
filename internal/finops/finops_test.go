package finops

import (
	"testing"
	"time"
)

func TestEvaluateNeverInventsCostOrSavings(t *testing.T) {
	now := time.Now()
	empty := Evaluate(now, nil)
	if empty.KnownCost != nil || empty.Avoidable != nil || empty.Status != "unavailable" {
		t.Fatalf("fabricated %#v", empty)
	}
	got := Evaluate(now, []CostEvidence{{ID: "a", Source: "invoice", Currency: "USD", Amount: 12, ObservedAt: now}})
	if got.KnownCost == nil || *got.KnownCost != 12 || got.Avoidable != nil {
		t.Fatalf("unexpected %#v", got)
	}
}
