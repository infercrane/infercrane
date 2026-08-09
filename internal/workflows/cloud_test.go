package workflows

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/provision"
)

type fakeCloudStore struct {
	deployment  domain.Deployment
	replica     domain.Replica
	replicas    map[string]domain.Replica
	checkpoints []string
	target      domain.Target
	targetNames []string
	deleted     bool
}

func (f *fakeCloudStore) ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error) {
	return domain.ResolvedDeployment{Deployment: f.deployment}, nil
}
func (f *fakeCloudStore) EnsureReplicaIntent(_ context.Context, replica domain.Replica) (domain.Replica, bool, error) {
	if f.replicas == nil {
		f.replicas = map[string]domain.Replica{}
		if f.replica.ID != "" {
			f.replicas[f.replica.ID] = f.replica
		}
	}
	for _, existing := range f.replicas {
		if existing.ExternalKey == replica.ExternalKey {
			f.replica = existing
			return existing, false, nil
		}
	}
	replica.ID, replica.LifecycleState = fmt.Sprintf("replica-%d", len(f.replicas)+1), "pending"
	f.replicas[replica.ID] = replica
	f.replica = replica
	return replica, true, nil
}
func (f *fakeCloudStore) ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error) {
	if len(f.replicas) > 0 {
		out := make([]domain.Replica, 0, len(f.replicas))
		for _, replica := range f.replicas {
			out = append(out, replica)
		}
		return out, nil
	}
	if f.replica.ID != "" {
		return []domain.Replica{f.replica}, nil
	}
	return nil, nil
}
func (f *fakeCloudStore) SetReplicaProviderIdentity(_ context.Context, id, requestID, resourceID string) error {
	if f.replica.ProviderResourceID != "" && f.replica.ProviderResourceID != resourceID {
		return domain.ErrConflict
	}
	if requestID != "" {
		f.replica.ProviderRequestID = requestID
	}
	f.replica.ProviderResourceID = resourceID
	if f.replicas != nil {
		f.replicas[id] = f.replica
	}
	return nil
}
func (f *fakeCloudStore) ObserveReplica(_ context.Context, id, lifecycle, endpoint, health, details string, observed time.Time) error {
	if replica, ok := f.replicas[id]; ok {
		f.replica = replica
	}
	f.replica.LifecycleState, f.replica.Endpoint, f.replica.Health, f.replica.ProviderDetails = lifecycle, endpoint, health, details
	f.replica.LastObservedAt = &observed
	if f.replicas != nil {
		f.replicas[id] = f.replica
	}
	return nil
}
func (f *fakeCloudStore) MarkReplicaDeleted(_ context.Context, id string) error {
	if replica, ok := f.replicas[id]; ok {
		f.replica = replica
	}
	f.replica.LifecycleState = "deleted"
	if f.replicas != nil {
		f.replicas[id] = f.replica
	}
	return nil
}
func (f *fakeCloudStore) CheckpointClaimedOperation(_ context.Context, _, _ string, _ int64, step, _, _ string, _ int, _ string) error {
	f.checkpoints = append(f.checkpoints, step)
	return nil
}
func (f *fakeCloudStore) AddTargetForTenant(_ context.Context, _ string, target domain.Target) (domain.Target, error) {
	target.ID = fmt.Sprintf("target-%d", len(f.targetNames)+1)
	f.target = target
	f.targetNames = append(f.targetNames, target.Name)
	return target, nil
}
func (f *fakeCloudStore) UpdateProvisionedTarget(_ context.Context, _, resourceID, details string) error {
	f.target.ProviderResourceID, f.target.ProviderDetails = resourceID, details
	return nil
}
func (f *fakeCloudStore) ApplyDeploymentForTenant(_ context.Context, _ string, deployment domain.Deployment, targets []string) (domain.Deployment, error) {
	if len(targets) != len(f.targetNames) {
		return domain.Deployment{}, errors.New("target was not registered")
	}
	deployment.ID = f.deployment.ID
	return deployment, nil
}

