package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/testtools/providerfixture"
)

func (f *fakeCloudStore) ReleaseProvisionedTarget(_ context.Context, _, name, _ string) error {
	f.released = append(f.released, name)
	kept := f.targetNames[:0]
	for _, existing := range f.targetNames {
		if existing != name {
			kept = append(kept, existing)
		}
	}
	f.targetNames = kept
	return nil
}

const recoveryConverge = `{"deployment_id":"deployment-1","name":"qwen-opt-12345678","no_endpoint":true,"model":"Qwen/Qwen3-8B","cloud":"runpod","gpu":"L40S","tenant_id":"global","min_replicas":1,"max_replicas":1}`

// recoveryFixture is a converged one-replica candidate deployment on the
// fault-injectable fixture provider.
func recoveryFixture(t *testing.T) (*fakeCloudStore, *providerfixture.Elastic, map[string]operations.Handler, domain.Replica) {
	t.Helper()
	spec, _ := json.Marshal(domain.DeploymentRevisionSpec{Model: "Qwen/Qwen3-8B", Runtime: "vllm", ComputeMode: "elastic", Cloud: "runpod", GPU: "L40S", MinReplicas: 1, MaxReplicas: 1})
	store := &fakeCloudStore{
		deployment: domain.Deployment{ID: "deployment-1", Name: "qwen-opt-12345678", Model: "Qwen/Qwen3-8B", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1, ActiveRevisionID: "revision-1"},
		revision:   domain.DeploymentRevision{ID: "revision-1", SpecJSON: string(spec)},
	}
	provider := providerfixture.NewElastic()
	handlers := QualifiedCloudHandlers(store, provider, fakeInspector{ready: true})
	if _, err := handlers[ConvergeKind](t.Context(), domain.Operation{ID: "converge-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: recoveryConverge}); err != nil {
		t.Fatal(err)
	}
	replica := liveReplica(t, store)
	if replica.ExternalKey != "deployment-1-r0" || !provider.Exists(replica.ExternalKey) {
		t.Fatalf("converged replica = %+v", replica)
	}
	return store, provider, handlers, replica
}

func liveReplica(t *testing.T, store *fakeCloudStore) domain.Replica {
	t.Helper()
	var live []domain.Replica
	for _, replica := range store.replicas {
		if replica.LifecycleState != "deleted" {
			live = append(live, replica)
		}
	}
	if len(live) != 1 {
		t.Fatalf("live replicas = %+v", live)
	}
	return live[0]
}

func removeLost(t *testing.T, handlers map[string]operations.Handler, replicaID string) (RemoveLostReplicaResult, error) {
	t.Helper()
	request, _ := json.Marshal(RemoveLostReplicaRequest{TenantID: "global", Name: "qwen-opt-12345678", DeploymentID: "deployment-1", ReplicaID: replicaID})
	raw, err := handlers[ReplicaRemoveLostKind](t.Context(), domain.Operation{ID: "remove-" + replicaID, LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: string(request)})
	var result RemoveLostReplicaResult
	if err == nil {
		if decodeErr := json.Unmarshal([]byte(raw), &result); decodeErr != nil {
			t.Fatal(decodeErr)
		}
	}
	return result, err
}

func TestRemoveLostReplicaLeavesAServingMachineAlone(t *testing.T) {
	store, provider, handlers, replica := recoveryFixture(t)
	result, err := removeLost(t, handlers, replica.ID)
	if err != nil || result.Lost || provider.DeleteCalls != 0 || !provider.Exists(replica.ExternalKey) || len(store.released) != 0 {
		t.Fatalf("a serving machine must never be removed: result=%+v deletes=%d released=%v err=%v", result, provider.DeleteCalls, store.released, err)
	}
	if current := liveReplica(t, store); current.ID != replica.ID || current.LifecycleState != "active" || current.Health != "healthy" {
		t.Fatalf("serving replica changed: %+v", current)
	}
}

func TestLostMachineIsRemovedAndConvergeReplacesItUnderANewProviderIdentity(t *testing.T) {
	store, provider, handlers, lost := recoveryFixture(t)
	if !provider.Lose(lost.ExternalKey) {
		t.Fatal("fixture machine was not created")
	}
	result, err := removeLost(t, handlers, lost.ID)
	if err != nil || !result.Lost || result.ReplicaID != lost.ID || result.RevisionID != "revision-1" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if store.replicas[lost.ID].LifecycleState != "deleted" || len(store.released) != 1 || store.released[0] != "qwen-opt-12345678-r0" {
		t.Fatalf("lost replica=%+v released=%v", store.replicas[lost.ID], store.released)
	}
	// A retried removal finds the work done and reports the same outcome.
	if again, againErr := removeLost(t, handlers, lost.ID); againErr != nil || !again.Lost {
		t.Fatalf("retry=%+v err=%v", again, againErr)
	}

	if _, err = handlers[ConvergeKind](t.Context(), domain.Operation{ID: "converge-2", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: recoveryConverge}); err != nil {
		t.Fatal(err)
	}
	replacement := liveReplica(t, store)
	if replacement.ID == lost.ID || replacement.ExternalKey != "deployment-1-r0-g1" || replacement.Ordinal != 0 || replacement.RevisionID != "revision-1" || replacement.Health != "healthy" {
		t.Fatalf("replacement = %+v", replacement)
	}
	if provider.CreatedResourceCount != 2 || !provider.Exists(replacement.ExternalKey) || provider.Exists(lost.ExternalKey) {
		t.Fatalf("provider created=%d", provider.CreatedResourceCount)
	}
	// Converging again adopts the replacement instead of creating a third.
	if _, err = handlers[ConvergeKind](t.Context(), domain.Operation{ID: "converge-3", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: recoveryConverge}); err != nil || provider.CreatedResourceCount != 2 || liveReplica(t, store).ID != replacement.ID {
		t.Fatalf("second converge created=%d err=%v", provider.CreatedResourceCount, err)
	}
}

func TestStoppedMachineStillListedIsDeletedBeforeItIsReplaced(t *testing.T) {
	store, provider, handlers, stopped := recoveryFixture(t)
	provider.SetState(stopped.ExternalKey, "failed")
	result, err := removeLost(t, handlers, stopped.ID)
	if err != nil || !result.Lost || provider.DeleteCalls != 1 || provider.Exists(stopped.ExternalKey) || store.replicas[stopped.ID].LifecycleState != "deleted" {
		t.Fatalf("a stopped machine must be deleted at the provider first: result=%+v deletes=%d err=%v", result, provider.DeleteCalls, err)
	}
}

func TestMachineRestartingInPlaceIsWaitedFor(t *testing.T) {
	store, provider, handlers, restarting := recoveryFixture(t)
	provider.SetState(restarting.ExternalKey, "starting")
	_, err := removeLost(t, handlers, restarting.ID)
	var failure operations.Failure
	if !errors.As(err, &failure) || !failure.Retryable || failure.Code != "replica_restarting" || provider.DeleteCalls != 0 || store.replicas[restarting.ID].LifecycleState == "deleted" {
		t.Fatalf("restart must be waited for: err=%v deletes=%d", err, provider.DeleteCalls)
	}
	provider.SetState(restarting.ExternalKey, "ready")
	if result, err := removeLost(t, handlers, restarting.ID); err != nil || result.Lost {
		t.Fatalf("a machine that came back is not lost: %+v err=%v", result, err)
	}
}

func TestRemovalThatStartedFinishesEvenIfTheMachineReappears(t *testing.T) {
	store, provider, handlers, flapping := recoveryFixture(t)
	provider.SetState(flapping.ExternalKey, "failed")
	provider.FailDeleteOnce = true
	_, err := removeLost(t, handlers, flapping.ID)
	var failure operations.Failure
	if !errors.As(err, &failure) || !failure.Retryable || store.replicas[flapping.ID].LifecycleState != "failed" {
		t.Fatalf("first attempt err=%v replica=%+v", err, store.replicas[flapping.ID])
	}
	provider.SetState(flapping.ExternalKey, "ready")
	if result, err := removeLost(t, handlers, flapping.ID); err != nil || !result.Lost || provider.Exists(flapping.ExternalKey) {
		t.Fatalf("a decided removal must finish: %+v err=%v", result, err)
	}
}

func TestRemoveLostReplicaRefusesAReplicaOfAnotherDeployment(t *testing.T) {
	_, _, handlers, _ := recoveryFixture(t)
	_, err := removeLost(t, handlers, "replica-of-someone-else")
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Retryable || failure.Code != "replica_missing" {
		t.Fatalf("err=%v", err)
	}
}

func TestReplicaTargetNameIsStableAcrossReplacements(t *testing.T) {
	if got := replicaTargetName("qwen", "revision-abcdefghij", true, 1); got != "qwen-revision-r1" {
		t.Fatalf("candidate target = %q", got)
	}
	if got := replicaTargetName("qwen", "revision-abcdefghij", false, 0); got != "qwen-r0" {
		t.Fatalf("active target = %q", got)
	}
}

func TestOneFailedRuntimeCheckNeverRemovesAMachineTheProviderReportsReady(t *testing.T) {
	store, provider, _, silent := recoveryFixture(t)
	handlers := QualifiedCloudHandlers(store, provider, fakeInspector{ready: false})
	request, _ := json.Marshal(RemoveLostReplicaRequest{TenantID: "global", Name: "qwen-opt-12345678", DeploymentID: "deployment-1", ReplicaID: silent.ID})
	for attempt := 1; attempt < lostRuntimeConfirmAttempts; attempt++ {
		_, err := handlers[ReplicaRemoveLostKind](t.Context(), domain.Operation{ID: "remove", LeaseOwner: "worker", LeaseGeneration: 1, Attempt: attempt, CreatedAt: time.Now().Add(-time.Hour), RequestJSON: string(request)})
		var failure operations.Failure
		if !errors.As(err, &failure) || !failure.Retryable || failure.Code != "replica_runtime_unconfirmed" || provider.DeleteCalls != 0 || store.replicas[silent.ID].LifecycleState == "failed" {
			t.Fatalf("attempt %d: err=%v deletes=%d replica=%+v", attempt, err, provider.DeleteCalls, store.replicas[silent.ID])
		}
	}
	// Enough attempts, but not yet over the window.
	if _, err := handlers[ReplicaRemoveLostKind](t.Context(), domain.Operation{ID: "remove", LeaseOwner: "worker", LeaseGeneration: 1, Attempt: lostRuntimeConfirmAttempts, CreatedAt: time.Now(), RequestJSON: string(request)}); err == nil || provider.DeleteCalls != 0 {
		t.Fatalf("removed inside the confirmation window: err=%v deletes=%d", err, provider.DeleteCalls)
	}
	raw, err := handlers[ReplicaRemoveLostKind](t.Context(), domain.Operation{ID: "remove", LeaseOwner: "worker", LeaseGeneration: 1, Attempt: lostRuntimeConfirmAttempts, CreatedAt: time.Now().Add(-2 * lostRuntimeConfirmWindow), RequestJSON: string(request)})
	var result RemoveLostReplicaResult
	_ = json.Unmarshal([]byte(raw), &result)
	if err != nil || !result.Lost || provider.DeleteCalls != 1 || store.replicas[silent.ID].LifecycleState != "deleted" {
		t.Fatalf("a runtime silent over the whole window is lost: result=%+v err=%v deletes=%d", result, err, provider.DeleteCalls)
	}
}

// A retirement removes a machine that still serves, so an optimization run
// can measure its revision again on a fresh one; converge then provisions
// that revision under a new provider identity, as for a lost machine.
func TestRetiredMachineIsRemovedAlthoughItServesAndIsReplaced(t *testing.T) {
	store, provider, handlers, serving := recoveryFixture(t)
	request, _ := json.Marshal(RemoveLostReplicaRequest{TenantID: "global", Name: "qwen-opt-12345678", DeploymentID: "deployment-1", ReplicaID: serving.ID, Retire: true})
	raw, err := handlers[ReplicaRemoveLostKind](t.Context(), domain.Operation{ID: "retire-" + serving.ID, LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: string(request)})
	var result RemoveLostReplicaResult
	if err != nil || json.Unmarshal([]byte(raw), &result) != nil || !result.Retired || result.Lost || result.ReplicaID != serving.ID {
		t.Fatalf("result=%s err=%v", raw, err)
	}
	if provider.DeleteCalls != 1 || provider.Exists(serving.ExternalKey) || store.replicas[serving.ID].LifecycleState != "deleted" || len(store.released) != 1 {
		t.Fatalf("a retired machine must be deleted at the provider: deletes=%d replica=%+v released=%v", provider.DeleteCalls, store.replicas[serving.ID], store.released)
	}
	if _, err = handlers[ConvergeKind](t.Context(), domain.Operation{ID: "converge-retired", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: recoveryConverge}); err != nil {
		t.Fatal(err)
	}
	if fresh := liveReplica(t, store); fresh.ID == serving.ID || fresh.ExternalKey == serving.ExternalKey || fresh.RevisionID != serving.RevisionID {
		t.Fatalf("fresh replica = %+v, retired %+v", fresh, serving)
	}
}
