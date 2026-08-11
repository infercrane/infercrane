// Package controlapi exposes stable, authenticated control-plane HTTP contracts.
package controlapi

import (
	"context"
	"crypto/ed25519"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/decision"
	"github.com/infercrane/infercrane/internal/doctor"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/support"
	"github.com/infercrane/infercrane/internal/workflows"
)

type Store interface {
	Operation(context.Context, string) (domain.Operation, error)
	ActiveOperationForResource(context.Context, string, string, string) (domain.Operation, error)
	RequestOperationCancel(context.Context, string) error
	EnqueueOperation(context.Context, domain.Operation) (domain.Operation, bool, error)
	SubmitCloudDeployment(context.Context, domain.Deployment, domain.Operation) (domain.Deployment, domain.Operation, bool, error)
	SubmitDeploymentDelete(context.Context, string, string, string, domain.Operation) (domain.Operation, bool, error)
	ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error)
	EventsForTenant(context.Context, string, string) ([]domain.Event, error)
	RequestStats(context.Context, string, time.Duration) (domain.RequestStats, error)
	ColdStartStats(context.Context, string, time.Duration) (domain.ColdStartStats, error)
	ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error)
	Revisions(context.Context, string, string) ([]domain.DeploymentRevision, error)
	OperationEvents(context.Context, string, int) ([]domain.OperationEvent, error)
	ScalingDecisionsForTenant(context.Context, string, string, int) ([]domain.ScalingDecision, error)
	ModelArtifactForRevision(context.Context, string, string) (domain.ModelArtifact, error)
	ReleaseGuardEvaluations(context.Context, string, string, int) ([]domain.ReleaseGuardEvaluation, error)
	ReleaseGuardPolicy(context.Context, string, string) (domain.ReleaseGuardPolicy, error)
	SetReleaseGuardPolicy(context.Context, string, string, domain.ReleaseGuardPolicy) (domain.ReleaseGuardPolicy, error)
	AddTargetForTenant(context.Context, string, domain.Target) (domain.Target, error)
	TargetsForTenant(context.Context, string) ([]domain.Target, error)
	TargetForTenantByName(context.Context, string, string) (domain.Target, error)
	DeploymentsForTenant(context.Context, string) ([]domain.Deployment, error)
	OrphanedTargetsForTenant(context.Context, string) ([]domain.Orphan, error)
	Audit(context.Context, domain.AuditEvent) error
	AuditEventsForTenant(context.Context, string, time.Time, int) ([]domain.AuditEvent, error)
	SetTenantQuota(context.Context, string, int, int, int) error
	CreatePrincipalScoped(context.Context, string, string, authz.Role, []authz.Action) (domain.Principal, string, error)
	RotatePrincipalForTenant(context.Context, string, string) (string, error)
	RevokePrincipalForTenant(context.Context, string, string) error
	CreateTenant(context.Context, string, string) error
	CreateSecretReference(context.Context, string, string, string, string) (domain.SecretReference, error)
	SecretReferencesForTenant(context.Context, string) ([]domain.SecretReference, error)
	DeleteSecretReferenceForTenant(context.Context, string, string) error
	SetExternalTargetPolicyForTenant(context.Context, domain.ExternalTargetPolicy) (domain.ExternalTargetPolicy, error)
	ExternalTargetPolicyForDeployment(context.Context, string, string) (domain.ExternalTargetPolicy, error)
	SetRouteForTenant(context.Context, string, string, string) error
	RecordBenchmark(context.Context, domain.BenchmarkResult) (domain.BenchmarkResult, error)
	BenchmarksForDeployment(context.Context, string, string, int) ([]domain.BenchmarkResult, error)
}

type decisionStore interface {
	SetSLOPolicy(context.Context, string, string, domain.SLOPolicy) (domain.SLOPolicy, error)
	SLOPolicy(context.Context, string, string) (domain.SLOPolicy, error)
	DeleteSLOPolicy(context.Context, string, string) error
	RecordInferenceRecommendation(context.Context, domain.InferenceRecommendation) (domain.InferenceRecommendation, error)
	InferenceRecommendations(context.Context, string, string, int) ([]domain.InferenceRecommendation, error)
	LatestCapacityEvidence(context.Context, string, string, string, string, string, string) (domain.CapacityEvidence, error)
}
type passportStore interface {
	InferencePassportPayload(context.Context, string, string, string) (passport.Payload, error)
	RecordInferencePassport(context.Context, domain.InferencePassport) (domain.InferencePassport, error)
	InferencePassports(context.Context, string, string, int) ([]domain.InferencePassport, error)
}
type endpointStore interface {
	CreateEnvironment(context.Context, string, domain.Environment) (domain.Environment, error)
	EnvironmentsForTenant(context.Context, string) ([]domain.Environment, error)
	EnvironmentForTenant(context.Context, string, string) (domain.Environment, error)
	CreateLogicalModel(context.Context, string, domain.LogicalModel) (domain.LogicalModel, error)
	LogicalModelsForTenant(context.Context, string) ([]domain.LogicalModel, error)
	LogicalModelForTenant(context.Context, string, string) (domain.LogicalModel, error)
	CreateEndpoint(context.Context, string, domain.Endpoint) (domain.Endpoint, error)
	EndpointsForTenant(context.Context, string) ([]domain.Endpoint, error)
	ResolveEndpointForTenant(context.Context, string, string) (domain.ResolvedEndpoint, error)
	CreateBackendBinding(context.Context, string, domain.BackendBinding) (domain.BackendBinding, error)
	CreateServingPlan(context.Context, string, domain.ServingPlan) (domain.ServingPlan, error)
	SetEndpointPlan(context.Context, string, string, string, string) error
	DeleteEndpointForTenant(context.Context, string, string) error
	EndpointReleaseGuardPolicy(context.Context, string, string) (domain.EndpointReleaseGuardPolicy, error)
	SetEndpointReleaseGuardPolicy(context.Context, string, string, domain.EndpointReleaseGuardPolicy) (domain.EndpointReleaseGuardPolicy, error)
	EvaluateEndpointReleaseGuard(context.Context, string, string, time.Duration) (domain.EndpointReleaseGuardEvaluation, error)
	EndpointReleaseGuardEvaluations(context.Context, string, string, int) ([]domain.EndpointReleaseGuardEvaluation, error)
	EndpointReleaseGuardAccepted(context.Context, string, string, string) (bool, error)
}
type API struct {
	Store         Store
	APIKey        string
	Authenticator interface {
		AuthenticatePrincipal(context.Context, string) (domain.Principal, error)
	}
	BenchmarkRunner interface {
		Run(context.Context, benchmark.Config) (benchmark.Result, error)
	}
	Diagnostics              func(context.Context, bool, bool, bool, bool) doctor.Report
	Backends                 map[string]BackendMetadata
	Integrations             integration.Snapshot
	GatewayURL, AIPerfBinary string
	PassportPrivateKey       ed25519.PrivateKey
	EndpointRefresh          func(context.Context) error
}

type BackendMetadata struct {
	APIKey     string
	APIKeyEnv  string
	Serverless bool
}
type identityKey struct{}

func (a API) decisions() (decisionStore, bool) {
	store, ok := a.Store.(decisionStore)
	return store, ok
}

func (a API) passportsStore() (passportStore, bool) {
	store, ok := a.Store.(passportStore)
	return store, ok
}

func (a API) endpointResources() (endpointStore, bool) {
	store, ok := a.Store.(endpointStore)
	return store, ok
}

