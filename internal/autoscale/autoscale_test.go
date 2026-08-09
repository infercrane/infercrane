package autoscale

import (
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

func TestEvaluateHonorsCooldown(t *testing.T) {
	now := time.Now()
	p := Policy{Enabled: true, MinReplicas: 1, MaxReplicas: 3, QueueThreshold: 1, ScaleUpIntervals: 1, ScaleDownIntervals: 1, Cooldown: time.Minute}
	d, err := Evaluate(p, State{Replicas: 1, LastScaledAt: now.Add(-time.Second)}, Signals{Waiting: 9, ObservedAt: now})
	if err != nil || d.Action != "hold" || d.Reason != "cooldown active" {
		t.Fatalf("unexpected decision: %#v %v", d, err)
	}
}
