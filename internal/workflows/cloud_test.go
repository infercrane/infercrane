package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/provision"
)

type fakeCloudStore struct {
	deployment     domain.Deployment
	replica        domain.Replica
	replicas       map[string]domain.Replica
	checkpoints    []string
	target         domain.Target
	targetNames    []string
	deleted        bool
	revision       domain.DeploymentRevision
	applied        bool
	rejectedReason string
	capacity       domain.CapacityEvidence
}

func (f *fakeCloudStore) RecordCapacityEvidence(_ context.Context, row domain.CapacityEvidence) (domain.CapacityEvidence, error) {
	f.capacity = row
	return row, nil
}

func testElasticProfile(adapter, cloud string) integration.ProviderProfile {
	return integration.ProviderProfile{Adapter: adapter, Cloud: cloud, ContractVersion: integration.ProviderContractV1, AdapterVersion: "test", Modes: []integration.ComputeMode{integration.ElasticMode}, Qualification: []integration.Qualification{{State: integration.QualificationSimulated, Environment: "hermetic"}}}
}

func testRuntimeBackends(t *testing.T, inspector integration.RuntimeInspector) integration.RuntimeBackends {
	t.Helper()
	backends, err := integration.NewRuntimeBackends(integration.RuntimeBackend{
		Profile:   integration.RuntimeProfile{Runtime: "vllm", ContractVersion: integration.RuntimeContractV1, AdapterVersion: "test", Protocol: "openai", Qualification: []integration.Qualification{{State: integration.QualificationSimulated, Environment: "hermetic"}}},
		Inspector: inspector,
	})
	if err != nil {
		t.Fatal(err)
	}
	return backends
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
	f.applied = true
	return deployment, nil
}

func TestConvergeCreatesExactlyOneResourcePerMinimumReplica(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", Model: "Qwen/Qwen3-8B", RoutingStrategy: "round-robin", MinReplicas: 2, MaxReplicas: 4}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready", Endpoint: "http://gpu:8000", Details: `{}`}}
	operation := domain.Operation{ID: "operation-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global","min_replicas":2,"max_replicas":4}`}
	_, err := QualifiedV01CloudHandlers(store, provider, fakeInspector{ready: true})[ConvergeKind](context.Background(), operation)
	if err != nil || provider.ensureCalls != 2 || len(store.replicas) != 2 || len(store.targetNames) != 2 {
		t.Fatalf("ensure_calls=%d replicas=%d targets=%v err=%v", provider.ensureCalls, len(store.replicas), store.targetNames, err)
	}
}

func TestConvergeRejectsProviderWithoutMatchingRuntimeBackend(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", Model: "Qwen/Qwen3-8B", MinReplicas: 1, MaxReplicas: 1}}
	provider := &fakeReplicaProvider{}
	backends, err := NewReplicaBackends(ReplicaBackend{Name: "fixture", Cloud: "runpod", Runtime: "vllm", Profile: testElasticProfile("fixture", "runpod"), Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	operation := domain.Operation{RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","runtime":"vllm","tenant_id":"global","min_replicas":1,"max_replicas":1}`}
	emptyRuntimeBackends, runtimeErr := integration.NewRuntimeBackends()
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	_, err = CloudHandlersWithBackends(store, backends, emptyRuntimeBackends)[ConvergeKind](context.Background(), operation)
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Code != "runtime_backend_unavailable" || provider.ensureCalls != 0 {
		t.Fatalf("failure=%+v ensure_calls=%d err=%v", failure, provider.ensureCalls, err)
	}
}

