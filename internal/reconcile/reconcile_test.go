package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/router"
	"github.com/infercrane/infercrane/internal/routes"
)

type fakeStore struct {
	deployment       domain.Deployment
	target           domain.Target
	targets          []domain.Target
	state            string
	generation       domain.RouterGeneration
	recorded         bool
	endpoints        []domain.Endpoint
	resolvedEndpoint domain.ResolvedEndpoint
	endpointState    string
}

type isolatingStore struct {
	*fakeStore
	deployments []domain.Deployment
	failedName  string
	resolved    []string
}

func (f *isolatingStore) Deployments(context.Context) ([]domain.Deployment, error) {
	return f.deployments, nil
}

func (f *isolatingStore) ResolveForTenant(_ context.Context, _, name string) (domain.ResolvedDeployment, error) {
	f.resolved = append(f.resolved, name)
	if name == f.failedName {
		return domain.ResolvedDeployment{}, errors.New("injected lookup failure")
	}
	deployment := f.deployment
	for _, candidate := range f.deployments {
		if candidate.Name == name {
			deployment = candidate
			break
		}
	}
	return domain.ResolvedDeployment{Deployment: deployment, Targets: f.resolvedTargets()}, nil
}

func (f *fakeStore) Deployments(context.Context) ([]domain.Deployment, error) {
	return []domain.Deployment{f.deployment}, nil
}
func (f *fakeStore) Resolve(context.Context, string) (domain.ResolvedDeployment, error) {
	return domain.ResolvedDeployment{Deployment: f.deployment, Targets: f.resolvedTargets()}, nil
}
func (f *fakeStore) ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error) {
	return domain.ResolvedDeployment{Deployment: f.deployment, Targets: f.resolvedTargets()}, nil
}
func (f *fakeStore) resolvedTargets() []domain.Target {
	if f.targets != nil {
		return f.targets
	}
	return []domain.Target{f.target}
}
func (f *fakeStore) SetTargetHealth(context.Context, string, string) error { return nil }
func (f *fakeStore) SetDeploymentState(_ context.Context, _ string, state string) error {
	f.state = state
	return nil
}
func (f *fakeStore) Event(context.Context, string, string, string, string, string) error {
	return nil
}
func (f *fakeStore) ActiveGeneration(context.Context, string, string) (domain.RouterGeneration, error) {
	return f.generation, nil
}
func (f *fakeStore) RecordGeneration(_ context.Context, generation domain.RouterGeneration) (domain.RouterGeneration, error) {
	f.recorded = true
	generation.ID, generation.Status = "new-generation", "active"
	f.generation = generation
	return generation, nil
}
func (f *fakeStore) EndpointsForTenant(context.Context, string) ([]domain.Endpoint, error) {
	return f.endpoints, nil
}
func (f *fakeStore) TenantsWithEndpoints(context.Context) ([]string, error) {
	if len(f.endpoints) == 0 {
		return nil, nil
	}
	return []string{"global"}, nil
}
func (f *fakeStore) TargetForTenantByID(context.Context, string, string) (domain.Target, error) {
	return f.target, nil
}
func (f *fakeStore) ResolveEndpointForTenant(context.Context, string, string) (domain.ResolvedEndpoint, error) {
	return f.resolvedEndpoint, nil
}
func (f *fakeStore) SetEndpointState(_ context.Context, _, _, state string) error {
	f.endpointState = state
	return nil
}

type healthyRuntime struct{}

