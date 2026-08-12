package autoscale

import (
	"context"
	"errors"
	"strings"
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

type selectiveSignals struct{}

func (selectiveSignals) Signals(_ context.Context, deploymentID string) (Signals, error) {
	if deploymentID == "broken" {
		return Signals{}, errors.New("metrics unavailable")
	}
	return Signals{Waiting: 4}, nil
}

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

type multiRepository struct {
	deployments []Deployment
	decisions   map[string]Decision
}

func (f *multiRepository) AutoscalingDeployments(context.Context) ([]Deployment, error) {
	return f.deployments, nil
}
func (f *multiRepository) RecordDecision(_ context.Context, id string, d Decision, _ string) error {
	if f.decisions == nil {
		f.decisions = map[string]Decision{}
	}
	f.decisions[id] = d
	return nil
}
func (f *multiRepository) SaveState(context.Context, string, State) error { return nil }

type multiFleet struct{ scaled map[string]int }

func (f *multiFleet) ScaleTo(_ context.Context, id string, replicas int) error {
	if f.scaled == nil {
		f.scaled = map[string]int{}
	}
	f.scaled[id] = replicas
	return nil
}

func TestControllerIsolatesDeploymentSignalFailure(t *testing.T) {
	policy := Policy{Enabled: true, MinReplicas: 1, MaxReplicas: 2, QueueThreshold: 1, ScaleUpIntervals: 1, ScaleDownIntervals: 1}
	repo := &multiRepository{deployments: []Deployment{
		{ID: "broken", Policy: policy, State: State{Replicas: 1}},
		{ID: "healthy", Policy: policy, State: State{Replicas: 1}},
	}}
	fleet := &multiFleet{}
	err := (Controller{Repository: repo, Signals: selectiveSignals{}, Fleet: fleet}).Once(context.Background())
	if err == nil || !strings.Contains(err.Error(), "signals for broken") || fleet.scaled["healthy"] != 2 {
		t.Fatalf("expected isolated metrics error and healthy deployment scale-up, err=%v scaled=%v", err, fleet.scaled)
	}
}
