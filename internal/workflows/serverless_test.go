package workflows

import (
	"context"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/provision"
)

type fakeServerlessProvider struct {
	endpoint      provision.ServerlessEndpoint
	ensureCalls   int
	deleteCalls   int
	deletedIDs    []string
	deletePending bool
	deleted       bool
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
func (f *fakeServerlessProvider) DeleteEndpoint(_ context.Context, id string) error {
	f.deleteCalls++
	f.deletedIDs = append(f.deletedIDs, id)
	if !f.deletePending {
		f.deleted = true
	}
	return nil
}
func (f *fakeServerlessProvider) EndpointURL(id string) string {
	return "https://api.runpod.invalid/v2/" + id + "/openai"
}

func TestServerlessConvergeRegistersScaleToZeroEndpointWithoutWarmingWorker(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", ActiveRevisionID: "rev-1", RoutingStrategy: "round-robin", MinReplicas: 0, MaxReplicas: 4}}
	provider := &fakeServerlessProvider{endpoint: provision.ServerlessEndpoint{ID: "endpoint-1", Name: "infercrane-deployment-1-rev-1-serverless", TemplateID: "template-1", WorkersMin: 0, WorkersMax: 4, Workers: 0}}
	operation := domain.Operation{ID: "serverless-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"name":"qwen","model":"Qwen/Qwen3-8B","compute_mode":"serverless","cloud":"runpod","gpu":"L40S","tenant_id":"global","min_replicas":0,"max_replicas":4}`}
	result, err := ServerlessHandlers(store, testServerlessBackend(provider), fakeArtifactResolver{})[ServerlessConvergeKind](context.Background(), operation)
	if err != nil || result == "" || provider.ensureCalls != 1 || store.replica.Provider != "runpod-serverless" || store.replica.ProviderResourceID != "endpoint-1" || store.target.URL != "https://api.runpod.invalid/v2/endpoint-1/openai" || !store.applied {
		t.Fatalf("result=%s ensure=%d replica=%+v target=%+v applied=%t err=%v", result, provider.ensureCalls, store.replica, store.target, store.applied, err)
	}
}

func TestServerlessDeleteConfirmsEndpointAbsentBeforeDeletingDeployment(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen"}, replica: domain.Replica{ID: "replica-1", DeploymentID: "deployment-1", Provider: "runpod-serverless", ProviderResourceID: "endpoint-1", Endpoint: "https://api.runpod.invalid/v2/endpoint-1/openai", LifecycleState: "ready"}}
	provider := &fakeServerlessProvider{endpoint: provision.ServerlessEndpoint{ID: "endpoint-1"}}
	operation := domain.Operation{ID: "delete-1", RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","tenant_id":"global"}`}
	result, err := ServerlessHandlers(store, testServerlessBackend(provider), fakeArtifactResolver{})[ServerlessDeleteKind](context.Background(), operation)
	if err != nil || result == "" || provider.deleteCalls != 1 || store.replica.LifecycleState != "deleted" || !store.deleted {
		t.Fatalf("result=%s delete_calls=%d replica=%+v deployment_deleted=%t err=%v", result, provider.deleteCalls, store.replica, store.deleted, err)
	}
}

func TestServerlessDeleteRecoversEndpointByDurableNameWhenCreateResponseWasLost(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen"}, replica: domain.Replica{ID: "replica-1", DeploymentID: "deployment-1", RevisionID: "rev-1", ExternalKey: "deployment-1-rev-1", Provider: "runpod-serverless", LifecycleState: "provisioning"}}
	provider := &fakeServerlessProvider{endpoint: provision.ServerlessEndpoint{ID: "endpoint-recovered", Name: provision.ServerlessEndpointName("deployment-1-rev-1")}}
	operation := domain.Operation{ID: "cancel-1", RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","tenant_id":"global"}`}
	result, err := ServerlessHandlers(store, testServerlessBackend(provider), fakeArtifactResolver{})[ServerlessConvergeKind+".cancel"](context.Background(), operation)
	if err != nil || result == "" || provider.deleteCalls != 1 || len(provider.deletedIDs) != 1 || provider.deletedIDs[0] != "endpoint-recovered" || store.replica.LifecycleState != "deleted" || !store.deleted {
		t.Fatalf("result=%s deleted_ids=%v replica=%+v deployment_deleted=%t err=%v", result, provider.deletedIDs, store.replica, store.deleted, err)
	}
}

func TestServerlessDeleteDoesNotPersistDeletionWhileRecoveredEndpointRemains(t *testing.T) {
	store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen"}, replica: domain.Replica{ID: "replica-1", DeploymentID: "deployment-1", ExternalKey: "deployment-1-rev-1", Provider: "runpod-serverless", LifecycleState: "provisioning"}}
	provider := &fakeServerlessProvider{endpoint: provision.ServerlessEndpoint{ID: "endpoint-pending", Name: provision.ServerlessEndpointName("deployment-1-rev-1")}, deletePending: true}
	operation := domain.Operation{ID: "delete-pending", RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","tenant_id":"global"}`}
	_, err := ServerlessHandlers(store, testServerlessBackend(provider), fakeArtifactResolver{})[ServerlessDeleteKind](context.Background(), operation)
	if err == nil || provider.deleteCalls != 1 || store.replica.LifecycleState == "deleted" || store.deleted {
		t.Fatalf("delete_calls=%d replica=%+v deployment_deleted=%t err=%v", provider.deleteCalls, store.replica, store.deleted, err)
	}
}

type fakeArtifactResolver struct{}

func testServerlessBackend(provider ServerlessProvider) ServerlessBackend {
	return ServerlessBackend{Name: "runpod-serverless", Cloud: "runpod", Runtime: "vllm", Provider: provider}
}

func (fakeArtifactResolver) Resolve(context.Context, string, string) (domain.ModelArtifact, error) {
	return domain.ModelArtifact{Repository: "Qwen/Qwen3-8B", ImmutableRevision: "0123456789abcdef", ModelIdentity: "Qwen/Qwen3-8B@0123456789abcdef", CacheState: "unknown"}, nil
}
