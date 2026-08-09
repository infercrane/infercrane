package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/provision"
)

type ServerlessProvider interface {
	EnsureEndpoint(context.Context, provision.ServerlessEndpointSpec) (provision.ServerlessEndpoint, error)
	ListEndpoints(context.Context) ([]provision.ServerlessEndpoint, error)
	DeleteEndpoint(context.Context, string) error
	EndpointURL(string) string
}

func ServerlessHandlers(store CloudStore, provider ServerlessProvider, artifactResolver ArtifactResolver) map[string]operations.Handler {
	converge := func(ctx context.Context, operation domain.Operation) (string, error) {
		request, err := decodeCloudRequest(operation)
		if err != nil {
			return "", err
		}
		if request.ComputeMode != "serverless" || request.Cloud != "runpod" || request.MinReplicas != 0 || request.MaxReplicas < 1 {
			return "", operations.Permanent("invalid_serverless_spec", errors.New("RunPod Serverless requires compute_mode=serverless, min=0, and max>=1"))
		}
		resolved, err := store.ResolveForTenant(ctx, request.TenantID, request.Name)
		if err != nil || (request.DeploymentID != "" && resolved.Deployment.ID != request.DeploymentID) {
			return "", operations.Permanent("deployment_missing", fmt.Errorf("resolve desired deployment: %w", err))
		}
		request.DeploymentID = resolved.Deployment.ID
		if request.RevisionID == "" {
			request.RevisionID = resolved.Deployment.ActiveRevisionID
		}
		modelArtifact, artifactErr := store.ModelArtifactForRevision(ctx, request.TenantID, request.RevisionID)
		if errors.Is(artifactErr, domain.ErrNotFound) {
			if artifactResolver == nil {
				return "", operations.Permanent("artifact_resolver_unavailable", errors.New("Hugging Face artifact resolver is required"))
			}
			modelArtifact, artifactErr = artifactResolver.Resolve(ctx, request.Model, request.ModelRevision)
			if artifactErr == nil {
				modelArtifact, artifactErr = store.AttachModelArtifact(ctx, request.TenantID, request.RevisionID, modelArtifact)
			}
		}
		if artifactErr != nil {
			return "", operations.Retryable("artifact_resolution_failed", artifactErr)
		}
		_ = checkpoint(ctx, store, operation, "model.artifact", "succeeded", map[string]any{"identity": modelArtifact.ModelIdentity, "cache_state": modelArtifact.CacheState}, 15, "Immutable model artifact resolved")

		externalKey := request.DeploymentID + "-" + request.RevisionID
		replica, _, err := store.EnsureReplicaIntent(ctx, domain.Replica{TenantID: request.TenantID, DeploymentID: request.DeploymentID, RevisionID: request.RevisionID, Ordinal: 0, ExternalKey: externalKey, Provider: "runpod-serverless"})
		if err != nil {
			return "", classify("serverless_intent_failed", err)
		}
		endpoint, err := provider.EnsureEndpoint(ctx, provision.ServerlessEndpointSpec{ExternalKey: externalKey, Model: request.Model, ModelRevision: modelArtifact.ImmutableRevision, GPU: request.GPU, Region: request.Region, WorkersMax: request.MaxReplicas})
		if err != nil {
			return "", operations.Retryable("serverless_endpoint_ensure_failed", err)
		}
		if err = store.SetReplicaProviderIdentity(ctx, replica.ID, "", endpoint.ID); err != nil {
			return "", classify("serverless_identity_failed", err)
		}
		endpointURL := provider.EndpointURL(endpoint.ID)
		details, _ := json.Marshal(map[string]any{"workers_min": endpoint.WorkersMin, "workers_max": endpoint.WorkersMax, "workers_observed": endpoint.Workers, "template_id": endpoint.TemplateID})
		if err = store.ObserveReplica(ctx, replica.ID, "ready", endpointURL, "healthy", string(details), time.Now()); err != nil {
			return "", classify("serverless_observation_failed", err)
		}
		targetName := request.Name + "-serverless"
		target, err := store.AddTargetForTenant(ctx, request.TenantID, domain.Target{Name: targetName, URL: endpointURL, Provider: "runpod-serverless", Runtime: "vllm", Health: "healthy", UpstreamModel: request.Model, ProviderResourceID: endpoint.ID, ProviderDetails: string(details)})
		if err != nil {
			return "", classify("serverless_target_failed", err)
		}
		if _, err = store.ApplyDeploymentForTenant(ctx, request.TenantID, domain.Deployment{Name: request.Name, Model: request.Model, ComputeMode: "serverless", RoutingStrategy: resolved.Deployment.RoutingStrategy, MinReplicas: 0, MaxReplicas: request.MaxReplicas, AutoscalingEnabled: false}, []string{target.Name}); err != nil {
			return "", classify("serverless_routing_failed", err)
		}
		_ = checkpoint(ctx, store, operation, "serverless.endpoint", "succeeded", map[string]any{"endpoint_id": endpoint.ID, "workers_min": 0, "workers_max": request.MaxReplicas}, 100, "RunPod Serverless endpoint registered; workers scale from zero on demand")
		result, _ := json.Marshal(map[string]any{"deployment_id": request.DeploymentID, "endpoint_id": endpoint.ID, "endpoint": endpointURL, "workers_min": 0, "workers_max": request.MaxReplicas})
		return string(result), nil
	}

	remove := func(ctx context.Context, operation domain.Operation) (string, error) {
		var request DeleteRequest
		if err := json.Unmarshal([]byte(operation.RequestJSON), &request); err != nil || request.Name == "" || request.TenantID == "" {
			return "", operations.Permanent("invalid_request", errors.New("deployment name and tenant are required"))
		}
		if request.DeploymentID == "" {
			resolved, resolveErr := store.ResolveForTenant(ctx, request.TenantID, request.Name)
			if resolveErr != nil {
				return "", operations.Permanent("deployment_missing", resolveErr)
			}
			request.DeploymentID = resolved.Deployment.ID
		}
		replicas, err := store.ReplicasForDeployment(ctx, request.TenantID, request.DeploymentID)
		if err != nil {
			return "", operations.Retryable("replica_lookup_failed", err)
		}
		var targetURLs []string
		for _, replica := range replicas {
			if replica.Provider != "runpod-serverless" || replica.LifecycleState == "deleted" {
				continue
			}
			if err = provider.DeleteEndpoint(ctx, replica.ProviderResourceID); err != nil {
				return "", operations.Retryable("serverless_endpoint_delete_failed", err)
			}
			endpoints, listErr := provider.ListEndpoints(ctx)
			if listErr != nil {
				return "", operations.Retryable("serverless_endpoint_observe_failed", listErr)
			}
			for _, endpoint := range endpoints {
				if endpoint.ID == replica.ProviderResourceID {
					return "", operations.Retryable("serverless_endpoint_delete_pending", errors.New("RunPod Serverless endpoint deletion is pending"))
				}
			}
			if err = store.MarkReplicaDeleted(ctx, replica.ID); err != nil {
				return "", classify("replica_delete_persist_failed", err)
			}
			if replica.Endpoint != "" {
				targetURLs = append(targetURLs, replica.Endpoint)
			}
		}
		if err = store.DeleteDeploymentForTenant(ctx, request.TenantID, request.Name); err != nil {
			return "", classify("deployment_delete_failed", err)
		}
		for _, targetURL := range targetURLs {
			if err = store.DeleteProvisionedTargetByURL(ctx, request.TenantID, targetURL); err != nil && !errors.Is(err, domain.ErrNotFound) {
				return "", classify("target_delete_failed", err)
			}
		}
		return `{"deleted":true}`, nil
	}

	return map[string]operations.Handler{
		ServerlessConvergeKind:             converge,
		ServerlessConvergeKind + ".cancel": remove,
		ServerlessDeleteKind:               remove,
		ServerlessDeleteKind + ".cancel":   remove,
	}
}
