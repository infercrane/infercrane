package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type healthRecordingStore struct {
	*fakeStore
	health      map[string]string
	silentSince time.Time
}

func (s *healthRecordingStore) SetTargetHealth(_ context.Context, id, health string) error {
	s.health[id] = health
	return nil
}

func (s *healthRecordingStore) MarkSilentAgentTargets(_ context.Context, before time.Time) (int64, error) {
	s.silentSince = before
	return 0, nil
}

type staticCredentials map[string]string

func (c staticCredentials) Resolve(_ context.Context, _, targetID string) (string, error) {
	if value, ok := c[targetID]; ok {
		return value, nil
	}
	return "", domain.ErrNotFound
}

func adoptedFixture(t *testing.T, target domain.Target, ownership string) (*healthRecordingStore, *Reconciler, *countingRuntime) {
	t.Helper()
	base, directory := reconcilerFixture()
	base.target = target
	endpoint := domain.Endpoint{ID: "endpoint", TenantID: "global", Name: "local-qwen", LogicalModelID: "model-id", EnvironmentID: "environment-id", DesiredState: "serving", ObservedState: "pending", ActiveServingPlanID: "plan"}
	binding := domain.BackendBinding{ID: "binding", EndpointID: endpoint.ID, Kind: "external", TargetID: target.ID, OwnershipMode: ownership}
	base.endpoints = []domain.Endpoint{endpoint}
	base.resolvedEndpoint = domain.ResolvedEndpoint{Endpoint: endpoint, ActivePlan: domain.ServingPlan{ID: "plan", EndpointID: endpoint.ID, RoutingPolicy: "manual", Bindings: []domain.ServingPlanBinding{{BindingID: binding.ID, Weight: 100}}}, Bindings: []domain.BackendBinding{binding}}
	store := &healthRecordingStore{fakeStore: base, health: map[string]string{}}
	runtime := &countingRuntime{}
	return store, &Reconciler{Store: store, Routes: directory, Router: &fakeRouter{routes: directory}, Runtimes: testRuntimeBackends(t, runtime)}, runtime
}

func TestAgentTargetsAreNeverDialledOrRouted(t *testing.T) {
	target := domain.Target{ID: "agent-target", URL: "http://127.0.0.1:8000", Runtime: "vllm", Provider: "external", UpstreamModel: "model", Health: "healthy", AgentID: "agent-1"}
	store, reconciler, runtime := adoptedFixture(t, target, "observe-only")
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	reconciler.Now = func() time.Time { return now }
	if err := reconciler.observeAdoptedTargets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.calls != 0 || len(store.health) != 0 {
		t.Fatalf("the control plane probed an agent-only target: calls=%d health=%v", runtime.calls, store.health)
	}
	// Each pass marks servers whose agent went silent.
	_ = reconciler.Once(context.Background())
	if !store.silentSince.Equal(now.Add(-agentSilenceWindow)) {
		t.Fatalf("silent sweep cutoff=%s", store.silentSince)
	}
	// Even if storage claimed traffic-managed, an agent target never routes.
	store, reconciler, _ = adoptedFixture(t, target, "traffic-managed")
	if err := reconciler.RefreshEndpoints(context.Background()); err != nil {
		t.Fatal(err)
	}
	if route, _, ok := reconciler.Routes.AcquireForTenant("global", "local-qwen"); ok {
		t.Fatalf("agent target published a route: %#v", route)
	}
}

func TestDirectAdoptedProbeUsesStoredCredentialAndNeverDowngrades(t *testing.T) {
	target := domain.Target{ID: "private-target", URL: "https://vllm.internal.example/v1", Runtime: "vllm", Provider: "external", UpstreamModel: "model", Health: "starting"}
	base, directory := reconcilerFixture()
	base.target = target
	endpoint := domain.Endpoint{ID: "endpoint", TenantID: "global", Name: "private", DesiredState: "serving", ActiveServingPlanID: "plan"}
	binding := domain.BackendBinding{ID: "binding", EndpointID: endpoint.ID, Kind: "external", TargetID: target.ID, OwnershipMode: "observe-only"}
	base.endpoints = []domain.Endpoint{endpoint}
	base.resolvedEndpoint = domain.ResolvedEndpoint{Endpoint: endpoint, Bindings: []domain.BackendBinding{binding}}
	store := &healthRecordingStore{fakeStore: base, health: map[string]string{}}
	runtime := &credentialRuntime{}
	reconciler := &Reconciler{Store: store, Routes: directory, Router: &fakeRouter{routes: directory}, Runtimes: testRuntimeBackends(t, runtime), UpstreamCredentials: staticCredentials{"private-target": "server-token"}}
	if err := reconciler.observeAdoptedTargets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if runtime.credential != "server-token" || runtime.unauthenticatedCall || store.health["private-target"] != "healthy" {
		t.Fatalf("credential probe: runtime=%+v health=%v", runtime, store.health)
	}
	broken := &credentialRuntime{}
	reconciler.Runtimes = testRuntimeBackends(t, broken)
	reconciler.UpstreamCredentials = brokenCredentials{}
	if err := reconciler.observeAdoptedTargets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if broken.unauthenticatedCall || broken.credential != "" || store.health["private-target"] != "unhealthy" {
		t.Fatalf("an unreadable credential downgraded to an unauthenticated probe: %+v health=%v", broken, store.health)
	}
}

type brokenCredentials struct{}

func (brokenCredentials) Resolve(context.Context, string, string) (string, error) {
	return "", context.DeadlineExceeded
}
