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
	"github.com/infercrane/infercrane/internal/router"
)

const (
	ConvergeKind         = "deployment.converge"
	DeleteKind           = "deployment.delete"
	ReplicaProvisionKind = "replica.provision"
	ReplicaDeleteKind    = "replica.delete"
	ScaleKind            = "deployment.scale"
)

type CloudRequest struct {
	DeploymentID     string   `json:"deployment_id"`
	Name             string   `json:"name"`
	Model            string   `json:"model"`
	Cloud            string   `json:"cloud"`
	GPU              string   `json:"gpu"`
	Region           string   `json:"region,omitempty"`
	RuntimeVersion   string   `json:"runtime_version,omitempty"`
	RuntimeArgs      []string `json:"runtime_args,omitempty"`
	Port             int      `json:"port,omitempty"`
	MinReplicas      int      `json:"min_replicas,omitempty"`
	MaxReplicas      int      `json:"max_replicas,omitempty"`
	DesiredReplicas  int      `json:"desired_replicas,omitempty"`
	PreviousReplicas int      `json:"previous_replicas,omitempty"`
	Actor            string   `json:"actor,omitempty"`
	TenantID         string   `json:"tenant_id,omitempty"`
}

func (r CloudRequest) Validate() error {
	if r.Name == "" || r.Model == "" || r.Cloud == "" || r.GPU == "" {
		return errors.New("name, model, cloud, and gpu are required")
	}
	if r.MinReplicas < 0 || r.MaxReplicas < 0 || (r.MaxReplicas > 0 && r.MaxReplicas < r.MinReplicas) {
		return errors.New("replica bounds must satisfy 0 <= min <= max")
	}
	return nil
}