func (healthyRuntime) Inspect(context.Context, string) (bool, map[string]struct{}) {
	return true, map[string]struct{}{"model": {}}
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

type fakeRouter struct {
	startErr error
	started  string
	stopped  []string
	running  []string
	routes   *routes.Directory
}

func (f *fakeRouter) Start(_ context.Context, spec router.Spec) (string, error) {
	f.started = spec.ProcessID
	if f.startErr != nil {
		return "", f.startErr
	}
	return "http://new-router", nil
}
func (f *fakeRouter) Stop(id string) error {
	if id == "deployment-g1" && f.routes != nil {
		published, ok := f.routes.Get("prod")
		if !ok || published.RouterURL != "http://new-router" {
			return errors.New("old router stopped before candidate publication")
		}
	}
	f.stopped = append(f.stopped, id)
	return nil
}
func (f *fakeRouter) Running(id string) bool {
	f.running = append(f.running, id)
	return true
}

func reconcilerFixture() (*fakeStore, *routes.Directory) {
	store := &fakeStore{
		deployment: domain.Deployment{ID: "deployment", TenantID: "global", Name: "prod", Model: "model", RoutingStrategy: "round-robin"},
		target:     domain.Target{ID: "target", Name: "gpu", URL: "http://gpu", Runtime: "vllm", UpstreamModel: "model", Health: "healthy"},
		generation: domain.RouterGeneration{DeploymentID: "deployment", OwnerID: "instance", Generation: 1, WorkerSetHash: "old-hash", InternalEndpoint: "http://old-router", Status: "active"},
	}
	directory := routes.New()
	directory.Put(routes.Snapshot{DeploymentID: "deployment", Alias: "prod", UpstreamModel: "model", RouterURL: "http://old-router", RouterProcessID: "deployment-g1"})
	return store, directory
}

func TestUnchangedGenerationUsesGenerationProcessIdentity(t *testing.T) {
	store, directory := reconcilerFixture()
	store.generation.WorkerSetHash = router.WorkerSetHash("round-robin", []string{"http://gpu"})
	backend := &fakeRouter{routes: directory}
	reconciler := Reconciler{Store: store, Routes: directory, Router: backend, Runtimes: testRuntimeBackends(t, healthyRuntime{}), RouterStartPort: 18080, InstanceID: "instance"}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backend.started != "" || len(backend.stopped) != 0 || len(backend.running) != 1 || backend.running[0] != "deployment-g1" {
		t.Fatalf("started=%q stopped=%v running=%v", backend.started, backend.stopped, backend.running)
	}
}

func TestReconcilerIsolatesDeploymentLookupFailure(t *testing.T) {
	base, directory := reconcilerFixture()
	healthy := base.deployment
	healthy.ID, healthy.Name = "healthy-deployment", "healthy"
	broken := healthy
	broken.ID, broken.Name = "broken-deployment", "broken"
	store := &isolatingStore{fakeStore: base, deployments: []domain.Deployment{broken, healthy}, failedName: broken.Name}
	backend := &fakeRouter{routes: directory}
	reconciler := Reconciler{Store: store, Routes: directory, Router: backend, Runtimes: testRuntimeBackends(t, healthyRuntime{}), RouterStartPort: 18080, InstanceID: "instance"}
	err := reconciler.Once(context.Background())
	if err == nil || !strings.Contains(err.Error(), "injected lookup failure") {
		t.Fatalf("aggregate error=%v", err)
	}
	if len(store.resolved) != 2 || store.resolved[1] != healthy.Name {
		t.Fatalf("resolved deployments=%v; healthy deployment was starved", store.resolved)
	}
}

func TestRouterCandidateFailureLeavesOldGenerationServing(t *testing.T) {
	store, directory := reconcilerFixture()
	backend := &fakeRouter{startErr: errors.New("candidate failed"), routes: directory}
	reconciler := Reconciler{Store: store, Routes: directory, Router: backend, Runtimes: testRuntimeBackends(t, healthyRuntime{}), RouterStartPort: 18080, InstanceID: "instance"}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	published, ok := directory.Get("prod")
	if !ok || published.RouterURL != "http://old-router" || len(backend.stopped) != 0 || store.recorded {
		t.Fatalf("published=%#v stopped=%v recorded=%t", published, backend.stopped, store.recorded)
	}
}

func TestUnregisteredRuntimeNeverBecomesRoutable(t *testing.T) {
	store, directory := reconcilerFixture()
	store.target.Runtime = "sglang"
	backend := &fakeRouter{routes: directory}
	reconciler := Reconciler{Store: store, Routes: directory, Router: backend, Runtimes: testRuntimeBackends(t, healthyRuntime{}), RouterStartPort: 18080, InstanceID: "instance"}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := directory.Get("prod"); ok || backend.started != "" {
		t.Fatalf("unregistered runtime remained routable; started=%q", backend.started)
	}
}

func TestRouterCandidatePublishesBeforeOldRetires(t *testing.T) {
	store, directory := reconcilerFixture()
	backend := &fakeRouter{routes: directory}
	reconciler := Reconciler{Store: store, Routes: directory, Router: backend, Runtimes: testRuntimeBackends(t, healthyRuntime{}), RouterStartPort: 18080, InstanceID: "instance"}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	published, ok := directory.Get("prod")
	if !ok || published.RouterURL != "http://new-router" || backend.started != "deployment-g2" || len(backend.stopped) != 0 {
		t.Fatalf("published=%#v started=%s stopped=%v", published, backend.started, backend.stopped)
	}
	store.generation.WorkerSetHash = router.WorkerSetHash("round-robin", []string{"http://gpu"})
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(backend.stopped) != 1 || backend.stopped[0] != "deployment-g1" {
		t.Fatalf("retired routers stopped=%v", backend.stopped)
	}
}

func TestRouterRetirementWaitsForPinnedRequest(t *testing.T) {
	store, directory := reconcilerFixture()
	_, release, ok := directory.AcquireForTenant("global", "prod")
	if !ok {
		t.Fatal("old route was not acquired")
	}
	backend := &fakeRouter{routes: directory}
	reconciler := Reconciler{Store: store, Routes: directory, Router: backend, Runtimes: testRuntimeBackends(t, healthyRuntime{}), RouterStartPort: 18080, InstanceID: "instance"}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.generation.WorkerSetHash = router.WorkerSetHash("round-robin", []string{"http://gpu"})
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(backend.stopped) != 0 {
		t.Fatalf("old router stopped with active request: %v", backend.stopped)
	}
	release()
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(backend.stopped) != 1 || backend.stopped[0] != "deployment-g1" {
		t.Fatalf("old router was not reaped: %v", backend.stopped)
	}
}

func TestReconcilerCompilesStableEndpointWithoutDatabaseRequestLookup(t *testing.T) {
	store, directory := reconcilerFixture()
	store.generation.WorkerSetHash = router.WorkerSetHash("round-robin", []string{"http://gpu"})
	endpoint := domain.Endpoint{ID: "endpoint", TenantID: "global", Name: "coder-production", LogicalModelID: "model-id", EnvironmentID: "environment-id", DesiredState: "serving", ObservedState: "pending", ActiveServingPlanID: "plan"}
	binding := domain.BackendBinding{ID: "binding", EndpointID: endpoint.ID, Kind: "deployment", DeploymentID: store.deployment.ID}
	store.endpoints = []domain.Endpoint{endpoint}
	store.resolvedEndpoint = domain.ResolvedEndpoint{Endpoint: endpoint, ActivePlan: domain.ServingPlan{ID: "plan", EndpointID: endpoint.ID, RoutingPolicy: "manual", Bindings: []domain.ServingPlanBinding{{BindingID: binding.ID, Weight: 100}}}, Bindings: []domain.BackendBinding{binding}}
	reconciler := Reconciler{Store: store, Routes: directory, Router: &fakeRouter{routes: directory}, Runtimes: testRuntimeBackends(t, healthyRuntime{}), RouterStartPort: 18080, InstanceID: "instance"}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	route, release, ok := directory.AcquireForTenant("global", endpoint.Name)
	if !ok {
		t.Fatal("compiled endpoint is not routable")
	}
	defer release()
	if route.EndpointID != endpoint.ID || route.LogicalModelID != "model-id" || route.EnvironmentID != "environment-id" || route.ServingPlanID != "plan" || route.BindingID != "binding" || route.DeploymentID != store.deployment.ID {
		t.Fatalf("compiled route=%#v", route)
	}
	if store.endpointState != "serving" {
		t.Fatalf("endpoint state=%q", store.endpointState)
	}
}

func TestAdoptedEndpointOwnershipControlsRouteCompilation(t *testing.T) {
	for _, test := range []struct {
		ownership string
		routable  bool
	}{{"observe-only", false}, {"traffic-managed", true}} {
		t.Run(test.ownership, func(t *testing.T) {
			store, directory := reconcilerFixture()
			store.target = domain.Target{ID: "external-target", URL: "https://runtime.example/v1", Runtime: "vllm", Provider: "external", UpstreamModel: "model", Health: "healthy"}
			endpoint := domain.Endpoint{ID: "external-endpoint", TenantID: "global", Name: "adopted", LogicalModelID: "model-id", EnvironmentID: "environment-id", DesiredState: "serving", ObservedState: "pending", ActiveServingPlanID: "plan"}
			binding := domain.BackendBinding{ID: "external-binding", EndpointID: endpoint.ID, Kind: "external", TargetID: store.target.ID, OwnershipMode: test.ownership}
			store.endpoints = []domain.Endpoint{endpoint}
			store.resolvedEndpoint = domain.ResolvedEndpoint{Endpoint: endpoint, ActivePlan: domain.ServingPlan{ID: "plan", EndpointID: endpoint.ID, RoutingPolicy: "manual", Bindings: []domain.ServingPlanBinding{{BindingID: binding.ID, Weight: 100}}}, Bindings: []domain.BackendBinding{binding}}
			reconciler := Reconciler{Store: store, Routes: directory, Router: &fakeRouter{routes: directory}, Runtimes: testRuntimeBackends(t, healthyRuntime{})}
			if err := reconciler.RefreshEndpoints(context.Background()); err != nil {
				t.Fatal(err)
			}
			route, _, ok := directory.AcquireForTenant("global", endpoint.Name)
			if ok != test.routable {
				t.Fatalf("routable=%t want=%t route=%#v", ok, test.routable, route)
			}
			if ok && (route.DeploymentID != "" || route.TargetID != store.target.ID || route.ComputeMode != "external") {
				t.Fatalf("external route=%#v", route)
			}
		})
	}
}

type countingRuntime struct{ calls int }

func (r *countingRuntime) Inspect(context.Context, string) (bool, map[string]struct{}) {
	r.calls++
	return false, nil
}

type zeroWorkerStatus struct{}

func (zeroWorkerStatus) EndpointHealth(context.Context, string) (provision.ServerlessHealth, error) {
	return provision.ServerlessHealth{}, nil
}

type fixtureExternalFallback struct{ calls int }

func (f *fixtureExternalFallback) Owns(provider string) bool { return provider == "openrouter" }
func (f *fixtureExternalFallback) Resolve(_ context.Context, deployment domain.Deployment, targets []domain.Target) (routes.Snapshot, error) {
	f.calls++
	for _, target := range targets {
		if target.Provider == "openrouter" {
			return routes.Snapshot{DeploymentID: deployment.ID, TargetID: target.ID, TenantID: deployment.TenantID, Alias: deployment.Name, RouterURL: target.URL, Provider: "openrouter", ComputeMode: "external", ExternalPolicyID: "policy", SelectionReason: "explicit fallback"}, nil
		}
	}
	return routes.Snapshot{}, errors.New("fallback missing")
}

func TestHealthyPrimaryIsNotDegradedByConfiguredExternalTarget(t *testing.T) {
	store, directory := reconcilerFixture()
	store.targets = []domain.Target{
		store.target,
		{ID: "external", Name: "fallback", Provider: "openrouter", URL: "https://openrouter.invalid/api/v1", Runtime: "vllm", UpstreamModel: "provider/model", Health: "healthy"},
	}
	fallback := &fixtureExternalFallback{}
	reconciler := Reconciler{Store: store, Routes: directory, Router: &fakeRouter{routes: directory}, Runtimes: testRuntimeBackends(t, healthyRuntime{}), ExternalFallback: fallback, RouterStartPort: 18080, InstanceID: "instance"}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	published, ok := directory.Get("prod")
	if !ok || published.ComputeMode == "external" || fallback.calls != 0 || store.state != "healthy" {
		t.Fatalf("published=%#v fallback_calls=%d state=%q", published, fallback.calls, store.state)
	}
}

func TestExternalFallbackPublishesOnlyWhenNoPrimaryTargetIsHealthy(t *testing.T) {
	store, directory := reconcilerFixture()
	store.target.Provider = "openrouter"
	store.target.URL = "https://external.invalid/api"
	fallback := &fixtureExternalFallback{}
	reconciler := Reconciler{Store: store, Routes: directory, Router: &fakeRouter{routes: directory}, Runtimes: testRuntimeBackends(t, healthyRuntime{}), ExternalFallback: fallback, RouterStartPort: 18080, InstanceID: "instance"}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	published, ok := directory.Get("prod")
	if !ok || published.ComputeMode != "external" || published.ExternalPolicyID != "policy" || fallback.calls != 1 {
		t.Fatalf("published=%#v calls=%d", published, fallback.calls)
	}
}

func TestServerlessRouteDoesNotWarmWorkersOrStartRouter(t *testing.T) {
	store, directory := reconcilerFixture()
	store.target.Provider = "runpod-serverless"
	store.target.ProviderResourceID = "endpoint-1"
	store.target.URL = "https://api.runpod.invalid/v2/endpoint/openai"
	backend := &fakeRouter{routes: directory}
	runtime := &countingRuntime{}
	reconciler := Reconciler{Store: store, Routes: directory, Router: backend, Runtimes: testRuntimeBackends(t, runtime), DirectTargets: map[string]DirectTargetBackend{"runpod-serverless": {Provider: "runpod", APIKey: "runpod-secret", Status: zeroWorkerStatus{}}}, RouterStartPort: 18080, InstanceID: "instance"}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	published, ok := directory.Get("prod")
	if !ok || published.RouterURL != store.target.URL || published.ComputeMode != "serverless" || published.Provider != "runpod" || published.UpstreamAPIKey != "runpod-secret" || published.ProviderWorkers == nil || *published.ProviderWorkers != 0 || published.ProviderObservedAt.IsZero() || runtime.calls != 0 || backend.started != "" {
		t.Fatalf("published=%#v runtime_calls=%d router_started=%q", published, runtime.calls, backend.started)
	}
}