func (a API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/operations/{id}", a.auth(authz.Read, a.operation))
	mux.HandleFunc("GET /api/v1/doctor", a.auth(authz.Read, a.diagnostics))
	mux.HandleFunc("GET /api/v1/whoami", a.auth(authz.Read, a.whoami))
	mux.HandleFunc("GET /api/v1/integrations", a.auth(authz.Read, a.integrations))
	mux.HandleFunc("GET /api/v1/environments", a.auth(authz.Read, a.environments))
	mux.HandleFunc("POST /api/v1/environments", a.auth(authz.Deploy, a.createEnvironment))
	mux.HandleFunc("GET /api/v1/logical-models", a.auth(authz.Read, a.logicalModels))
	mux.HandleFunc("POST /api/v1/logical-models", a.auth(authz.Deploy, a.createLogicalModel))
	mux.HandleFunc("GET /api/v1/endpoints", a.auth(authz.Read, a.endpoints))
	mux.HandleFunc("POST /api/v1/endpoints", a.auth(authz.Deploy, a.createEndpoint))
	mux.HandleFunc("GET /api/v1/endpoints/{name}", a.auth(authz.Read, a.endpoint))
	mux.HandleFunc("DELETE /api/v1/endpoints/{name}", a.auth(authz.Delete, a.deleteEndpoint))
	mux.HandleFunc("POST /api/v1/endpoints/{name}/bindings", a.auth(authz.Deploy, a.createEndpointBinding))
	mux.HandleFunc("POST /api/v1/endpoints/{name}/plans", a.auth(authz.Deploy, a.createEndpointPlan))
	mux.HandleFunc("PUT /api/v1/endpoints/{name}/plans/{plan}/active", a.auth(authz.Deploy, a.activateEndpointPlan))
	mux.HandleFunc("PUT /api/v1/endpoints/{name}/plans/{plan}/candidate", a.auth(authz.Deploy, a.stageEndpointPlan))
	mux.HandleFunc("GET /api/v1/endpoints/{name}/release-guard/policy", a.auth(authz.Read, a.endpointGuardPolicy))
	mux.HandleFunc("PUT /api/v1/endpoints/{name}/release-guard/policy", a.auth(authz.Deploy, a.setEndpointGuardPolicy))
	mux.HandleFunc("POST /api/v1/endpoints/{name}/release-guard/evaluate", a.auth(authz.Deploy, a.evaluateEndpointGuard))
	mux.HandleFunc("GET /api/v1/endpoints/{name}/release-guard/evaluations", a.auth(authz.Read, a.endpointGuardEvaluations))
	mux.HandleFunc("GET /api/v1/operations/{id}/events", a.auth(authz.Read, a.operationEvents))
	mux.HandleFunc("POST /api/v1/operations/{id}/cancel", a.auth(authz.Deploy, a.cancel))
	mux.HandleFunc("POST /api/v1/deployments/apply", a.auth(authz.Deploy, a.applyDeployment))
	mux.HandleFunc("POST /api/v1/deployments", a.auth(authz.Deploy, a.createCloudDeployment))
	mux.HandleFunc("DELETE /api/v1/deployments/{name}", a.auth(authz.Delete, a.deleteDeployment))
	mux.HandleFunc("GET /api/v1/deployments", a.auth(authz.Read, a.deployments))
	mux.HandleFunc("GET /api/v1/deployments/{name}", a.auth(authz.Read, a.deployment))
	mux.HandleFunc("GET /api/v1/deployments/{name}/events", a.auth(authz.Read, a.deploymentEvents))
	mux.HandleFunc("POST /api/v1/deployments/{name}/benchmarks", a.auth(authz.Deploy, a.runBenchmark))
	mux.HandleFunc("GET /api/v1/deployments/{name}/benchmarks", a.auth(authz.Read, a.benchmarks))
	mux.HandleFunc("POST /api/v1/deployments/{name}/passports", a.auth(authz.Deploy, a.createPassport))
	mux.HandleFunc("GET /api/v1/deployments/{name}/passports", a.auth(authz.Read, a.passports))
	mux.HandleFunc("GET /api/v1/deployments/{name}/slo-policy", a.auth(authz.Read, a.sloPolicy))
	mux.HandleFunc("PUT /api/v1/deployments/{name}/slo-policy", a.auth(authz.Deploy, a.setSLOPolicy))
	mux.HandleFunc("DELETE /api/v1/deployments/{name}/slo-policy", a.auth(authz.Deploy, a.deleteSLOPolicy))
	mux.HandleFunc("POST /api/v1/deployments/{name}/recommendations", a.auth(authz.Deploy, a.recommendDeployment))
	mux.HandleFunc("GET /api/v1/deployments/{name}/recommendations", a.auth(authz.Read, a.recommendations))
	mux.HandleFunc("GET /api/v1/deployments/{name}/revisions", a.auth(authz.Read, a.revisions))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollouts", a.auth(authz.Deploy, a.createRollout))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollouts/guard/evaluate", a.auth(authz.Deploy, a.evaluateReleaseGuard))
	mux.HandleFunc("GET /api/v1/deployments/{name}/release-guard/policy", a.auth(authz.Read, a.releaseGuardPolicy))
	mux.HandleFunc("PUT /api/v1/deployments/{name}/release-guard/policy", a.auth(authz.Deploy, a.setReleaseGuardPolicy))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollouts/{revision}/promote", a.auth(authz.Deploy, a.promoteRollout))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollouts/{revision}/provision", a.auth(authz.Deploy, a.provisionRollout))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollouts/{revision}/reject", a.auth(authz.Deploy, a.rejectRollout))
	mux.HandleFunc("POST /api/v1/deployments/{name}/rollback", a.auth(authz.Deploy, a.rollbackDeployment))
	mux.HandleFunc("GET /api/v1/deployments/{name}/scaling-decisions", a.auth(authz.Read, a.scalingDecisions))
	mux.HandleFunc("PUT /api/v1/deployments/{name}/route", a.auth(authz.Deploy, a.setRoute))
	mux.HandleFunc("GET /api/v1/targets", a.auth(authz.Read, a.targets))
	mux.HandleFunc("POST /api/v1/targets", a.auth(authz.Deploy, a.addTarget))
	mux.HandleFunc("GET /api/v1/orphans", a.auth(authz.Read, a.orphans))
	mux.HandleFunc("GET /api/v1/audit-events", a.auth(authz.ManageTenant, a.auditEvents))
	mux.HandleFunc("PUT /api/v1/tenant/quota", a.auth(authz.ManageTenant, a.setQuota))
	mux.HandleFunc("POST /api/v1/tenants", a.auth(authz.ManageTenant, a.createTenant))
	mux.HandleFunc("POST /api/v1/principals", a.auth(authz.ManageTenant, a.createPrincipal))
	mux.HandleFunc("POST /api/v1/principals/{id}/rotate", a.auth(authz.ManageTenant, a.rotatePrincipal))
	mux.HandleFunc("DELETE /api/v1/principals/{id}", a.auth(authz.ManageTenant, a.revokePrincipal))
	mux.HandleFunc("GET /api/v1/secrets", a.auth(authz.ManageSecrets, a.secretReferences))
	mux.HandleFunc("POST /api/v1/secrets", a.auth(authz.ManageSecrets, a.createSecretReference))
	mux.HandleFunc("DELETE /api/v1/secrets/{id}", a.auth(authz.ManageSecrets, a.deleteSecretReference))
	mux.HandleFunc("GET /api/v1/deployments/{name}/external-policy", a.auth(authz.Read, a.externalTargetPolicy))
	mux.HandleFunc("PUT /api/v1/deployments/{name}/external-policy", a.auth(authz.ManageExternal, a.setExternalTargetPolicy))
	return mux
}

func (a API) integrations(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": a.Integrations})
}

func (a API) environments(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := store.EnvironmentsForTenant(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "environments could not be listed")
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data = append(data, environmentResponse(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (a API) createEnvironment(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	var request struct {
		Name   string          `json:"name"`
		Policy json.RawMessage `json:"policy"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if len(request.Policy) == 0 {
		request.Policy = json.RawMessage(`{}`)
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	item, err := store.CreateEnvironment(r.Context(), actor.TenantID, domain.Environment{Name: request.Name, PolicyJSON: string(request.Policy)})
	if writeEndpointMutationError(w, err) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "environment.create", ResourceType: "environment", ResourceName: item.Name, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"environment": environmentResponse(item)})
}

func (a API) logicalModels(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := store.LogicalModelsForTenant(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "logical models could not be listed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items})
}

func (a API) createLogicalModel(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	var request struct {
		Name, Description string
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	item, err := store.CreateLogicalModel(r.Context(), actor.TenantID, domain.LogicalModel{Name: request.Name, Description: request.Description})
	if writeEndpointMutationError(w, err) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "logical_model.create", ResourceType: "logical_model", ResourceName: item.Name, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"logical_model": logicalModelResponse(item)})
}

func (a API) endpoints(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := store.EndpointsForTenant(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "endpoints could not be listed")
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data = append(data, endpointResponse(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (a API) createEndpoint(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	var request struct {
		Name         string `json:"name"`
		LogicalModel string `json:"logical_model"`
		Environment  string `json:"environment"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	model, err := store.LogicalModelForTenant(r.Context(), actor.TenantID, request.LogicalModel)
	if err != nil {
		writeEndpointMutationError(w, err)
		return
	}
	environment, err := store.EnvironmentForTenant(r.Context(), actor.TenantID, request.Environment)
	if err != nil {
		writeEndpointMutationError(w, err)
		return
	}
	item, err := store.CreateEndpoint(r.Context(), actor.TenantID, domain.Endpoint{Name: request.Name, LogicalModelID: model.ID, EnvironmentID: environment.ID})
	if writeEndpointMutationError(w, err) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint.create", ResourceType: "endpoint", ResourceName: item.Name, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"endpoint": endpointResponse(item)})
}

func (a API) endpoint(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	resolved, err := store.ResolveEndpointForTenant(r.Context(), actor.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "endpoint was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "endpoint could not be resolved")
		return
	}
	writeJSON(w, http.StatusOK, resolvedEndpointResponse(resolved))
}

func (a API) deleteEndpoint(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if err := store.DeleteEndpointForTenant(r.Context(), actor.TenantID, r.PathValue("name")); err != nil {
		writeEndpointMutationError(w, err)
		return
	}
	refresh := a.refreshEndpoints(r.Context())
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint.delete", ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]string{"endpoint": r.PathValue("name"), "state": "deleted", "route_refresh": refresh})
}

func (a API) createEndpointBinding(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	var request struct {
		Name          string          `json:"name"`
		Kind          string          `json:"kind"`
		OwnershipMode string          `json:"ownership_mode"`
		Deployment    string          `json:"deployment"`
		Target        string          `json:"target"`
		Config        json.RawMessage `json:"config"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	resolved, err := store.ResolveEndpointForTenant(r.Context(), actor.TenantID, r.PathValue("name"))
	if err != nil {
		writeEndpointMutationError(w, err)
		return
	}
	binding := domain.BackendBinding{EndpointID: resolved.Endpoint.ID, Name: request.Name, Kind: request.Kind, OwnershipMode: request.OwnershipMode, ConfigJSON: string(request.Config)}
	if request.Kind == "deployment" {
		deployment, lookupErr := a.Store.ResolveForTenant(r.Context(), actor.TenantID, request.Deployment)
		if lookupErr != nil {
			writeEndpointMutationError(w, lookupErr)
			return
		}
		binding.DeploymentID = deployment.Deployment.ID
	} else if request.Kind == "external" {
		target, lookupErr := a.Store.TargetForTenantByName(r.Context(), actor.TenantID, request.Target)
		if lookupErr != nil {
			writeEndpointMutationError(w, lookupErr)
			return
		}
		binding.TargetID = target.ID
	}
	item, err := store.CreateBackendBinding(r.Context(), actor.TenantID, binding)
	if writeEndpointMutationError(w, err) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint_binding.create", ResourceType: "endpoint", ResourceName: resolved.Endpoint.Name, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"binding": bindingResponse(item)})
}

func (a API) createEndpointPlan(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	var request struct {
		RoutingPolicy string `json:"routing_policy"`
		Bindings      []struct {
			Name     string `json:"name"`
			Priority int    `json:"priority"`
			Weight   int    `json:"weight"`
		} `json:"bindings"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	resolved, err := store.ResolveEndpointForTenant(r.Context(), actor.TenantID, r.PathValue("name"))
	if err != nil {
		writeEndpointMutationError(w, err)
		return
	}
	byName := make(map[string]string, len(resolved.Bindings))
	for _, binding := range resolved.Bindings {
		byName[binding.Name] = binding.ID
	}
	plan := domain.ServingPlan{EndpointID: resolved.Endpoint.ID, RoutingPolicy: request.RoutingPolicy}
	for _, requested := range request.Bindings {
		id, found := byName[requested.Name]
		if !found {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", "serving plan references an unknown endpoint binding: "+requested.Name)
			return
		}
		plan.Bindings = append(plan.Bindings, domain.ServingPlanBinding{BindingID: id, Priority: requested.Priority, Weight: requested.Weight})
	}
	plan, err = store.CreateServingPlan(r.Context(), actor.TenantID, plan)
	if writeEndpointMutationError(w, err) {
		return
	}
	slot := "candidate"
	if resolved.Endpoint.ActiveServingPlanID == "" {
		slot = "active"
	}
	if err = store.SetEndpointPlan(r.Context(), actor.TenantID, resolved.Endpoint.Name, plan.ID, slot); err != nil {
		writeEndpointMutationError(w, err)
		return
	}
	refresh := a.refreshEndpoints(r.Context())
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "serving_plan.create", ResourceType: "endpoint", ResourceName: resolved.Endpoint.Name, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"plan": servingPlanResponse(plan), "slot": slot, "route_refresh": refresh})
}

func (a API) activateEndpointPlan(w http.ResponseWriter, r *http.Request) {
	a.setEndpointPlanSlot(w, r, "active")
}

func (a API) stageEndpointPlan(w http.ResponseWriter, r *http.Request) {
	a.setEndpointPlanSlot(w, r, "candidate")
}

func (a API) setEndpointPlanSlot(w http.ResponseWriter, r *http.Request, slot string) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if slot == "active" {
		accepted, err := store.EndpointReleaseGuardAccepted(r.Context(), actor.TenantID, r.PathValue("name"), r.PathValue("plan"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "endpoint Release Guard evidence could not be read")
			return
		}
		if !accepted {
			writeError(w, http.StatusConflict, "release_guard_required", "candidate plan promotion requires a current PASS decision for the active/candidate pair")
			return
		}
	}
	if err := store.SetEndpointPlan(r.Context(), actor.TenantID, r.PathValue("name"), r.PathValue("plan"), slot); err != nil {
		writeEndpointMutationError(w, err)
		return
	}
	refresh := a.refreshEndpoints(r.Context())
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "serving_plan." + slot, ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]string{"endpoint": r.PathValue("name"), "plan_id": r.PathValue("plan"), "slot": slot, "route_refresh": refresh})
}

