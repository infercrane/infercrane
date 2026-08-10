package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/router"
	"github.com/infercrane/infercrane/internal/support"
)

const (
	ConvergeKind           = "deployment.converge"
	DeleteKind             = "deployment.delete"
	ServerlessConvergeKind = "deployment.serverless.converge"
	ServerlessDeleteKind   = "deployment.serverless.delete"
	ReplicaProvisionKind   = "replica.provision"
	ReplicaDeleteKind      = "replica.delete"
	ScaleKind              = "deployment.scale"
)

type CloudRequest struct {
	DeploymentID           string   `json:"deployment_id"`
	Name                   string   `json:"name"`
	Model                  string   `json:"model"`
	Runtime                string   `json:"runtime,omitempty"`
	Cloud                  string   `json:"cloud"`
	ComputeMode            string   `json:"compute_mode,omitempty"`
	GPU                    string   `json:"gpu"`
	Region                 string   `json:"region,omitempty"`
	RuntimeVersion         string   `json:"runtime_version,omitempty"`
	ModelRevision          string   `json:"model_revision,omitempty"`
	ImmutableModelRevision string   `json:"immutable_model_revision,omitempty"`
	RevisionID             string   `json:"revision_id,omitempty"`
	RuntimeArgs            []string `json:"runtime_args,omitempty"`
	Port                   int      `json:"port,omitempty"`
	MinReplicas            int      `json:"min_replicas,omitempty"`
	MaxReplicas            int      `json:"max_replicas,omitempty"`
	DesiredReplicas        int      `json:"desired_replicas,omitempty"`
	PreviousReplicas       int      `json:"previous_replicas,omitempty"`
	Candidate              bool     `json:"candidate,omitempty"`
	Actor                  string   `json:"actor,omitempty"`
	TenantID               string   `json:"tenant_id,omitempty"`
}

