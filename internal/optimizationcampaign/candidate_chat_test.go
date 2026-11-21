package optimizationcampaign

import (
	"context"
	"errors"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/workercredential"
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

// A replica's address belongs to its workload, which may be the tenant's own
// image in the tenant's own account. Whatever is sent there is what that
// workload can read, so it must be that replica's credential and no other.
func TestDirectReplicaIsSentOnlyTheCredentialItWasStartedWith(t *testing.T) {
	const master, shared = "worker-credential-master-0123456789ab", "control-plane-api-key"
	scoped := `{"provider":"runpod-pods","worker_credential_version":"` + workercredential.Version + `"}`
	replica := func(details string) []domain.Replica {
		return []domain.Replica{{TenantID: "tenant-a", DeploymentID: "dep-1", RevisionID: "rev-2", Endpoint: "http://first", Provider: "runpod-pods", Health: "healthy", LifecycleState: "active", ProviderDetails: details}}
	}
	deployment := func(connection string) map[string]domain.Deployment {
		return map[string]domain.Deployment{"tenant-a/support-opt-1": {ID: "dep-1", TenantID: "tenant-a", Name: "support-opt-1", ComputeConnectionID: connection}}
	}
	backends := map[string]BenchmarkBackend{"runpod-pods": {APIKey: shared, WorkerMaster: master}}
	own := workercredential.Derive(master, "tenant-a", "dep-1")

	for name, test := range map[string]struct {
		connection, details string
		backends            map[string]BenchmarkBackend
		want                string
	}{
		"scoped worker on managed capacity":  {"", scoped, backends, own},
		"scoped worker in a tenant account":  {"connection-a", scoped, backends, own},
		"worker that shares the operator's":  {"", `{"worker_credential_version":""}`, backends, shared},
		"tenant account without a version":   {"connection-a", `{"worker_credential_version":""}`, backends, ""},
		"tenant account without details":     {"connection-a", "", backends, ""},
		"scoped worker, no derivation key":   {"", scoped, map[string]BenchmarkBackend{"runpod-pods": {APIKey: shared}}, ""},
		"version this build does not derive": {"", `{"worker_credential_version":"tenant-hmac-v9"}`, backends, ""},
	} {
		store := chatTargetStore{deployments: deployment(test.connection), replicas: replica(test.details)}
		target, err := CandidateChatTarget(context.Background(), store, test.backends, decidedCandidate(CandidateQualified))
		if test.want == "" {
			if !errors.Is(err, ErrCandidateUnreachable) || target.APIKey != "" {
				t.Errorf("%s: a credential was chosen for a worker that must not be sent one: target=%+v err=%v", name, target, err)
			}
			continue
		}
		if err != nil || target.APIKey != test.want {
			t.Errorf("%s: credential=%q err=%v, want %q", name, target.APIKey, err, test.want)
		}
	}
	if own == shared || own == "" {
		t.Fatalf("scoped credential %q is not distinct from the shared one", own)
	}
}
