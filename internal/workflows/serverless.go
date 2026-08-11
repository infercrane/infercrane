package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/provision"
)

type ServerlessProvider = integration.ServerlessProvider

// ServerlessBackend binds provider-neutral endpoint lifecycle operations to
// persisted identity. A new provider supplies another backend; workflow logic
// does not need provider-specific conditionals.
type ServerlessBackend struct {
	Name, Cloud, Runtime string
	Profile              integration.ProviderProfile
	Provider             ServerlessProvider
}

func (b ServerlessBackend) validate() error {
	if b.Name == "" || b.Cloud == "" || b.Runtime == "" || b.Provider == nil {
		return errors.New("serverless backend name, cloud, runtime, and provider are required")
	}
	if err := b.Profile.Validate(); err != nil {
		return fmt.Errorf("serverless backend %q profile: %w", b.Name, err)
	}
	if b.Profile.Adapter != b.Name || b.Profile.Cloud != b.Cloud || !integration.HasMode(b.Profile, integration.ServerlessMode) {
		return fmt.Errorf("serverless backend %q profile does not match its adapter, cloud, and serverless mode", b.Name)
	}
	return nil
}

func ServerlessHandlers(store CloudStore, backend ServerlessBackend, artifactResolver ArtifactResolver) map[string]operations.Handler {
	provider := backend.Provider
	converge := func(ctx context.Context, operation domain.Operation) (string, error) {
		if err := backend.validate(); err != nil {
			return "", operations.Permanent("serverless_backend_invalid", err)
		}
		request, err := decodeCloudRequest(operation)
		if err != nil {
			return "", err
		}
		if request.ComputeMode != "serverless" || request.Cloud != backend.Cloud || (request.ProviderAdapter != "" && request.ProviderAdapter != backend.Name) || request.MinReplicas != 0 || request.MaxReplicas < 1 {
			return "", operations.Permanent("invalid_serverless_spec", fmt.Errorf("%s requires cloud=%s, compute_mode=serverless, min=0, and max>=1", backend.Name, backend.Cloud))
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
		replica, _, err := store.EnsureReplicaIntent(ctx, domain.Replica{TenantID: request.TenantID, DeploymentID: request.DeploymentID, RevisionID: request.RevisionID, Ordinal: 0, ExternalKey: externalKey, Provider: backend.Name})
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
		target, err := store.AddTargetForTenant(ctx, request.TenantID, domain.Target{Name: targetName, URL: endpointURL, Provider: backend.Name, Runtime: backend.Runtime, Health: "healthy", UpstreamModel: request.Model, ProviderResourceID: endpoint.ID, ProviderDetails: string(details)})
		if err != nil {
			return "", classify("serverless_target_failed", err)
		}
		if _, err = store.ApplyDeploymentForTenant(ctx, request.TenantID, domain.Deployment{Name: request.Name, Model: request.Model, ComputeMode: "serverless", RoutingStrategy: resolved.Deployment.RoutingStrategy, MinReplicas: 0, MaxReplicas: request.MaxReplicas, AutoscalingEnabled: false}, []string{target.Name}); err != nil {
			return "", classify("serverless_routing_failed", err)
		}
		_ = checkpoint(ctx, store, operation, "serverless.endpoint", "succeeded", map[string]any{"backend": backend.Name, "endpoint_id": endpoint.ID, "workers_min": 0, "workers_max": request.MaxReplicas}, 100, "Serverless endpoint registered; workers scale from zero on demand")
		result, _ := json.Marshal(map[string]any{"deployment_id": request.DeploymentID, "endpoint_id": endpoint.ID, "endpoint": endpointURL, "workers_min": 0, "workers_max": request.MaxReplicas})
		return string(result), nil
	}

	remove := func(ctx context.Context, operation domain.Operation) (string, error) {
		if err := backend.validate(); err != nil {
			return "", operations.Permanent("serverless_backend_invalid", err)
		}
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
			if replica.Provider != backend.Name || replica.LifecycleState == "deleted" {
				continue
			}
			endpoints, listErr := provider.ListEndpoints(ctx)
			if listErr != nil {
				return "", operations.Retryable("serverless_endpoint_inventory_failed", listErr)
			}
			ownedIDs := map[string]struct{}{}
			if replica.ProviderResourceID != "" {
				ownedIDs[replica.ProviderResourceID] = struct{}{}
			}
			expectedName := provision.ServerlessEndpointName(replica.ExternalKey)
			for _, endpoint := range endpoints {
				if endpoint.Name == expectedName {
					ownedIDs[endpoint.ID] = struct{}{}
				}
			}
			for endpointID := range ownedIDs {
				if err = provider.DeleteEndpoint(ctx, endpointID); err != nil {
					return "", operations.Retryable("serverless_endpoint_delete_failed", err)
				}
			}
			endpoints, listErr = provider.ListEndpoints(ctx)
			if listErr != nil {
				return "", operations.Retryable("serverless_endpoint_observe_failed", listErr)
			}
			for _, endpoint := range endpoints {
				_, identified := ownedIDs[endpoint.ID]
				if identified || endpoint.Name == expectedName {
					return "", operations.Retryable("serverless_endpoint_delete_pending", fmt.Errorf("%s endpoint deletion is pending", backend.Name))
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
			if err = store.DeleteProvisionedTargetByURL(ctx, request.TenantID, targetURL, backend.Name); err != nil && !errors.Is(err, domain.ErrNotFound) {
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
