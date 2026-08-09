package autoscale

import (
	"context"
	"testing"
	"time"
)

type fakeRepository struct {
	deployment Deployment
	decision   Decision
	saved      State
}

func (f *fakeRepository) AutoscalingDeployments(context.Context) ([]Deployment, error) {
	return []Deployment{f.deployment}, nil
}
func (f *fakeRepository) RecordDecision(_ context.Context, _ string, d Decision, _ string) error {
	f.decision = d
	return nil
}
func (f *fakeRepository) SaveState(_ context.Context, _ string, s State) error {
	f.saved = s
	return nil
}

type fakeSignals struct{}

func (fakeSignals) Signals(context.Context, string) (Signals, error) { return Signals{Waiting: 4}, nil }

type fakeFleet struct{ replicas int }

func (f *fakeFleet) ScaleTo(_ context.Context, _ string, n int) error { f.replicas = n; return nil }

func TestControllerExecutesAndRecordsDecision(t *testing.T) {
	repo := &fakeRepository{deployment: Deployment{ID: "d", Policy: Policy{Enabled: true, MinReplicas: 1, MaxReplicas: 2, QueueThreshold: 1, ScaleUpIntervals: 1, ScaleDownIntervals: 1}, State: State{Replicas: 1}}}
	fleet := &fakeFleet{}
	err := (Controller{Repository: repo, Signals: fakeSignals{}, Fleet: fleet, Now: func() time.Time { return time.Unix(10, 0) }}).Once(context.Background())
	if err != nil || fleet.replicas != 2 || repo.decision.Action != "scale_up" || repo.saved.Replicas != 2 {
		t.Fatalf("controller result: %v %#v %#v", err, repo.decision, repo.saved)
	}
}
