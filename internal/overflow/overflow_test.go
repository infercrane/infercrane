package overflow

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDecisionSerializesBothHysteresisCounters(t *testing.T) {
	encoded, err := json.Marshal(Decision{ConsecutiveHigh: 2, ConsecutiveLow: 3})
	if err != nil || string(encoded) != `{"route":"","action":"","reason":"","consecutive_high":2,"consecutive_low":3}` {
		t.Fatalf("encoded=%s err=%v", encoded, err)
	}
}

func TestQueueOverflowRequiresConsecutiveEvidenceAndBoundedRecovery(t *testing.T) {
	now := time.Date(2026, 8, 11, 8, 0, 0, 0, time.UTC)
	waiting := 5.0
	policy := Policy{Mode: "health_and_queue", QueueThreshold: 4, BreachIntervals: 2, RecoveryIntervals: 2, Cooldown: time.Minute, SignalMaxAge: time.Minute, PrivacyAcknowledged: true, BudgetAvailable: true}
	state := State{}
	first, err := Evaluate(policy, state, Signal{PrimaryHealthy: true, Waiting: &waiting, ObservedAt: now}, now)
	if err != nil || first.Route != "primary" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	state.ConsecutiveHigh = first.ConsecutiveHigh
	second, _ := Evaluate(policy, state, Signal{PrimaryHealthy: true, Waiting: &waiting, ObservedAt: now.Add(time.Second)}, now.Add(time.Second))
	if second.Route != "external" || second.Action != "overflow" {
		t.Fatalf("second=%#v", second)
	}
	low := 0.0
	state = State{External: true, LastChangedAt: now.Add(time.Second)}
	early, _ := Evaluate(policy, state, Signal{PrimaryHealthy: true, Waiting: &low, ObservedAt: now.Add(30 * time.Second)}, now.Add(30*time.Second))
	if early.Route != "external" {
		t.Fatalf("cooldown oscillation: %#v", early)
	}
	state.ConsecutiveLow = 1
	recovered, _ := Evaluate(policy, state, Signal{PrimaryHealthy: true, Waiting: &low, ObservedAt: now.Add(2 * time.Minute)}, now.Add(2*time.Minute))
	if recovered.Route != "primary" || recovered.Action != "recover" {
		t.Fatalf("recovery=%#v", recovered)
	}
}

func TestOverflowFailsClosedForBudgetPrivacyAndStaleSignals(t *testing.T) {
	now := time.Now().UTC()
	waiting := 99.0
	base := Policy{Mode: "health_and_queue", QueueThreshold: 1, BreachIntervals: 1, RecoveryIntervals: 1, SignalMaxAge: time.Minute, PrivacyAcknowledged: true, BudgetAvailable: false}
	decision, err := Evaluate(base, State{}, Signal{PrimaryHealthy: false, Waiting: &waiting, ObservedAt: now}, now)
	if err != nil || decision.Route != "unavailable" || decision.Action != "deny" {
		t.Fatalf("budget failure did not fail closed: %#v %v", decision, err)
	}
	recovered, err := Evaluate(base, State{External: true}, Signal{PrimaryHealthy: true}, now)
	if err != nil || recovered.Route != "primary" || recovered.Action != "recover" {
		t.Fatalf("budget loss did not explicitly recover: %#v %v", recovered, err)
	}
	base.BudgetAvailable = true
	decision, err = Evaluate(base, State{}, Signal{PrimaryHealthy: true, Waiting: &waiting, ObservedAt: now.Add(-2 * time.Minute)}, now)
	if err != nil || decision.Route != "primary" {
		t.Fatalf("stale signal did not fail closed: %#v %v", decision, err)
	}
	decision, err = Evaluate(base, State{External: true}, Signal{PrimaryHealthy: true, Waiting: &waiting, ObservedAt: now.Add(-2 * time.Minute)}, now)
	if err != nil || decision.Route != "external" {
		t.Fatalf("stale recovery oscillated route: %#v %v", decision, err)
	}
	base.PrivacyAcknowledged = false
	if _, err = Evaluate(base, State{}, Signal{}, now); err == nil {
		t.Fatal("privacy denial accepted")
	}
}

func TestHealthFailureAndProviderRecoveryAreDeterministic(t *testing.T) {
	now := time.Now().UTC()
	policy := Policy{Mode: "health", BreachIntervals: 1, RecoveryIntervals: 1, PrivacyAcknowledged: true, BudgetAvailable: true}
	failed, _ := Evaluate(policy, State{}, Signal{PrimaryHealthy: false}, now)
	if failed.Route != "external" {
		t.Fatalf("failed=%#v", failed)
	}
	recovered, _ := Evaluate(policy, State{External: true}, Signal{PrimaryHealthy: true}, now)
	if recovered.Route != "primary" || recovered.Action != "recover" {
		t.Fatalf("recovered=%#v", recovered)
	}
}

func TestHealthModeUsesConfiguredBreachAndRecoveryHysteresis(t *testing.T) {
	now := time.Unix(100, 0)
	policy := Policy{Mode: "health", BreachIntervals: 2, RecoveryIntervals: 2, PrivacyAcknowledged: true, BudgetAvailable: true}
	first, err := Evaluate(policy, State{}, Signal{PrimaryHealthy: false}, now)
	if err != nil || first.Route != "primary" || first.ConsecutiveHigh != 1 {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, _ := Evaluate(policy, State{ConsecutiveHigh: first.ConsecutiveHigh}, Signal{PrimaryHealthy: false}, now.Add(time.Second))
	if second.Route != "external" || second.Action != "overflow" {
		t.Fatalf("second=%#v", second)
	}
	recovering, _ := Evaluate(policy, State{External: true}, Signal{PrimaryHealthy: true}, now.Add(2*time.Second))
	if recovering.Route != "external" || recovering.ConsecutiveLow != 1 {
		t.Fatalf("recovering=%#v", recovering)
	}
	recovered, _ := Evaluate(policy, State{External: true, ConsecutiveLow: 1}, Signal{PrimaryHealthy: true}, now.Add(3*time.Second))
	if recovered.Route != "primary" || recovered.Action != "recover" {
		t.Fatalf("recovered=%#v", recovered)
	}
}