func TestCandidateProvisioningCreatesIsolatedRevisionCapacity(t *testing.T) {
	spec := domain.DeploymentRevisionSpec{Model: "Qwen/Qwen3-8B", Runtime: "vllm", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1, ComputeMode: "elastic", Cloud: "runpod", GPU: "L40S"}
	encoded, _ := json.Marshal(spec)
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", ActiveRevisionID: "rev-1"}, revision: domain.DeploymentRevision{ID: "rev-2", Status: "candidate", SpecJSON: string(encoded)}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready", Endpoint: "http://candidate:8000", Details: `{}`}}
	operation := domain.Operation{ID: "operation-candidate", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"name":"qwen","candidate_id":"rev-2","tenant_id":"global"}`}
	result, err := QualifiedV01CloudHandlers(store, provider, fakeInspector{ready: true})[RolloutProvisionKind](context.Background(), operation)
	if err != nil || result == "" || provider.ensureCalls != 1 || store.replica.RevisionID != "rev-2" || store.replica.LifecycleState != "ready" || store.applied {
		t.Fatalf("result=%s replica=%#v applied=%t ensure_calls=%d err=%v", result, store.replica, store.applied, provider.ensureCalls, err)
	}
	if store.replica.ExternalKey != "deployment-1-rev-2-r0" || store.target.Name != "qwen-rev-2-r0" {
		t.Fatalf("replica=%#v target=%#v", store.replica, store.target)
	}
}

func TestBadCandidateIsRejectedAndCapacityIsDeleted(t *testing.T) {
	spec := domain.DeploymentRevisionSpec{Model: "Bad/Model", Runtime: "vllm", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1, ComputeMode: "elastic", Cloud: "runpod", GPU: "L40S"}
	encoded, _ := json.Marshal(spec)
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", ActiveRevisionID: "rev-1", CandidateRevisionID: "rev-bad"}, revision: domain.DeploymentRevision{ID: "rev-bad", Status: "candidate", SpecJSON: string(encoded)}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready", Endpoint: "http://bad:8000", Details: `{}`}}
	operation := domain.Operation{ID: "operation-bad", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"name":"qwen","candidate_id":"rev-bad","tenant_id":"global"}`}
	_, err := QualifiedV01CloudHandlers(store, provider, fakeInspector{ready: true})[RolloutProvisionKind](context.Background(), operation)
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Code != "runtime_model_mismatch" || failure.Retryable || provider.deleteCalls != 1 || store.replica.LifecycleState != "deleted" || store.revision.Status != "rejected" || store.rejectedReason == "" || store.deployment.ActiveRevisionID != "rev-1" {
		t.Fatalf("failure=%+v delete_calls=%d replica=%#v revision=%#v reason=%q active=%s err=%v", failure, provider.deleteCalls, store.replica, store.revision, store.rejectedReason, store.deployment.ActiveRevisionID, err)
	}
}

func TestRejectedCandidateProvisionRetryOnlyResumesCleanup(t *testing.T) {
	store := &fakeCloudStore{
		deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", ActiveRevisionID: "rev-1"},
		revision:   domain.DeploymentRevision{ID: "rev-bad", Status: "rejected", Reason: "candidate validation failed"},
		replicas: map[string]domain.Replica{
			"candidate": {ID: "candidate", DeploymentID: "deployment-1", RevisionID: "rev-bad", ExternalKey: "deployment-1-rev-bad-r0", LifecycleState: "failed", ProviderResourceID: "bad-resource"},
		},
	}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready"}}
	operation := domain.Operation{ID: "operation-bad", LeaseOwner: "worker", LeaseGeneration: 2, RequestJSON: `{"name":"qwen","candidate_id":"rev-bad","tenant_id":"global"}`}
	_, err := QualifiedV01CloudHandlers(store, provider, fakeInspector{ready: true})[RolloutProvisionKind](context.Background(), operation)
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Code != "candidate_rejected" || failure.Retryable || provider.ensureCalls != 0 || provider.deleteCalls != 1 || store.replicas["candidate"].LifecycleState != "deleted" {
		t.Fatalf("failure=%+v ensure_calls=%d delete_calls=%d replicas=%#v err=%v", failure, provider.ensureCalls, provider.deleteCalls, store.replicas, err)
	}
}

func TestCandidateCancellationDeletesOnlyCandidateCapacity(t *testing.T) {
	replicas := map[string]domain.Replica{
		"active":    {ID: "active", DeploymentID: "deployment-1", RevisionID: "rev-1", ExternalKey: "deployment-1-r0", LifecycleState: "active", ProviderResourceID: "active-resource"},
		"candidate": {ID: "candidate", DeploymentID: "deployment-1", RevisionID: "rev-2", ExternalKey: "deployment-1-rev-2-r0", LifecycleState: "ready", ProviderResourceID: "candidate-resource"},
	}
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", ActiveRevisionID: "rev-1", CandidateRevisionID: "rev-2"}, replicas: replicas}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready"}}
	operation := domain.Operation{RequestJSON: `{"name":"qwen","candidate_id":"rev-2","tenant_id":"global"}`}
	_, err := QualifiedV01CloudHandlers(store, provider, fakeInspector{})[RolloutProvisionKind+".cancel"](context.Background(), operation)
	if err != nil || provider.deleteCalls != 1 || store.replicas["candidate"].LifecycleState != "deleted" || store.replicas["active"].LifecycleState != "active" || store.deleted {
		t.Fatalf("replicas=%#v delete_calls=%d deployment_deleted=%t err=%v", store.replicas, provider.deleteCalls, store.deleted, err)
	}
}

