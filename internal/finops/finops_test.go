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

func TestEvaluateRejectsFutureOrCurrencylessEvidenceAndUsesLatestScope(t *testing.T) {
	now := time.Now()
	got := Evaluate(now, []CostEvidence{{ID: "future", Scope: "rate", Source: "api", Currency: "USD", Amount: 1, ObservedAt: now.Add(time.Minute)}, {ID: "currencyless", Scope: "rate", Source: "api", Amount: 2, ObservedAt: now}})
	if got.Status != "unavailable" || got.KnownCost != nil {
		t.Fatalf("untrustworthy cost accepted: %#v", got)
	}
	got = Evaluate(now, []CostEvidence{{ID: "old", Scope: "rate", Source: "api", Currency: "USD", Amount: 10, ObservedAt: now.Add(-time.Hour)}, {ID: "new", Scope: "rate", Source: "api", Currency: "USD", Amount: 7, ObservedAt: now}})
	if got.KnownCost == nil || *got.KnownCost != 7 || len(got.Evidence) != 1 || got.Evidence[0].ID != "new" {
		t.Fatalf("dedup=%#v", got)
	}
}
