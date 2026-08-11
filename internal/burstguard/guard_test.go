package burstguard

import (
	"testing"
	"time"
)

func TestEvaluateFailsClosedAndBoundsCost(t *testing.T) {
	now := time.Now()
	p := Policy{Enabled: true, QueueThreshold: 4, BreachIntervals: 3, RecoveryIntervals: 2, SignalMaxAge: time.Minute, MaxIncrementalCostMicrousdHour: 100}
	if got := Evaluate(p, Signal{QueueDepth: 8, ConsecutiveBreaches: 3, IncrementalCostMicrousdHour: 101, ObservedAt: now, ExternalHealthy: true}, now); got.Action != "hold" {
		t.Fatal(got)
	}
	if got := Evaluate(p, Signal{QueueDepth: 8, ConsecutiveBreaches: 3, IncrementalCostMicrousdHour: 90, ObservedAt: now, ExternalHealthy: true}, now); got.Action != "overflow" {
		t.Fatal(got)
	}
	if got := Evaluate(p, Signal{}, now); got.Action != "unknown" {
		t.Fatal(got)
	}
}
