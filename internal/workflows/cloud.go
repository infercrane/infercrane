package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/router"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/servingcontract"
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
	DeploymentID           string                   `json:"deployment_id"`
	Name                   string                   `json:"name"`
	EndpointName           string                   `json:"endpoint_name,omitempty"`
	Model                  string                   `json:"model"`
	Runtime                string                   `json:"runtime,omitempty"`
	Cloud                  string                   `json:"cloud"`
	ProviderAdapter        string                   `json:"provider_adapter,omitempty"`
	ComputeMode            string                   `json:"compute_mode,omitempty"`
	GPU                    string                   `json:"gpu"`
	Region                 string                   `json:"region,omitempty"`
	RuntimeVersion         string                   `json:"runtime_version,omitempty"`
	ModelRevision          string                   `json:"model_revision,omitempty"`
	ImmutableModelRevision string                   `json:"immutable_model_revision,omitempty"`
	RevisionID             string                   `json:"revision_id,omitempty"`
	RuntimeArgs            []string                 `json:"runtime_args,omitempty"`
	Port                   int                      `json:"port,omitempty"`
	MinReplicas            int                      `json:"min_replicas,omitempty"`
	MaxReplicas            int                      `json:"max_replicas,omitempty"`
	DesiredReplicas        int                      `json:"desired_replicas,omitempty"`
	PreviousReplicas       int                      `json:"previous_replicas,omitempty"`
	Candidate              bool                     `json:"candidate,omitempty"`
	Actor                  string                   `json:"actor,omitempty"`
	TenantID               string                   `json:"tenant_id,omitempty"`
	Workload               runtimecontract.Workload `json:"workload,omitzero"`
	Serving                servingcontract.Topology `json:"serving,omitzero"`
}

var endpointNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)

