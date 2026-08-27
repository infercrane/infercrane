package autoscale

import (
	"strings"
	"testing"
	"time"
)

func TestEvaluateUsesStabilityAndBounds(t *testing.T) {
	p := Policy{Enabled: true, MinReplicas: 1, MaxReplicas: 3, QueueThreshold: 2, LowLoadThreshold: .5, ScaleUpIntervals: 2, ScaleDownIntervals: 3, Cooldown: time.Minute}
	now := time.Now()
	d, err := Evaluate(p, State{Replicas: 2, ConsecutiveHigh: 1}, Signals{Waiting: 4, ObservedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != "scale_up" || d.NewReplicas != 3 {
		t.Fatalf("unexpected decision: %#v", d)
	}
	d, err = Evaluate(p, State{Replicas: 3, ConsecutiveHigh: 4}, Signals{Waiting: 4, ObservedAt: now})
	if err != nil || d.Action != "hold" {
		t.Fatalf("must honor maximum: %#v %v", d, err)
	}
}

func TestEvaluateWithSLOUsesComparableGoodputAndPreservesHysteresis(t *testing.T) {
	now := time.Unix(100, 0)
	ttftLimit, latencyLimit := 200.0, 800.0
	rps, goodput, ttft, latency := 18.0, 5.0, 250.0, 700.0
	policy := Policy{Enabled: true, MinReplicas: 1, MaxReplicas: 6, ScaleUpIntervals: 2, ScaleDownIntervals: 3, MaxTTFTP95MS: &ttftLimit, MaxLatencyP95MS: &latencyLimit}
	evidence := SLOEvidence{RequestSamples: 100, RequestsPerSecond: &rps, P95TTFTMS: &ttft, P95LatencyMS: &latency, BenchmarkID: "benchmark-exact", BenchmarkSamples: 100, GoodputPerReplica: &goodput, Comparable: true}
	decision, err := EvaluateWithSLO(policy, State{Replicas: 2, ConsecutiveHigh: 1}, Signals{ObservedAt: now}, evidence)
	if err != nil || decision.Action != "scale_up" || decision.NewReplicas != 3 || !strings.Contains(decision.Reason, "target 4") || !strings.Contains(decision.Reason, "benchmark-exact") {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
}

func TestEvaluateWithSLOFallsBackWithoutFabricatingCapacity(t *testing.T) {
	limit, rps, ttft := 200.0, 10.0, 250.0
	policy := Policy{Enabled: true, MinReplicas: 1, MaxReplicas: 3, QueueThreshold: 2, LowLoadThreshold: .5, ScaleUpIntervals: 1, ScaleDownIntervals: 2, MaxTTFTP95MS: &limit}
	decision, err := EvaluateWithSLO(policy, State{Replicas: 1}, Signals{Waiting: 3, ObservedAt: time.Unix(100, 0)}, SLOEvidence{RequestSamples: 100, RequestsPerSecond: &rps, P95TTFTMS: &ttft})
	if err != nil || decision.Action != "scale_up" || !strings.Contains(decision.Reason, "capacity evidence absent") || !strings.Contains(decision.Reason, "queue policy") {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
}

func TestEvaluateWithSLOFailsClosedOnLowRequestSamples(t *testing.T) {
	limit := 200.0
	policy := Policy{Enabled: true, MinReplicas: 1, MaxReplicas: 2, QueueThreshold: 10, ScaleUpIntervals: 1, ScaleDownIntervals: 1, MaxTTFTP95MS: &limit}
	decision, err := EvaluateWithSLO(policy, State{Replicas: 1}, Signals{ObservedAt: time.Unix(100, 0)}, SLOEvidence{RequestSamples: MinimumSLOSamples - 1})
	if err != nil || decision.Action != "hold" || !strings.Contains(decision.Reason, "evidence insufficient") || !strings.Contains(decision.Reason, "capacity evidence absent") {
		t.Fatalf("decision=%#v err=%v", decision, err)
	}
}

func TestEvaluateHonorsCooldown(t *testing.T) {
	now := time.Now()
	p := Policy{Enabled: true, MinReplicas: 1, MaxReplicas: 3, QueueThreshold: 1, ScaleUpIntervals: 1, ScaleDownIntervals: 1, Cooldown: time.Minute}
	d, err := Evaluate(p, State{Replicas: 1, LastScaledAt: now.Add(-time.Second)}, Signals{Waiting: 9, ObservedAt: now})
	if err != nil || d.Action != "hold" || d.Reason != "cooldown active" {
		t.Fatalf("unexpected decision: %#v %v", d, err)
	}
}