func (a API) endpointGuardPolicy(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	policy, err := store.EndpointReleaseGuardPolicy(r.Context(), actor.TenantID, r.PathValue("name"))
	if writeEndpointMutationError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": policy})
}

func (a API) setEndpointGuardPolicy(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	var policy domain.EndpointReleaseGuardPolicy
	if !decodeMutationBody(w, r, &policy) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	policy, err := store.SetEndpointReleaseGuardPolicy(r.Context(), actor.TenantID, r.PathValue("name"), policy)
	if writeEndpointMutationError(w, err) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint_guard.policy", ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"policy": policy})
}

func (a API) evaluateEndpointGuard(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	var request struct {
		WindowSeconds int `json:"window_seconds"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.WindowSeconds == 0 {
		request.WindowSeconds = 3600
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	evaluation, err := store.EvaluateEndpointReleaseGuard(r.Context(), actor.TenantID, r.PathValue("name"), time.Duration(request.WindowSeconds)*time.Second)
	if writeEndpointMutationError(w, err) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint_guard.evaluate", ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: evaluation.Decision})
	writeJSON(w, http.StatusCreated, map[string]any{"evaluation": endpointGuardEvaluationResponse(evaluation)})
}

func (a API) endpointGuardEvaluations(w http.ResponseWriter, r *http.Request) {
	store, ok := a.endpointResources()
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint resources are not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := store.EndpointReleaseGuardEvaluations(r.Context(), actor.TenantID, r.PathValue("name"), 20)
	if writeEndpointMutationError(w, err) {
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data = append(data, endpointGuardEvaluationResponse(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func endpointGuardEvaluationResponse(item domain.EndpointReleaseGuardEvaluation) map[string]any {
	decode := func(raw string) any {
		var value any
		if json.Unmarshal([]byte(raw), &value) != nil {
			return nil
		}
		return value
	}
	return map[string]any{"id": item.ID, "tenant_id": item.TenantID, "endpoint_id": item.EndpointID, "active_serving_plan_id": item.ActiveServingPlanID, "candidate_serving_plan_id": item.CandidateServingPlanID, "decision": item.Decision, "reasons": decode(item.ReasonCodesJSON), "metrics": decode(item.MetricsJSON), "policy": decode(item.PolicyJSON), "created_at": item.CreatedAt}
}

func (a API) refreshEndpoints(parent context.Context) string {
	if a.EndpointRefresh == nil {
		return "pending"
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	if err := a.EndpointRefresh(ctx); err != nil {
		return "pending"
	}
	return "converged"
}

func writeEndpointMutationError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	} else if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", err.Error())
	} else {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
	}
	return true
}

func environmentResponse(item domain.Environment) map[string]any {
	var policy any = map[string]any{}
	if item.PolicyJSON != "" {
		_ = json.Unmarshal([]byte(item.PolicyJSON), &policy)
	}
	return map[string]any{"id": item.ID, "tenant_id": item.TenantID, "name": item.Name, "policy": policy, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt}
}

func logicalModelResponse(item domain.LogicalModel) map[string]any {
	return map[string]any{"id": item.ID, "tenant_id": item.TenantID, "name": item.Name, "description": item.Description, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt}
}

func endpointResponse(item domain.Endpoint) map[string]any {
	return map[string]any{"id": item.ID, "tenant_id": item.TenantID, "logical_model_id": item.LogicalModelID, "environment_id": item.EnvironmentID, "name": item.Name, "desired_state": item.DesiredState, "observed_state": item.ObservedState, "active_serving_plan_id": item.ActiveServingPlanID, "candidate_serving_plan_id": item.CandidateServingPlanID, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt}
}

func bindingResponse(item domain.BackendBinding) map[string]any {
	var config any = map[string]any{}
	if item.ConfigJSON != "" {
		_ = json.Unmarshal([]byte(item.ConfigJSON), &config)
	}
	return map[string]any{"id": item.ID, "tenant_id": item.TenantID, "endpoint_id": item.EndpointID, "name": item.Name, "kind": item.Kind, "ownership_mode": item.OwnershipMode, "deployment_id": item.DeploymentID, "target_id": item.TargetID, "config": config, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt}
}

func servingPlanResponse(item domain.ServingPlan) map[string]any {
	return map[string]any{"id": item.ID, "tenant_id": item.TenantID, "endpoint_id": item.EndpointID, "version": item.Version, "routing_policy": item.RoutingPolicy, "spec_digest": item.SpecDigest, "bindings": item.Bindings, "created_at": item.CreatedAt}
}

func resolvedEndpointResponse(item domain.ResolvedEndpoint) map[string]any {
	bindings := make([]map[string]any, 0, len(item.Bindings))
	for _, binding := range item.Bindings {
		bindings = append(bindings, bindingResponse(binding))
	}
	response := map[string]any{"endpoint": endpointResponse(item.Endpoint), "logical_model": logicalModelResponse(item.LogicalModel), "environment": environmentResponse(item.Environment), "active_plan": servingPlanResponse(item.ActivePlan), "bindings": bindings}
	if item.CandidatePlan != nil {
		response["candidate_plan"] = servingPlanResponse(*item.CandidatePlan)
	}
	return response
}

func (a API) whoami(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	writeJSON(w, http.StatusOK, map[string]any{"principal": map[string]any{"id": principal.ID, "tenant_id": principal.TenantID, "name": principal.Name, "role": principal.Role, "kind": principal.Kind, "scopes": principal.Scopes}})
}

func (a API) diagnostics(w http.ResponseWriter, r *http.Request) {
	if a.Diagnostics == nil {
		writeError(w, http.StatusServiceUnavailable, "diagnostics_unavailable", "control-plane diagnostics are not configured")
		return
	}
	cloud, err := strconv.ParseBool(defaultValue(r.URL.Query().Get("cloud"), "false"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "cloud must be true or false")
		return
	}
	serverless, err := strconv.ParseBool(defaultValue(r.URL.Query().Get("serverless"), "false"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "serverless must be true or false")
		return
	}
	aws, err := strconv.ParseBool(defaultValue(r.URL.Query().Get("aws"), "false"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "aws must be true or false")
		return
	}
	kubernetes, err := strconv.ParseBool(defaultValue(r.URL.Query().Get("kubernetes"), "false"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "kubernetes must be true or false")
		return
	}
	writeJSON(w, http.StatusOK, a.Diagnostics(r.Context(), cloud, serverless, aws, kubernetes))
}

func defaultValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (a API) runBenchmark(w http.ResponseWriter, r *http.Request) {
	if a.BenchmarkRunner == nil || a.GatewayURL == "" {
		writeError(w, http.StatusServiceUnavailable, "benchmark_unavailable", "AIPerf benchmark execution is not configured")
		return
	}
	var request struct {
		Requests    int    `json:"requests"`
		Concurrency int    `json:"concurrency"`
		RandomSeed  int64  `json:"random_seed"`
		Revision    string `json:"revision"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.Requests < 1 || request.Requests > 100000 || request.Concurrency < 1 || request.Concurrency > request.Requests {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "requests must be 1..100000 and concurrency must be 1..requests")
		return
	}
	if request.RandomSeed == 0 {
		request.RandomSeed = 17
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	resolved, err := a.Store.ResolveForTenant(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "deployment lookup failed")
		return
	}
	deployment := resolved.Deployment
	if deployment.ActiveRevisionID == "" {
		writeError(w, 409, "deployment_not_ready", "deployment has no active revision")
		return
	}
	revisions, err := a.Store.Revisions(r.Context(), principal.TenantID, deployment.Name)
	if err != nil {
		writeError(w, 500, "internal", "revision lookup failed")
		return
	}
	selectedRevisionID := deployment.ActiveRevisionID
	selector := strings.TrimSpace(request.Revision)
	if selector == "" || selector == "active" {
		selector = "active"
	} else if selector == "candidate" {
		if deployment.CandidateRevisionID == "" {
			writeError(w, http.StatusConflict, "candidate_unavailable", "deployment has no candidate revision")
			return
		}
		selectedRevisionID = deployment.CandidateRevisionID
	} else {
		selectedRevisionID = selector
	}
	var revision domain.DeploymentRevision
	for _, candidate := range revisions {
		if candidate.ID == selectedRevisionID {
			revision = candidate
			break
		}
	}
	if revision.ID == "" {
		writeError(w, 404, "revision_not_found", "selected revision metadata is unavailable")
		return
	}
	var revisionSpec domain.DeploymentRevisionSpec
	if err = json.Unmarshal([]byte(revision.SpecJSON), &revisionSpec); err != nil {
		writeError(w, 500, "internal", "selected revision specification is invalid")
		return
	}
	artifact, artifactErr := a.Store.ModelArtifactForRevision(r.Context(), principal.TenantID, revision.ID)
	if errors.Is(artifactErr, domain.ErrNotFound) {
		writeError(w, 409, "artifact_unresolved", "selected revision has no immutable ModelArtifact identity")
		return
	}
	if artifactErr != nil {
		writeError(w, 500, "internal", "model artifact lookup failed")
		return
	}
	endpoint, model, apiKeyEnv := a.GatewayURL, deployment.Name, "INFERCRANE_API_KEY"
	credential := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	provider := ""
	if selectedRevisionID != deployment.ActiveRevisionID {
		replicas, replicaErr := a.Store.ReplicasForDeployment(r.Context(), principal.TenantID, deployment.ID)
		if replicaErr != nil {
			writeError(w, 500, "internal", "selected revision replica lookup failed")
			return
		}
		selectedOrdinal := int(^uint(0) >> 1)
		for _, replica := range replicas {
			if replica.RevisionID == selectedRevisionID && replica.Health == "healthy" && (replica.LifecycleState == "ready" || replica.LifecycleState == "active") && replica.Endpoint != "" && replica.Ordinal < selectedOrdinal {
				endpoint, provider, selectedOrdinal = replica.Endpoint, replica.Provider, replica.Ordinal
			}
		}
		if provider == "" {
			writeError(w, 409, "candidate_not_ready", "selected revision has no healthy ready endpoint")
			return
		}
		backend := a.Backends[provider]
		credential = backend.APIKey
		if credential == "" {
			writeError(w, 503, "benchmark_unavailable", "candidate benchmark credential is not configured for provider "+provider)
			return
		}
		model = revisionSpec.Model
		if backend.APIKeyEnv != "" {
			apiKeyEnv = backend.APIKeyEnv
		}
	}
	measured, err := a.BenchmarkRunner.Run(r.Context(), benchmark.Config{Binary: a.AIPerfBinary, Endpoint: endpoint, APIKey: credential, APIKeyEnv: apiKeyEnv, Model: model, Tokenizer: artifact.Repository, Requests: request.Requests, Concurrency: request.Concurrency, RandomSeed: request.RandomSeed})
	if err != nil {
		writeError(w, 502, "benchmark_failed", err.Error())
		return
	}
	if provider == "" && len(resolved.Targets) > 0 {
		provider = resolved.Targets[0].Provider
	}
	benchmarkProvider := revisionSpec.Cloud
	if benchmarkProvider == "" {
		benchmarkProvider = provider
	}
	benchmarkRegion := revisionSpec.Region
	if benchmarkRegion == "" {
		replicas, replicaErr := a.Store.ReplicasForDeployment(r.Context(), principal.TenantID, deployment.ID)
		if replicaErr != nil {
			writeError(w, 500, "internal", "benchmark replica metadata lookup failed")
			return
		}
		for _, replica := range replicas {
			if replica.RevisionID == selectedRevisionID {
				benchmarkRegion = regionFromProviderDetails(replica.ProviderDetails)
				if benchmarkRegion != "" {
					break
				}
			}
		}
	}
	if revisionSpec.RuntimeVersion == "" && revisionSpec.Runtime == support.DefaultRuntime {
		revisionSpec.RuntimeVersion = support.DefaultRuntimeVersion
	}
	modelIdentity, artifactID := artifact.ModelIdentity, artifact.ID
	if revisionSpec.ComputeMode == "" {
		revisionSpec.ComputeMode = "elastic"
	}
	workload, _ := json.Marshal(map[string]any{"endpoint_type": "chat", "streaming": true, "request_count": request.Requests, "concurrency": request.Concurrency, "random_seed": request.RandomSeed, "output_tokens": 32, "server_token_count": true, "revision_selector": selector, "direct_revision_validation": selectedRevisionID != deployment.ActiveRevisionID})
	runtimeConfig, _ := json.Marshal(map[string]any{"args": revisionSpec.RuntimeArgs})
	var gpuCount *int
	if revisionSpec.GPU != "" {
		count := 1
		gpuCount = &count
	}
	costMetadata, _ := json.Marshal(map[string]any{"available": false, "reason": "provider cost was not measured by this benchmark"})
	persisted, err := a.Store.RecordBenchmark(r.Context(), domain.BenchmarkResult{TenantID: principal.TenantID, DeploymentID: deployment.ID, DeploymentName: deployment.Name, RevisionID: revision.ID, ModelArtifactID: artifactID, ModelIdentity: modelIdentity, Runtime: revisionSpec.Runtime, RuntimeVersion: revisionSpec.RuntimeVersion, RuntimeConfigJSON: string(runtimeConfig), Provider: benchmarkProvider, Region: benchmarkRegion, GPU: revisionSpec.GPU, GPUCount: gpuCount, ComputeMode: revisionSpec.ComputeMode, Tool: measured.Tool, ToolVersion: measured.ToolVersion, WorkloadJSON: string(workload), ReproductionCommand: measured.Command, RequestCount: measured.Requests, Succeeded: measured.Succeeded, Failed: measured.Failed, DurationSeconds: measured.DurationSeconds, RequestThroughput: measured.RequestThroughput, OutputTokenThroughput: measured.OutputTokenThroughput, TTFTP50MS: measured.TTFTP50MS, TTFTP95MS: measured.TTFTP95MS, TPOTP50MS: measured.TPOTP50MS, TPOTP95MS: measured.TPOTP95MS, LatencyP50MS: measured.LatencyP50MS, LatencyP95MS: measured.LatencyP95MS, CostMetadataJSON: string(costMetadata)})
	if err != nil {
		writeError(w, 500, "internal", "benchmark result could not be persisted")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"benchmark": benchmarkResponse(persisted)})
}

