package optimizationcampaign

import (
	"context"
	"errors"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

type chatTargetStore struct {
	deployments map[string]domain.Deployment
	replicas    []domain.Replica
	resolveErr  error
}

func (s chatTargetStore) ResolveForTenant(_ context.Context, tenant, name string) (domain.ResolvedDeployment, error) {
	if s.resolveErr != nil {
		return domain.ResolvedDeployment{}, s.resolveErr
	}
	deployment, ok := s.deployments[tenant+"/"+name]
	if !ok {
		return domain.ResolvedDeployment{}, domain.ErrNotFound
	}
	return domain.ResolvedDeployment{Deployment: deployment}, nil
}

func (s chatTargetStore) ReplicasForDeployment(_ context.Context, tenant, deploymentID string) ([]domain.Replica, error) {
	var rows []domain.Replica
	for _, replica := range s.replicas {
		if replica.TenantID == tenant && replica.DeploymentID == deploymentID {
			rows = append(rows, replica)
		}
	}
	return rows, nil
}

func decidedCandidate(state string) domain.OptimizationCandidateRun {
	return domain.OptimizationCandidateRun{ID: "candidate-1", TenantID: "tenant-a", State: state, DeploymentName: "support-opt-1", RevisionID: "rev-2", DeploymentSpecJSON: `{"name":"support","model":{"id":"acme/model"},"runtime":{"engine":"vllm"}}`}
}

func TestCandidateChatTargetAddressesTheExactDecidedRevision(t *testing.T) {
	store := chatTargetStore{
		deployments: map[string]domain.Deployment{"tenant-a/support-opt-1": {ID: "dep-1", Name: "support-opt-1"}},
		replicas: []domain.Replica{
			{TenantID: "tenant-a", DeploymentID: "dep-1", RevisionID: "rev-1", Endpoint: "http://old", Provider: "runpod-pods", Health: "healthy", LifecycleState: "active", Ordinal: 0},
			{TenantID: "tenant-a", DeploymentID: "dep-1", RevisionID: "rev-2", Endpoint: "http://unhealthy", Provider: "runpod-pods", Health: "unhealthy", LifecycleState: "active", Ordinal: 0},
			{TenantID: "tenant-a", DeploymentID: "dep-1", RevisionID: "rev-2", Endpoint: "http://second", Provider: "runpod-pods", Health: "healthy", LifecycleState: "ready", Ordinal: 2},
			{TenantID: "tenant-a", DeploymentID: "dep-1", RevisionID: "rev-2", Endpoint: "http://first", Provider: "runpod-pods", Health: "healthy", LifecycleState: "active", Ordinal: 1},
			{TenantID: "tenant-b", DeploymentID: "dep-1", RevisionID: "rev-2", Endpoint: "http://foreign", Provider: "runpod-pods", Health: "healthy", LifecycleState: "active", Ordinal: 0},
		},
	}
	backends := map[string]BenchmarkBackend{"runpod-pods": {APIKey: "worker-key"}}
	for _, state := range []string{CandidateQualified, CandidateGuardPassed, CandidatePromoted, CandidateObserved} {
		target, err := CandidateChatTarget(context.Background(), store, backends, decidedCandidate(state))
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		want := ChatTarget{Endpoint: "http://first", APIKey: "worker-key", Model: "acme/model", DeploymentName: "support-opt-1", RevisionID: "rev-2"}
		if target != want {
			t.Fatalf("%s: target=%+v", state, target)
		}
	}
}

func TestCandidateChatTargetRefusesWhatCannotAnswerSafely(t *testing.T) {
	store := chatTargetStore{deployments: map[string]domain.Deployment{"tenant-a/support-opt-1": {ID: "dep-1"}}, replicas: []domain.Replica{{TenantID: "tenant-a", DeploymentID: "dep-1", RevisionID: "rev-2", Endpoint: "http://first", Provider: "runpod-pods", Health: "healthy", LifecycleState: "active"}}}
	backends := map[string]BenchmarkBackend{"runpod-pods": {APIKey: "worker-key"}}
	// A prompt beside a measurement would disturb it.
	for _, state := range []string{CandidateProposed, CandidateProvisioning, CandidateReady, CandidateMeasuring, CandidateValidating, CandidateRanked, CandidateGuarding} {
		if _, err := CandidateChatTarget(context.Background(), store, backends, decidedCandidate(state)); !errors.Is(err, ErrCandidateUndecided) {
			t.Errorf("%s: err=%v, want undecided", state, err)
		}
	}
	for _, state := range []string{CandidateRejected, CandidateInconclusive, CandidateFailed, CandidateCancelled, CandidateCleaned} {
		if _, err := CandidateChatTarget(context.Background(), store, backends, decidedCandidate(state)); !errors.Is(err, ErrCandidateStopped) {
			t.Errorf("%s: err=%v, want stopped", state, err)
		}
	}
	missing := decidedCandidate(CandidateQualified)
	missing.DeploymentName = "deleted"
	if _, err := CandidateChatTarget(context.Background(), store, backends, missing); !errors.Is(err, ErrCandidateStopped) {
		t.Errorf("deleted deployment: err=%v", err)
	}
	unprovisioned := decidedCandidate(CandidateQualified)
	unprovisioned.RevisionID = ""
	if _, err := CandidateChatTarget(context.Background(), store, backends, unprovisioned); !errors.Is(err, ErrCandidateUnreachable) {
		t.Errorf("no revision: err=%v", err)
	}
	foreign := decidedCandidate(CandidateQualified)
	foreign.TenantID = "tenant-b"
	if _, err := CandidateChatTarget(context.Background(), store, backends, foreign); !errors.Is(err, ErrCandidateStopped) {
		t.Errorf("another tenant's deployment: err=%v", err)
	}
	if _, err := CandidateChatTarget(context.Background(), store, map[string]BenchmarkBackend{}, decidedCandidate(CandidateQualified)); !errors.Is(err, ErrCandidateUnreachable) {
		t.Errorf("no credential: err=%v", err)
	}
	failing := store
	failing.resolveErr = errors.New("database unavailable")
	if _, err := CandidateChatTarget(context.Background(), failing, backends, decidedCandidate(CandidateQualified)); err == nil || errors.Is(err, ErrCandidateStopped) || errors.Is(err, ErrCandidateUnreachable) {
		t.Errorf("store failure must not read as a candidate state: err=%v", err)
	}
}