func TestGuardedPromotionCutsOverBeforeDeletingOldCapacity(t *testing.T) {
	spec := domain.DeploymentRevisionSpec{Model: "Qwen/Qwen3-8B", Runtime: "vllm", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1, ComputeMode: "elastic", Cloud: "runpod", GPU: "H100"}
	encoded, _ := json.Marshal(spec)
	replicas := map[string]domain.Replica{
		"old": {ID: "old", DeploymentID: "deployment-1", RevisionID: "rev-1", ExternalKey: "deployment-1-r0", LifecycleState: "active", ProviderResourceID: "old-resource", Endpoint: "http://old:8000", Health: "healthy"},
		"new": {ID: "new", DeploymentID: "deployment-1", RevisionID: "rev-2", ExternalKey: "deployment-1-rev-2-r0", LifecycleState: "ready", ProviderResourceID: "new-resource", Endpoint: "http://new:8000", Health: "healthy"},
	}
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", ActiveRevisionID: "rev-1", CandidateRevisionID: "rev-2"}, revision: domain.DeploymentRevision{ID: "rev-2", Status: "candidate", SpecJSON: string(encoded)}, replicas: replicas}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready"}}
	operation := domain.Operation{ID: "promote", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"name":"qwen","candidate_id":"rev-2","tenant_id":"global"}`}
	result, err := QualifiedV01CloudHandlers(store, provider, fakeInspector{})[RolloutPromoteKind](context.Background(), operation)
	if err != nil || result == "" || store.deployment.ActiveRevisionID != "rev-2" || store.replicas["new"].LifecycleState != "active" || store.replicas["old"].LifecycleState != "deleted" || provider.deleteCalls != 1 {
		t.Fatalf("result=%s deployment=%#v replicas=%#v delete_calls=%d err=%v", result, store.deployment, store.replicas, provider.deleteCalls, err)
	}
}

type fakeDrainTracker struct{ active int }

func (f *fakeDrainTracker) RetiringInFlight(string) int      { return f.active }
func (f *fakeDrainTracker) HasCurrentDeployment(string) bool { return false }

func TestGuardedPromotionWaitsForRetiringRequests(t *testing.T) {
	spec := domain.DeploymentRevisionSpec{Model: "Qwen/Qwen3-8B", Runtime: "vllm", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1, ComputeMode: "elastic", Cloud: "runpod", GPU: "H100"}
	encoded, _ := json.Marshal(spec)
	replicas := map[string]domain.Replica{
		"old": {ID: "old", DeploymentID: "deployment-1", RevisionID: "rev-1", ExternalKey: "deployment-1-r0", LifecycleState: "active", ProviderResourceID: "old-resource", Endpoint: "http://old:8000", Health: "healthy"},
		"new": {ID: "new", DeploymentID: "deployment-1", RevisionID: "rev-2", ExternalKey: "deployment-1-rev-2-r0", LifecycleState: "ready", ProviderResourceID: "new-resource", Endpoint: "http://new:8000", Health: "healthy"},
	}
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", ActiveRevisionID: "rev-1", CandidateRevisionID: "rev-2"}, revision: domain.DeploymentRevision{ID: "rev-2", Status: "candidate", SpecJSON: string(encoded)}, replicas: replicas}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready"}}
	backends, err := NewReplicaBackends(ReplicaBackend{Name: "skypilot", Cloud: "runpod", Runtime: "vllm", Profile: testElasticProfile("skypilot", "runpod"), Provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	drain := &fakeDrainTracker{active: 1}
	handler := CloudHandlersWithBackendsAndDrain(store, backends, testRuntimeBackends(t, fakeInspector{}), drain)[RolloutPromoteKind]
	operation := domain.Operation{ID: "promote", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"name":"qwen","candidate_id":"rev-2","tenant_id":"global"}`}
	_, err = handler(context.Background(), operation)
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Code != "active_requests_draining" || !failure.Retryable || provider.deleteCalls != 0 {
		t.Fatalf("failure=%+v deletes=%d err=%v", failure, provider.deleteCalls, err)
	}
	drain.active = 0
	if _, err = handler(context.Background(), operation); err != nil || provider.deleteCalls != 1 {
		t.Fatalf("deletes=%d err=%v", provider.deleteCalls, err)
	}
}

type fakeReleaseV2Store struct {
	*fakeCloudStore
	monitor      domain.ReleaseGuardMonitor
	evaluation   domain.ReleaseGuardEvaluation
	rolledBack   bool
	routeMatches []bool
}

func (f *fakeReleaseV2Store) RoutingGenerationMatches(context.Context, string, string) (bool, error) {
	if len(f.routeMatches) == 0 {
		return true, nil
	}
	value := f.routeMatches[0]
	f.routeMatches = f.routeMatches[1:]
	return value, nil
}

