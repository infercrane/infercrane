package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/router"
	"github.com/infercrane/infercrane/internal/routes"
)

type fakeStore struct {
	deployment domain.Deployment
	target     domain.Target
	generation domain.RouterGeneration
	recorded   bool
}

func (f *fakeStore) Deployments(context.Context) ([]domain.Deployment, error) {
	return []domain.Deployment{f.deployment}, nil
}
func (f *fakeStore) Resolve(context.Context, string) (domain.ResolvedDeployment, error) {
	return domain.ResolvedDeployment{Deployment: f.deployment, Targets: []domain.Target{f.target}}, nil
}
func (f *fakeStore) ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error) {
	return domain.ResolvedDeployment{Deployment: f.deployment, Targets: []domain.Target{f.target}}, nil
}
func (f *fakeStore) SetTargetHealth(context.Context, string, string) error    { return nil }
func (f *fakeStore) SetDeploymentState(context.Context, string, string) error { return nil }
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

type healthyRuntime struct{}

func (healthyRuntime) Inspect(context.Context, string) (bool, map[string]struct{}) {
	return true, map[string]struct{}{"model": {}}
}

type fakeRouter struct {
	startErr error
	started  string
	stopped  []string
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
func (f *fakeRouter) Running(string) bool { return true }

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

func TestRouterCandidateFailureLeavesOldGenerationServing(t *testing.T) {
	store, directory := reconcilerFixture()
	backend := &fakeRouter{startErr: errors.New("candidate failed"), routes: directory}
	reconciler := Reconciler{Store: store, Routes: directory, Router: backend, Runtime: healthyRuntime{}, RouterStartPort: 18080, InstanceID: "instance"}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	published, ok := directory.Get("prod")
	if !ok || published.RouterURL != "http://old-router" || len(backend.stopped) != 0 || store.recorded {
		t.Fatalf("published=%#v stopped=%v recorded=%t", published, backend.stopped, store.recorded)
	}
}

func TestRouterCandidatePublishesBeforeOldRetires(t *testing.T) {
	store, directory := reconcilerFixture()
	backend := &fakeRouter{routes: directory}
	reconciler := Reconciler{Store: store, Routes: directory, Router: backend, Runtime: healthyRuntime{}, RouterStartPort: 18080, InstanceID: "instance"}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	published, ok := directory.Get("prod")
	if !ok || published.RouterURL != "http://new-router" || backend.started != "deployment-g2" || len(backend.stopped) != 1 || backend.stopped[0] != "deployment-g1" {
		t.Fatalf("published=%#v started=%s stopped=%v", published, backend.started, backend.stopped)
	}
}
