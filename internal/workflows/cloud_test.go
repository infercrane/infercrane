package workflows

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/provision"
)

type fakeCloudStore struct {
	deployment  domain.Deployment
	replica     domain.Replica
	checkpoints []string
	target      domain.Target
	deleted     bool
}

func (f *fakeCloudStore) ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error) {
	return domain.ResolvedDeployment{Deployment: f.deployment}, nil
}
func (f *fakeCloudStore) EnsureReplicaIntent(_ context.Context, replica domain.Replica) (domain.Replica, bool, error) {
	if f.replica.ID != "" {
		return f.replica, false, nil
	}
	replica.ID, replica.LifecycleState = "replica-1", "pending"
	f.replica = replica
	return replica, true, nil
}
func (f *fakeCloudStore) ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error) {
	if f.replica.ID == "" {
		return nil, nil
	}
	return []domain.Replica{f.replica}, nil
}
func (f *fakeCloudStore) SetReplicaProviderIdentity(_ context.Context, _, requestID, resourceID string) error {
	if f.replica.ProviderResourceID != "" && f.replica.ProviderResourceID != resourceID {
		return domain.ErrConflict
	}
	if requestID != "" {
		f.replica.ProviderRequestID = requestID
	}
	f.replica.ProviderResourceID = resourceID
	return nil
}
func (f *fakeCloudStore) ObserveReplica(_ context.Context, _, lifecycle, endpoint, health, details string, observed time.Time) error {
	f.replica.LifecycleState, f.replica.Endpoint, f.replica.Health, f.replica.ProviderDetails = lifecycle, endpoint, health, details
	f.replica.LastObservedAt = &observed
	return nil
}
func (f *fakeCloudStore) MarkReplicaDeleted(context.Context, string) error {
	f.replica.LifecycleState = "deleted"
	return nil
}
func (f *fakeCloudStore) CheckpointClaimedOperation(_ context.Context, _, _ string, _ int64, step, _, _ string, _ int, _ string) error {
	f.checkpoints = append(f.checkpoints, step)
	return nil
}
func (f *fakeCloudStore) AddTargetForTenant(_ context.Context, _ string, target domain.Target) (domain.Target, error) {
	target.ID = "target-1"
	f.target = target
	return target, nil
}
func (f *fakeCloudStore) UpdateProvisionedTarget(_ context.Context, _, resourceID, details string) error {
	f.target.ProviderResourceID, f.target.ProviderDetails = resourceID, details
	return nil
}
func (f *fakeCloudStore) ApplyDeploymentForTenant(_ context.Context, _ string, deployment domain.Deployment, targets []string) (domain.Deployment, error) {
	if len(targets) != 1 || f.target.Name != targets[0] {
		return domain.Deployment{}, errors.New("target was not registered")
	}
	deployment.ID = f.deployment.ID
	return deployment, nil
}
func (f *fakeCloudStore) DeleteDeploymentForTenant(context.Context, string, string) error {
	f.deleted = true
	return nil
}
func (f *fakeCloudStore) Audit(context.Context, domain.AuditEvent) error { return nil }

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