type DeleteRequest struct {
	DeploymentID string `json:"deployment_id"`
	Name         string `json:"name"`
	Actor        string `json:"actor,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
}

type CloudStore interface {
	ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error)
	EnsureReplicaIntent(context.Context, domain.Replica) (domain.Replica, bool, error)
	ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error)
	SetReplicaProviderIdentity(context.Context, string, string, string) error
	ObserveReplica(context.Context, string, string, string, string, string, time.Time) error
	MarkReplicaDeleted(context.Context, string) error
	CheckpointClaimedOperation(context.Context, string, string, int64, string, string, string, int, string) error
	AddTargetForTenant(context.Context, string, domain.Target) (domain.Target, error)
	UpdateProvisionedTarget(context.Context, string, string, string) error
	ApplyDeploymentForTenant(context.Context, string, domain.Deployment, []string) (domain.Deployment, error)
	DeleteDeploymentForTenant(context.Context, string, string) error
	RoutingGenerationMatches(context.Context, string, string) (bool, error)
	DeleteProvisionedTarget(context.Context, string, string) error
	Audit(context.Context, domain.AuditEvent) error
}

type ReplicaProvider interface {
	Handle(string) provision.ProviderHandle
	EnsureReplica(context.Context, provision.ReplicaSpec) (provision.ProviderHandle, error)
	ObserveReplica(context.Context, provision.ProviderHandle, int) (provision.Observation, error)
	DeleteReplica(context.Context, provision.ProviderHandle) error
}

type RuntimeInspector interface {
	Inspect(context.Context, string) (bool, map[string]struct{})
}

func CloudHandlers(store CloudStore, provider ReplicaProvider, runtime RuntimeInspector) map[string]operations.Handler {
	converge := func(ctx context.Context, operation domain.Operation) (string, error) {
		request, err := decodeCloudRequest(operation)
		if err != nil {
			return "", err
		}
		resolved, err := store.ResolveForTenant(ctx, request.TenantID, request.Name)
		if err != nil || (request.DeploymentID != "" && resolved.Deployment.ID != request.DeploymentID) {
			return "", operations.Permanent("deployment_missing", fmt.Errorf("resolve desired deployment: %w", err))
		}
		request.DeploymentID = resolved.Deployment.ID
		desired := request.DesiredReplicas
		if desired == 0 {
			desired = resolved.Deployment.MinReplicas
		}
		if desired < resolved.Deployment.MinReplicas || desired > resolved.Deployment.MaxReplicas {
			return "", operations.Permanent("invalid_scale_target", fmt.Errorf("desired replicas %d outside %d..%d", desired, resolved.Deployment.MinReplicas, resolved.Deployment.MaxReplicas))
		}
		targetNames := make([]string, 0, desired)
		targetURLs := make([]string, 0, desired)
		resourceIDs := make([]string, 0, desired)
		for ordinal := 0; ordinal < desired; ordinal++ {
			targetName, targetURL, resourceID, ensureErr := ensureCloudReplica(ctx, store, provider, runtime, operation, request, ordinal)
			if ensureErr != nil {
				return "", ensureErr
			}
			targetNames = append(targetNames, targetName)
			targetURLs = append(targetURLs, targetURL)
			resourceIDs = append(resourceIDs, resourceID)
		}
		deployment, err := store.ApplyDeploymentForTenant(ctx, request.TenantID, domain.Deployment{Name: request.Name, Model: request.Model, RoutingStrategy: resolved.Deployment.RoutingStrategy, MinReplicas: resolved.Deployment.MinReplicas, MaxReplicas: resolved.Deployment.MaxReplicas, AutoscalingEnabled: resolved.Deployment.AutoscalingEnabled}, targetNames)
		if err != nil {
			return "", classify("routing_registration_failed", err)
		}
		replicas, err := store.ReplicasForDeployment(ctx, request.TenantID, request.DeploymentID)
		if err != nil {
			return "", operations.Retryable("replica_lookup_failed", err)
		}
		var draining []domain.Replica
		for _, replica := range replicas {
			if replica.Ordinal >= desired && replica.LifecycleState != "deleted" {
				draining = append(draining, replica)
				_ = store.ObserveReplica(ctx, replica.ID, "draining", replica.Endpoint, replica.Health, replica.ProviderDetails, time.Now())
			}
		}
		if len(draining) > 0 {
			matched, matchErr := store.RoutingGenerationMatches(ctx, request.DeploymentID, router.WorkerSetHash(resolved.Deployment.RoutingStrategy, targetURLs))
			if matchErr != nil {
				return "", operations.Retryable("router_generation_lookup_failed", matchErr)
			}
			if !matched {
				_ = checkpoint(ctx, store, operation, "deployment.drain", "waiting", map[string]int{"replicas": len(draining)}, 80, "Waiting for router generation to withdraw draining replicas")
				return "", operations.Retryable("router_drain_pending", errors.New("router has not published the reduced worker set"))
			}
			for _, replica := range draining {
				handle := provider.Handle(replica.ExternalKey)
				handle.RequestID, handle.ResourceID = replica.ProviderRequestID, replica.ProviderResourceID
				if err = provider.DeleteReplica(ctx, handle); err != nil {
					return "", operations.Retryable("provider_delete_failed", err)
				}
				observation, observeErr := provider.ObserveReplica(ctx, handle, request.Port)
				if observeErr != nil {
					return "", operations.Retryable("provider_delete_observe_failed", observeErr)
				}
				if observation.Exists {
					return "", operations.Retryable("provider_delete_pending", errors.New("provider resource deletion is pending"))
				}
				if err = store.MarkReplicaDeleted(ctx, replica.ID); err != nil {
					return "", classify("replica_delete_persist_failed", err)
				}
				if err = store.DeleteProvisionedTarget(ctx, request.TenantID, fmt.Sprintf("%s-r%d", request.Name, replica.Ordinal)); err != nil {
					return "", classify("target_delete_failed", err)
				}
			}
		}
		_ = checkpoint(ctx, store, operation, "deployment.route", "succeeded", map[string]any{"targets": targetNames}, 95, "Healthy capacity published")
		result, _ := json.Marshal(map[string]any{"deployment_id": deployment.ID, "resource_ids": resourceIDs, "replicas": len(targetNames)})
		return string(result), nil
	}

	cleanup := func(ctx context.Context, operation domain.Operation) (string, error) {
		request, err := decodeDeleteCompatible(operation)
		if err != nil {
			return "", err
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
		for _, replica := range replicas {
			if replica.LifecycleState == "deleted" {
				continue
			}
			handle := provider.Handle(replica.ExternalKey)
			if replica.ProviderResourceID != "" {
				handle.ResourceID = replica.ProviderResourceID
			}
			_ = store.ObserveReplica(ctx, replica.ID, "deleting", replica.Endpoint, replica.Health, replica.ProviderDetails, time.Now())
			if err = provider.DeleteReplica(ctx, handle); err != nil {
				return "", operations.Retryable("provider_delete_failed", err)
			}
			observation, observeErr := provider.ObserveReplica(ctx, handle, 8000)
			if observeErr != nil {
				return "", operations.Retryable("provider_delete_observe_failed", observeErr)
			}
			if observation.Exists {
				return "", operations.Retryable("provider_delete_pending", errors.New("provider resource deletion is pending"))
			}
			if err = store.MarkReplicaDeleted(ctx, replica.ID); err != nil {
				return "", classify("replica_delete_persist_failed", err)
			}
		}
		if err = store.DeleteDeploymentForTenant(ctx, request.TenantID, request.Name); err != nil {
			return "", classify("deployment_delete_failed", err)
		}
		_ = checkpoint(ctx, store, operation, "deployment.delete", "succeeded", map[string]int{"replicas": len(replicas)}, 95, "Provider resources deleted")
		return `{"deleted":true}`, nil
	}
	cancelScale := func(ctx context.Context, operation domain.Operation) (string, error) {
		request, err := decodeCloudRequest(operation)
		if err != nil {
			return "", err
		}
		if request.PreviousReplicas < 1 {
			return "", operations.Permanent("invalid_scale_cancel", errors.New("previous replica count is missing"))
		}
		request.DesiredReplicas = request.PreviousReplicas
		operation.RequestJSON = mustJSON(request)
		return converge(ctx, operation)
	}

	return map[string]operations.Handler{
		ConvergeKind: converge, ReplicaProvisionKind: converge, ReplicaDeleteKind: converge, ScaleKind: converge,
		DeleteKind:               cleanup,
		ConvergeKind + ".cancel": cleanup, ReplicaProvisionKind + ".cancel": cleanup,
		ScaleKind + ".cancel": cancelScale,
	}
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func ensureCloudReplica(ctx context.Context, store CloudStore, provider ReplicaProvider, runtime RuntimeInspector, operation domain.Operation, request CloudRequest, ordinal int) (string, string, string, error) {
	externalKey := fmt.Sprintf("%s-r%d", request.DeploymentID, ordinal)
	replica, _, err := store.EnsureReplicaIntent(ctx, domain.Replica{TenantID: request.TenantID, DeploymentID: request.DeploymentID, Ordinal: ordinal, ExternalKey: externalKey, Provider: "skypilot"})
	if err != nil {
		return "", "", "", classify("replica_intent_failed", err)
	}
	handle := provider.Handle(externalKey)
	if err = store.SetReplicaProviderIdentity(ctx, replica.ID, "", handle.ResourceID); err != nil {
		return "", "", "", classify("replica_identity_failed", err)
	}
	step := fmt.Sprintf("replica.%d", ordinal)
	if err = checkpoint(ctx, store, operation, step+".intent", "succeeded", map[string]string{"replica_id": replica.ID, "external_key": externalKey, "resource_id": handle.ResourceID}, 15, "Replica identity persisted"); err != nil {
		return "", "", "", err
	}
	ensured, err := provider.EnsureReplica(ctx, provision.ReplicaSpec{ExternalKey: externalKey, RequestID: replica.ProviderRequestID, Name: fmt.Sprintf("%s-r%d", request.Name, ordinal), Model: request.Model, Cloud: request.Cloud, GPU: request.GPU, Region: request.Region, RuntimeVersion: request.RuntimeVersion, RuntimeArgs: request.RuntimeArgs, Port: request.Port})
	if err != nil {
		return "", "", "", operations.Retryable("provider_ensure_failed", err)
	}
	if err = store.SetReplicaProviderIdentity(ctx, replica.ID, ensured.RequestID, ensured.ResourceID); err != nil {
		return "", "", "", classify("provider_identity_failed", err)
	}
	if err = checkpoint(ctx, store, operation, step+".ensure", "succeeded", ensured, 40, "Provider accepted replica"); err != nil {
		return "", "", "", err
	}
	observation, err := provider.ObserveReplica(ctx, ensured, request.Port)
	if err != nil {
		return "", "", "", operations.Retryable("provider_observe_failed", err)
	}
	if !observation.Exists {
		return "", "", "", operations.Retryable("provider_not_visible", errors.New("replica is not visible in provider inventory yet"))
	}
	if observation.State != "ready" || observation.Endpoint == "" {
		_ = store.ObserveReplica(ctx, replica.ID, observation.State, observation.Endpoint, "starting", observation.Details, time.Now())
		_ = checkpoint(ctx, store, operation, step+".ready", "waiting", observation, 55, "Waiting for provider endpoint")
		return "", "", "", operations.Retryable("replica_starting", errors.New("replica is not ready"))
	}
	ready, models := runtime.Inspect(ctx, observation.Endpoint)
	if _, present := models[request.Model]; !ready || !present {
		_ = store.ObserveReplica(ctx, replica.ID, "starting", observation.Endpoint, "starting", observation.Details, time.Now())
		_ = checkpoint(ctx, store, operation, step+".runtime", "waiting", observation, 70, "Waiting for vLLM model readiness")
		return "", "", "", operations.Retryable("runtime_starting", errors.New("vLLM model is not ready"))
	}
	if err = store.ObserveReplica(ctx, replica.ID, "ready", observation.Endpoint, "healthy", observation.Details, time.Now()); err != nil {
		return "", "", "", classify("observation_failed", err)
	}
	targetName := fmt.Sprintf("%s-r%d", request.Name, ordinal)
	target, err := store.AddTargetForTenant(ctx, request.TenantID, domain.Target{Name: targetName, URL: observation.Endpoint, Provider: "skypilot", Runtime: "vllm", UpstreamModel: request.Model})
	if err != nil {
		return "", "", "", classify("target_registration_failed", err)
	}
	if err = store.UpdateProvisionedTarget(ctx, target.ID, ensured.ResourceID, observation.Details); err != nil {
		return "", "", "", classify("target_metadata_failed", err)
	}
	if err = store.ObserveReplica(ctx, replica.ID, "active", observation.Endpoint, "healthy", observation.Details, time.Now()); err != nil {
		return "", "", "", classify("activation_failed", err)
	}
	return targetName, observation.Endpoint, ensured.ResourceID, nil
}

func decodeCloudRequest(operation domain.Operation) (CloudRequest, error) {
	var request CloudRequest
	if err := json.Unmarshal([]byte(operation.RequestJSON), &request); err != nil {
		return request, operations.Permanent("invalid_request", err)
	}
	if err := request.Validate(); err != nil {
		return request, operations.Permanent("invalid_request", err)
	}
	return request, nil
}

func decodeDeleteCompatible(operation domain.Operation) (DeleteRequest, error) {
	var request DeleteRequest
	if err := json.Unmarshal([]byte(operation.RequestJSON), &request); err != nil {
		return request, operations.Permanent("invalid_request", err)
	}
	if request.Name == "" || request.TenantID == "" {
		return request, operations.Permanent("invalid_request", errors.New("name and tenant_id are required"))
	}
	return request, nil
}

func checkpoint(ctx context.Context, store CloudStore, operation domain.Operation, step, status string, payload any, progress int, message string) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return store.CheckpointClaimedOperation(ctx, operation.ID, operation.LeaseOwner, operation.LeaseGeneration, step, status, string(encoded), progress, message)
}

func classify(code string, err error) error {
	if errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrNotFound) {
		return operations.Permanent(code, err)
	}
	return operations.Retryable(code, err)
}