func regionFromProviderDetails(details string) string {
	if strings.TrimSpace(details) == "" {
		return ""
	}
	var value any
	if json.Unmarshal([]byte(details), &value) != nil {
		return ""
	}
	var find func(any) string
	find = func(current any) string {
		switch typed := current.(type) {
		case map[string]any:
			for _, key := range []string{"region", "Region"} {
				if region, ok := typed[key].(string); ok && strings.TrimSpace(region) != "" {
					return strings.TrimSpace(region)
				}
			}
			for _, nested := range typed {
				if region := find(nested); region != "" {
					return region
				}
			}
		case []any:
			for _, nested := range typed {
				if region := find(nested); region != "" {
					return region
				}
			}
		}
		return ""
	}
	return find(value)
}

func (a API) benchmarks(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.Store.BenchmarksForDeployment(r.Context(), principal.TenantID, r.PathValue("name"), limit)
	if err != nil {
		writeError(w, 500, "internal", "benchmark history lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, benchmarkResponse(row))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

func benchmarkResponse(row domain.BenchmarkResult) map[string]any {
	return map[string]any{"id": row.ID, "deployment": row.DeploymentName, "revision_id": row.RevisionID, "model_artifact_id": row.ModelArtifactID, "model_identity": row.ModelIdentity, "runtime": row.Runtime, "runtime_version": row.RuntimeVersion, "runtime_configuration": json.RawMessage(row.RuntimeConfigJSON), "provider": row.Provider, "region": row.Region, "gpu": row.GPU, "gpu_count": row.GPUCount, "compute_mode": row.ComputeMode, "tool": row.Tool, "tool_version": row.ToolVersion, "workload": json.RawMessage(row.WorkloadJSON), "reproduction_command": row.ReproductionCommand, "request_count": row.RequestCount, "succeeded": row.Succeeded, "failed": row.Failed, "duration_seconds": row.DurationSeconds, "request_throughput": row.RequestThroughput, "output_token_throughput": row.OutputTokenThroughput, "ttft_p50_ms": row.TTFTP50MS, "ttft_p95_ms": row.TTFTP95MS, "tpot_p50_ms": row.TPOTP50MS, "tpot_p95_ms": row.TPOTP95MS, "latency_p50_ms": row.LatencyP50MS, "latency_p95_ms": row.LatencyP95MS, "goodput": row.Goodput, "gpu_utilization": row.GPUUtilization, "cost_metadata": json.RawMessage(row.CostMetadataJSON), "created_at": row.CreatedAt}
}

func (a API) createPassport(w http.ResponseWriter, r *http.Request) {
	store, ok := a.passportsStore()
	if !ok {
		writeError(w, http.StatusNotImplemented, "passport_unavailable", "passport persistence is unavailable")
		return
	}
	if len(a.PassportPrivateKey) != ed25519.PrivateKeySize {
		writeError(w, http.StatusServiceUnavailable, "passport_signing_unavailable", "configure INFERCRANE_PASSPORT_SIGNING_KEY_FILE before issuing passports")
		return
	}
	var body struct {
		RevisionID string `json:"revision_id"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	payload, err := store.InferencePassportPayload(r.Context(), principal.TenantID, r.PathValue("name"), body.RevisionID)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment or revision was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "passport evidence could not be assembled")
		return
	}
	payload.Qualification = passport.SelectQualification(a.Integrations, payload.RevisionSpec)
	passport.FinalizeEvidence(&payload)
	envelope, err := passport.Sign(payload, a.PassportPrivateKey)
	if err != nil {
		writeError(w, 500, "passport_signing_failed", err.Error())
		return
	}
	resolved, err := a.Store.ResolveForTenant(r.Context(), principal.TenantID, r.PathValue("name"))
	if err != nil {
		writeError(w, 500, "internal", "deployment identity lookup failed")
		return
	}
	value, err := store.RecordInferencePassport(r.Context(), domain.InferencePassport{TenantID: principal.TenantID, DeploymentID: resolved.Deployment.ID, RevisionID: payload.RevisionID, PayloadJSON: envelope.PayloadJSON, PayloadDigest: envelope.Digest, Signature: envelope.Signature, PublicKey: envelope.PublicKey, Algorithm: envelope.Algorithm, KeyID: envelope.KeyID})
	if err != nil {
		writeError(w, 500, "passport_persist_failed", "signed passport could not be persisted")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "passport.issue", ResourceType: "deployment", ResourceName: r.PathValue("name"), Outcome: "succeeded", Payload: `{"digest":"` + value.PayloadDigest + `","key_id":"` + value.KeyID + `"}`})
	writeJSON(w, http.StatusCreated, map[string]any{"passport": passportResponse(value), "verified": passport.Verify(envelope) == nil})
}

func (a API) passports(w http.ResponseWriter, r *http.Request) {
	store, ok := a.passportsStore()
	if !ok {
		writeError(w, http.StatusNotImplemented, "passport_unavailable", "passport persistence is unavailable")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := store.InferencePassports(r.Context(), principal.TenantID, r.PathValue("name"), 100)
	if err != nil {
		writeError(w, 500, "internal", "passport history lookup failed")
		return
	}
	data := make([]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, passportResponse(row))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

func passportResponse(row domain.InferencePassport) map[string]any {
	envelope := passport.Envelope{PayloadJSON: row.PayloadJSON, Digest: row.PayloadDigest, Signature: row.Signature, PublicKey: row.PublicKey, Algorithm: row.Algorithm, KeyID: row.KeyID}
	var payload passport.Payload
	_ = json.Unmarshal([]byte(row.PayloadJSON), &payload)
	return map[string]any{"id": row.ID, "revision_id": row.RevisionID, "payload": json.RawMessage(row.PayloadJSON), "digest": row.PayloadDigest, "signature": row.Signature, "public_key": row.PublicKey, "algorithm": row.Algorithm, "key_id": row.KeyID, "verified": passport.Verify(envelope) == nil, "complete": len(payload.MissingEvidence) == 0, "missing_evidence": payload.MissingEvidence, "created_at": row.CreatedAt}
}

func (a API) sloPolicy(w http.ResponseWriter, r *http.Request) {
	store, ok := a.decisions()
	if !ok {
		writeError(w, 503, "decision_store_unavailable", "inference decision storage is unavailable")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	policy, err := store.SLOPolicy(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "SLO policy was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "SLO policy lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"policy": policy})
}

func (a API) setSLOPolicy(w http.ResponseWriter, r *http.Request) {
	store, ok := a.decisions()
	if !ok {
		writeError(w, 503, "decision_store_unavailable", "inference decision storage is unavailable")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	var policy domain.SLOPolicy
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		writeError(w, 400, "invalid_request", "request body must be one strict JSON object")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, 400, "invalid_request", "request body must contain exactly one JSON object")
		return
	}
	if err := (decision.SLOPolicy{MaxTTFTP95MS: policy.MaxTTFTP95MS, MaxLatencyP95MS: policy.MaxLatencyP95MS, MaxErrorRate: policy.MaxErrorRate, MinOutputTokensSecond: policy.MinOutputTokensSecond, MaxHourlyCost: policy.MaxHourlyCost}).Validate(); err != nil {
		writeError(w, 422, "invalid_slo_policy", err.Error())
		return
	}
	persisted, err := store.SetSLOPolicy(r.Context(), principal.TenantID, r.PathValue("name"), policy)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, 422, "invalid_slo_policy", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "slo_policy.update", ResourceType: "deployment", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, 200, map[string]any{"policy": persisted})
}

func (a API) deleteSLOPolicy(w http.ResponseWriter, r *http.Request) {
	store, ok := a.decisions()
	if !ok {
		writeError(w, 503, "decision_store_unavailable", "inference decision storage is unavailable")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	if err := store.DeleteSLOPolicy(r.Context(), principal.TenantID, r.PathValue("name")); errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "SLO policy was not found")
		return
	} else if err != nil {
		writeError(w, 500, "internal", "SLO policy could not be deleted")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "slo_policy.delete", ResourceType: "deployment", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	w.WriteHeader(http.StatusNoContent)
}

func (a API) recommendDeployment(w http.ResponseWriter, r *http.Request) {
	store, ok := a.decisions()
	if !ok {
		writeError(w, 503, "decision_store_unavailable", "inference decision storage is unavailable")
		return
	}
	var request map[string]json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&request); err != nil || request == nil || len(request) != 0 {
		writeError(w, 400, "invalid_request", "request body must be an empty JSON object")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, 400, "invalid_request", "request body must contain exactly one JSON object")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	name := r.PathValue("name")
	resolved, err := a.Store.ResolveForTenant(r.Context(), principal.TenantID, name)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "deployment lookup failed")
		return
	}
	policy, err := store.SLOPolicy(r.Context(), principal.TenantID, name)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 409, "slo_policy_required", "set an explicit SLO policy before requesting a recommendation")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "SLO policy lookup failed")
		return
	}
	benchmarks, err := a.Store.BenchmarksForDeployment(r.Context(), principal.TenantID, name, 100)
	if err != nil {
		writeError(w, 500, "internal", "benchmark evidence lookup failed")
		return
	}
	decisionPolicy := decision.SLOPolicy{MaxTTFTP95MS: policy.MaxTTFTP95MS, MaxLatencyP95MS: policy.MaxLatencyP95MS, MaxErrorRate: policy.MaxErrorRate, MinOutputTokensSecond: policy.MinOutputTokensSecond, MaxHourlyCost: policy.MaxHourlyCost}
	activeModelIdentity := ""
	if resolved.Deployment.ActiveRevisionID != "" {
		if artifact, artifactErr := a.Store.ModelArtifactForRevision(r.Context(), principal.TenantID, resolved.Deployment.ActiveRevisionID); artifactErr == nil {
			activeModelIdentity = artifact.ModelIdentity
		}
	}
	baselineWorkload := ""
	if len(benchmarks) > 0 {
		baselineWorkload = canonicalJSON(benchmarks[0].WorkloadJSON)
	}
	evidence := make([]decision.Evidence, 0, len(benchmarks))
	for _, benchmark := range benchmarks {
		workload := canonicalJSON(benchmark.WorkloadJSON)
		row := decision.Evidence{ID: benchmark.ID, ModelIdentity: benchmark.ModelIdentity, Runtime: benchmark.Runtime, RuntimeVersion: benchmark.RuntimeVersion, Provider: benchmark.Provider, Region: benchmark.Region, GPU: benchmark.GPU, ComputeMode: benchmark.ComputeMode, ComparableModel: activeModelIdentity != "" && benchmark.ModelIdentity == activeModelIdentity, ComparableWorkload: baselineWorkload != "" && workload == baselineWorkload, Requests: benchmark.RequestCount, Failed: benchmark.Failed, TTFTP95MS: benchmark.TTFTP95MS, LatencyP95MS: benchmark.LatencyP95MS, OutputTokensSecond: benchmark.OutputTokenThroughput, CreatedAt: benchmark.CreatedAt}
		runtimeVersionQualified := false
		for _, runtimeProfile := range a.Integrations.Runtimes {
			if runtimeProfile.Runtime == row.Runtime && (runtimeProfile.EngineVersion == "" || runtimeProfile.EngineVersion == row.RuntimeVersion) {
				runtimeVersionQualified = true
				break
			}
		}
		for _, compatibility := range a.Integrations.Compatibility {
			if compatibility.Runtime == row.Runtime && compatibility.Cloud == row.Provider && string(compatibility.Mode) == row.ComputeMode {
				row.QualificationState = string(compatibility.State)
				row.Qualified = runtimeVersionQualified && (compatibility.State == integration.QualificationReal || compatibility.State == integration.QualificationLocal)
				if !runtimeVersionQualified {
					row.QualificationState = "runtime-version-mismatch"
				}
				break
			}
		}
		if capacity, capacityErr := store.LatestCapacityEvidence(r.Context(), principal.TenantID, row.Provider, row.Runtime, row.ComputeMode, row.Region, row.GPU); capacityErr == nil {
			row.CapacityState, row.CapacitySource = capacity.State, capacity.Source
			row.CapacityObservedAt, row.CapacityExpiresAt = &capacity.ObservedAt, &capacity.ExpiresAt
		}
		var cost struct {
			Available  bool       `json:"available"`
			Hourly     *float64   `json:"hourly"`
			Source     string     `json:"source"`
			ObservedAt *time.Time `json:"observed_at"`
		}
		if json.Unmarshal([]byte(benchmark.CostMetadataJSON), &cost) == nil && cost.Available && cost.Hourly != nil && cost.Source != "" && cost.ObservedAt != nil {
			row.HourlyCost, row.CostSource, row.CostObservedAt = cost.Hourly, cost.Source, cost.ObservedAt
		}
		evidence = append(evidence, row)
	}
	result := decision.Recommend(decisionPolicy, evidence)
	missing, _ := json.Marshal(result.Missing)
	candidates, _ := json.Marshal(result.Candidates)
	snapshot, err := decision.Snapshot(decisionPolicy, evidence, result)
	if err != nil {
		writeError(w, 500, "internal", "recommendation evidence could not be canonicalized")
		return
	}
	persisted, err := store.RecordInferenceRecommendation(r.Context(), domain.InferenceRecommendation{TenantID: principal.TenantID, DeploymentID: resolved.Deployment.ID, Status: result.Status, AlgorithmVersion: result.AlgorithmVersion, SelectedEvidenceID: result.SelectedEvidence, Reason: result.Reason, MissingJSON: string(missing), CandidatesJSON: string(candidates), InputSnapshotJSON: snapshot})
	if err != nil {
		writeError(w, 500, "internal", "recommendation could not be persisted")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "recommendation.evaluate", ResourceType: "deployment", ResourceName: name, Outcome: "succeeded"})
	writeJSON(w, 201, map[string]any{"recommendation": recommendationResponse(persisted)})
}

func (a API) recommendations(w http.ResponseWriter, r *http.Request) {
	store, ok := a.decisions()
	if !ok {
		writeError(w, 503, "decision_store_unavailable", "inference decision storage is unavailable")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := store.InferenceRecommendations(r.Context(), principal.TenantID, r.PathValue("name"), limit)
	if err != nil {
		writeError(w, 500, "internal", "recommendation history lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, recommendationResponse(row))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

func recommendationResponse(row domain.InferenceRecommendation) map[string]any {
	return map[string]any{"id": row.ID, "deployment_id": row.DeploymentID, "status": row.Status, "algorithm_version": row.AlgorithmVersion, "selected_evidence_id": row.SelectedEvidenceID, "reason": row.Reason, "missing": json.RawMessage(row.MissingJSON), "candidates": json.RawMessage(row.CandidatesJSON), "input_snapshot": json.RawMessage(row.InputSnapshotJSON), "input_digest": row.InputDigest, "created_at": row.CreatedAt}
}

func canonicalJSON(value string) string {
	var decoded any
	if json.Unmarshal([]byte(value), &decoded) != nil {
		return ""
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func (a API) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must not exceed 128 characters")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	resolved, err := a.Store.ResolveForTenant(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "deployment lookup failed")
		return
	}
	request := workflows.DeleteRequest{DeploymentID: resolved.Deployment.ID, Name: resolved.Deployment.Name, Actor: principal.Name, TenantID: principal.TenantID}
	encoded, _ := json.Marshal(request)
	deleteKind := workflows.DeleteKind
	for _, target := range resolved.Targets {
		if a.Backends[target.Provider].Serverless {
			deleteKind = workflows.ServerlessDeleteKind
			break
		}
	}
	operation, created, err := a.Store.SubmitDeploymentDelete(r.Context(), principal.TenantID, resolved.Deployment.Name, resolved.Deployment.ID, domain.Operation{Kind: deleteKind, IdempotencyKey: key, RequestJSON: string(encoded)})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "deletion could not be submitted")
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"operation": operationResponse(operation), "created": created})
}

func (a API) createCloudDeployment(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must not exceed 128 characters")
		return
	}
	var request workflows.CloudRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid: "+err.Error())
		return
	}
	if err := request.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	request.Actor, request.TenantID = principal.Name, principal.TenantID
	if request.ComputeMode == "" {
		request.ComputeMode = "elastic"
	}
	minReplicas, maxReplicas := 1, 1
	operationKind := workflows.ConvergeKind
	if request.ComputeMode == "serverless" {
		minReplicas, operationKind = 0, workflows.ServerlessConvergeKind
	} else if request.MinReplicas > 0 {
		minReplicas = request.MinReplicas
	}
	if request.MaxReplicas > 0 {
		maxReplicas = request.MaxReplicas
	} else {
		maxReplicas = minReplicas
	}
	encoded, _ := json.Marshal(request)
	autoscalingEnabled := request.ComputeMode != "serverless" && maxReplicas > minReplicas
	deployment, operation, created, err := a.Store.SubmitCloudDeployment(r.Context(), domain.Deployment{TenantID: principal.TenantID, Name: request.Name, Model: request.Model, Runtime: request.Runtime, MinReplicas: minReplicas, MaxReplicas: maxReplicas, AutoscalingEnabled: autoscalingEnabled}, domain.Operation{TenantID: principal.TenantID, Kind: operationKind, IdempotencyKey: key, RequestJSON: string(encoded)})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cloud deployment could not be submitted")
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	status := http.StatusAccepted
	if !created && operation.Status == "succeeded" {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"deployment": deploymentResponse(deployment), "operation": operationResponse(operation), "created": created})
}

func (a API) auditEvents(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	var before time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, 400, "invalid_cursor", "before must be RFC3339")
			return
		}
		before = parsed
	}
	rows, err := a.Store.AuditEventsForTenant(r.Context(), principal.TenantID, before, limit)
	if err != nil {
		writeError(w, 500, "internal", "audit lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, map[string]any{"id": row.ID, "actor": row.Actor, "action": row.Action, "resource_type": row.ResourceType, "resource_name": row.ResourceName, "outcome": row.Outcome, "request_id": row.RequestID, "payload": json.RawMessage(row.Payload), "created_at": row.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

func (a API) setQuota(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	var request struct {
		MaxDeployments       int `json:"max_deployments"`
		MaxReplicas          int `json:"max_replicas"`
		MaxRequestsPerMinute int `json:"max_requests_per_minute"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if err := a.Store.SetTenantQuota(r.Context(), principal.TenantID, request.MaxDeployments, request.MaxReplicas, request.MaxRequestsPerMinute); err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "quota.update", ResourceType: "tenant", ResourceName: principal.TenantID, Outcome: "succeeded"})
	w.WriteHeader(http.StatusNoContent)
}
func (a API) createTenant(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if actor.ID != "bootstrap" {
		writeError(w, http.StatusForbidden, "forbidden", "only the bootstrap administrator can create tenants")
		return
	}
	var request struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.ID == "" {
		writeError(w, 400, "invalid_request", "tenant id is required")
		return
	}
	if request.Name == "" {
		request.Name = request.ID
	}
	if err := a.Store.CreateTenant(r.Context(), request.ID, request.Name); errors.Is(err, domain.ErrConflict) {
		writeError(w, 409, "conflict", err.Error())
		return
	} else if err != nil {
		writeError(w, 500, "internal", "tenant could not be created")
		return
	}
	writeJSON(w, 201, map[string]any{"tenant": request})
}
func (a API) createPrincipal(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	var request struct {
		Name   string         `json:"name"`
		Role   authz.Role     `json:"role"`
		Scopes []authz.Action `json:"scopes,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if len(request.Scopes) == 0 {
		request.Scopes = authz.DefaultScopes(request.Role)
	}
	if err := authz.ValidateScopes(request.Role, request.Scopes); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	principal, token, err := a.Store.CreatePrincipalScoped(r.Context(), actor.TenantID, request.Name, request.Role, request.Scopes)
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, 409, "conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "principal.create", ResourceType: "principal", ResourceName: principal.ID, Outcome: "succeeded"})
	writeJSON(w, 201, map[string]any{"principal": map[string]any{"id": principal.ID, "name": principal.Name, "role": principal.Role, "kind": principal.Kind, "scopes": principal.Scopes, "tenant_id": principal.TenantID}, "credential": token})
}
func (a API) rotatePrincipal(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	token, err := a.Store.RotatePrincipalForTenant(r.Context(), actor.TenantID, r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "principal was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "credential rotation failed")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "principal.rotate", ResourceType: "principal", ResourceName: r.PathValue("id"), Outcome: "succeeded"})
	writeJSON(w, 200, map[string]string{"credential": token})
}
func (a API) revokePrincipal(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if err := a.Store.RevokePrincipalForTenant(r.Context(), actor.TenantID, r.PathValue("id")); errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "principal was not found")
		return
	} else if err != nil {
		writeError(w, 500, "internal", "principal revocation failed")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "principal.revoke", ResourceType: "principal", ResourceName: r.PathValue("id"), Outcome: "succeeded"})
	w.WriteHeader(http.StatusNoContent)
}

func (a API) secretReferences(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := a.Store.SecretReferencesForTenant(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "secret references could not be listed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items})
}

func (a API) createSecretReference(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	var request struct {
		Name, Resolver, Reference string
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain exactly one JSON object")
		return
	}
	item, err := a.Store.CreateSecretReference(r.Context(), actor.TenantID, request.Name, request.Resolver, request.Reference)
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "secret.create", ResourceType: "secret_reference", ResourceName: item.ID, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"secret": item})
}

func (a API) deleteSecretReference(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	id := r.PathValue("id")
	if err := a.Store.DeleteSecretReferenceForTenant(r.Context(), actor.TenantID, id); errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "secret reference was not found")
		return
	} else if err != nil {
		writeError(w, http.StatusConflict, "conflict", "secret reference could not be deleted")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "secret.delete", ResourceType: "secret_reference", ResourceName: id, Outcome: "succeeded"})
	w.WriteHeader(http.StatusNoContent)
}

func (a API) externalTargetPolicy(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	resolved, err := a.Store.ResolveForTenant(r.Context(), actor.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "deployment could not be resolved")
		return
	}
	policy, err := a.Store.ExternalTargetPolicyForDeployment(r.Context(), actor.TenantID, resolved.Deployment.ID)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "external target policy was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "external target policy could not be read")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": policy})
}

func (a API) setExternalTargetPolicy(w http.ResponseWriter, r *http.Request) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	var request struct {
		TargetName             string   `json:"target"`
		Adapter                string   `json:"adapter"`
		SecretReferenceID      string   `json:"secret_reference_id"`
		Enabled                bool     `json:"enabled"`
		PrivacyAcknowledged    bool     `json:"privacy_acknowledged"`
		RequestLimit           int64    `json:"request_limit"`
		CostLimitMicrousd      int64    `json:"cost_limit_microusd"`
		MaxRequestCostMicrousd int64    `json:"max_request_cost_microusd"`
		OverflowMode           string   `json:"overflow_mode"`
		QueueThreshold         *float64 `json:"queue_threshold"`
		BreachIntervals        int      `json:"breach_intervals"`
		RecoveryIntervals      int      `json:"recovery_intervals"`
		CooldownSeconds        int      `json:"cooldown_seconds"`
		SignalMaxAgeSeconds    int      `json:"signal_max_age_seconds"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain exactly one JSON object")
		return
	}
	if request.Enabled && !request.PrivacyAcknowledged {
		writeError(w, http.StatusUnprocessableEntity, "privacy_acknowledgement_required", "enabled external fallback requires explicit privacy acknowledgement")
		return
	}
	if request.RequestLimit < 1 || request.CostLimitMicrousd < 1 || request.MaxRequestCostMicrousd < 1 || request.MaxRequestCostMicrousd > request.CostLimitMicrousd {
		writeError(w, http.StatusUnprocessableEntity, "budget_required", "positive hard request, cost, and worst-case per-request budgets are required")
		return
	}
	resolved, err := a.Store.ResolveForTenant(r.Context(), actor.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "deployment could not be resolved")
		return
	}
	target, targetErr := a.Store.TargetForTenantByName(r.Context(), actor.TenantID, request.TargetName)
	if errors.Is(targetErr, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "external target was not found")
		return
	}
	if targetErr != nil {
		writeError(w, http.StatusInternalServerError, "internal", "external target could not be read")
		return
	}
	if target.ID == "" || target.Provider != request.Adapter {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "target must match the external adapter")
		return
	}
	policy, err := a.Store.SetExternalTargetPolicyForTenant(r.Context(), domain.ExternalTargetPolicy{TenantID: actor.TenantID, DeploymentID: resolved.Deployment.ID, TargetID: target.ID, Adapter: request.Adapter, SecretReferenceID: request.SecretReferenceID, Enabled: request.Enabled, PrivacyAcknowledged: request.PrivacyAcknowledged, RequestLimit: request.RequestLimit, CostLimitMicrousd: request.CostLimitMicrousd, MaxRequestCostMicrousd: request.MaxRequestCostMicrousd, OverflowMode: request.OverflowMode, QueueThreshold: request.QueueThreshold, BreachIntervals: request.BreachIntervals, RecoveryIntervals: request.RecoveryIntervals, CooldownSeconds: request.CooldownSeconds, SignalMaxAgeSeconds: request.SignalMaxAgeSeconds})
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "target or secret reference was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "external_policy.update", ResourceType: "deployment", ResourceName: resolved.Deployment.Name, Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"policy": policy})
}

