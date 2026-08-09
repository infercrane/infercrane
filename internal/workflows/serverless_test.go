package workflows

import (
	"context"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/provision"
)

type fakeServerlessProvider struct {
	endpoint    provision.ServerlessEndpoint
	ensureCalls int
	deleteCalls int
	deleted     bool
}

func (f *fakeServerlessProvider) EnsureEndpoint(context.Context, provision.ServerlessEndpointSpec) (provision.ServerlessEndpoint, error) {
	f.ensureCalls++
	return f.endpoint, nil
}
func (f *fakeServerlessProvider) ListEndpoints(context.Context) ([]provision.ServerlessEndpoint, error) {
	if f.deleted {
		return nil, nil
	}
	return []provision.ServerlessEndpoint{f.endpoint}, nil
}
func (f *fakeServerlessProvider) DeleteEndpoint(context.Context, string) error {
	f.deleteCalls++
	f.deleted = true
	return nil
}
func (f *fakeServerlessProvider) EndpointURL(id string) string {
	return "https://api.runpod.invalid/v2/" + id + "/openai"
}

func TestServerlessConvergeRegistersScaleToZeroEndpointWithoutWarmingWorker(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", ActiveRevisionID: "rev-1", RoutingStrategy: "round-robin", MinReplicas: 0, MaxReplicas: 4}}
	provider := &fakeServerlessProvider{endpoint: provision.ServerlessEndpoint{ID: "endpoint-1", Name: "infercrane-deployment-1-rev-1-serverless", TemplateID: "template-1", WorkersMin: 0, WorkersMax: 4, Workers: 0}}
	operation := domain.Operation{ID: "serverless-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"name":"qwen","model":"Qwen/Qwen3-8B","compute_mode":"serverless","cloud":"runpod","gpu":"L40S","tenant_id":"global","min_replicas":0,"max_replicas":4}`}
	result, err := ServerlessHandlers(store, provider, fakeArtifactResolver{})[ServerlessConvergeKind](context.Background(), operation)
	if err != nil || result == "" || provider.ensureCalls != 1 || store.replica.Provider != "runpod-serverless" || store.replica.ProviderResourceID != "endpoint-1" || store.target.URL != "https://api.runpod.invalid/v2/endpoint-1/openai" || !store.applied {
		t.Fatalf("result=%s ensure=%d replica=%+v target=%+v applied=%t err=%v", result, provider.ensureCalls, store.replica, store.target, store.applied, err)
	}
}

func TestServerlessDeleteConfirmsEndpointAbsentBeforeDeletingDeployment(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen"}, replica: domain.Replica{ID: "replica-1", DeploymentID: "deployment-1", Provider: "runpod-serverless", ProviderResourceID: "endpoint-1", Endpoint: "https://api.runpod.invalid/v2/endpoint-1/openai", LifecycleState: "ready"}}
	provider := &fakeServerlessProvider{endpoint: provision.ServerlessEndpoint{ID: "endpoint-1"}}
	operation := domain.Operation{ID: "delete-1", RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","tenant_id":"global"}`}
	result, err := ServerlessHandlers(store, provider, fakeArtifactResolver{})[ServerlessDeleteKind](context.Background(), operation)
	if err != nil || result == "" || provider.deleteCalls != 1 || store.replica.LifecycleState != "deleted" || !store.deleted {
		t.Fatalf("result=%s delete_calls=%d replica=%+v deployment_deleted=%t err=%v", result, provider.deleteCalls, store.replica, store.deleted, err)
	}
}

type fakeArtifactResolver struct{}

func (fakeArtifactResolver) Resolve(context.Context, string, string) (domain.ModelArtifact, error) {
	return domain.ModelArtifact{Repository: "Qwen/Qwen3-8B", ImmutableRevision: "0123456789abcdef", ModelIdentity: "Qwen/Qwen3-8B@0123456789abcdef", CacheState: "unknown"}, nil
}