func (r CloudRequest) Validate() error {
	if r.Name == "" || r.Model == "" || r.Cloud == "" || r.GPU == "" {
		return errors.New("name, model, cloud, and gpu are required")
	}
	if r.ComputeMode == "" {
		r.ComputeMode = "elastic"
	}
	if r.ComputeMode != "elastic" && r.ComputeMode != "serverless" {
		return errors.New("compute mode must be elastic or serverless")
	}
	if r.MinReplicas < 0 || r.MaxReplicas < 0 || (r.MaxReplicas > 0 && r.MaxReplicas < r.MinReplicas) {
		return errors.New("replica bounds must satisfy 0 <= min <= max")
	}
	if r.ComputeMode == "serverless" && r.MinReplicas != 0 {
		return errors.New("serverless compute requires min replicas 0")
	}
	if err := support.V01().Validate(r.Runtime, r.Cloud, r.ComputeMode); err != nil {
		return err
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
	DeleteProvisionedTarget(context.Context, string, string, string) error
	DeleteProvisionedTargetByURL(context.Context, string, string, string) error
	ModelArtifactForRevision(context.Context, string, string) (domain.ModelArtifact, error)
	AttachModelArtifact(context.Context, string, string, domain.ModelArtifact) (domain.ModelArtifact, error)
	Revision(context.Context, string, string, string) (domain.DeploymentRevision, error)
	RejectCandidateRevision(context.Context, string, string, string, string) error
	PromoteGuardedCandidate(context.Context, string, string, string, []string) error
	Audit(context.Context, domain.AuditEvent) error
}

type ReplicaProvider interface {
	Handle(string) provision.ProviderHandle
	EnsureReplica(context.Context, provision.ReplicaSpec) (provision.ProviderHandle, error)
	ObserveReplica(context.Context, provision.ProviderHandle, int) (provision.Observation, error)
	DeleteReplica(context.Context, provision.ProviderHandle) error
}

// ReplicaBackend binds a provider adapter to durable identity and the runtime
// it launches. Provider support is registered at composition time rather than
// selected by conditionals inside lifecycle code.
type ReplicaBackend struct {
	Name, Cloud, Runtime string
	Provider             ReplicaProvider
}

type ReplicaBackends struct {
	byCloudRuntime map[string]ReplicaBackend
	byProvider     map[string]ReplicaBackend
}

func cloudRuntimeKey(cloud, runtime string) string { return cloud + "\x00" + runtime }

func NewReplicaBackends(backends ...ReplicaBackend) (ReplicaBackends, error) {
	registry := ReplicaBackends{byCloudRuntime: make(map[string]ReplicaBackend, len(backends)), byProvider: make(map[string]ReplicaBackend, len(backends))}
	for _, backend := range backends {
		if backend.Name == "" || backend.Cloud == "" || backend.Runtime == "" || backend.Provider == nil {
			return ReplicaBackends{}, errors.New("replica backend name, cloud, runtime, and provider are required")
		}
		key := cloudRuntimeKey(backend.Cloud, backend.Runtime)
		if _, exists := registry.byCloudRuntime[key]; exists {
			return ReplicaBackends{}, fmt.Errorf("replica backend for cloud %q and runtime %q is already registered", backend.Cloud, backend.Runtime)
		}
		if _, exists := registry.byProvider[backend.Name]; exists {
			return ReplicaBackends{}, fmt.Errorf("replica backend %q is already registered", backend.Name)
		}
		registry.byCloudRuntime[key], registry.byProvider[backend.Name] = backend, backend
	}
	return registry, nil
}

func (r ReplicaBackends) ForCloud(cloud, runtime string) (ReplicaBackend, error) {
	if runtime == "" {
		runtime = support.DefaultRuntime
	}
	backend, ok := r.byCloudRuntime[cloudRuntimeKey(cloud, runtime)]
	if !ok {
		return ReplicaBackend{}, fmt.Errorf("no replica backend is registered for cloud %q and runtime %q", cloud, runtime)
	}
	return backend, nil
}

func (r ReplicaBackends) ForProvider(name string) (ReplicaBackend, error) {
	// Replicas created before provider identity was persisted have an empty
	// adapter name. They are safe to replay only when composition is unambiguous.
	if name == "" && len(r.byProvider) == 1 {
		for _, backend := range r.byProvider {
			return backend, nil
		}
	}
	backend, ok := r.byProvider[name]
	if !ok {
		return ReplicaBackend{}, fmt.Errorf("no replica backend is registered for provider %q", name)
	}
	return backend, nil
}

type RuntimeInspector interface {
	Inspect(context.Context, string) (bool, map[string]struct{})
}

type ArtifactResolver interface {
	Resolve(context.Context, string, string) (domain.ModelArtifact, error)
}

type DrainTracker interface {
	RetiringInFlight(string) int
	HasCurrentDeployment(string) bool
}

// QualifiedV01CloudHandlers is a compatibility constructor for the single
// backend qualified by v0.1. Production composition and future integrations
// use CloudHandlersWithBackends directly.
func QualifiedV01CloudHandlers(store CloudStore, provider ReplicaProvider, runtime RuntimeInspector, artifactResolvers ...ArtifactResolver) map[string]operations.Handler {
	backends, err := NewReplicaBackends(ReplicaBackend{Name: "skypilot", Cloud: "runpod", Runtime: support.DefaultRuntime, Provider: provider})
	if err != nil {
		panic(err)
	}
	return CloudHandlersWithBackends(store, backends, runtime, artifactResolvers...)
}

func CloudHandlersWithBackends(store CloudStore, backends ReplicaBackends, runtime RuntimeInspector, artifactResolvers ...ArtifactResolver) map[string]operations.Handler {
	return CloudHandlersWithBackendsAndDrain(store, backends, runtime, nil, artifactResolvers...)
}

func CloudHandlersWithBackendsAndDrain(store CloudStore, backends ReplicaBackends, runtime RuntimeInspector, drain DrainTracker, artifactResolvers ...ArtifactResolver) map[string]operations.Handler {
	var artifactResolver ArtifactResolver
	if len(artifactResolvers) > 0 {
		artifactResolver = artifactResolvers[0]
	}
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
		backend, err := backends.ForCloud(request.Cloud, request.Runtime)
		if err != nil {
			return "", operations.Permanent("provider_backend_unavailable", err)
		}
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
		request.ImmutableModelRevision = modelArtifact.ImmutableRevision
		_ = checkpoint(ctx, store, operation, "model.artifact", "succeeded", map[string]any{"identity": modelArtifact.ModelIdentity, "approximate_size_bytes": modelArtifact.ApproximateSizeBytes, "cache_state": modelArtifact.CacheState}, 10, "Immutable model artifact resolved")
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
			targetName, targetURL, resourceID, ensureErr := ensureCloudReplica(ctx, store, backend, runtime, operation, request, ordinal)
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
			if drain != nil {
				if active := drain.RetiringInFlight(resolved.Deployment.ID); active > 0 {
					_ = checkpoint(ctx, store, operation, "deployment.drain", "waiting", map[string]int{"active_requests": active}, 85, "Waiting for active requests on the withdrawn router generation")
					return "", operations.Retryable("active_requests_draining", fmt.Errorf("%d active request(s) still use the withdrawn router generation", active))
				}
			}
			for _, replica := range draining {
				replicaBackend, backendErr := backends.ForProvider(replica.Provider)
				if backendErr != nil {
					return "", operations.Permanent("provider_backend_unavailable", backendErr)
				}
				provider := replicaBackend.Provider
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
				if err = store.DeleteProvisionedTarget(ctx, request.TenantID, fmt.Sprintf("%s-r%d", request.Name, replica.Ordinal), replicaBackend.Name); err != nil {
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
		if drain != nil {
			if drain.HasCurrentDeployment(request.DeploymentID) {
				return "", operations.Retryable("router_withdrawal_pending", errors.New("router has not withdrawn the deleting deployment"))
			}
			if active := drain.RetiringInFlight(request.DeploymentID); active > 0 {
				return "", operations.Retryable("active_requests_draining", fmt.Errorf("%d active request(s) still use the deleting deployment", active))
			}
		}
		for _, replica := range replicas {
			if replica.LifecycleState == "deleted" {
				continue
			}
			replicaBackend, backendErr := backends.ForProvider(replica.Provider)
			if backendErr != nil {
				return "", operations.Permanent("provider_backend_unavailable", backendErr)
			}
			provider := replicaBackend.Provider
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
		for _, replica := range replicas {
			if replica.Endpoint != "" {
				replicaBackend, backendErr := backends.ForProvider(replica.Provider)
				if backendErr != nil {
					return "", operations.Permanent("provider_backend_unavailable", backendErr)
				}
				if err = store.DeleteProvisionedTargetByURL(ctx, request.TenantID, replica.Endpoint, replicaBackend.Name); err != nil && !errors.Is(err, domain.ErrNotFound) {
					return "", classify("target_delete_failed", err)
				}
			}
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
	var candidateCleanup operations.Handler
	candidate := func(ctx context.Context, operation domain.Operation) (string, error) {
		var rollout RolloutRequest
		if err := json.Unmarshal([]byte(operation.RequestJSON), &rollout); err != nil || rollout.Name == "" || rollout.TenantID == "" || rollout.CandidateID == "" {
			return "", operations.Permanent("invalid_request", errors.New("deployment and candidate revision are required"))
		}
		revision, err := store.Revision(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID)
		if err != nil {
			return "", operations.Permanent("candidate_not_found", fmt.Errorf("candidate revision lookup: %w", err))
		}
		if revision.Status == "rejected" {
			if _, cleanupErr := candidateCleanup(context.WithoutCancel(ctx), operation); cleanupErr != nil {
				return "", operations.Retryable("candidate_cleanup_failed", cleanupErr)
			}
			return "", operations.Permanent("candidate_rejected", errors.New("candidate revision was rejected"))
		}
		if revision.Status != "candidate" {
			return "", operations.Permanent("candidate_not_found", errors.New("revision is not the current candidate"))
		}
		var spec domain.DeploymentRevisionSpec
		if err = json.Unmarshal([]byte(revision.SpecJSON), &spec); err != nil {
			return "", operations.Permanent("invalid_candidate_spec", err)
		}
		if spec.ComputeMode != "elastic" {
			return "", operations.Permanent("unsupported_compute_mode", errors.New("candidate provisioning currently requires elastic compute"))
		}
		resolved, err := store.ResolveForTenant(ctx, rollout.TenantID, rollout.Name)
		if err != nil {
			return "", operations.Permanent("deployment_missing", err)
		}
		request := CloudRequest{DeploymentID: resolved.Deployment.ID, Name: rollout.Name, Model: spec.Model, ModelRevision: spec.ModelRevision, RevisionID: revision.ID, Cloud: spec.Cloud, GPU: spec.GPU, Region: spec.Region, RuntimeVersion: spec.RuntimeVersion, RuntimeArgs: spec.RuntimeArgs, Port: spec.Port, MinReplicas: spec.MinReplicas, MaxReplicas: spec.MaxReplicas, DesiredReplicas: spec.MinReplicas, TenantID: rollout.TenantID, Actor: rollout.Actor, Candidate: true}
		request.Runtime = spec.Runtime
		backend, backendErr := backends.ForCloud(request.Cloud, request.Runtime)
		if backendErr != nil {
			return "", operations.Permanent("provider_backend_unavailable", backendErr)
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
		request.ImmutableModelRevision = modelArtifact.ImmutableRevision
		var endpoints []string
		var resourceIDs []string
		for ordinal := 0; ordinal < request.DesiredReplicas; ordinal++ {
			_, endpoint, resourceID, ensureErr := ensureCloudReplica(ctx, store, backend, runtime, operation, request, ordinal)
			if ensureErr != nil {
				var failure operations.Failure
				if errors.As(ensureErr, &failure) && !failure.Retryable {
					reason := "candidate validation failed: " + ensureErr.Error()
					if rejectErr := store.RejectCandidateRevision(context.WithoutCancel(ctx), rollout.TenantID, rollout.Name, rollout.CandidateID, reason); rejectErr != nil && !errors.Is(rejectErr, domain.ErrConflict) {
						return "", operations.Retryable("candidate_reject_failed", rejectErr)
					}
					if _, cleanupErr := candidateCleanup(context.WithoutCancel(ctx), operation); cleanupErr != nil {
						return "", operations.Retryable("candidate_cleanup_failed", cleanupErr)
					}
				}
				return "", ensureErr
			}
			endpoints = append(endpoints, endpoint)
			resourceIDs = append(resourceIDs, resourceID)
		}
		_ = checkpoint(ctx, store, operation, "candidate.ready", "succeeded", map[string]any{"revision_id": revision.ID, "endpoints": endpoints}, 95, "Candidate capacity is ready and isolated from active routing")
		result, _ := json.Marshal(map[string]any{"candidate_id": revision.ID, "endpoints": endpoints, "resource_ids": resourceIDs, "replicas": len(endpoints)})
		return string(result), nil
	}
	candidateCleanup = func(ctx context.Context, operation domain.Operation) (string, error) {
		var rollout RolloutRequest
		if err := json.Unmarshal([]byte(operation.RequestJSON), &rollout); err != nil || rollout.Name == "" || rollout.TenantID == "" || rollout.CandidateID == "" {
			return "", operations.Permanent("invalid_request", errors.New("deployment and candidate revision are required"))
		}
		resolved, err := store.ResolveForTenant(ctx, rollout.TenantID, rollout.Name)
		if err != nil {
			return "", operations.Permanent("deployment_missing", err)
		}
		replicas, err := store.ReplicasForDeployment(ctx, rollout.TenantID, resolved.Deployment.ID)
		if err != nil {
			return "", operations.Retryable("replica_lookup_failed", err)
		}
		deleted := 0
		for _, replica := range replicas {
			if replica.RevisionID != rollout.CandidateID || replica.LifecycleState == "deleted" {
				continue
			}
			replicaBackend, backendErr := backends.ForProvider(replica.Provider)
			if backendErr != nil {
				return "", operations.Permanent("provider_backend_unavailable", backendErr)
			}
			provider := replicaBackend.Provider
			handle := provider.Handle(replica.ExternalKey)
			handle.RequestID, handle.ResourceID = replica.ProviderRequestID, replica.ProviderResourceID
			if err = provider.DeleteReplica(ctx, handle); err != nil {
				return "", operations.Retryable("provider_delete_failed", err)
			}
			observation, observeErr := provider.ObserveReplica(ctx, handle, 0)
			if observeErr != nil {
				return "", operations.Retryable("provider_delete_observe_failed", observeErr)
			}
			if observation.Exists {
				return "", operations.Retryable("provider_delete_pending", errors.New("candidate resource deletion is pending"))
			}
			if err = store.MarkReplicaDeleted(ctx, replica.ID); err != nil {
				return "", classify("replica_delete_persist_failed", err)
			}
			targetName := fmt.Sprintf("%s-%s-r%d", rollout.Name, rollout.CandidateID[:min(8, len(rollout.CandidateID))], replica.Ordinal)
			if err = store.DeleteProvisionedTarget(ctx, rollout.TenantID, targetName, replicaBackend.Name); err != nil && !errors.Is(err, domain.ErrNotFound) {
				return "", classify("target_delete_failed", err)
			}
			deleted++
		}
		result, _ := json.Marshal(map[string]any{"candidate_id": rollout.CandidateID, "deleted_replicas": deleted})
		return string(result), nil
	}
	candidateReject := func(ctx context.Context, operation domain.Operation) (string, error) {
		if _, err := candidateCleanup(ctx, operation); err != nil {
			return "", err
		}
		var rollout RolloutRequest
		if err := json.Unmarshal([]byte(operation.RequestJSON), &rollout); err != nil {
			return "", operations.Permanent("invalid_request", err)
		}
		if rollout.Reason == "" {
			return "", operations.Permanent("invalid_request", errors.New("rejection reason is required"))
		}
		if err := store.RejectCandidateRevision(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID, rollout.Reason); err != nil {
			return "", classify("candidate_reject_failed", err)
		}
		result, _ := json.Marshal(map[string]any{"candidate_id": rollout.CandidateID, "rejected": true, "reason": rollout.Reason})
		return string(result), nil
	}
	candidatePromote := func(ctx context.Context, operation domain.Operation) (string, error) {
		var rollout RolloutRequest
		if err := json.Unmarshal([]byte(operation.RequestJSON), &rollout); err != nil || rollout.Name == "" || rollout.TenantID == "" || rollout.CandidateID == "" {
			return "", operations.Permanent("invalid_request", errors.New("deployment and candidate revision are required"))
		}
		resolved, err := store.ResolveForTenant(ctx, rollout.TenantID, rollout.Name)
		if err != nil {
			return "", operations.Permanent("deployment_missing", err)
		}
		revision, err := store.Revision(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID)
		if err != nil {
			return "", operations.Permanent("candidate_not_found", err)
		}
		if revision.Status != "candidate" && resolved.Deployment.ActiveRevisionID != rollout.CandidateID {
			return "", operations.Permanent("candidate_not_found", errors.New("revision is neither current candidate nor active cutover"))
		}
		var spec domain.DeploymentRevisionSpec
		if err = json.Unmarshal([]byte(revision.SpecJSON), &spec); err != nil {
			return "", operations.Permanent("invalid_candidate_spec", err)
		}
		replicas, err := store.ReplicasForDeployment(ctx, rollout.TenantID, resolved.Deployment.ID)
		if err != nil {
			return "", operations.Retryable("replica_lookup_failed", err)
		}
		var targetNames, targetURLs []string
		for _, replica := range replicas {
			if replica.RevisionID != rollout.CandidateID || (replica.LifecycleState != "ready" && replica.LifecycleState != "active") || replica.Health != "healthy" || replica.Endpoint == "" {
				continue
			}
			targetNames = append(targetNames, fmt.Sprintf("%s-%s-r%d", rollout.Name, rollout.CandidateID[:min(8, len(rollout.CandidateID))], replica.Ordinal))
			targetURLs = append(targetURLs, replica.Endpoint)
		}
		if len(targetNames) < spec.MinReplicas {
			return "", operations.Permanent("candidate_not_ready", errors.New("candidate does not have its minimum healthy replica count"))
		}
		if err = store.PromoteGuardedCandidate(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID, targetNames); err != nil {
			return "", classify("guarded_promotion_failed", err)
		}
		expectedHash := router.WorkerSetHash(spec.RoutingStrategy, targetURLs)
		matched, err := store.RoutingGenerationMatches(ctx, resolved.Deployment.ID, expectedHash)
		if err != nil {
			return "", operations.Retryable("router_generation_lookup_failed", err)
		}
		if !matched {
			_ = checkpoint(ctx, store, operation, "candidate.route", "waiting", map[string]any{"worker_set_hash": expectedHash}, 80, "Waiting for guarded router generation")
			return "", operations.Retryable("router_cutover_pending", errors.New("candidate router generation is not active yet"))
		}
		if drain != nil {
			if active := drain.RetiringInFlight(resolved.Deployment.ID); active > 0 {
				_ = checkpoint(ctx, store, operation, "candidate.drain", "waiting", map[string]int{"active_requests": active}, 90, "Waiting for active requests on the previous revision")
				return "", operations.Retryable("active_requests_draining", fmt.Errorf("%d active request(s) still use the previous revision", active))
			}
		}
		deleted := 0
		for _, replica := range replicas {
			if replica.RevisionID == rollout.CandidateID || replica.LifecycleState == "deleted" {
				continue
			}
			replicaBackend, backendErr := backends.ForProvider(replica.Provider)
			if backendErr != nil {
				return "", operations.Permanent("provider_backend_unavailable", backendErr)
			}
			provider := replicaBackend.Provider
			handle := provider.Handle(replica.ExternalKey)
			handle.RequestID, handle.ResourceID = replica.ProviderRequestID, replica.ProviderResourceID
			if err = provider.DeleteReplica(ctx, handle); err != nil {
				return "", operations.Retryable("provider_delete_failed", err)
			}
			observation, observeErr := provider.ObserveReplica(ctx, handle, 0)
			if observeErr != nil {
				return "", operations.Retryable("provider_delete_observe_failed", observeErr)
			}
			if observation.Exists {
				return "", operations.Retryable("provider_delete_pending", errors.New("old revision resource deletion is pending"))
			}
			if err = store.MarkReplicaDeleted(ctx, replica.ID); err != nil {
				return "", classify("replica_delete_persist_failed", err)
			}
			if replica.Endpoint != "" {
				if err = store.DeleteProvisionedTargetByURL(ctx, rollout.TenantID, replica.Endpoint, replicaBackend.Name); err != nil {
					return "", classify("target_delete_failed", err)
				}
			}
			deleted++
		}
		_ = checkpoint(ctx, store, operation, "candidate.drain", "succeeded", map[string]int{"deleted_replicas": deleted}, 95, "Old revision capacity drained and deleted")
		result, _ := json.Marshal(map[string]any{"candidate_id": rollout.CandidateID, "promoted": true, "deleted_old_replicas": deleted})
		return string(result), nil
	}
	candidatePromoteCancel := func(ctx context.Context, operation domain.Operation) (string, error) {
		var rollout RolloutRequest
		if err := json.Unmarshal([]byte(operation.RequestJSON), &rollout); err != nil {
			return "", operations.Permanent("invalid_request", err)
		}
		resolved, err := store.ResolveForTenant(ctx, rollout.TenantID, rollout.Name)
		if err != nil {
			return "", operations.Permanent("deployment_missing", err)
		}
		if resolved.Deployment.ActiveRevisionID == rollout.CandidateID {
			return candidatePromote(ctx, operation)
		}
		return candidateCleanup(ctx, operation)
	}

	return map[string]operations.Handler{
		ConvergeKind: converge, ReplicaProvisionKind: converge, ReplicaDeleteKind: converge, ScaleKind: converge,
		DeleteKind:               cleanup,
		ConvergeKind + ".cancel": cleanup, ReplicaProvisionKind + ".cancel": cleanup,
		ScaleKind + ".cancel":            cancelScale,
		RolloutProvisionKind:             candidate,
		RolloutProvisionKind + ".cancel": candidateCleanup,
		RolloutRejectKind:                candidateReject,
		RolloutPromoteKind:               candidatePromote,
		RolloutPromoteKind + ".cancel":   candidatePromoteCancel,
	}
}

func mustJSON(value any) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func ensureCloudReplica(ctx context.Context, store CloudStore, backend ReplicaBackend, runtime RuntimeInspector, operation domain.Operation, request CloudRequest, ordinal int) (string, string, string, error) {
	provider := backend.Provider
	externalKey := fmt.Sprintf("%s-r%d", request.DeploymentID, ordinal)
	if request.Candidate {
		externalKey = fmt.Sprintf("%s-%s-r%d", request.DeploymentID, request.RevisionID, ordinal)
	}
	replica, _, err := store.EnsureReplicaIntent(ctx, domain.Replica{TenantID: request.TenantID, DeploymentID: request.DeploymentID, RevisionID: request.RevisionID, Ordinal: ordinal, ExternalKey: externalKey, Provider: backend.Name})
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
	ensured, err := provider.EnsureReplica(ctx, provision.ReplicaSpec{ExternalKey: externalKey, RequestID: replica.ProviderRequestID, Name: fmt.Sprintf("%s-r%d", request.Name, ordinal), Model: request.Model, ModelRevision: request.ImmutableModelRevision, Cloud: request.Cloud, GPU: request.GPU, Region: request.Region, RuntimeVersion: request.RuntimeVersion, RuntimeArgs: request.RuntimeArgs, Port: request.Port})
	if err != nil {
		if errors.Is(err, provision.ErrRequestFailed) {
			return "", "", "", operations.Retryable("provider_request_failed", fmt.Errorf("provider launch failed before the replica became ready; requested capacity may be unavailable: %w", err))
		}
		return "", "", "", operations.Retryable("provider_ensure_failed", err)
	}
	if err = store.SetReplicaProviderIdentity(ctx, replica.ID, ensured.RequestID, ensured.ResourceID); err != nil {
		return "", "", "", classify("provider_identity_failed", err)
	}
	if err = checkpoint(ctx, store, operation, step+".ensure", "succeeded", ensured, 40, "Replica request accepted; provider is allocating capacity"); err != nil {
		return "", "", "", err
	}
	observation, err := provider.ObserveReplica(ctx, ensured, request.Port)
	if err != nil {
		return "", "", "", operations.Retryable("provider_observe_failed", err)
	}
	if !observation.Exists {
		return "", "", "", operations.Retryable("provider_not_visible", errors.New("replica is not visible in provider inventory yet"))
	}
	if observation.State == "failed" {
		_ = store.ObserveReplica(ctx, replica.ID, "failed", observation.Endpoint, "unhealthy", observation.Details, time.Now())
		if deleteErr := provider.DeleteReplica(ctx, ensured); deleteErr != nil {
			return "", "", "", operations.Retryable("runtime_bootstrap_cleanup_failed", fmt.Errorf("runtime bootstrap failed and provider cleanup did not complete: %w", deleteErr))
		}
		if deleteErr := store.MarkReplicaDeleted(ctx, replica.ID); deleteErr != nil {
			return "", "", "", classify("runtime_bootstrap_cleanup_failed", deleteErr)
		}
		return "", "", "", operations.Permanent("runtime_bootstrap_failed", errors.New("runtime process exited before readiness; inspect provider details for the vLLM error"))
	}
	if observation.State != "ready" || observation.Endpoint == "" {
		_ = store.ObserveReplica(ctx, replica.ID, observation.State, observation.Endpoint, "starting", observation.Details, time.Now())
		_ = checkpoint(ctx, store, operation, step+".ready", "waiting", observation, 55, providerCapacityMessage(observation))
		return "", "", "", operations.Retryable("replica_starting", errors.New("provider is allocating capacity or bootstrapping the worker"))
	}
	ready, models := runtime.Inspect(ctx, observation.Endpoint)
	_, present := models[request.Model]
	if ready && !present {
		_ = store.ObserveReplica(ctx, replica.ID, "failed", observation.Endpoint, "unhealthy", observation.Details, time.Now())
		return "", "", "", operations.Permanent("runtime_model_mismatch", fmt.Errorf("healthy %s runtime does not serve candidate model", backend.Runtime))
	}
	if !ready {
		_ = store.ObserveReplica(ctx, replica.ID, "starting", observation.Endpoint, "starting", observation.Details, time.Now())
		_ = checkpoint(ctx, store, operation, step+".runtime", "waiting", observation, 70, "Worker reachable; waiting for model artifact and runtime readiness")
		return "", "", "", operations.Retryable("runtime_starting", fmt.Errorf("worker is reachable; %s is downloading the model artifact or initializing the runtime", backend.Runtime))
	}
	if err = store.ObserveReplica(ctx, replica.ID, "ready", observation.Endpoint, "healthy", observation.Details, time.Now()); err != nil {
		return "", "", "", classify("observation_failed", err)
	}
	targetName := fmt.Sprintf("%s-r%d", request.Name, ordinal)
	if request.Candidate {
		targetName = fmt.Sprintf("%s-%s-r%d", request.Name, request.RevisionID[:min(8, len(request.RevisionID))], ordinal)
	}
	target, err := store.AddTargetForTenant(ctx, request.TenantID, domain.Target{Name: targetName, URL: observation.Endpoint, Provider: backend.Name, Runtime: backend.Runtime, UpstreamModel: request.Model})
	if err != nil {
		return "", "", "", classify("target_registration_failed", err)
	}
	if err = store.UpdateProvisionedTarget(ctx, target.ID, ensured.ResourceID, observation.Details); err != nil {
		return "", "", "", classify("target_metadata_failed", err)
	}
	lifecycle := "active"
	if request.Candidate {
		lifecycle = "ready"
	}
	if err = store.ObserveReplica(ctx, replica.ID, lifecycle, observation.Endpoint, "healthy", observation.Details, time.Now()); err != nil {
		return "", "", "", classify("activation_failed", err)
	}
	return targetName, observation.Endpoint, ensured.ResourceID, nil
}

// providerCapacityMessage exposes only boundaries reported by the provider. It
// deliberately does not label provider placement time as container, artifact,
// or runtime startup time when no worker endpoint exists yet.
func providerCapacityMessage(observation provision.Observation) string {
	fields := map[string]string{}
	var value any
	if json.Unmarshal([]byte(observation.Details), &value) == nil {
		collectProviderFields(value, fields)
	}
	state := fields["status"]
	if state == "" {
		state = observation.State
	}
	reason, region := fields["init_status_reason"], fields["region"]
	message := "Provider capacity"
	if reason != "" {
		message += ": " + reason
	} else if state != "" {
		message += ": " + state
	}
	if region != "" && !strings.Contains(strings.ToLower(message), strings.ToLower(region)) {
		message += " (region " + region + ")"
	}
	if observation.Endpoint == "" {
		message += "; worker endpoint not exposed yet"
	} else {
		message += "; worker endpoint exposed, bootstrap still in progress"
	}
	return message
}

func collectProviderFields(value any, fields map[string]string) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			collectProviderFields(item, fields)
		}
	case map[string]any:
		for _, key := range []string{"init_status_reason", "status", "region"} {
			if fields[key] == "" {
				if text, ok := typed[key].(string); ok && strings.TrimSpace(text) != "" {
					fields[key] = strings.TrimSpace(text)
				}
			}
		}
		for _, item := range typed {
			collectProviderFields(item, fields)
		}
	}
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