func (r *CloudRequest) Validate() error {
	providedPort := r.Port
	if r.Runtime == "" {
		r.Runtime = support.DefaultRuntime
	}
	if r.RuntimeVersion == "" {
		if r.Runtime == support.DefaultRuntime {
			r.RuntimeVersion = support.DefaultRuntimeVersion
		} else if r.Runtime == "sglang" {
			r.RuntimeVersion = support.SGLangRuntimeVersion
		}
	}
	r.Workload = support.NormalizeWorkload(r.Runtime, r.Workload)
	if !r.Workload.Empty() {
		if providedPort != 0 && providedPort != r.Workload.Port {
			return errors.New("port conflicts with workload.port")
		}
		r.Port = r.Workload.Port
	}
	if r.Name == "" || r.Model == "" || r.Cloud == "" || r.GPU == "" {
		return errors.New("name, model, cloud, and gpu are required")
	}
	if r.EndpointName == "" {
		r.EndpointName = r.Name
	}
	if !endpointNamePattern.MatchString(r.EndpointName) {
		return errors.New("endpoint_name must contain 1 to 64 lowercase letters, numbers, dots, underscores, or dashes and must start and end with a letter or number")
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
	r.Serving = r.Serving.Normalize()
	if err := r.Serving.Validate(r.Runtime, r.Cloud, r.ProviderAdapter, r.MinReplicas, r.MaxReplicas); err != nil {
		return fmt.Errorf("serving topology: %w", err)
	}
	if r.Runtime != "" && r.Runtime != support.DefaultRuntime && r.MaxReplicas > r.MinReplicas {
		return errors.New("autoscaling is not yet qualified for this runtime; set min and max replicas equal")
	}
	if r.Cloud == "aws" && r.Region == "" {
		return errors.New("AWS BYOC requires an explicit region")
	}
	if r.Runtime == "custom-oci" && r.Workload.Empty() {
		return errors.New("custom-oci runtime requires an explicit workload contract")
	}
	if !r.Workload.Empty() {
		if r.ComputeMode != "elastic" {
			return errors.New("custom OCI workloads currently require elastic compute")
		}
		if err := r.Workload.Validate(); err != nil {
			return fmt.Errorf("runtime workload: %w", err)
		}
	}
	if err := support.V1().Validate(r.Runtime, r.Cloud, r.ComputeMode); err != nil {
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
	PublishDeploymentEndpoint(context.Context, string, string, string) (domain.ResolvedEndpoint, error)
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

type releaseGuardMonitoringStore interface {
	ReleaseGuardPolicy(context.Context, string, string) (domain.ReleaseGuardPolicy, error)
	EnsureReleaseGuardMonitor(context.Context, string, string, string, string, time.Duration) (domain.ReleaseGuardMonitor, error)
	ReleaseGuardMonitor(context.Context, string, string, string) (domain.ReleaseGuardMonitor, error)
	EvaluateReleaseGuardMonitor(context.Context, string, string, string, time.Duration) (domain.ReleaseGuardEvaluation, error)
	RollbackGuardedPromotion(context.Context, string, string, string, string, string, []string) error
	MarkReleaseGuardMonitorRolledBack(context.Context, string, string, string) error
	PreviousRevisionID(context.Context, string, string, string) (string, error)
}

type ReplicaProvider = integration.ElasticProvider

type CapacityAdvisor interface {
	Availability(context.Context, provision.AvailabilityRequest) (provision.Availability, error)
}
type CapacityEvidenceStore interface {
	RecordCapacityEvidence(context.Context, domain.CapacityEvidence) (domain.CapacityEvidence, error)
}
type CapacityOperationStore interface {
	RecordCapacityOperation(context.Context, domain.CapacityOperation) (domain.CapacityOperation, error)
}

// ReplicaBackend binds a provider adapter to durable identity and the runtime
// it launches. Provider support is registered at composition time rather than
// selected by conditionals inside lifecycle code.
type ReplicaBackend struct {
	Name, Cloud, Runtime string
	Default              bool
	Profile              integration.ProviderProfile
	Provider             ReplicaProvider
	Capacity             CapacityAdvisor
}

type ReplicaBackends struct {
	byCloudRuntime map[string][]ReplicaBackend
	byProvider     map[string]ReplicaBackend
}

func cloudRuntimeKey(cloud, runtime string) string { return cloud + "\x00" + runtime }

func NewReplicaBackends(backends ...ReplicaBackend) (ReplicaBackends, error) {
	registry := ReplicaBackends{byCloudRuntime: make(map[string][]ReplicaBackend, len(backends)), byProvider: make(map[string]ReplicaBackend, len(backends))}
	for _, backend := range backends {
		if backend.Name == "" || backend.Cloud == "" || backend.Runtime == "" || backend.Provider == nil {
			return ReplicaBackends{}, errors.New("replica backend name, cloud, runtime, and provider are required")
		}
		if err := backend.Profile.Validate(); err != nil {
			return ReplicaBackends{}, fmt.Errorf("replica backend %q profile: %w", backend.Name, err)
		}
		if backend.Profile.Adapter != backend.Name || backend.Profile.Cloud != backend.Cloud || !integration.HasMode(backend.Profile, integration.ElasticMode) {
			return ReplicaBackends{}, fmt.Errorf("replica backend %q profile does not match its adapter, cloud, and elastic mode", backend.Name)
		}
		key := cloudRuntimeKey(backend.Cloud, backend.Runtime)
		for _, existing := range registry.byCloudRuntime[key] {
			if existing.Name == backend.Name {
				return ReplicaBackends{}, fmt.Errorf("replica backend %q for cloud %q and runtime %q is already registered", backend.Name, backend.Cloud, backend.Runtime)
			}
			if existing.Default && backend.Default {
				return ReplicaBackends{}, fmt.Errorf("cloud %q and runtime %q have multiple default replica backends", backend.Cloud, backend.Runtime)
			}
		}
		registry.byCloudRuntime[key] = append(registry.byCloudRuntime[key], backend)
		if existing, exists := registry.byProvider[backend.Name]; exists {
			if existing.Cloud != backend.Cloud {
				return ReplicaBackends{}, fmt.Errorf("replica backend %q cannot bind different provider implementations", backend.Name)
			}
		} else {
			registry.byProvider[backend.Name] = backend
		}
	}
	return registry, nil
}

func (r ReplicaBackends) ForCloud(cloud, runtime string) (ReplicaBackend, error) {
	return r.ForAdapter(cloud, runtime, "")
}

func (r ReplicaBackends) ForAdapter(cloud, runtime, adapter string) (ReplicaBackend, error) {
	if runtime == "" {
		runtime = support.DefaultRuntime
	}
	backends := r.byCloudRuntime[cloudRuntimeKey(cloud, runtime)]
	if len(backends) == 0 {
		return ReplicaBackend{}, fmt.Errorf("no replica backend is registered for cloud %q and runtime %q", cloud, runtime)
	}
	if adapter != "" {
		for _, backend := range backends {
			if backend.Name == adapter {
				return backend, nil
			}
		}
		return ReplicaBackend{}, fmt.Errorf("replica backend %q is not registered for cloud %q and runtime %q", adapter, cloud, runtime)
	}
	if len(backends) == 1 {
		return backends[0], nil
	}
	for _, backend := range backends {
		if backend.Default {
			return backend, nil
		}
	}
	return ReplicaBackend{}, fmt.Errorf("cloud %q and runtime %q have multiple replica backends; provider_adapter is required", cloud, runtime)
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

type RuntimeInspector = integration.RuntimeInspector

type ArtifactResolver interface {
	Resolve(context.Context, string, string) (domain.ModelArtifact, error)
}

type DrainTracker interface {
	RetiringInFlight(string) int
	HasCurrentDeployment(string) bool
}

// QualifiedCloudHandlers is a compatibility constructor for a single backend.
// Production composition uses CloudHandlersWithBackends directly.
func QualifiedCloudHandlers(store CloudStore, provider ReplicaProvider, runtime RuntimeInspector, artifactResolvers ...ArtifactResolver) map[string]operations.Handler {
	backends, err := NewReplicaBackends(ReplicaBackend{Name: "skypilot", Cloud: "runpod", Runtime: support.DefaultRuntime, Profile: builtinElasticProfile("skypilot", "runpod"), Provider: provider})
	if err != nil {
		panic(err)
	}
	runtimes, err := integration.NewRuntimeBackends(integration.RuntimeBackend{Profile: builtinRuntimeProfile(support.DefaultRuntime), Inspector: runtime})
	if err != nil {
		panic(err)
	}
	return CloudHandlersWithBackends(store, backends, runtimes, artifactResolvers...)
}

func builtinElasticProfile(adapter, cloud string) integration.ProviderProfile {
	return integration.ProviderProfile{Adapter: adapter, Cloud: cloud, ContractVersion: integration.ProviderContractV1, AdapterVersion: "builtin", Modes: []integration.ComputeMode{integration.ElasticMode}, Qualification: []integration.Qualification{{State: integration.QualificationRegistered}}}
}

func builtinRuntimeProfile(name string) integration.RuntimeProfile {
	return integration.RuntimeProfile{Runtime: name, ContractVersion: integration.RuntimeContractV1, AdapterVersion: "builtin", Protocol: "openai", Qualification: []integration.Qualification{{State: integration.QualificationRegistered}}}
}

func CloudHandlersWithBackends(store CloudStore, backends ReplicaBackends, runtimes integration.RuntimeBackends, artifactResolvers ...ArtifactResolver) map[string]operations.Handler {
	return CloudHandlersWithBackendsAndDrain(store, backends, runtimes, nil, artifactResolvers...)
}

func CloudHandlersWithBackendsAndDrain(store CloudStore, backends ReplicaBackends, runtimes integration.RuntimeBackends, drain DrainTracker, artifactResolvers ...ArtifactResolver) map[string]operations.Handler {
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
		if request.Runtime == "" {
			request.Runtime = resolved.Deployment.Runtime
			if request.Runtime == "" {
				request.Runtime = support.DefaultRuntime
			}
		}
		backend, err := backends.ForAdapter(request.Cloud, request.Runtime, request.ProviderAdapter)
		if err != nil {
			return "", operations.Permanent("provider_backend_unavailable", err)
		}
		runtimeBackend, err := runtimes.ForRuntime(request.Runtime)
		if err != nil {
			return "", operations.Permanent("runtime_backend_unavailable", err)
		}
		if request.Workload.Empty() && !runtimeBackend.Profile.DefaultWorkload.Empty() {
			request.Workload = runtimeBackend.Profile.DefaultWorkload
			request.Port = request.Workload.Port
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
			targetName, targetURL, resourceID, ensureErr := ensureCloudReplica(ctx, store, backend, runtimeBackend.Inspector, operation, request, ordinal)
			if ensureErr != nil {
				return "", ensureErr
			}
			targetNames = append(targetNames, targetName)
			targetURLs = append(targetURLs, targetURL)
			resourceIDs = append(resourceIDs, resourceID)
		}
		deployment, err := store.ApplyDeploymentForTenant(ctx, request.TenantID, domain.Deployment{Name: request.Name, Model: request.Model, Runtime: request.Runtime, RoutingStrategy: resolved.Deployment.RoutingStrategy, MinReplicas: resolved.Deployment.MinReplicas, MaxReplicas: resolved.Deployment.MaxReplicas, AutoscalingEnabled: resolved.Deployment.AutoscalingEnabled}, targetNames)
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
		// Applying the desired target set and publishing a request-path router
		// generation are separate durable boundaries. Production composition
		// supplies a drain tracker backed by the live route directory; do not
		// report success while the stable model alias can still return 404. A
		// scale-down must also prove the reduced generation even in lower-level
		// workflow tests that intentionally omit the live directory.
		if drain != nil || len(draining) > 0 {
			expectedWorkerSet := router.WorkerSetHash(resolved.Deployment.RoutingStrategy, targetURLs)
			matched, matchErr := store.RoutingGenerationMatches(ctx, request.DeploymentID, expectedWorkerSet)
			if matchErr != nil {
				return "", operations.Retryable("router_generation_lookup_failed", matchErr)
			}
			published := drain == nil || drain.HasCurrentDeployment(request.DeploymentID)
			if !matched || !published {
				if len(draining) > 0 {
					_ = checkpoint(ctx, store, operation, "deployment.drain", "waiting", map[string]int{"replicas": len(draining)}, 80, "Waiting for router generation to withdraw draining replicas")
					return "", operations.Retryable("router_drain_pending", errors.New("router has not published the reduced worker set"))
				}
				_ = checkpoint(ctx, store, operation, "deployment.route", "waiting", map[string]any{"targets": targetNames}, 95, "Waiting for the stable endpoint to publish healthy capacity")
				return "", operations.Retryable("router_generation_pending", errors.New("stable endpoint has not published the healthy worker set"))
			}
		}
		if len(draining) > 0 {
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
		published, err := store.PublishDeploymentEndpoint(ctx, request.TenantID, request.EndpointName, request.Name)
		if err != nil {
			return "", classify("endpoint_publication_failed", err)
		}
		_ = checkpoint(ctx, store, operation, "endpoint.publish", "succeeded", map[string]any{"endpoint_name": published.Endpoint.Name}, 100, "Stable application endpoint published")
		result, _ := json.Marshal(map[string]any{"deployment_id": deployment.ID, "endpoint_name": published.Endpoint.Name, "resource_ids": resourceIDs, "replicas": len(targetNames)})
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
		request := CloudRequest{DeploymentID: resolved.Deployment.ID, Name: rollout.Name, Model: spec.Model, ModelRevision: spec.ModelRevision, RevisionID: revision.ID, Cloud: spec.Cloud, ProviderAdapter: spec.ProviderAdapter, GPU: spec.GPU, Region: spec.Region, Runtime: spec.Runtime, RuntimeVersion: spec.RuntimeVersion, RuntimeArgs: spec.RuntimeArgs, Port: spec.Port, Workload: spec.Workload, Serving: spec.Serving, MinReplicas: spec.MinReplicas, MaxReplicas: spec.MaxReplicas, DesiredReplicas: spec.MinReplicas, TenantID: rollout.TenantID, Actor: rollout.Actor, Candidate: true}
		request.Runtime = spec.Runtime
		if request.Runtime == "" {
			request.Runtime = support.DefaultRuntime
		}
		backend, backendErr := backends.ForAdapter(request.Cloud, request.Runtime, request.ProviderAdapter)
		if backendErr != nil {
			return "", operations.Permanent("provider_backend_unavailable", backendErr)
		}
		runtimeBackend, runtimeErr := runtimes.ForRuntime(request.Runtime)
		if runtimeErr != nil {
			return "", operations.Permanent("runtime_backend_unavailable", runtimeErr)
		}
		if request.Workload.Empty() && !runtimeBackend.Profile.DefaultWorkload.Empty() {
			request.Workload = runtimeBackend.Profile.DefaultWorkload
			request.Port = request.Workload.Port
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
			_, endpoint, resourceID, ensureErr := ensureCloudReplica(ctx, store, backend, runtimeBackend.Inspector, operation, request, ordinal)
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
		guardedStore, hasGuardedStore := store.(releaseGuardMonitoringStore)
		var recoveryMonitor domain.ReleaseGuardMonitor
		recoveringRollback := false
		if revision.Status != "candidate" && resolved.Deployment.ActiveRevisionID != rollout.CandidateID {
			if hasGuardedStore {
				recoveryMonitor, err = guardedStore.ReleaseGuardMonitor(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID)
				recoveringRollback = err == nil && resolved.Deployment.ActiveRevisionID == recoveryMonitor.RollbackRevisionID
			}
			if !recoveringRollback {
				return "", operations.Permanent("candidate_not_found", errors.New("revision is neither current candidate, active cutover, nor persisted rollback cleanup"))
			}
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
			targetRevisionID := rollout.CandidateID
			if recoveringRollback {
				targetRevisionID = recoveryMonitor.RollbackRevisionID
			}
			if replica.RevisionID != targetRevisionID || (replica.LifecycleState != "ready" && replica.LifecycleState != "active") || replica.Health != "healthy" || replica.Endpoint == "" {
				continue
			}
			targetNames = append(targetNames, fmt.Sprintf("%s-%s-r%d", rollout.Name, targetRevisionID[:min(8, len(targetRevisionID))], replica.Ordinal))
			targetURLs = append(targetURLs, replica.Endpoint)
		}
		if recoveringRollback {
			rollbackRevision, revisionErr := store.Revision(ctx, rollout.TenantID, rollout.Name, recoveryMonitor.RollbackRevisionID)
			if revisionErr != nil {
				return "", operations.Permanent("rollback_revision_missing", revisionErr)
			}
			if err = json.Unmarshal([]byte(rollbackRevision.SpecJSON), &spec); err != nil {
				return "", operations.Permanent("invalid_rollback_spec", err)
			}
		}
		if len(targetNames) < spec.MinReplicas {
			return "", operations.Permanent("candidate_not_ready", errors.New("candidate does not have its minimum healthy replica count"))
		}
		previousRevisionID := resolved.Deployment.ActiveRevisionID
		if !recoveringRollback {
			err = store.PromoteGuardedCandidate(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID, targetNames)
		}
		if err != nil {
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
		keepRevisionID := rollout.CandidateID
		autoRolledBack := false
		if recoveringRollback {
			keepRevisionID, autoRolledBack = recoveryMonitor.RollbackRevisionID, true
			if err = guardedStore.MarkReleaseGuardMonitorRolledBack(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID); err != nil {
				return "", operations.Retryable("release_monitor_finalize_failed", err)
			}
		}
		if hasGuardedStore && !recoveringRollback {
			policy, policyErr := guardedStore.ReleaseGuardPolicy(ctx, rollout.TenantID, rollout.Name)
			if policyErr != nil {
				return "", operations.Retryable("release_guard_policy_lookup_failed", policyErr)
			}
			existing, monitorErr := guardedStore.ReleaseGuardMonitor(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID)
			monitorExists := monitorErr == nil
			if monitorErr != nil && !errors.Is(monitorErr, domain.ErrNotFound) {
				return "", operations.Retryable("release_monitor_lookup_failed", monitorErr)
			}
			if policy.AutoRollbackEnabled || monitorExists {
				if monitorExists {
					previousRevisionID = existing.RollbackRevisionID
				} else if errors.Is(monitorErr, domain.ErrNotFound) && previousRevisionID == rollout.CandidateID {
					previousRevisionID, monitorErr = guardedStore.PreviousRevisionID(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID)
					if monitorErr != nil {
						return "", operations.Retryable("rollback_revision_lookup_failed", monitorErr)
					}
				}
				monitor, monitorErr := guardedStore.EnsureReleaseGuardMonitor(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID, previousRevisionID, time.Duration(policy.AutoRollbackWindowSeconds)*time.Second)
				if monitorErr != nil {
					return "", classify("release_monitor_create_failed", monitorErr)
				}
				evaluation, evaluationErr := guardedStore.EvaluateReleaseGuardMonitor(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID, time.Duration(policy.AutoRollbackWindowSeconds)*time.Second)
				if evaluationErr != nil {
					return "", operations.Retryable("release_monitor_evaluation_failed", evaluationErr)
				}
				if evaluation.Decision == "WAIT" {
					_ = checkpoint(ctx, store, operation, "candidate.observe", "waiting", map[string]any{"monitor_id": monitor.ID, "deadline": monitor.Deadline, "evaluation_id": evaluation.ID}, 88, "Observing promoted revision against persisted Release Guard policy")
					return "", operations.Retryable("release_guard_observation_pending", errors.New("post-promotion evidence is not sufficient yet"))
				}
				if evaluation.Decision == "REJECT" {
					var rollbackTargets, rollbackURLs []string
					for _, replica := range replicas {
						if replica.RevisionID == previousRevisionID && replica.LifecycleState != "deleted" && replica.Health == "healthy" && replica.Endpoint != "" {
							rollbackTargets = append(rollbackTargets, fmt.Sprintf("%s-%s-r%d", rollout.Name, previousRevisionID[:min(8, len(previousRevisionID))], replica.Ordinal))
							rollbackURLs = append(rollbackURLs, replica.Endpoint)
						}
					}
					if err = guardedStore.RollbackGuardedPromotion(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID, previousRevisionID, "automatic rollback after Release Guard rejection", rollbackTargets); err != nil {
						return "", classify("automatic_rollback_failed", err)
					}
					rollbackRevision, revisionErr := store.Revision(ctx, rollout.TenantID, rollout.Name, previousRevisionID)
					if revisionErr != nil {
						return "", operations.Permanent("rollback_revision_missing", revisionErr)
					}
					var rollbackSpec domain.DeploymentRevisionSpec
					if err = json.Unmarshal([]byte(rollbackRevision.SpecJSON), &rollbackSpec); err != nil {
						return "", operations.Permanent("invalid_rollback_spec", err)
					}
					rollbackHash := router.WorkerSetHash(rollbackSpec.RoutingStrategy, rollbackURLs)
					rollbackMatched, matchErr := store.RoutingGenerationMatches(ctx, resolved.Deployment.ID, rollbackHash)
					if matchErr != nil {
						return "", operations.Retryable("router_generation_lookup_failed", matchErr)
					}
					if !rollbackMatched {
						_ = checkpoint(ctx, store, operation, "candidate.rollback.route", "waiting", map[string]any{"worker_set_hash": rollbackHash}, 92, "Waiting for rollback router generation")
						return "", operations.Retryable("rollback_router_cutover_pending", errors.New("rollback router generation is not active yet"))
					}
					if err = guardedStore.MarkReleaseGuardMonitorRolledBack(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID); err != nil {
						return "", operations.Retryable("release_monitor_finalize_failed", err)
					}
					keepRevisionID, autoRolledBack = previousRevisionID, true
				}
			}
		}
		if drain != nil {
			if active := drain.RetiringInFlight(resolved.Deployment.ID); active > 0 {
				_ = checkpoint(ctx, store, operation, "candidate.drain", "waiting", map[string]int{"active_requests": active}, 90, "Waiting for active requests on the previous revision")
				return "", operations.Retryable("active_requests_draining", fmt.Errorf("%d active request(s) still use the previous revision", active))
			}
		}
		deleted := 0
		for _, replica := range replicas {
			if replica.RevisionID == keepRevisionID || replica.LifecycleState == "deleted" {
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
		result, _ := json.Marshal(map[string]any{"candidate_id": rollout.CandidateID, "promoted": !autoRolledBack, "auto_rolled_back": autoRolledBack, "active_revision_id": keepRevisionID, "deleted_retired_replicas": deleted})
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
		if guardedStore, ok := store.(releaseGuardMonitoringStore); ok {
			if monitor, monitorErr := guardedStore.ReleaseGuardMonitor(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID); monitorErr == nil {
				if resolved.Deployment.ActiveRevisionID == monitor.RollbackRevisionID {
					return candidatePromote(ctx, operation)
				}
				if resolved.Deployment.ActiveRevisionID == rollout.CandidateID && monitor.Status == "observing" {
					replicas, lookupErr := store.ReplicasForDeployment(ctx, rollout.TenantID, resolved.Deployment.ID)
					if lookupErr != nil {
						return "", operations.Retryable("replica_lookup_failed", lookupErr)
					}
					rollbackRevision, revisionErr := store.Revision(ctx, rollout.TenantID, rollout.Name, monitor.RollbackRevisionID)
					if revisionErr != nil {
						return "", operations.Permanent("rollback_revision_missing", revisionErr)
					}
					var rollbackSpec domain.DeploymentRevisionSpec
					if json.Unmarshal([]byte(rollbackRevision.SpecJSON), &rollbackSpec) != nil {
						return "", operations.Permanent("invalid_rollback_spec", errors.New("retained rollback revision spec is invalid"))
					}
					var names, urls []string
					for _, replica := range replicas {
						if replica.RevisionID == monitor.RollbackRevisionID && replica.LifecycleState != "deleted" && replica.Health == "healthy" && replica.Endpoint != "" {
							names = append(names, fmt.Sprintf("%s-%s-r%d", rollout.Name, monitor.RollbackRevisionID[:min(8, len(monitor.RollbackRevisionID))], replica.Ordinal))
							urls = append(urls, replica.Endpoint)
						}
					}
					if err = guardedStore.RollbackGuardedPromotion(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID, monitor.RollbackRevisionID, "automatic rollback after operator cancelled observation", names); err != nil {
						return "", classify("automatic_rollback_failed", err)
					}
					hash := router.WorkerSetHash(rollbackSpec.RoutingStrategy, urls)
					matched, matchErr := store.RoutingGenerationMatches(ctx, resolved.Deployment.ID, hash)
					if matchErr != nil {
						return "", operations.Retryable("router_generation_lookup_failed", matchErr)
					}
					if !matched {
						return "", operations.Retryable("rollback_router_cutover_pending", errors.New("rollback router generation is not active yet"))
					}
					if err = guardedStore.MarkReleaseGuardMonitorRolledBack(ctx, rollout.TenantID, rollout.Name, rollout.CandidateID); err != nil {
						return "", operations.Retryable("release_monitor_finalize_failed", err)
					}
				}
			}
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

func ensureCloudReplica(ctx context.Context, store CloudStore, backend ReplicaBackend, runtime RuntimeInspector, operation domain.Operation, request CloudRequest, ordinal int) (targetName, endpoint, resourceID string, resultErr error) {
	started := time.Now().UTC()
	provider := backend.Provider
	externalKey := fmt.Sprintf("%s-r%d", request.DeploymentID, ordinal)
	if request.Candidate {
		externalKey = fmt.Sprintf("%s-%s-r%d", request.DeploymentID, request.RevisionID, ordinal)
	}
	defer func() {
		evidenceStore, ok := store.(CapacityOperationStore)
		if !ok {
			return
		}
		outcome, errorCode := "succeeded", ""
		if resultErr != nil {
			outcome = "provider_failed"
			var failure operations.Failure
			if errors.As(resultErr, &failure) {
				errorCode = failure.Code
				switch {
				case failure.Code == "provider_capacity_unavailable" || failure.Code == "provider_capacity_constrained":
					outcome = "capacity_unavailable"
				case strings.HasPrefix(failure.Code, "runtime_"):
					outcome = "runtime_failed"
				case failure.Retryable:
					outcome = "pending"
				}
			}
		}
		_, _ = evidenceStore.RecordCapacityOperation(context.WithoutCancel(ctx), domain.CapacityOperation{TenantID: request.TenantID, Provider: backend.Name, Runtime: request.Runtime, ComputeMode: request.ComputeMode, Region: request.Region, GPU: request.GPU, Operation: "replica.ensure", ResourceKey: externalKey, Outcome: outcome, ErrorCode: errorCode, StartedAt: started})
	}()
	replica, _, err := store.EnsureReplicaIntent(ctx, domain.Replica{TenantID: request.TenantID, DeploymentID: request.DeploymentID, RevisionID: request.RevisionID, Ordinal: ordinal, ExternalKey: externalKey, Provider: backend.Name})
	if err != nil {
		return "", "", "", classify("replica_intent_failed", err)
	}
	// A reconcile attempt is not a new placement attempt. Measure successful
	// readiness from the durable replica intent so retries and restarts do not
	// turn a multi-minute cold start into the duration of its final poll.
	if !replica.CreatedAt.IsZero() && replica.CreatedAt.Before(started) {
		started = replica.CreatedAt
	}
	handle := provider.Handle(externalKey)
	if err = store.SetReplicaProviderIdentity(ctx, replica.ID, handle.RequestID, handle.ResourceID); err != nil {
		return "", "", "", classify("replica_identity_failed", err)
	}
	if handle.RequestID != "" {
		replica.ProviderRequestID = handle.RequestID
	}
	if handle.ResourceID != "" {
		replica.ProviderResourceID = handle.ResourceID
	}
	step := fmt.Sprintf("replica.%d", ordinal)
	if err = checkpoint(ctx, store, operation, step+".intent", "succeeded", map[string]string{"replica_id": replica.ID, "external_key": externalKey, "resource_id": handle.ResourceID}, 15, "Replica identity persisted"); err != nil {
		return "", "", "", err
	}
	if backend.Capacity != nil {
		// Discover first so replay after a lost create response always adopts the
		// deterministic resource. Availability is advisory, not a reservation.
		existing, observeErr := provider.ObserveReplica(ctx, handle, request.Port)
		if observeErr != nil {
			return "", "", "", operations.Retryable("provider_discovery_failed", fmt.Errorf("discover capacity before availability check: %w", observeErr))
		}
		if !existing.Exists {
			availability, availabilityErr := backend.Capacity.Availability(ctx, provision.AvailabilityRequest{Cloud: request.Cloud, GPU: request.GPU, Region: request.Region, Count: 1})
			if availabilityErr != nil {
				availability = provision.Availability{State: "unknown", Message: "Provider availability check failed; continuing because stock signals are advisory", Details: availabilityErr.Error()}
			}
			if evidenceStore, ok := store.(CapacityEvidenceStore); ok {
				observed := time.Now().UTC()
				details, _ := json.Marshal(map[string]string{"message": availability.Message, "details": availability.Details})
				_, _ = evidenceStore.RecordCapacityEvidence(ctx, domain.CapacityEvidence{TenantID: request.TenantID, Provider: request.Cloud, Runtime: request.Runtime, ComputeMode: request.ComputeMode, Region: request.Region, GPU: request.GPU, State: availability.State, Source: backend.Name + ".availability", EvidenceJSON: string(details), ObservedAt: observed, ExpiresAt: observed.Add(30 * time.Second)})
			}
			if err = checkpoint(ctx, store, operation, step+".availability", availabilityCheckpointStatus(availability.State), availability, 25, availability.Message); err != nil {
				return "", "", "", err
			}
			if availability.State == "unavailable" {
				return "", "", "", operations.Retryable("provider_capacity_unavailable", errors.New(availability.Message))
			}
		}
	}
	ensured, err := provider.EnsureReplica(ctx, provision.ReplicaSpec{ExternalKey: externalKey, RequestID: replica.ProviderRequestID, Name: fmt.Sprintf("%s-r%d", request.Name, ordinal), Model: request.Model, ModelRevision: request.ImmutableModelRevision, Cloud: request.Cloud, GPU: request.GPU, Region: request.Region, Runtime: request.Runtime, RuntimeVersion: request.RuntimeVersion, RuntimeArgs: request.RuntimeArgs, Port: request.Port, Workload: request.Workload, Serving: request.Serving})
	if err != nil {
		if errors.Is(err, provision.ErrProviderAuthorization) {
			return "", "", "", operations.Permanent("provider_authorization_failed", err)
		}
		if errors.Is(err, provision.ErrProviderQuota) {
			return "", "", "", operations.Retryable("provider_capacity_constrained", err)
		}
		if errors.Is(err, provision.ErrProviderCapacity) {
			return "", "", "", operations.Retryable("provider_capacity_unavailable", err)
		}
		if errors.Is(err, provision.ErrInvalidReplicaSpec) {
			return "", "", "", operations.Permanent("provider_configuration_invalid", err)
		}
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
		return "", "", "", operations.Permanent("runtime_bootstrap_failed", fmt.Errorf("%s process exited before readiness; inspect provider details for the runtime error", backend.Runtime))
	}
	if observation.State != "ready" || observation.Endpoint == "" {
		_ = store.ObserveReplica(ctx, replica.ID, observation.State, observation.Endpoint, "starting", observation.Details, time.Now())
		message, errorCode := providerCapacityMessage(observation)
		_ = checkpoint(ctx, store, operation, step+".ready", "waiting", observation, 55, message)
		if errorCode != "" {
			return "", "", "", operations.Retryable(errorCode, errors.New(message))
		}
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
		_ = checkpoint(ctx, store, operation, step+".runtime", "waiting", observation, 70, "Provider endpoint assigned; waiting for runtime readiness")
		return "", "", "", operations.Retryable("runtime_starting", fmt.Errorf("provider endpoint is assigned; %s may be pulling artifacts, initializing, or restarting", backend.Runtime))
	}
	readyAt := time.Now().UTC()
	readyDetails := withRuntimeReadyEvidence(observation.Details, readyAt)
	if err = store.ObserveReplica(ctx, replica.ID, "ready", observation.Endpoint, "healthy", readyDetails, readyAt); err != nil {
		return "", "", "", classify("observation_failed", err)
	}
	targetName = fmt.Sprintf("%s-r%d", request.Name, ordinal)
	if request.Candidate {
		targetName = fmt.Sprintf("%s-%s-r%d", request.Name, request.RevisionID[:min(8, len(request.RevisionID))], ordinal)
	}
	target, err := store.AddTargetForTenant(ctx, request.TenantID, domain.Target{Name: targetName, URL: observation.Endpoint, Provider: backend.Name, Runtime: backend.Runtime, UpstreamModel: request.Model})
	if err != nil {
		return "", "", "", classify("target_registration_failed", err)
	}
	if err = store.UpdateProvisionedTarget(ctx, target.ID, ensured.ResourceID, readyDetails); err != nil {
		return "", "", "", classify("target_metadata_failed", err)
	}
	lifecycle := "active"
	if request.Candidate {
		lifecycle = "ready"
	}
	if err = store.ObserveReplica(ctx, replica.ID, lifecycle, observation.Endpoint, "healthy", readyDetails, readyAt); err != nil {
		return "", "", "", classify("activation_failed", err)
	}
	return targetName, observation.Endpoint, ensured.ResourceID, nil
}

func withRuntimeReadyEvidence(details string, readyAt time.Time) string {
	value := map[string]any{}
	if json.Unmarshal([]byte(details), &value) != nil {
		value = map[string]any{}
	}
	value["runtime_ready_at"] = readyAt.UTC()
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{}`
	}
	return string(encoded)
}

// providerCapacityMessage exposes only boundaries reported by the provider. It
// deliberately does not label provider placement time as container, artifact,
// or runtime startup time when no worker endpoint exists yet.
func providerCapacityMessage(observation provision.Observation) (string, string) {
	if message, code := providerBootstrapDiagnostic(observation.Details); code != "" {
		return message, code
	}
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
		message += "; 1 resource observed; worker endpoint not exposed yet; billing state unavailable"
	} else {
		message += "; 1 resource observed; worker endpoint exposed, bootstrap still in progress; billing state unavailable"
	}
	return message, ""
}

func providerBootstrapDiagnostic(details string) (string, string) {
	normalized := strings.ToLower(details)
	switch {
	case strings.Contains(normalized, "failed to pull image") || strings.Contains(normalized, "error pulling image"):
		reason := "Provider container-image pull failed"
		if strings.Contains(normalized, "unexpected eof") {
			reason += " because the registry stream ended unexpectedly"
		}
		return reason + "; the existing resource is retained for provider retry; cancel the durable operation before choosing another placement", "provider_image_pull_failed"
	case strings.Contains(normalized, "no space left on device"):
		return "Provider worker bootstrap failed because the assigned host ran out of container storage; cancel the durable operation before choosing another placement", "provider_container_storage_exhausted"
	}
	return "", ""
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

func availabilityCheckpointStatus(state string) string {
	if state == "unavailable" {
		return "waiting"
	}
	return "succeeded"
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