func (f *fakeReleaseV2Store) ReleaseGuardPolicy(context.Context, string, string) (domain.ReleaseGuardPolicy, error) {
	return domain.ReleaseGuardPolicy{Enabled: true, AutoRollbackEnabled: true, AutoRollbackWindowSeconds: 300}, nil
}
func (f *fakeReleaseV2Store) EnsureReleaseGuardMonitor(_ context.Context, _, _, promoted, rollback string, _ time.Duration) (domain.ReleaseGuardMonitor, error) {
	if f.monitor.ID == "" {
		f.monitor = domain.ReleaseGuardMonitor{ID: "monitor", PromotedRevisionID: promoted, RollbackRevisionID: rollback, Status: "observing", Deadline: time.Now().Add(time.Minute)}
	}
	return f.monitor, nil
}
func (f *fakeReleaseV2Store) ReleaseGuardMonitor(context.Context, string, string, string) (domain.ReleaseGuardMonitor, error) {
	if f.monitor.ID == "" {
		return domain.ReleaseGuardMonitor{}, domain.ErrNotFound
	}
	return f.monitor, nil
}
func (f *fakeReleaseV2Store) EvaluateReleaseGuardMonitor(context.Context, string, string, string, time.Duration) (domain.ReleaseGuardEvaluation, error) {
	return f.evaluation, nil
}
func (f *fakeReleaseV2Store) RollbackGuardedPromotion(_ context.Context, _, _, promoted, rollback, _ string, _ []string) error {
	f.deployment.ActiveRevisionID = rollback
	if f.revision.ID == promoted {
		f.revision.Status = "superseded"
	}
	for id, replica := range f.replicas {
		if replica.RevisionID == rollback {
			replica.LifecycleState = "active"
		}
		if replica.RevisionID == promoted {
			replica.LifecycleState = "draining"
		}
		f.replicas[id] = replica
	}
	f.rolledBack = true
	return nil
}
func (f *fakeReleaseV2Store) MarkReleaseGuardMonitorRolledBack(context.Context, string, string, string) error {
	f.monitor.Status = "rolled_back"
	return nil
}
func (f *fakeReleaseV2Store) PreviousRevisionID(context.Context, string, string, string) (string, error) {
	return "rev-1", nil
}

func TestPostPromotionGuardAutomaticallyRollsBackBeforeDeletingRetainedCapacity(t *testing.T) {
	spec := domain.DeploymentRevisionSpec{Model: "Qwen/Qwen3-8B", Runtime: "vllm", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1, ComputeMode: "elastic", Cloud: "runpod", GPU: "H100"}
	encoded, _ := json.Marshal(spec)
	base := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", ActiveRevisionID: "rev-1", CandidateRevisionID: "rev-2"}, revision: domain.DeploymentRevision{ID: "rev-2", Status: "candidate", SpecJSON: string(encoded)}, replicas: map[string]domain.Replica{"old": {ID: "old", DeploymentID: "deployment-1", RevisionID: "rev-1", ExternalKey: "deployment-1-r0", LifecycleState: "active", ProviderResourceID: "old-resource", Endpoint: "http://old:8000", Health: "healthy"}, "new": {ID: "new", DeploymentID: "deployment-1", RevisionID: "rev-2", ExternalKey: "deployment-1-rev-2-r0", LifecycleState: "ready", ProviderResourceID: "new-resource", Endpoint: "http://new:8000", Health: "healthy"}}}
	store := &fakeReleaseV2Store{fakeCloudStore: base, evaluation: domain.ReleaseGuardEvaluation{ID: "evaluation", Decision: "REJECT"}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready"}}
	operation := domain.Operation{ID: "promote", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"name":"qwen","candidate_id":"rev-2","tenant_id":"global"}`}
	result, err := QualifiedV01CloudHandlers(store, provider, fakeInspector{})[RolloutPromoteKind](context.Background(), operation)
	if err != nil || !store.rolledBack || store.deployment.ActiveRevisionID != "rev-1" || store.replicas["old"].LifecycleState != "active" || store.replicas["new"].LifecycleState != "deleted" || provider.deleteCalls != 1 || !strings.Contains(result, `"auto_rolled_back":true`) {
		t.Fatalf("result=%s deployment=%#v replicas=%#v deletes=%d rollback=%t err=%v", result, store.deployment, store.replicas, provider.deleteCalls, store.rolledBack, err)
	}
}

func TestAutomaticRollbackResumesCleanupAfterRouterRestartBoundary(t *testing.T) {
	spec := domain.DeploymentRevisionSpec{Model: "model", Runtime: "vllm", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1, ComputeMode: "elastic", Cloud: "runpod", GPU: "H100"}
	encoded, _ := json.Marshal(spec)
	base := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "prod", ActiveRevisionID: "rev-1", CandidateRevisionID: "rev-2"}, revision: domain.DeploymentRevision{ID: "rev-2", Status: "candidate", SpecJSON: string(encoded)}, replicas: map[string]domain.Replica{"old": {ID: "old", DeploymentID: "deployment-1", RevisionID: "rev-1", ExternalKey: "old", Provider: "skypilot", LifecycleState: "active", Endpoint: "http://old", Health: "healthy"}, "new": {ID: "new", DeploymentID: "deployment-1", RevisionID: "rev-2", ExternalKey: "new", Provider: "skypilot", LifecycleState: "ready", Endpoint: "http://new", Health: "healthy"}}}
	store := &fakeReleaseV2Store{fakeCloudStore: base, evaluation: domain.ReleaseGuardEvaluation{Decision: "REJECT"}, routeMatches: []bool{true, false}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready"}}
	handler := QualifiedV01CloudHandlers(store, provider, fakeInspector{})[RolloutPromoteKind]
	op := domain.Operation{ID: "promote", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"name":"prod","candidate_id":"rev-2","tenant_id":"global"}`}
	if _, err := handler(context.Background(), op); err == nil || store.deployment.ActiveRevisionID != "rev-1" || provider.deleteCalls != 0 {
		t.Fatalf("first err=%v active=%s deletes=%d", err, store.deployment.ActiveRevisionID, provider.deleteCalls)
	}
	store.revision.Status = "superseded"
	store.routeMatches = []bool{true}
	result, err := handler(context.Background(), op)
	if err != nil || provider.deleteCalls != 1 || store.replicas["new"].LifecycleState != "deleted" || !strings.Contains(result, `"auto_rolled_back":true`) {
		t.Fatalf("result=%s err=%v deletes=%d replicas=%#v", result, err, provider.deleteCalls, store.replicas)
	}
}