func TestConvergeCreatesExactlyOneResourcePerMinimumReplica(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", Model: "Qwen/Qwen3-8B", RoutingStrategy: "round-robin", MinReplicas: 2, MaxReplicas: 4}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready", Endpoint: "http://gpu:8000", Details: `{}`}}
	operation := domain.Operation{ID: "operation-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global","min_replicas":2,"max_replicas":4}`}
	_, err := CloudHandlers(store, provider, fakeInspector{ready: true})[ConvergeKind](context.Background(), operation)
	if err != nil || provider.ensureCalls != 2 || len(store.replicas) != 2 || len(store.targetNames) != 2 {
		t.Fatalf("ensure_calls=%d replicas=%d targets=%v err=%v", provider.ensureCalls, len(store.replicas), store.targetNames, err)
	}
}

func TestScaleDownWithdrawsRouterBeforeDeletingReplica(t *testing.T) {
	replicas := map[string]domain.Replica{
		"replica-1": {ID: "replica-1", DeploymentID: "deployment-1", ExternalKey: "deployment-1-r0", Ordinal: 0, LifecycleState: "active", ProviderResourceID: "infercrane-deployment-1-r0"},
		"replica-2": {ID: "replica-2", DeploymentID: "deployment-1", ExternalKey: "deployment-1-r1", Ordinal: 1, LifecycleState: "active", ProviderResourceID: "infercrane-deployment-1-r1"},
	}
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", Model: "Qwen/Qwen3-8B", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 2}, replicas: replicas}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready", Endpoint: "http://gpu:8000", Details: `{}`}}
	operation := domain.Operation{ID: "operation-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global","desired_replicas":1}`}
	_, err := CloudHandlers(store, provider, fakeInspector{ready: true})[ReplicaDeleteKind](context.Background(), operation)
	if err != nil || provider.deleteCalls != 1 || store.replicas["replica-2"].LifecycleState != "deleted" {
		t.Fatalf("delete_calls=%d replica=%#v err=%v", provider.deleteCalls, store.replicas["replica-2"], err)
	}
}
func (f *fakeCloudStore) DeleteDeploymentForTenant(context.Context, string, string) error {
	f.deleted = true
	return nil
}
func (f *fakeCloudStore) RoutingGenerationMatches(context.Context, string, string) (bool, error) {
	return true, nil
}
func (f *fakeCloudStore) DeleteProvisionedTarget(context.Context, string, string) error { return nil }
func (f *fakeCloudStore) Audit(context.Context, domain.AuditEvent) error                { return nil }

type fakeReplicaProvider struct {
	observation provision.Observation
	ensureCalls int
	deleteCalls int
}

func (f *fakeReplicaProvider) Handle(key string) provision.ProviderHandle {
	return provision.ProviderHandle{ExternalKey: key, ResourceID: "infercrane-" + key}
}
func (f *fakeReplicaProvider) EnsureReplica(_ context.Context, spec provision.ReplicaSpec) (provision.ProviderHandle, error) {
	f.ensureCalls++
	return provision.ProviderHandle{ExternalKey: spec.ExternalKey, ResourceID: "infercrane-" + spec.ExternalKey, RequestID: "request-1"}, nil
}
func (f *fakeReplicaProvider) ObserveReplica(context.Context, provision.ProviderHandle, int) (provision.Observation, error) {
	return f.observation, nil
}
func (f *fakeReplicaProvider) DeleteReplica(context.Context, provision.ProviderHandle) error {
	f.deleteCalls++
	f.observation = provision.Observation{}
	return nil
}

type fakeInspector struct{ ready bool }

func (f fakeInspector) Inspect(context.Context, string) (bool, map[string]struct{}) {
	if !f.ready {
		return false, nil
	}
	return true, map[string]struct{}{"Qwen/Qwen3-8B": {}}
}

func TestConvergeResumesAfterProviderCheckpoint(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", Model: "Qwen/Qwen3-8B", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "starting"}}
	operation := domain.Operation{ID: "operation-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global"}`}
	handler := CloudHandlers(store, provider, fakeInspector{ready: true})[ConvergeKind]
	if _, err := handler(context.Background(), operation); err == nil {
		t.Fatal("starting replica should return retryable wait")
	}
	if store.replica.ProviderResourceID == "" || provider.ensureCalls != 1 {
		t.Fatalf("identity=%q ensure_calls=%d", store.replica.ProviderResourceID, provider.ensureCalls)
	}

	// Simulate process restart: a fresh handler receives only durable store and
	// provider state, reuses the same external identity, and finishes.
	provider.observation = provision.Observation{Exists: true, State: "ready", Endpoint: "http://gpu:8000", Details: `{}`}
	result, err := CloudHandlers(store, provider, fakeInspector{ready: true})[ConvergeKind](context.Background(), operation)
	if err != nil || result == "" || provider.ensureCalls != 2 || store.replica.LifecycleState != "active" || store.target.URL != "http://gpu:8000" {
		t.Fatalf("result=%s replica=%#v target=%#v ensure_calls=%d err=%v", result, store.replica, store.target, provider.ensureCalls, err)
	}
}

func TestConvergeCancellationDeletesProviderResource(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen"}, replica: domain.Replica{ID: "replica-1", DeploymentID: "deployment-1", ExternalKey: "deployment-1-r0", ProviderResourceID: "infercrane-deployment-1-r0", LifecycleState: "starting"}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "starting"}}
	operation := domain.Operation{ID: "operation-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global"}`}
	_, err := CloudHandlers(store, provider, fakeInspector{})[ConvergeKind+".cancel"](context.Background(), operation)
	if err != nil || provider.deleteCalls != 1 || store.replica.LifecycleState != "deleted" || !store.deleted {
		t.Fatalf("delete_calls=%d replica=%#v deleted=%t err=%v", provider.deleteCalls, store.replica, store.deleted, err)
	}
}