func (a API) deployments(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := a.Store.DeploymentsForTenant(r.Context(), principal.TenantID)
	if err != nil {
		writeError(w, 500, "internal", "deployment lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, deploymentResponse(row))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) deployment(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	resolved, err := a.Store.ResolveForTenant(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "deployment lookup failed")
		return
	}
	replicas, err := a.Store.ReplicasForDeployment(r.Context(), principal.TenantID, resolved.Deployment.ID)
	if err != nil {
		writeError(w, 500, "internal", "replica lookup failed")
		return
	}
	revisions, err := a.Store.Revisions(r.Context(), principal.TenantID, resolved.Deployment.Name)
	if err != nil {
		writeError(w, 500, "internal", "revision lookup failed")
		return
	}
	stats, err := a.Store.RequestStats(r.Context(), resolved.Deployment.ID, 5*time.Minute)
	if err != nil {
		writeError(w, 500, "internal", "request statistics lookup failed")
		return
	}
	coldStarts, err := a.Store.ColdStartStats(r.Context(), resolved.Deployment.ID, 24*time.Hour)
	if err != nil {
		writeError(w, 500, "internal", "cold-start statistics lookup failed")
		return
	}
	guardEvaluations, err := a.Store.ReleaseGuardEvaluations(r.Context(), principal.TenantID, resolved.Deployment.Name, 20)
	if err != nil {
		writeError(w, 500, "internal", "Release Guard evaluation lookup failed")
		return
	}
	guardPolicy, err := a.Store.ReleaseGuardPolicy(r.Context(), principal.TenantID, resolved.Deployment.Name)
	if err != nil {
		writeError(w, 500, "internal", "Release Guard policy lookup failed")
		return
	}
	activeOperation, operationErr := a.Store.ActiveOperationForResource(r.Context(), principal.TenantID, "deployment", resolved.Deployment.Name)
	if operationErr != nil && !errors.Is(operationErr, domain.ErrNotFound) {
		writeError(w, 500, "internal", "active operation lookup failed")
		return
	}
	targets := make([]map[string]any, 0, len(resolved.Targets))
	for _, target := range resolved.Targets {
		targets = append(targets, targetResponse(target))
	}
	replicaData := make([]map[string]any, 0, len(replicas))
	for _, replica := range replicas {
		replicaData = append(replicaData, replicaResponse(replica))
	}
	revisionData := make([]map[string]any, 0, len(revisions))
	artifactData := make([]map[string]any, 0, len(revisions))
	for _, revision := range revisions {
		revisionData = append(revisionData, revisionResponse(revision))
		modelArtifact, artifactErr := a.Store.ModelArtifactForRevision(r.Context(), principal.TenantID, revision.ID)
		if artifactErr == nil {
			artifactData = append(artifactData, artifactResponse(revision.ID, modelArtifact))
		} else if !errors.Is(artifactErr, domain.ErrNotFound) {
			writeError(w, 500, "internal", "model artifact lookup failed")
			return
		}
	}
	guardData := make([]map[string]any, 0, len(guardEvaluations))
	for _, evaluation := range guardEvaluations {
		guardData = append(guardData, releaseGuardResponse(evaluation))
	}
	response := map[string]any{"deployment": deploymentResponse(resolved.Deployment), "targets": targets, "replicas": replicaData, "revisions": revisionData, "model_artifacts": artifactData, "request_stats": stats, "cold_start_stats": coldStarts, "release_guard_policy": guardPolicy, "release_guard_evaluations": guardData}
	response["lifecycle_status"] = deploymentLifecycleStatus(resolved, replicas, revisions, activeOperation, operationErr == nil)
	if operationErr == nil {
		response["active_operation"] = operationResponse(activeOperation)
	}
	writeJSON(w, 200, response)
}

type lifecycleStatus struct {
	ServingState          string `json:"serving_state"`
	ConvergenceState      string `json:"convergence_state"`
	CandidateState        string `json:"candidate_state"`
	ReadyReplicas         int    `json:"ready_replicas"`
	DesiredReplicas       int    `json:"desired_replicas"`
	ProvisioningReplicas  int    `json:"provisioning_replicas"`
	DrainingReplicas      int    `json:"draining_replicas"`
	UnhealthyTargets      int    `json:"unhealthy_targets"`
	BlockingOperationID   string `json:"blocking_operation_id,omitempty"`
	BlockingOperationKind string `json:"blocking_operation_kind,omitempty"`
}

func deploymentLifecycleStatus(resolved domain.ResolvedDeployment, replicas []domain.Replica, revisions []domain.DeploymentRevision, operation domain.Operation, hasOperation bool) lifecycleStatus {
	status := lifecycleStatus{ServingState: "unavailable", ConvergenceState: "converged", CandidateState: "none", DesiredReplicas: resolved.Deployment.MinReplicas}
	healthyTargets := 0
	for _, target := range resolved.Targets {
		if target.Health == "healthy" {
			status.ServingState = "serving"
			healthyTargets++
		} else {
			status.UnhealthyTargets++
		}
	}
	// Existing-target deployments have no provider-managed replica rows. Their
	// healthy routed targets are the serving capacity and must participate in
	// the same normalized readiness summary without double-counting managed
	// replicas, which also materialize as targets once ready.
	if len(replicas) == 0 {
		status.ReadyReplicas = healthyTargets
		status.DesiredReplicas = len(resolved.Targets)
	}
	for _, replica := range replicas {
		if replica.RevisionID == resolved.Deployment.ActiveRevisionID || resolved.Deployment.ActiveRevisionID == "" {
			if replica.Health == "healthy" && (replica.LifecycleState == "active" || replica.LifecycleState == "ready") {
				status.ReadyReplicas++
			}
		}
		switch replica.LifecycleState {
		case "pending", "provisioning", "starting":
			status.ProvisioningReplicas++
		case "draining", "deleting":
			status.DrainingReplicas++
		}
	}
	if status.ServingState == "unavailable" && status.ReadyReplicas > 0 {
		// Route publication can lag a freshly healthy replica by one reconciliation
		// interval. Report it as ready but not yet serving, not as unhealthy.
		status.ServingState = "ready"
	}
	for _, revision := range revisions {
		if revision.ID == resolved.Deployment.CandidateRevisionID {
			status.CandidateState = revision.Status
			break
		}
	}
	if hasOperation {
		status.ConvergenceState = "converging"
		status.BlockingOperationID = operation.ID
		status.BlockingOperationKind = operation.Kind
		var request struct {
			DesiredReplicas int `json:"desired_replicas"`
		}
		if json.Unmarshal([]byte(operation.RequestJSON), &request) == nil && request.DesiredReplicas > 0 {
			status.DesiredReplicas = request.DesiredReplicas
		}
	} else if status.ProvisioningReplicas > 0 || status.DrainingReplicas > 0 {
		status.ConvergenceState = "converging"
	} else if status.UnhealthyTargets > 0 {
		status.ConvergenceState = "degraded"
	}
	return status
}
func (a API) deploymentEvents(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := a.Store.EventsForTenant(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "deployment event lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, event := range rows {
		data = append(data, map[string]any{"id": event.ID, "type": event.Type, "summary": event.Summary, "payload": json.RawMessage(event.Payload), "target_id": event.TargetID, "created_at": event.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) revisions(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := a.Store.Revisions(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	} else if err != nil {
		writeError(w, 500, "internal", "revision lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, revision := range rows {
		data = append(data, revisionResponse(revision))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

func (a API) createRollout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Spec json.RawMessage `json:"spec"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	a.enqueueRollout(w, r, workflows.RolloutCreateKind, workflows.RolloutRequest{Name: r.PathValue("name"), Spec: body.Spec})
}

func (a API) evaluateReleaseGuard(w http.ResponseWriter, r *http.Request) {
	a.enqueueRollout(w, r, workflows.ReleaseGuardEvaluateKind, workflows.RolloutRequest{Name: r.PathValue("name")})
}

func (a API) releaseGuardPolicy(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	policy, err := a.Store.ReleaseGuardPolicy(r.Context(), principal.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Release Guard policy lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

func (a API) setReleaseGuardPolicy(w http.ResponseWriter, r *http.Request) {
	var policy domain.ReleaseGuardPolicy
	if !decodeMutationBody(w, r, &policy) {
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	updated, err := a.Store.SetReleaseGuardPolicy(r.Context(), principal.TenantID, r.PathValue("name"), policy)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "release_guard.policy.update", ResourceType: "deployment", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, updated)
}

func (a API) promoteRollout(w http.ResponseWriter, r *http.Request) {
	a.enqueueRollout(w, r, workflows.RolloutPromoteKind, workflows.RolloutRequest{Name: r.PathValue("name"), CandidateID: r.PathValue("revision")})
}

func (a API) provisionRollout(w http.ResponseWriter, r *http.Request) {
	a.enqueueRollout(w, r, workflows.RolloutProvisionKind, workflows.RolloutRequest{Name: r.PathValue("name"), CandidateID: r.PathValue("revision")})
}

func (a API) rejectRollout(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	a.enqueueRollout(w, r, workflows.RolloutRejectKind, workflows.RolloutRequest{Name: r.PathValue("name"), CandidateID: r.PathValue("revision"), Reason: body.Reason})
}

func (a API) rollbackDeployment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RevisionID string `json:"revision_id"`
		Reason     string `json:"reason"`
	}
	if !decodeMutationBody(w, r, &body) {
		return
	}
	a.enqueueRollout(w, r, workflows.RolloutRollbackKind, workflows.RolloutRequest{Name: r.PathValue("name"), RevisionID: body.RevisionID, Reason: body.Reason})
}

func decodeMutationBody(w http.ResponseWriter, r *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return false
	}
	return true
}

func (a API) enqueueRollout(w http.ResponseWriter, r *http.Request, kind string, request workflows.RolloutRequest) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must not exceed 128 characters")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	request.Actor, request.TenantID = principal.Name, principal.TenantID
	encoded, _ := json.Marshal(request)
	maxAttempts := 5
	if kind == workflows.RolloutProvisionKind || kind == workflows.RolloutPromoteKind || kind == workflows.RolloutRejectKind {
		maxAttempts = 120
	}
	operation, created, err := a.Store.EnqueueOperation(r.Context(), domain.Operation{TenantID: principal.TenantID, Kind: kind, ResourceType: "deployment", ResourceName: request.Name, IdempotencyKey: key, RequestJSON: string(encoded), MaxAttempts: maxAttempts})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "rollout operation could not be queued")
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	status := http.StatusAccepted
	if !created && operation.Status == "succeeded" {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"operation": operationResponse(operation), "created": created})
}
func (a API) scalingDecisions(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.Store.ScalingDecisionsForTenant(r.Context(), principal.TenantID, r.PathValue("name"), limit)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	} else if err != nil {
		writeError(w, 500, "internal", "scaling decision lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, map[string]any{"id": row.ID, "action": row.Action, "old_replicas": row.OldReplicas, "new_replicas": row.NewReplicas, "reason": row.Reason, "signals": json.RawMessage(row.SignalsJSON), "created_at": row.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) setRoute(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	var request struct {
		Strategy string `json:"strategy"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || request.Strategy == "" {
		writeError(w, 400, "invalid_request", "routing strategy is required")
		return
	}
	if err := a.Store.SetRouteForTenant(r.Context(), principal.TenantID, r.PathValue("name"), request.Strategy); errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	} else if err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"deployment": r.PathValue("name"), "routing_strategy": request.Strategy})
}
func (a API) targets(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := a.Store.TargetsForTenant(r.Context(), principal.TenantID)
	if err != nil {
		writeError(w, 500, "internal", "target lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, targetResponse(row))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) orphans(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := a.Store.OrphanedTargetsForTenant(r.Context(), principal.TenantID)
	if err != nil {
		writeError(w, 500, "internal", "orphan lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, map[string]any{"target_id": row.TargetID, "name": row.Name, "provider": row.Provider, "provider_resource_id": row.ProviderResourceID, "created_at": row.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) addTarget(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	var request struct {
		Name          string `json:"name"`
		URL           string `json:"url"`
		Provider      string `json:"provider,omitempty"`
		Runtime       string `json:"runtime"`
		UpstreamModel string `json:"upstream_model,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, 400, "invalid_request", err.Error())
		return
	}
	if request.Provider == "" {
		request.Provider = "existing"
	}
	if request.Runtime == "" {
		request.Runtime = support.DefaultRuntime
		if request.Provider != "existing" {
			request.Runtime = "openai-compatible-api"
		}
	}
	if request.Provider != "existing" && request.Provider != "openrouter" && request.Provider != "openai-compatible-external" {
		writeError(w, 422, "validation_failed", "provider must be existing, openrouter, or openai-compatible-external")
		return
	}
	if request.Provider != "existing" {
		if request.UpstreamModel == "" {
			writeError(w, 422, "validation_failed", "external targets require an explicit upstream_model")
			return
		}
		if !authz.AllowedScoped(authz.Role(principal.Role), principal.Scopes, authz.ManageExternal) {
			writeError(w, http.StatusForbidden, "forbidden", "principal is not allowed to register external targets")
			return
		}
	}
	parsed, err := url.Parse(request.URL)
	if request.Name == "" || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		writeError(w, 422, "validation_failed", "name and an absolute HTTP(S) URL without credentials or a fragment are required")
		return
	}
	if request.Provider != "existing" && parsed.RawQuery != "" {
		writeError(w, 422, "validation_failed", "external target URLs cannot contain query parameters")
		return
	}
	target, err := a.Store.AddTargetForTenant(r.Context(), principal.TenantID, domain.Target{Name: request.Name, URL: request.URL, Provider: request.Provider, Runtime: request.Runtime, UpstreamModel: request.UpstreamModel})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, 409, "conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "target could not be registered")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "target.create", ResourceType: "target", ResourceName: request.Name, Outcome: "succeeded"})
	writeJSON(w, 201, map[string]any{"target": targetResponse(target)})
}

func deploymentResponse(row domain.Deployment) map[string]any {
	return map[string]any{"id": row.ID, "tenant_id": row.TenantID, "name": row.Name, "model": row.Model, "runtime": row.Runtime, "routing_strategy": row.RoutingStrategy, "desired_state": row.DesiredState, "observed_state": row.ObservedState, "min_replicas": row.MinReplicas, "max_replicas": row.MaxReplicas, "autoscaling_enabled": row.AutoscalingEnabled, "active_revision_id": row.ActiveRevisionID, "candidate_revision_id": row.CandidateRevisionID, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}
}
func targetResponse(row domain.Target) map[string]any {
	return map[string]any{"id": row.ID, "name": row.Name, "url": row.URL, "provider": row.Provider, "runtime": row.Runtime, "upstream_model": row.UpstreamModel, "health": row.Health, "provider_resource_id": row.ProviderResourceID, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}
}
func replicaResponse(row domain.Replica) map[string]any {
	return map[string]any{"id": row.ID, "revision_id": row.RevisionID, "ordinal": row.Ordinal, "external_key": row.ExternalKey, "lifecycle_state": row.LifecycleState, "provider": row.Provider, "provider_request_id": row.ProviderRequestID, "provider_resource_id": row.ProviderResourceID, "endpoint": row.Endpoint, "health": row.Health, "provider_details": json.RawMessage(row.ProviderDetails), "last_observed_at": row.LastObservedAt, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}
}
func revisionResponse(row domain.DeploymentRevision) map[string]any {
	return map[string]any{"id": row.ID, "number": row.Number, "status": row.Status, "spec": json.RawMessage(row.SpecJSON), "source_revision_id": row.SourceRevisionID, "reason": row.Reason, "created_at": row.CreatedAt, "activated_at": row.ActivatedAt, "completed_at": row.CompletedAt}
}
func artifactResponse(revisionID string, row domain.ModelArtifact) map[string]any {
	return map[string]any{"id": row.ID, "revision_id": revisionID, "source": row.Source, "repository": row.Repository, "requested_revision": row.RequestedRevision, "immutable_revision": row.ImmutableRevision, "model_identity": row.ModelIdentity, "approximate_size_bytes": row.ApproximateSizeBytes, "cache_state": row.CacheState, "runtime_compatibility": json.RawMessage(row.RuntimeCompatibilityJSON), "resolved_at": row.ResolvedAt}
}

func releaseGuardResponse(row domain.ReleaseGuardEvaluation) map[string]any {
	return map[string]any{"id": row.ID, "active_revision_id": row.ActiveRevisionID, "candidate_revision_id": row.CandidateRevisionID, "decision": row.Decision, "reasons": json.RawMessage(row.ReasonCodesJSON), "metrics": json.RawMessage(row.MetricsJSON), "policy": json.RawMessage(row.PolicyJSON), "created_at": row.CreatedAt}
}

func (a API) applyDeployment(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must not exceed 128 characters")
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request workflows.ApplyExistingRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid: "+err.Error())
		return
	}
	if err := request.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	request.Actor = principal.Name
	request.TenantID = principal.TenantID
	encoded, _ := json.Marshal(request)
	operation, created, err := a.Store.EnqueueOperation(r.Context(), domain.Operation{TenantID: principal.TenantID, Kind: workflows.ApplyExistingKind, ResourceType: "deployment", ResourceName: request.Name, IdempotencyKey: key, RequestJSON: string(encoded)})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "operation could not be queued")
		return
	}
	w.Header().Set("Location", "/api/v1/operations/"+operation.ID)
	status := http.StatusAccepted
	if !created && operation.Status == "succeeded" {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"operation": operationResponse(operation), "created": created})
}
func (a API) auth(action authz.Action, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		header := r.Header.Get("Authorization")
		token := strings.TrimPrefix(header, "Bearer ")
		var principal domain.Principal
		expected := "Bearer " + a.APIKey
		if a.APIKey != "" && subtle.ConstantTimeCompare([]byte(header), []byte(expected)) == 1 {
			principal = domain.Principal{ID: "bootstrap", TenantID: "global", Name: "bootstrap", Role: string(authz.Admin), Kind: "bootstrap", Scopes: authz.DefaultScopeNames(authz.Admin)}
		} else if a.Authenticator != nil && token != "" {
			resolved, err := a.Authenticator.AuthenticatePrincipal(r.Context(), token)
			if err == nil {
				principal = resolved
			}
		}
		if principal.ID == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "valid bearer authentication is required")
			return
		}
		if !authz.AllowedScoped(authz.Role(principal.Role), principal.Scopes, action) {
			writeError(w, http.StatusForbidden, "forbidden", "principal is not allowed to perform this action")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, principal)))
	}
}
func (a API) operation(w http.ResponseWriter, r *http.Request) {
	op, err := a.Store.Operation(r.Context(), r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "operation was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "operation lookup failed")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	if op.TenantID != principal.TenantID {
		writeError(w, http.StatusNotFound, "not_found", "operation was not found")
		return
	}
	writeJSON(w, http.StatusOK, operationResponse(op))
}
func (a API) operationEvents(w http.ResponseWriter, r *http.Request) {
	op, err := a.Store.Operation(r.Context(), r.PathValue("id"))
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	if err != nil || op.TenantID != principal.TenantID {
		writeError(w, http.StatusNotFound, "not_found", "operation was not found")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := a.Store.OperationEvents(r.Context(), op.ID, limit)
	if err != nil {
		writeError(w, 500, "internal", "operation event lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, event := range rows {
		data = append(data, map[string]any{"sequence": event.Sequence, "level": event.Level, "type": event.Type, "message": event.Message, "payload": json.RawMessage(event.Payload), "created_at": event.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) cancel(w http.ResponseWriter, r *http.Request) {
	op, lookupErr := a.Store.Operation(r.Context(), r.PathValue("id"))
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	if lookupErr != nil || op.TenantID != principal.TenantID {
		writeError(w, http.StatusNotFound, "not_found", "operation was not found")
		return
	}
	err := a.Store.RequestOperationCancel(r.Context(), r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusConflict, "not_cancellable", "operation is missing or already terminal")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "cancellation request failed")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "operation.cancel", ResourceType: "operation", ResourceName: r.PathValue("id"), Outcome: "accepted"})
	writeJSON(w, http.StatusAccepted, map[string]string{"operation_id": r.PathValue("id"), "status": "cancellation_requested"})
}
func operationResponse(op domain.Operation) map[string]any {
	return map[string]any{"id": op.ID, "tenant_id": op.TenantID, "kind": op.Kind, "resource_type": op.ResourceType, "resource_name": op.ResourceName, "status": op.Status, "progress": op.Progress, "message": op.Message, "error_code": op.ErrorCode, "attempt": op.Attempt, "max_attempts": op.MaxAttempts, "retryable": op.Retryable, "cancel_requested": op.CancelRequested, "created_at": op.CreatedAt, "updated_at": op.UpdatedAt, "completed_at": op.CompletedAt, "next_attempt_at": op.NextAttemptAt}
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	category, retryable, remediation := classifyError(status, code)
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "category": category, "message": message, "retryable": retryable, "remediation": remediation}})
}

func classifyError(status int, code string) (category string, retryable bool, remediation string) {
	switch {
	case code == "unauthenticated":
		return "authentication", false, "Configure a valid control-plane credential with infercrane init or INFERCRANE_API_KEY."
	case code == "forbidden":
		return "authorization", false, "Use a principal whose role permits this operation."
	case status == http.StatusNotFound:
		return "not_found", false, "Inspect the deployment, revision, or operation name and retry with an existing resource."
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return "validation", false, "Correct the request described by the message; infercrane plan can validate deployment changes without mutation."
	case status == http.StatusConflict:
		return "conflict", false, "Inspect current status and active durable operations before retrying with the same idempotency key."
	case status == http.StatusTooManyRequests:
		return "rate_limit", true, "Wait for provider or tenant capacity and retry with the same idempotency key."
	case status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout:
		return "dependency", true, "Inspect durable operation events and provider status, then retry with the same idempotency key."
	case status >= http.StatusInternalServerError:
		return "internal", true, "Inspect control-plane logs and durable events, then retry without changing the idempotency key."
	default:
		return "request", false, "Inspect the error message and current deployment state before retrying."
	}
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