func TestCanceledObservationResumesRollbackBeforeDeletingCandidate(t *testing.T) {
	spec := domain.DeploymentRevisionSpec{Model: "model", Runtime: "vllm", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1, ComputeMode: "elastic", Cloud: "runpod", GPU: "H100"}
	encoded, _ := json.Marshal(spec)
	base := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "prod", ActiveRevisionID: "rev-2"}, revision: domain.DeploymentRevision{ID: "rev-2", Status: "active", SpecJSON: string(encoded)}, replicas: map[string]domain.Replica{
		"old": {ID: "old", DeploymentID: "deployment-1", RevisionID: "rev-1", ExternalKey: "old", Provider: "skypilot", LifecycleState: "draining", Endpoint: "http://old", Health: "healthy"},
		"new": {ID: "new", DeploymentID: "deployment-1", RevisionID: "rev-2", ExternalKey: "new", Provider: "skypilot", LifecycleState: "active", Endpoint: "http://new", Health: "healthy"},
	}}
	store := &fakeReleaseV2Store{fakeCloudStore: base, monitor: domain.ReleaseGuardMonitor{ID: "monitor", PromotedRevisionID: "rev-2", RollbackRevisionID: "rev-1", Status: "observing"}, routeMatches: []bool{false}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready"}}
	handler := QualifiedV01CloudHandlers(store, provider, fakeInspector{})[RolloutPromoteKind+".cancel"]
	op := domain.Operation{ID: "promote", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"name":"prod","candidate_id":"rev-2","tenant_id":"global"}`}
	if _, err := handler(context.Background(), op); err == nil || store.deployment.ActiveRevisionID != "rev-1" || provider.deleteCalls != 0 {
		t.Fatalf("first err=%v active=%s deletes=%d", err, store.deployment.ActiveRevisionID, provider.deleteCalls)
	}
	store.routeMatches = []bool{true}
	result, err := handler(context.Background(), op)
	if err != nil || provider.deleteCalls != 1 || store.replicas["new"].LifecycleState != "deleted" || store.monitor.Status != "rolled_back" || !strings.Contains(result, `"auto_rolled_back":true`) {
		t.Fatalf("result=%s err=%v deletes=%d monitor=%#v replicas=%#v", result, err, provider.deleteCalls, store.monitor, store.replicas)
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
	_, err := QualifiedV01CloudHandlers(store, provider, fakeInspector{ready: true})[ReplicaDeleteKind](context.Background(), operation)
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
func (f *fakeCloudStore) DeleteProvisionedTarget(context.Context, string, string, string) error {
	return nil
}
func (f *fakeCloudStore) DeleteProvisionedTargetByURL(context.Context, string, string, string) error {
	return nil
}
func (f *fakeCloudStore) ModelArtifactForRevision(context.Context, string, string) (domain.ModelArtifact, error) {
	return domain.ModelArtifact{Repository: "Qwen/Qwen3-8B", ImmutableRevision: "0123456789abcdef0123456789abcdef01234567", ModelIdentity: "Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567", CacheState: "unknown"}, nil
}
func (f *fakeCloudStore) Revision(context.Context, string, string, string) (domain.DeploymentRevision, error) {
	if f.revision.ID == "" {
		return domain.DeploymentRevision{}, domain.ErrNotFound
	}
	return f.revision, nil
}

func (f *fakeCloudStore) RejectCandidateRevision(_ context.Context, _, _ string, candidateID, reason string) error {
	if f.revision.ID == candidateID {
		f.revision.Status = "rejected"
	}
	f.rejectedReason = reason
	return nil
}
func (f *fakeCloudStore) PromoteGuardedCandidate(_ context.Context, _, _ string, candidateID string, _ []string) error {
	f.deployment.ActiveRevisionID, f.deployment.CandidateRevisionID = candidateID, ""
	for id, replica := range f.replicas {
		if replica.RevisionID == candidateID {
			replica.LifecycleState = "active"
		} else if replica.LifecycleState == "active" {
			replica.LifecycleState = "draining"
		}
		f.replicas[id] = replica
	}
	return nil
}
func (f *fakeCloudStore) AttachModelArtifact(_ context.Context, _, _ string, artifact domain.ModelArtifact) (domain.ModelArtifact, error) {
	return artifact, nil
}
func (f *fakeCloudStore) Audit(context.Context, domain.AuditEvent) error { return nil }

type fakeReplicaProvider struct {
	observation provision.Observation
	ensureErr   error
	ensureCalls int
	deleteCalls int
}

func TestReplicaBackendsResolveByCloudRuntimeAndDurableName(t *testing.T) {
	first, second := &fakeReplicaProvider{}, &fakeReplicaProvider{}
	registry, err := NewReplicaBackends(
		ReplicaBackend{Name: "adapter-a", Cloud: "cloud-a", Runtime: "runtime-a", Profile: testElasticProfile("adapter-a", "cloud-a"), Provider: first},
		ReplicaBackend{Name: "adapter-b", Cloud: "cloud-a", Runtime: "runtime-b", Profile: testElasticProfile("adapter-b", "cloud-a"), Provider: second},
	)
	if err != nil {
		t.Fatal(err)
	}
	byCloud, err := registry.ForCloud("cloud-a", "runtime-b")
	if err != nil || byCloud.Provider != second {
		t.Fatalf("by cloud=%#v err=%v", byCloud, err)
	}
	byName, err := registry.ForProvider("adapter-a")
	if err != nil || byName.Provider != first {
		t.Fatalf("by name=%#v err=%v", byName, err)
	}
}

func TestReplicaBackendsAllowOneProviderAdapterToLaunchMultipleRuntimes(t *testing.T) {
	provider := &fakeReplicaProvider{}
	registry, err := NewReplicaBackends(
		ReplicaBackend{Name: "adapter-a", Cloud: "cloud-a", Runtime: "vllm", Profile: testElasticProfile("adapter-a", "cloud-a"), Provider: provider},
		ReplicaBackend{Name: "adapter-a", Cloud: "cloud-a", Runtime: "sglang", Profile: testElasticProfile("adapter-a", "cloud-a"), Provider: provider},
	)
	if err != nil {
		t.Fatal(err)
	}
	if backend, lookupErr := registry.ForCloud("cloud-a", "sglang"); lookupErr != nil || backend.Provider != provider {
		t.Fatalf("backend=%#v err=%v", backend, lookupErr)
	}
}

func TestReplicaBackendsRequireExplicitSelectionForAmbiguousRegistrations(t *testing.T) {
	provider := &fakeReplicaProvider{}
	registry, err := NewReplicaBackends(
		ReplicaBackend{Name: "adapter-a", Cloud: "cloud-a", Runtime: "runtime-a", Profile: testElasticProfile("adapter-a", "cloud-a"), Provider: provider},
		ReplicaBackend{Name: "adapter-b", Cloud: "cloud-a", Runtime: "runtime-a", Profile: testElasticProfile("adapter-b", "cloud-a"), Provider: provider},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registry.ForCloud("cloud-a", "runtime-a"); err == nil {
		t.Fatal("expected ambiguous lookup to require provider adapter")
	}
	selected, err := registry.ForAdapter("cloud-a", "runtime-a", "adapter-b")
	if err != nil || selected.Name != "adapter-b" {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
	_, err = NewReplicaBackends(
		ReplicaBackend{Name: "adapter-a", Cloud: "cloud-a", Runtime: "runtime-a", Profile: testElasticProfile("adapter-a", "cloud-a"), Provider: provider},
		ReplicaBackend{Name: "adapter-a", Cloud: "cloud-b", Runtime: "runtime-a", Profile: testElasticProfile("adapter-a", "cloud-b"), Provider: provider},
	)
	if err == nil {
		t.Fatal("expected duplicate durable adapter name to fail")
	}
}

func (f *fakeReplicaProvider) Handle(key string) provision.ProviderHandle {
	return provision.ProviderHandle{ExternalKey: key, ResourceID: "infercrane-" + key}
}
func (f *fakeReplicaProvider) EnsureReplica(_ context.Context, spec provision.ReplicaSpec) (provision.ProviderHandle, error) {
	f.ensureCalls++
	if f.ensureErr != nil {
		return provision.ProviderHandle{}, f.ensureErr
	}
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

type fakeCapacityAdvisor struct {
	availability provision.Availability
	calls        int
}

func (f *fakeCapacityAdvisor) Availability(context.Context, provision.AvailabilityRequest) (provision.Availability, error) {
	f.calls++
	return f.availability, nil
}

func (f fakeInspector) Inspect(context.Context, string) (bool, map[string]struct{}) {
	if !f.ready {
		return false, nil
	}
	return true, map[string]struct{}{"Qwen/Qwen3-8B": {}}
}

func TestEnsureCloudReplicaClassifiesFailedProviderRequest(t *testing.T) {
	store := &fakeCloudStore{}
	provider := &fakeReplicaProvider{ensureErr: fmt.Errorf("%w: no requested capacity", provision.ErrRequestFailed)}
	_, _, _, err := ensureCloudReplica(context.Background(), store, ReplicaBackend{Name: "sky", Cloud: "runpod", Runtime: "vllm", Provider: provider}, fakeInspector{}, domain.Operation{ID: "operation-1"}, CloudRequest{TenantID: "global", DeploymentID: "deployment-1", RevisionID: "revision-1", Name: "qwen", Model: "Qwen/Qwen3-8B", Cloud: "runpod", GPU: "L40S", Runtime: "vllm", Port: 8000}, 0)
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Code != "provider_request_failed" || !failure.Retryable || !strings.Contains(err.Error(), "requested capacity may be unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEnsureCloudReplicaDefersCreateWhenCapacityIsUnavailable(t *testing.T) {
	store := &fakeCloudStore{}
	provider := &fakeReplicaProvider{}
	advisor := &fakeCapacityAdvisor{availability: provision.Availability{State: "unavailable", Message: "Provider reports no current secure capacity for L40S"}}
	_, _, _, err := ensureCloudReplica(context.Background(), store, ReplicaBackend{Name: "sky", Cloud: "runpod", Runtime: "vllm", Provider: provider, Capacity: advisor}, fakeInspector{}, domain.Operation{ID: "operation-1"}, CloudRequest{TenantID: "global", DeploymentID: "deployment-1", RevisionID: "revision-1", Name: "qwen", Model: "Qwen/Qwen3-8B", Cloud: "runpod", GPU: "L40S", Runtime: "vllm", Port: 8000}, 0)
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Code != "provider_capacity_unavailable" || !failure.Retryable || provider.ensureCalls != 0 || advisor.calls != 1 || store.capacity.State != "unavailable" || store.capacity.Source != "sky.availability" || !store.capacity.ExpiresAt.After(store.capacity.ObservedAt) {
		t.Fatalf("failure=%+v ensure_calls=%d advisor_calls=%d err=%v", failure, provider.ensureCalls, advisor.calls, err)
	}
}

func TestAvailabilityCheckpointStatusUsesOperationLifecycleVocabulary(t *testing.T) {
	for providerState, expected := range map[string]string{
		"available":   "succeeded",
		"constrained": "succeeded",
		"unknown":     "succeeded",
		"unavailable": "waiting",
	} {
		if actual := availabilityCheckpointStatus(providerState); actual != expected {
			t.Fatalf("provider state %q mapped to %q, want %q", providerState, actual, expected)
		}
	}
}

func TestEnsureCloudReplicaAdoptsExistingCapacityBeforeStockCheck(t *testing.T) {
	store := &fakeCloudStore{}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready", Endpoint: "http://gpu:8000", Details: `{}`}}
	advisor := &fakeCapacityAdvisor{availability: provision.Availability{State: "unavailable"}}
	_, _, _, err := ensureCloudReplica(context.Background(), store, ReplicaBackend{Name: "sky", Cloud: "runpod", Runtime: "vllm", Provider: provider, Capacity: advisor}, fakeInspector{ready: true}, domain.Operation{ID: "operation-1"}, CloudRequest{TenantID: "global", DeploymentID: "deployment-1", RevisionID: "revision-1", Name: "qwen", Model: "Qwen/Qwen3-8B", Cloud: "runpod", GPU: "L40S", Runtime: "vllm", Port: 8000}, 0)
	if err != nil || provider.ensureCalls != 1 || advisor.calls != 0 {
		t.Fatalf("ensure_calls=%d advisor_calls=%d err=%v", provider.ensureCalls, advisor.calls, err)
	}
}

func TestProviderCapacityMessageUsesGroundedPlacementDetails(t *testing.T) {
	message, code := providerCapacityMessage(provision.Observation{State: "starting", Details: `[{"status":"INIT","region":"AU","init_status_reason":"Provisioning on runpod in AU"}]`})
	if code != "" {
		t.Fatalf("code=%q, want empty", code)
	}
	if message != "Provider capacity: Provisioning on runpod in AU; 1 resource observed; worker endpoint not exposed yet; billing state unavailable" {
		t.Fatalf("unexpected message: %q", message)
	}
}

func TestProviderCapacityMessageDoesNotInventUnavailableStages(t *testing.T) {
	message, code := providerCapacityMessage(provision.Observation{State: "starting", Details: `{}`})
	if code != "" {
		t.Fatalf("code=%q, want empty", code)
	}
	if message != "Provider capacity: starting; 1 resource observed; worker endpoint not exposed yet; billing state unavailable" || strings.Contains(message, "artifact") || strings.Contains(message, "container") {
		t.Fatalf("unexpected message: %q", message)
	}
}

func TestProviderCapacityMessageSurfacesImagePullFailure(t *testing.T) {
	message, code := providerCapacityMessage(provision.Observation{State: "provisioning", Details: `failed to pull image: unexpected EOF`})
	if code != "provider_image_pull_failed" || !strings.Contains(message, "registry stream ended unexpectedly") || !strings.Contains(message, "existing resource is retained") {
		t.Fatalf("message=%q code=%q", message, code)
	}
}

func TestEnsureCloudReplicaStopsAndCleansFailedRuntimeJob(t *testing.T) {
	store := &fakeCloudStore{}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "failed", Endpoint: "http://gpu:8000", Details: `{"status":"FAILED"}`}}
	_, _, _, err := ensureCloudReplica(context.Background(), store, ReplicaBackend{Name: "sky", Cloud: "runpod", Runtime: "vllm", Provider: provider}, fakeInspector{}, domain.Operation{ID: "operation-1"}, CloudRequest{TenantID: "global", DeploymentID: "deployment-1", RevisionID: "revision-1", Name: "qwen", Model: "Qwen/Qwen3-8B", Cloud: "runpod", GPU: "H100", Runtime: "vllm", Port: 8000}, 0)
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Code != "runtime_bootstrap_failed" || failure.Retryable || provider.deleteCalls != 1 || store.replica.LifecycleState != "deleted" {
		t.Fatalf("failure=%+v deletes=%d replica=%+v err=%v", failure, provider.deleteCalls, store.replica, err)
	}
}

func TestConvergeResumesAfterProviderCheckpoint(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", Model: "Qwen/Qwen3-8B", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "starting"}}
	operation := domain.Operation{ID: "operation-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global"}`}
	handler := QualifiedV01CloudHandlers(store, provider, fakeInspector{ready: true})[ConvergeKind]
	if _, err := handler(context.Background(), operation); err == nil {
		t.Fatal("starting replica should return retryable wait")
	}
	if store.replica.ProviderResourceID == "" || provider.ensureCalls != 1 {
		t.Fatalf("identity=%q ensure_calls=%d", store.replica.ProviderResourceID, provider.ensureCalls)
	}

	// Simulate process restart: a fresh handler receives only durable store and
	// provider state, reuses the same external identity, and finishes.
	provider.observation = provision.Observation{Exists: true, State: "ready", Endpoint: "http://gpu:8000", Details: `{}`}
	result, err := QualifiedV01CloudHandlers(store, provider, fakeInspector{ready: true})[ConvergeKind](context.Background(), operation)
	if err != nil || result == "" || provider.ensureCalls != 2 || store.replica.LifecycleState != "active" || store.target.URL != "http://gpu:8000" {
		t.Fatalf("result=%s replica=%#v target=%#v ensure_calls=%d err=%v", result, store.replica, store.target, provider.ensureCalls, err)
	}
}

func TestConvergeCancellationDeletesProviderResource(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen"}, replica: domain.Replica{ID: "replica-1", DeploymentID: "deployment-1", ExternalKey: "deployment-1-r0", ProviderResourceID: "infercrane-deployment-1-r0", LifecycleState: "starting"}}
	provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "starting"}}
	operation := domain.Operation{ID: "operation-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global"}`}
	_, err := QualifiedV01CloudHandlers(store, provider, fakeInspector{})[ConvergeKind+".cancel"](context.Background(), operation)
	if err != nil || provider.deleteCalls != 1 || store.replica.LifecycleState != "deleted" || !store.deleted {
		t.Fatalf("delete_calls=%d replica=%#v deleted=%t err=%v", provider.deleteCalls, store.replica, store.deleted, err)
	}
}
