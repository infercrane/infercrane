// Package controlapi exposes stable, authenticated control-plane HTTP contracts.
package controlapi

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/asyncinference"
	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/burstguard"
	"github.com/infercrane/infercrane/internal/contextpassport"
	"github.com/infercrane/infercrane/internal/decision"
	"github.com/infercrane/infercrane/internal/doctor"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/external"
	"github.com/infercrane/infercrane/internal/finops"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/recipe"
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
type adoptionStore interface {
	AdoptEndpoint(context.Context, string, string, string, string, string, string, string, string) (domain.ResolvedEndpoint, domain.AdoptedWorkload, error)
	PromoteAdoptionOwnership(context.Context, string, string, string) (domain.AdoptedWorkload, error)
}
type diagnosticStore interface {
	RequestInspectionForTenant(context.Context, string, string) (domain.RequestInspection, error)
	DiagnoseEndpoint(context.Context, string, string, time.Duration) ([]domain.DiagnosticFinding, error)
}
type intelligenceStore interface {
	CaptureReplayTrace(context.Context, string, string, time.Duration, int) (domain.ReplayTrace, error)
	ReplayTrace(context.Context, string, string) (domain.ReplayTrace, error)
	RecordArtifactCacheObservation(context.Context, string, domain.ArtifactCacheObservation) (domain.ArtifactCacheObservation, error)
	RequestArtifactPrefetch(context.Context, string, domain.ArtifactPrefetch) (domain.ArtifactPrefetch, bool, error)
	CapacityIntelligence(context.Context, string, time.Duration) ([]domain.CapacitySummary, error)
}
type optimizationStore interface {
	RecordFinOpsReport(context.Context, domain.FinOpsReport) (domain.FinOpsReport, error)
	FinOpsReports(context.Context, string, string, int) ([]domain.FinOpsReport, error)
	CreateAutopilotPlan(context.Context, domain.AutopilotPlan) (domain.AutopilotPlan, bool, error)
	AutopilotPlan(context.Context, string, string, string, string) (domain.AutopilotPlan, error)
	ApproveAutopilotPlan(context.Context, string, string, string) (domain.AutopilotPlan, error)
}
type contextBurstStore interface {
	CreateContextPassport(context.Context, string, string, domain.ContextPassport) (domain.ContextPassport, error)
	ContextPassport(context.Context, string, string) (domain.ContextPassport, error)
	SetBurstGuardPolicy(context.Context, string, string, domain.BurstGuardPolicy) (domain.BurstGuardPolicy, error)
	RecordBurstGuardDecision(context.Context, domain.BurstGuardDecision) (domain.BurstGuardDecision, error)
}
type alertStore interface {
	CreateAlertPolicy(context.Context, string, string, domain.AlertPolicy) (domain.AlertPolicy, error)
	AlertPoliciesForEndpoint(context.Context, string, string) ([]domain.AlertPolicy, error)
}
type admissionStore interface {
	AdmissionPolicy(context.Context, string, string) (domain.AdmissionPolicy, error)
	SetAdmissionPolicy(context.Context, string, string, domain.AdmissionPolicy) (domain.AdmissionPolicy, error)
}
type asyncInferenceService interface {
	Submit(context.Context, asyncinference.SubmitRequest) (domain.AsyncInferenceJob, bool, error)
	Result(context.Context, string, string) (domain.AsyncInferenceJob, []byte, error)
	Cancel(context.Context, string, string) error
}
type controlPlaneMembershipStore interface {
	ControlPlaneInstances(context.Context, time.Duration) ([]domain.ControlPlaneInstance, error)
}
type recipeLabStore interface {
	CreateModelRecipe(context.Context, string, domain.ModelRecipe) (domain.ModelRecipe, error)
	ModelRecipe(context.Context, string, string, string) (domain.ModelRecipe, error)
	ModelRecipes(context.Context, string, string, int) ([]domain.ModelRecipe, error)
	BenchmarksForModel(context.Context, string, string, int) ([]domain.BenchmarkResult, error)
	RecordLabEvaluation(context.Context, string, domain.LabEvaluation) (domain.LabEvaluation, error)
}
type qualityEvidenceStore interface {
	RecordQualityEvidence(context.Context, string, string, domain.QualityEvidence) (domain.QualityEvidence, bool, error)
	QualityEvidenceForDeployment(context.Context, string, string, int) ([]domain.QualityEvidence, error)
}
type consoleIdentityStore interface {
	ProvisionConsoleIdentity(context.Context, string, domain.ConsoleIdentityProvisioning) (domain.ConsoleIdentity, error)
}
type consoleIdentityListStore interface {
	ConsoleIdentitiesForTenant(context.Context, string) ([]domain.ConsoleIdentity, error)
}
type operationListStore interface {
	OperationsForTenant(context.Context, string, time.Time, int) ([]domain.Operation, error)
}
type principalListStore interface {
	PrincipalsForTenant(context.Context, string) ([]domain.Principal, error)
}
type artifactCacheStateStore interface {
	ArtifactCacheState(context.Context, string, string) ([]domain.ArtifactCacheObservation, []domain.ArtifactPrefetch, error)
}
type environmentPromotionStore interface {
	StageEnvironmentPromotion(context.Context, string, string, string, string) (domain.EnvironmentPromotion, bool, error)
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
	Diagnostics              func(context.Context, bool, bool, bool, bool, bool) doctor.Report
	Backends                 map[string]BackendMetadata
	Integrations             integration.Snapshot
	GatewayURL, AIPerfBinary string
	PassportPrivateKey       ed25519.PrivateKey
	EndpointRefresh          func(context.Context) error
	DiscoveryClient          *http.Client
	AlertDeliverer           interface {
		Deliver(context.Context, domain.AlertPolicy, domain.DiagnosticFinding) (domain.AlertDelivery, error)
	}
	AsyncInference   asyncInferenceService
	ContextPassports interface{ Put(contextpassport.Hint) }
	ProductVersion   string
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
	mux.HandleFunc("GET /api/v1/operations", a.auth(authz.Read, a.operations))
	mux.HandleFunc("GET /api/v1/doctor", a.auth(authz.Read, a.diagnostics))
	mux.HandleFunc("GET /api/v1/whoami", a.auth(authz.Read, a.whoami))
	mux.HandleFunc("GET /api/v1/console/session", a.auth(authz.Read, a.consoleSession))
	mux.HandleFunc("PUT /api/v1/console/access", a.auth(authz.ManageTenant, a.configureConsoleAccess))
	mux.HandleFunc("GET /api/v1/console/access", a.auth(authz.ManageTenant, a.consoleAccess))
	mux.HandleFunc("GET /api/v1/integrations", a.auth(authz.Read, a.integrations))
	mux.HandleFunc("GET /api/v1/system/instances", a.auth(authz.Read, a.controlPlaneInstances))
	mux.HandleFunc("POST /api/v1/deployments/{name}/recipes", a.auth(authz.Deploy, a.captureRecipe))
	mux.HandleFunc("GET /api/v1/recipes", a.auth(authz.Read, a.recipes))
	mux.HandleFunc("GET /api/v1/recipes/{name}/{version}", a.auth(authz.Read, a.getRecipe))
	mux.HandleFunc("POST /api/v1/lab/evaluations", a.auth(authz.Deploy, a.evaluateLab))
	mux.HandleFunc("POST /api/v1/deployments/{name}/replays", a.auth(authz.Deploy, a.captureReplay))
	mux.HandleFunc("GET /api/v1/replays/{id}", a.auth(authz.Read, a.getReplay))
	mux.HandleFunc("GET /api/v1/capacity/intelligence", a.auth(authz.Read, a.capacityIntelligence))
	mux.HandleFunc("POST /api/v1/artifacts/{id}/cache-observations", a.auth(authz.Deploy, a.recordCacheObservation))
	mux.HandleFunc("POST /api/v1/artifacts/{id}/prefetches", a.auth(authz.Deploy, a.requestPrefetch))
	mux.HandleFunc("GET /api/v1/artifacts/{id}/cache", a.auth(authz.Read, a.artifactCacheState))
	mux.HandleFunc("POST /api/v1/deployments/{name}/finops/reports", a.auth(authz.Deploy, a.createFinOpsReport))
	mux.HandleFunc("GET /api/v1/deployments/{name}/finops/reports", a.auth(authz.Read, a.finOpsReports))
	mux.HandleFunc("POST /api/v1/deployments/{name}/autopilot/plans", a.auth(authz.Deploy, a.createAutopilotPlan))
	mux.HandleFunc("GET /api/v1/autopilot/plans/{id}", a.auth(authz.Read, a.getAutopilotPlan))
	mux.HandleFunc("POST /api/v1/autopilot/plans/{id}/approve", a.auth(authz.Deploy, a.approveAutopilotPlan))
	mux.HandleFunc("POST /api/v1/context-passports", a.auth(authz.Deploy, a.createContextPassport))
	mux.HandleFunc("GET /api/v1/context-passports/{id}", a.auth(authz.Read, a.getContextPassport))
	mux.HandleFunc("POST /api/v1/deployments/{name}/burst-guard/evaluate", a.auth(authz.Deploy, a.evaluateBurstGuard))
	mux.HandleFunc("GET /api/v1/environments", a.auth(authz.Read, a.environments))
	mux.HandleFunc("POST /api/v1/environments", a.auth(authz.Deploy, a.createEnvironment))
	mux.HandleFunc("POST /api/v1/environment-promotions", a.auth(authz.Deploy, a.stageEnvironmentPromotion))
	mux.HandleFunc("GET /api/v1/logical-models", a.auth(authz.Read, a.logicalModels))
	mux.HandleFunc("POST /api/v1/logical-models", a.auth(authz.Deploy, a.createLogicalModel))
	mux.HandleFunc("GET /api/v1/endpoints", a.auth(authz.Read, a.endpoints))
	mux.HandleFunc("POST /api/v1/endpoints", a.auth(authz.Deploy, a.createEndpoint))
	mux.HandleFunc("POST /api/v1/adoptions/endpoints", a.auth(authz.Deploy, a.adoptEndpoint))
	mux.HandleFunc("PUT /api/v1/adoptions/endpoints/{name}/ownership", a.auth(authz.Deploy, a.promoteAdoptionOwnership))
	mux.HandleFunc("GET /api/v1/requests/{id}", a.auth(authz.Read, a.requestInspection))
	mux.HandleFunc("POST /api/v1/endpoints/{name}/doctor", a.auth(authz.Deploy, a.diagnoseEndpoint))
	mux.HandleFunc("GET /api/v1/endpoints/{name}/alerts", a.auth(authz.Read, a.alertPolicies))
	mux.HandleFunc("POST /api/v1/endpoints/{name}/alerts", a.auth(authz.Deploy, a.createAlertPolicy))
	mux.HandleFunc("POST /api/v1/endpoints/{name}/alerts/evaluate", a.auth(authz.Deploy, a.evaluateAlerts))
	mux.HandleFunc("GET /api/v1/endpoints/{name}/admission", a.auth(authz.Read, a.admissionPolicy))
	mux.HandleFunc("PUT /api/v1/endpoints/{name}/admission", a.auth(authz.Deploy, a.setAdmissionPolicy))
	mux.HandleFunc("POST /api/v1/endpoints/{name}/async", a.auth(authz.Deploy, a.submitAsyncInference))
	mux.HandleFunc("GET /api/v1/async/jobs/{id}", a.auth(authz.Read, a.asyncInferenceJob))
	mux.HandleFunc("DELETE /api/v1/async/jobs/{id}", a.auth(authz.Deploy, a.cancelAsyncInferenceJob))
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
	mux.HandleFunc("POST /api/v1/deployments/{name}/quality-evidence", a.auth(authz.Deploy, a.recordQualityEvidence))
	mux.HandleFunc("GET /api/v1/deployments/{name}/quality-evidence", a.auth(authz.Read, a.qualityEvidence))
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
	mux.HandleFunc("GET /api/v1/principals", a.auth(authz.ManageTenant, a.principals))
	mux.HandleFunc("POST /api/v1/principals/{id}/rotate", a.auth(authz.ManageTenant, a.rotatePrincipal))
	mux.HandleFunc("DELETE /api/v1/principals/{id}", a.auth(authz.ManageTenant, a.revokePrincipal))
	mux.HandleFunc("GET /api/v1/secrets", a.auth(authz.ManageSecrets, a.secretReferences))
	mux.HandleFunc("POST /api/v1/secrets", a.auth(authz.ManageSecrets, a.createSecretReference))
	mux.HandleFunc("DELETE /api/v1/secrets/{id}", a.auth(authz.ManageSecrets, a.deleteSecretReference))
	mux.HandleFunc("GET /api/v1/deployments/{name}/external-policy", a.auth(authz.Read, a.externalTargetPolicy))
	mux.HandleFunc("PUT /api/v1/deployments/{name}/external-policy", a.auth(authz.ManageExternal, a.setExternalTargetPolicy))
	return mux
}

func (a API) recordQualityEvidence(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(qualityEvidenceStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "quality evidence storage is not configured")
		return
	}
	var envelope passport.Envelope
	if !decodeMutationBody(w, r, &envelope) {
		return
	}
	payload, err := qualityevidence.Decode(envelope)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_quality_evidence", err.Error())
		return
	}
	if payload.Deployment != r.PathValue("name") {
		writeError(w, http.StatusUnprocessableEntity, "revision_mismatch", "quality evidence deployment does not match the request path")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	item := domain.QualityEvidence{RevisionID: payload.RevisionID, Suite: payload.Suite, SuiteVersion: payload.SuiteVersion, Evaluator: payload.Evaluator, EvaluatorVersion: payload.EvaluatorVersion, Score: payload.Score, Passed: payload.Passed, SampleCount: payload.SampleCount, ArtifactDigest: payload.ArtifactDigest, PayloadDigest: envelope.Digest, Signature: envelope.Signature, PublicKey: envelope.PublicKey, Algorithm: envelope.Algorithm, KeyID: envelope.KeyID, EvaluatedAt: payload.EvaluatedAt}
	item, created, err := store.RecordQualityEvidence(r.Context(), actor.TenantID, payload.Deployment, item)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "deployment or revision was not found")
		return
	}
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "idempotency_conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "quality evidence could not be persisted")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "quality_evidence.attach", ResourceType: "deployment_revision", ResourceName: payload.RevisionID, Outcome: "succeeded"})
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"evidence": qualityEvidenceResponse(item), "created": created, "content_recorded": false, "decision_authority": "persisted_release_guard_policy"})
}

func (a API) qualityEvidence(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(qualityEvidenceStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "quality evidence storage is not configured")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := store.QualityEvidenceForDeployment(r.Context(), actor.TenantID, r.PathValue("name"), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "quality evidence could not be listed")
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data = append(data, qualityEvidenceResponse(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "content_recorded": false})
}

func qualityEvidenceResponse(item domain.QualityEvidence) map[string]any {
	return map[string]any{"id": item.ID, "deployment_id": item.DeploymentID, "revision_id": item.RevisionID, "suite": item.Suite, "suite_version": item.SuiteVersion, "evaluator": item.Evaluator, "evaluator_version": item.EvaluatorVersion, "score": item.Score, "passed": item.Passed, "sample_count": item.SampleCount, "artifact_digest": item.ArtifactDigest, "payload_digest": item.PayloadDigest, "algorithm": item.Algorithm, "key_id": item.KeyID, "evaluated_at": item.EvaluatedAt, "created_at": item.CreatedAt}
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

func (a API) stageEnvironmentPromotion(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(environmentPromotionStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "environment promotion is not configured")
		return
	}
	var request struct {
		SourceEndpoint      string `json:"source_endpoint"`
		DestinationEndpoint string `json:"destination_endpoint"`
		IdempotencyKey      string `json:"idempotency_key"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	promotion, created, err := store.StageEnvironmentPromotion(r.Context(), actor.TenantID, request.SourceEndpoint, request.DestinationEndpoint, request.IdempotencyKey)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "promotion_conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	refresh := a.refreshEndpoints(r.Context())
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "environment_promotion.stage", ResourceType: "endpoint", ResourceName: request.DestinationEndpoint, Outcome: "succeeded"})
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"promotion": map[string]any{"id": promotion.ID, "source_endpoint_id": promotion.SourceEndpointID, "source_plan_id": promotion.SourcePlanID, "destination_endpoint_id": promotion.DestinationEndpointID, "destination_plan_id": promotion.DestinationPlanID, "idempotency_key": promotion.IdempotencyKey, "created_at": promotion.CreatedAt}, "created": created, "state": "candidate_staged", "activation": "requires_destination_release_guard_pass", "route_refresh": refresh})
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
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data = append(data, logicalModelResponse(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
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

func (a API) adoptEndpoint(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(adoptionStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint adoption is not supported by this store")
		return
	}
	var request struct {
		Name          string `json:"name"`
		LogicalModel  string `json:"logical_model"`
		UpstreamModel string `json:"upstream_model"`
		URL           string `json:"url"`
		Source        string `json:"source"`
		OwnershipMode string `json:"ownership_mode"`
		Runtime       string `json:"runtime"`
		Discover      bool   `json:"discover,omitempty"`
		Connector     string `json:"connector,omitempty"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	discovery := endpointDiscovery{Runtime: request.Runtime, Model: request.UpstreamModel, Health: "not-checked", Connector: request.Connector}
	if request.Discover {
		var discoveryErr error
		discovery, discoveryErr = discoverEndpoint(r.Context(), a.DiscoveryClient, request.URL, request.UpstreamModel, request.Connector)
		if discoveryErr != nil {
			writeError(w, http.StatusUnprocessableEntity, "endpoint_discovery_failed", discoveryErr.Error())
			return
		}
		request.UpstreamModel = discovery.Model
		request.Runtime = discovery.Runtime
		request.Source = "openai-compatible"
		if discovery.Runtime == "vllm" {
			request.Source = "vllm"
		}
	}
	resolved, adoption, err := store.AdoptEndpoint(r.Context(), actor.TenantID, request.Name, request.LogicalModel, request.UpstreamModel, request.URL, request.Source, request.OwnershipMode, request.Runtime)
	if writeEndpointMutationError(w, err) {
		return
	}
	refresh := a.refreshEndpoints(r.Context())
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint.adopt", ResourceType: "endpoint", ResourceName: request.Name, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"endpoint": resolvedEndpointResponse(resolved), "adoption": map[string]any{"id": adoption.ID, "target_id": adoption.TargetID, "binding_id": adoption.BindingID, "ownership_mode": adoption.OwnershipMode, "source": adoption.Source, "immutable_identity": adoption.ImmutableIdentity}, "discovery": discovery, "route_refresh": refresh})
}

func (a API) requestInspection(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(diagnosticStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "request inspection is not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	item, err := store.RequestInspectionForTenant(r.Context(), actor.TenantID, r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "request was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "request evidence could not be read")
		return
	}
	writeJSON(w, http.StatusOK, requestInspectionResponse(item))
}

func (a API) promoteAdoptionOwnership(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(adoptionStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint adoption is not supported by this store")
		return
	}
	var request struct {
		OwnershipMode string `json:"ownership_mode"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	item, err := store.PromoteAdoptionOwnership(r.Context(), actor.TenantID, r.PathValue("name"), request.OwnershipMode)
	if writeEndpointMutationError(w, err) {
		return
	}
	refresh := a.refreshEndpoints(r.Context())
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint_adoption.promote", ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"adoption": adoptedWorkloadResponse(item), "route_refresh": refresh})
}

func adoptedWorkloadResponse(item domain.AdoptedWorkload) map[string]any {
	return map[string]any{"id": item.ID, "endpoint_id": item.EndpointID, "binding_id": item.BindingID, "target_id": item.TargetID, "ownership_mode": item.OwnershipMode, "source": item.Source, "immutable_identity": item.ImmutableIdentity, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt}
}

func (a API) diagnoseEndpoint(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(diagnosticStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint diagnostics are not supported by this store")
		return
	}
	windowSeconds, err := strconv.Atoi(defaultValue(r.URL.Query().Get("window_seconds"), "3600"))
	if err != nil || windowSeconds < 1 || windowSeconds > 30*24*60*60 {
		writeError(w, http.StatusBadRequest, "invalid_request", "window_seconds must be between 1 and 2592000")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := store.DiagnoseEndpoint(r.Context(), actor.TenantID, r.PathValue("name"), time.Duration(windowSeconds)*time.Second)
	if writeEndpointMutationError(w, err) {
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data = append(data, diagnosticFindingResponse(item))
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint.doctor", ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "deterministic": true})
}

func requestInspectionResponse(item domain.RequestInspection) map[string]any {
	return map[string]any{"request_id": item.RequestID, "logical_model": item.LogicalModel, "endpoint": item.Endpoint, "environment": item.Environment, "serving_plan": item.ServingPlan, "binding": item.Binding, "deployment": item.Deployment, "revision": item.Revision, "target": item.Target, "provider": item.Provider, "runtime": item.Runtime, "compute_mode": item.ComputeMode, "operation": item.OperationName, "request_model": item.RequestModel, "response_model": item.ResponseModel, "started_at": item.StartedAt, "status_code": item.StatusCode, "latency_ms": item.LatencyMS, "ttft_ms": item.TTFTMS, "queue_ms": item.QueueMS, "generation_ms": item.GenerationMS, "input_tokens": item.InputTokens, "output_tokens": item.OutputTokens, "streaming": item.Streaming, "retry_count": item.RetryCount, "fallback_reason": item.FallbackReason, "error_type": item.ErrorType, "content_recorded": false}
}

func diagnosticFindingResponse(item domain.DiagnosticFinding) map[string]any {
	var evidence any
	_ = json.Unmarshal([]byte(item.EvidenceJSON), &evidence)
	return map[string]any{"id": item.ID, "endpoint_id": item.EndpointID, "code": item.Code, "severity": item.Severity, "confidence": item.Confidence, "summary": item.Summary, "evidence": evidence, "evidence_digest": item.EvidenceDigest, "observed_at": item.ObservedAt, "created_at": item.CreatedAt}
}

func (a API) alertPolicies(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(alertStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "alerts are not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := store.AlertPoliciesForEndpoint(r.Context(), actor.TenantID, r.PathValue("name"))
	if writeEndpointMutationError(w, err) {
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data = append(data, alertPolicyResponse(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (a API) createAlertPolicy(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(alertStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "alerts are not supported by this store")
		return
	}
	var request struct {
		Name              string `json:"name"`
		WebhookURL        string `json:"webhook_url"`
		SecretReferenceID string `json:"secret_reference_id"`
		MinimumSeverity   string `json:"minimum_severity"`
		Enabled           *bool  `json:"enabled"`
		MaxAttempts       int    `json:"max_attempts"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	if request.MinimumSeverity == "" {
		request.MinimumSeverity = "warning"
	}
	if request.MaxAttempts == 0 {
		request.MaxAttempts = 3
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	item, err := store.CreateAlertPolicy(r.Context(), actor.TenantID, r.PathValue("name"), domain.AlertPolicy{Name: request.Name, WebhookURL: request.WebhookURL, SecretReferenceID: request.SecretReferenceID, MinimumSeverity: request.MinimumSeverity, Enabled: enabled, MaxAttempts: request.MaxAttempts})
	if writeEndpointMutationError(w, err) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "alert_policy.create", ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"policy": alertPolicyResponse(item)})
}

func alertPolicyResponse(item domain.AlertPolicy) map[string]any {
	return map[string]any{"id": item.ID, "tenant_id": item.TenantID, "endpoint_id": item.EndpointID, "name": item.Name, "webhook_url": item.WebhookURL, "secret_reference_id": item.SecretReferenceID, "minimum_severity": item.MinimumSeverity, "enabled": item.Enabled, "max_attempts": item.MaxAttempts, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt}
}

func (a API) evaluateAlerts(w http.ResponseWriter, r *http.Request) {
	store, storeOK := a.Store.(alertStore)
	diagnostics, diagnosticsOK := a.Store.(diagnosticStore)
	if !storeOK || !diagnosticsOK || a.AlertDeliverer == nil {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "alert delivery is not configured")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	findings, err := diagnostics.DiagnoseEndpoint(r.Context(), actor.TenantID, r.PathValue("name"), time.Hour)
	if writeEndpointMutationError(w, err) {
		return
	}
	policies, err := store.AlertPoliciesForEndpoint(r.Context(), actor.TenantID, r.PathValue("name"))
	if writeEndpointMutationError(w, err) {
		return
	}
	deliveries := make([]map[string]any, 0)
	for _, policy := range policies {
		for _, finding := range findings {
			delivery, deliveryErr := a.AlertDeliverer.Deliver(r.Context(), policy, finding)
			if delivery.ID != "" {
				deliveries = append(deliveries, alertDeliveryResponse(delivery))
			}
			if deliveryErr != nil {
				writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{"code": "alert_delivery_failed", "message": deliveryErr.Error()}, "deliveries": deliveries})
				return
			}
		}
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "alerts.evaluate", ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"findings": len(findings), "deliveries": deliveries})
}

func alertDeliveryResponse(item domain.AlertDelivery) map[string]any {
	return map[string]any{"id": item.ID, "policy_id": item.PolicyID, "finding_id": item.FindingID, "status": item.Status, "error_code": item.ErrorCode, "attempts": item.Attempts, "response_status": item.ResponseStatus, "body_digest": item.BodyDigest, "created_at": item.CreatedAt, "updated_at": item.UpdatedAt, "delivered_at": item.DeliveredAt}
}

func (a API) admissionPolicy(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(admissionStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "admission policies are not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	policy, err := store.AdmissionPolicy(r.Context(), actor.TenantID, r.PathValue("name"))
	if writeEndpointMutationError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy": admissionPolicyResponse(policy)})
}

func (a API) setAdmissionPolicy(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(admissionStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "admission policies are not supported by this store")
		return
	}
	var request struct {
		MaxConcurrency    int      `json:"max_concurrency"`
		MaxQueueDepth     int      `json:"max_queue_depth"`
		QueueTimeoutMS    int      `json:"queue_timeout_ms"`
		MaxRequestBytes   int      `json:"max_request_bytes"`
		MaxOutputTokens   int      `json:"max_output_tokens"`
		AllowedPriorities []string `json:"allowed_priorities"`
		RetryBudget       int      `json:"retry_budget"`
		Enabled           *bool    `json:"enabled"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	enabled := true
	if request.Enabled != nil {
		enabled = *request.Enabled
	}
	priorities, _ := json.Marshal(request.AllowedPriorities)
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	policy, err := store.SetAdmissionPolicy(r.Context(), actor.TenantID, r.PathValue("name"), domain.AdmissionPolicy{MaxConcurrency: request.MaxConcurrency, MaxQueueDepth: request.MaxQueueDepth, QueueTimeoutMS: request.QueueTimeoutMS, MaxRequestBytes: request.MaxRequestBytes, MaxOutputTokens: request.MaxOutputTokens, AllowedPrioritiesJSON: string(priorities), RetryBudget: request.RetryBudget, Enabled: enabled})
	if writeEndpointMutationError(w, err) {
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint_admission.set", ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"policy": admissionPolicyResponse(policy), "effective_within_seconds": 1})
}

func (a API) submitAsyncInference(w http.ResponseWriter, r *http.Request) {
	if isNilAsyncInferenceService(a.AsyncInference) {
		writeError(w, http.StatusServiceUnavailable, "capability_unavailable", "async inference requires INFERCRANE_ASYNC_ENCRYPTION_KEY")
		return
	}
	var request struct {
		Protocol                 string          `json:"protocol"`
		Input                    json.RawMessage `json:"input"`
		IdempotencyKey           string          `json:"idempotency_key"`
		Priority                 int             `json:"priority"`
		ExecutionDeadlineSeconds int             `json:"execution_deadline_seconds"`
		RetentionSeconds         int             `json:"retention_seconds"`
		WebhookURL               string          `json:"webhook_url"`
		WebhookSecretReferenceID string          `json:"webhook_secret_reference_id"`
		StoreEncryptedContent    bool            `json:"store_encrypted_content"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if !request.StoreEncryptedContent {
		writeError(w, http.StatusBadRequest, "content_consent_required", "async inference requires explicit store_encrypted_content=true because request and result content are durably encrypted")
		return
	}
	if request.IdempotencyKey == "" || len(request.Input) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "idempotency_key and input are required")
		return
	}
	if request.Protocol == "" {
		request.Protocol = "chat"
	}
	if _, supported := map[string]struct{}{"chat": {}, "responses": {}, "embeddings": {}, "completions": {}, "batch": {}}[request.Protocol]; !supported {
		writeError(w, http.StatusBadRequest, "invalid_request", "protocol must be chat, responses, embeddings, completions, or batch")
		return
	}
	var input struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(request.Input, &input); err != nil || input.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "async input must be a protocol-native JSON object with model set to the endpoint name")
		return
	}
	if input.Model != r.PathValue("name") {
		writeError(w, http.StatusBadRequest, "invalid_request", "async input model must match the endpoint name in the request path")
		return
	}
	if request.ExecutionDeadlineSeconds == 0 {
		request.ExecutionDeadlineSeconds = 900
	}
	if request.RetentionSeconds == 0 {
		request.RetentionSeconds = 86400
	}
	if request.ExecutionDeadlineSeconds < 1 || request.ExecutionDeadlineSeconds > 86400 || request.RetentionSeconds <= request.ExecutionDeadlineSeconds || request.RetentionSeconds > 604800 {
		writeError(w, http.StatusBadRequest, "invalid_request", "deadline must be 1..86400 seconds and retention must be greater than deadline and at most 604800 seconds")
		return
	}
	if (request.WebhookURL == "") != (request.WebhookSecretReferenceID == "") {
		writeError(w, http.StatusBadRequest, "invalid_request", "webhook_url and webhook_secret_reference_id must be configured together")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	now := time.Now().UTC()
	job, created, err := a.AsyncInference.Submit(r.Context(), asyncinference.SubmitRequest{Tenant: actor.TenantID, Endpoint: r.PathValue("name"), Protocol: request.Protocol, IdempotencyKey: request.IdempotencyKey, Payload: request.Input, Priority: request.Priority, ExecutionDeadline: now.Add(time.Duration(request.ExecutionDeadlineSeconds) * time.Second), ExpiresAt: now.Add(time.Duration(request.RetentionSeconds) * time.Second), WebhookURL: request.WebhookURL, WebhookSecretReferenceID: request.WebhookSecretReferenceID})
	if writeEndpointMutationError(w, err) {
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	writeJSON(w, status, map[string]any{"job": asyncJobResponse(job), "created": created})
}

func (a API) asyncInferenceJob(w http.ResponseWriter, r *http.Request) {
	if isNilAsyncInferenceService(a.AsyncInference) {
		writeError(w, http.StatusServiceUnavailable, "capability_unavailable", "async inference is not configured")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	job, result, err := a.AsyncInference.Result(r.Context(), actor.TenantID, r.PathValue("id"))
	if writeEndpointMutationError(w, err) {
		return
	}
	response := map[string]any{"job": asyncJobResponse(job), "content_recorded": true, "content_encrypted_at_rest": true}
	if job.Status == "succeeded" {
		var decoded any
		if json.Unmarshal(result, &decoded) == nil {
			response["result"] = decoded
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (a API) cancelAsyncInferenceJob(w http.ResponseWriter, r *http.Request) {
	if isNilAsyncInferenceService(a.AsyncInference) {
		writeError(w, http.StatusServiceUnavailable, "capability_unavailable", "async inference is not configured")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if writeEndpointMutationError(w, a.AsyncInference.Cancel(r.Context(), actor.TenantID, r.PathValue("id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func isNilAsyncInferenceService(service asyncInferenceService) bool {
	if service == nil {
		return true
	}
	value := reflect.ValueOf(service)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

func asyncJobResponse(job domain.AsyncInferenceJob) map[string]any {
	return map[string]any{"id": job.ID, "request_id": job.RequestID, "endpoint_id": job.EndpointID, "protocol": job.Protocol, "status": job.Status, "priority": job.Priority, "attempt": job.Attempt, "execution_deadline": job.ExecutionDeadline, "expires_at": job.ExpiresAt, "error_code": job.ErrorCode, "error_message": job.ErrorMessage, "webhook_status": job.WebhookStatus, "webhook_attempts": job.WebhookAttempts, "webhook_error_code": job.WebhookErrorCode, "created_at": job.CreatedAt, "started_at": job.StartedAt, "completed_at": job.CompletedAt, "content_recorded": true, "content_encrypted_at_rest": true}
}

func admissionPolicyResponse(policy domain.AdmissionPolicy) map[string]any {
	var priorities any
	_ = json.Unmarshal([]byte(policy.AllowedPrioritiesJSON), &priorities)
	return map[string]any{"endpoint_id": policy.EndpointID, "max_concurrency": policy.MaxConcurrency, "max_queue_depth": policy.MaxQueueDepth, "queue_timeout_ms": policy.QueueTimeoutMS, "max_request_bytes": policy.MaxRequestBytes, "max_output_tokens": policy.MaxOutputTokens, "allowed_priorities": priorities, "retry_budget": policy.RetryBudget, "enabled": policy.Enabled, "created_at": policy.CreatedAt, "updated_at": policy.UpdatedAt}
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
	configJSON := string(request.Config)
	if configJSON == "" {
		configJSON = "{}"
	}
	binding := domain.BackendBinding{EndpointID: resolved.Endpoint.ID, Name: request.Name, Kind: request.Kind, OwnershipMode: request.OwnershipMode, ConfigJSON: configJSON}
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
		config, managed, configErr := external.ParseManagedBindingConfig(configJSON)
		providerManaged := target.Provider == "openrouter" || target.Provider == "openai-compatible-external"
		if configErr != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", configErr.Error())
			return
		}
		if providerManaged && !managed {
			writeError(w, http.StatusUnprocessableEntity, "managed_external_policy_required", "authenticated external provider bindings require reference-only credentials, explicit privacy acknowledgement, and hard request/cost budgets")
			return
		}
		if managed {
			if !authz.AllowedScoped(authz.Role(actor.Role), actor.Scopes, authz.ManageExternal) {
				writeError(w, http.StatusForbidden, "permission_denied", "principal is not allowed to manage external inference capacity")
				return
			}
			if request.OwnershipMode != "traffic-managed" || config.Adapter != target.Provider {
				writeError(w, http.StatusUnprocessableEntity, "validation_failed", "managed external policy must match a traffic-managed target adapter")
				return
			}
			references, secretErr := a.Store.SecretReferencesForTenant(r.Context(), actor.TenantID)
			if secretErr != nil {
				writeError(w, http.StatusInternalServerError, "internal", "secret references could not be validated")
				return
			}
			secretFound := false
			for _, reference := range references {
				if reference.ID == config.SecretReferenceID {
					secretFound = true
					break
				}
			}
			if !secretFound {
				writeError(w, http.StatusUnprocessableEntity, "secret_reference_not_found", "managed external binding secret reference was not found in the active tenant")
				return
			}
		}
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

func (a API) consoleSession(w http.ResponseWriter, r *http.Request) {
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	writeJSON(w, http.StatusOK, map[string]any{
		"principal":    map[string]any{"id": principal.ID, "tenant_id": principal.TenantID, "name": principal.Name, "role": principal.Role, "kind": principal.Kind, "scopes": principal.Scopes},
		"organization": map[string]any{"id": principal.TenantID},
		"entitlements": []string{"web_console_access"},
	})
}

func (a API) configureConsoleAccess(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(consoleIdentityStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "console identity storage is not configured")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	var request domain.ConsoleIdentityProvisioning
	if !decodeMutationBody(w, r, &request) {
		return
	}
	identity, err := store.ProvisionConsoleIdentity(r.Context(), actor.TenantID, request)
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "conflict", "external identity is already mapped")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	outcome := "revoked"
	if identity.Access {
		outcome = "granted"
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "console.access." + outcome, ResourceType: "user", ResourceName: identity.UserID, Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"identity": identity})
}

func (a API) consoleAccess(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(consoleIdentityListStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "console identity storage is not configured")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	identities, err := store.ConsoleIdentitiesForTenant(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "console membership lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": identities})
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
	gcp, err := strconv.ParseBool(defaultValue(r.URL.Query().Get("gcp"), "false"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "gcp must be true or false")
		return
	}
	kubernetes, err := strconv.ParseBool(defaultValue(r.URL.Query().Get("kubernetes"), "false"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "kubernetes must be true or false")
		return
	}
	writeJSON(w, http.StatusOK, a.Diagnostics(r.Context(), cloud, serverless, aws, gcp, kubernetes))
}

func (a API) controlPlaneInstances(w http.ResponseWriter, r *http.Request) {
	membership, ok := a.Store.(controlPlaneMembershipStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "membership_unavailable", "control-plane membership is not configured")
		return
	}
	instances, err := membership.ControlPlaneInstances(r.Context(), 45*time.Second)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "control-plane membership could not be read")
		return
	}
	data := make([]map[string]any, 0, len(instances))
	for _, instance := range instances {
		data = append(data, map[string]any{"id": instance.ID, "binary_version": instance.BinaryVersion, "protocol_min": instance.ProtocolMin, "protocol_max": instance.ProtocolMax, "started_at": instance.StartedAt, "heartbeat_at": instance.HeartbeatAt, "draining": instance.Draining})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "count": len(data), "live_window_seconds": 45})
}

func defaultValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func (a API) intelligence() (intelligenceStore, bool) {
	store, ok := a.Store.(intelligenceStore)
	return store, ok
}

func (a API) optimization() (optimizationStore, bool) {
	store, ok := a.Store.(optimizationStore)
	return store, ok
}
func (a API) contextBurst() (contextBurstStore, bool) {
	s, ok := a.Store.(contextBurstStore)
	return s, ok
}
func (a API) createContextPassport(w http.ResponseWriter, r *http.Request) {
	s, ok := a.contextBurst()
	if !ok {
		writeError(w, 501, "context_passport_unavailable", "Context Passport storage is not configured")
		return
	}
	var request struct {
		Deployment         string         `json:"deployment"`
		TTLSeconds         int            `json:"ttl_seconds"`
		PreferredBindingID string         `json:"preferred_binding_id"`
		PreferredTargetID  string         `json:"preferred_target_id"`
		CacheHints         map[string]any `json:"cache_hints"`
		Metadata           map[string]any `json:"metadata"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.TTLSeconds == 0 {
		request.TTLSeconds = 3600
	}
	if request.Deployment == "" || request.TTLSeconds < 60 || request.TTLSeconds > 2592000 {
		writeError(w, 422, "validation_failed", "deployment and ttl_seconds 60..2592000 are required")
		return
	}
	if request.CacheHints == nil {
		request.CacheHints = map[string]any{}
	}
	if request.Metadata == nil {
		request.Metadata = map[string]any{}
	}
	cache, _ := json.Marshal(request.CacheHints)
	metadata, _ := json.Marshal(request.Metadata)
	p := r.Context().Value(identityKey{}).(domain.Principal)
	row, err := s.CreateContextPassport(r.Context(), p.TenantID, request.Deployment, domain.ContextPassport{PreferredBindingID: request.PreferredBindingID, PreferredTargetID: request.PreferredTargetID, CacheHintsJSON: string(cache), MetadataJSON: string(metadata), ExpiresAt: time.Now().UTC().Add(time.Duration(request.TTLSeconds) * time.Second)})
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	if a.ContextPassports != nil {
		a.ContextPassports.Put(contextpassport.Hint{ID: row.ID, TenantID: row.TenantID, SubjectID: row.DeploymentName, PreferredBindingID: row.PreferredBindingID, PreferredTargetID: row.PreferredTargetID, ExpiresAt: row.ExpiresAt})
	}
	writeJSON(w, 201, map[string]any{"context_passport": contextPassportResponse(row), "durable_kv": false})
}
func (a API) getContextPassport(w http.ResponseWriter, r *http.Request) {
	s, ok := a.contextBurst()
	if !ok {
		writeError(w, 501, "context_passport_unavailable", "Context Passport storage is not configured")
		return
	}
	p := r.Context().Value(identityKey{}).(domain.Principal)
	row, err := s.ContextPassport(r.Context(), p.TenantID, r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "Context Passport was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "Context Passport lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"context_passport": contextPassportResponse(row), "durable_kv": false})
}
func contextPassportResponse(row domain.ContextPassport) map[string]any {
	return map[string]any{"id": row.ID, "deployment_id": row.DeploymentID, "endpoint_id": row.EndpointID, "status": row.Status, "preferred_binding_id": row.PreferredBindingID, "preferred_target_id": row.PreferredTargetID, "cache_hints": json.RawMessage(row.CacheHintsJSON), "metadata": json.RawMessage(row.MetadataJSON), "last_activity": row.LastActivity, "expires_at": row.ExpiresAt, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}
}
func (a API) evaluateBurstGuard(w http.ResponseWriter, r *http.Request) {
	s, ok := a.contextBurst()
	if !ok {
		writeError(w, 501, "burst_guard_unavailable", "Burst Guard storage is not configured")
		return
	}
	var request struct {
		Enabled                        bool      `json:"enabled"`
		QueueThreshold                 int       `json:"queue_threshold"`
		BreachIntervals                int       `json:"breach_intervals"`
		RecoveryIntervals              int       `json:"recovery_intervals"`
		CooldownSeconds                int       `json:"cooldown_seconds"`
		SignalMaxAgeSeconds            int       `json:"signal_max_age_seconds"`
		MaxIncrementalCostMicrousdHour int64     `json:"max_incremental_cost_microusd_hour"`
		QueueDepth                     int       `json:"queue_depth"`
		ConsecutiveBreaches            int       `json:"consecutive_breaches"`
		ConsecutiveRecovery            int       `json:"consecutive_recovery"`
		IncrementalCostMicrousdHour    int64     `json:"incremental_cost_microusd_hour"`
		ExternalHealthy                bool      `json:"external_healthy"`
		ObservedAt                     time.Time `json:"observed_at"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	p := r.Context().Value(identityKey{}).(domain.Principal)
	policy, err := s.SetBurstGuardPolicy(r.Context(), p.TenantID, r.PathValue("name"), domain.BurstGuardPolicy{Enabled: request.Enabled, QueueThreshold: request.QueueThreshold, BreachIntervals: request.BreachIntervals, RecoveryIntervals: request.RecoveryIntervals, CooldownSeconds: request.CooldownSeconds, SignalMaxAgeSeconds: request.SignalMaxAgeSeconds, MaxIncrementalCostMicrousdHour: request.MaxIncrementalCostMicrousdHour})
	if err != nil {
		writeError(w, 422, "policy_required", err.Error())
		return
	}
	decision := burstguard.Evaluate(burstguard.Policy{Enabled: policy.Enabled, QueueThreshold: policy.QueueThreshold, BreachIntervals: policy.BreachIntervals, RecoveryIntervals: policy.RecoveryIntervals, SignalMaxAge: time.Duration(policy.SignalMaxAgeSeconds) * time.Second, MaxIncrementalCostMicrousdHour: policy.MaxIncrementalCostMicrousdHour}, burstguard.Signal{QueueDepth: request.QueueDepth, ConsecutiveBreaches: request.ConsecutiveBreaches, ConsecutiveRecovery: request.ConsecutiveRecovery, IncrementalCostMicrousdHour: request.IncrementalCostMicrousdHour, ObservedAt: request.ObservedAt, ExternalHealthy: request.ExternalHealthy}, time.Now().UTC())
	evidence, _ := json.Marshal(request)
	row, err := s.RecordBurstGuardDecision(r.Context(), domain.BurstGuardDecision{TenantID: p.TenantID, DeploymentID: policy.DeploymentID, PolicyID: policy.ID, Decision: decision.Action, Reason: decision.Reason, IncrementalCostMicrousdHour: decision.Cost, EvidenceJSON: string(evidence)})
	if err != nil {
		writeError(w, 500, "internal", "Burst Guard decision could not be persisted")
		return
	}
	writeJSON(w, 201, map[string]any{"decision": map[string]any{"id": row.ID, "action": row.Decision, "reason": row.Reason, "incremental_cost_microusd_hour": row.IncrementalCostMicrousdHour, "evidence": json.RawMessage(row.EvidenceJSON), "created_at": row.CreatedAt}, "routing_mutation": "policy_controller_only"})
}

func (a API) createFinOpsReport(w http.ResponseWriter, r *http.Request) {
	store, ok := a.optimization()
	if !ok {
		writeError(w, 501, "finops_unavailable", "FinOps storage is not configured")
		return
	}
	var request struct {
		WindowSeconds int `json:"window_seconds"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.WindowSeconds == 0 {
		request.WindowSeconds = 30 * 86400
	}
	if request.WindowSeconds < 1 || request.WindowSeconds > 365*86400 {
		writeError(w, 422, "validation_failed", "window_seconds must be 1..31536000")
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
	end := time.Now().UTC()
	start := end.Add(-time.Duration(request.WindowSeconds) * time.Second)
	benchmarks, err := a.Store.BenchmarksForDeployment(r.Context(), principal.TenantID, name, 100)
	if err != nil {
		writeError(w, 500, "internal", "cost evidence lookup failed")
		return
	}
	var evidence []finops.CostEvidence
	for _, b := range benchmarks {
		if b.CreatedAt.Before(start) {
			continue
		}
		var cost struct {
			Available  bool       `json:"available"`
			Hourly     *float64   `json:"hourly"`
			Currency   string     `json:"currency"`
			Source     string     `json:"source"`
			ObservedAt *time.Time `json:"observed_at"`
			ExpiresAt  *time.Time `json:"expires_at"`
		}
		if json.Unmarshal([]byte(b.CostMetadataJSON), &cost) == nil && cost.Available && cost.Hourly != nil && cost.Source != "" && cost.Currency != "" && cost.ObservedAt != nil && !cost.ObservedAt.Before(start) && !cost.ObservedAt.After(end) {
			evidence = append(evidence, finops.CostEvidence{ID: b.ID, Scope: "deployment_hourly_rate", Source: cost.Source, Currency: cost.Currency, Amount: *cost.Hourly, ObservedAt: *cost.ObservedAt, ExpiresAt: cost.ExpiresAt})
		}
	}
	report := finops.Evaluate(end, evidence)
	summary, _ := json.Marshal(report)
	evidenceJSON, _ := json.Marshal(report.Evidence)
	persisted, err := store.RecordFinOpsReport(r.Context(), domain.FinOpsReport{TenantID: principal.TenantID, DeploymentID: resolved.Deployment.ID, DeploymentName: name, WindowStart: start, WindowEnd: end, Currency: report.Currency, Status: report.Status, KnownCost: report.KnownCost, EstimatedAvoidableCost: report.Avoidable, SummaryJSON: string(summary), EvidenceJSON: string(evidenceJSON), InputDigest: report.InputDigest})
	if err != nil {
		writeError(w, 500, "internal", "FinOps report could not be persisted")
		return
	}
	writeJSON(w, 201, map[string]any{"report": finOpsResponse(persisted)})
}

func (a API) finOpsReports(w http.ResponseWriter, r *http.Request) {
	store, ok := a.optimization()
	if !ok {
		writeError(w, 501, "finops_unavailable", "FinOps storage is not configured")
		return
	}
	p := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := store.FinOpsReports(r.Context(), p.TenantID, r.PathValue("name"), 20)
	if err != nil {
		writeError(w, 500, "internal", "FinOps report lookup failed")
		return
	}
	data := make([]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, finOpsResponse(row))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func finOpsResponse(row domain.FinOpsReport) map[string]any {
	return map[string]any{"id": row.ID, "deployment": row.DeploymentName, "window_start": row.WindowStart, "window_end": row.WindowEnd, "currency": row.Currency, "status": row.Status, "known_cost": row.KnownCost, "estimated_avoidable_cost": row.EstimatedAvoidableCost, "summary": json.RawMessage(row.SummaryJSON), "evidence": json.RawMessage(row.EvidenceJSON), "input_digest": row.InputDigest, "created_at": row.CreatedAt}
}

func (a API) createAutopilotPlan(w http.ResponseWriter, r *http.Request) {
	store, ok := a.optimization()
	if !ok {
		writeError(w, 501, "autopilot_unavailable", "Autopilot storage is not configured")
		return
	}
	decisions, ok := a.decisions()
	if !ok {
		writeError(w, 501, "autopilot_unavailable", "recommendation storage is not configured")
		return
	}
	var request struct {
		Objective string `json:"objective"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.Objective != "minimize_cost" {
		writeError(w, 422, "validation_failed", "v1 supports objective minimize_cost only")
		return
	}
	p := r.Context().Value(identityKey{}).(domain.Principal)
	name := r.PathValue("name")
	resolved, err := a.Store.ResolveForTenant(r.Context(), p.TenantID, name)
	if err != nil {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	}
	rows, err := decisions.InferenceRecommendations(r.Context(), p.TenantID, name, 1)
	if err != nil || len(rows) == 0 || rows[0].Status != "recommended" {
		writeError(w, 409, "recommendation_required", "a current eligible recommendation is required")
		return
	}
	recommendation := rows[0]
	digest := sha256.Sum256([]byte(recommendation.InputSnapshotJSON))
	plan, created, err := store.CreateAutopilotPlan(r.Context(), domain.AutopilotPlan{TenantID: p.TenantID, DeploymentID: resolved.Deployment.ID, DeploymentName: name, RecommendationID: recommendation.ID, Objective: request.Objective, CandidateJSON: recommendation.CandidatesJSON, EvidenceJSON: recommendation.InputSnapshotJSON, InputDigest: hex.EncodeToString(digest[:])})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, 409, "plan_conflict", "the recommendation already has different plan content")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "advisory plan could not be persisted")
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, map[string]any{"plan": autopilotResponse(plan), "mutation": "none"})
}
func (a API) getAutopilotPlan(w http.ResponseWriter, r *http.Request) {
	store, ok := a.optimization()
	if !ok {
		writeError(w, 501, "autopilot_unavailable", "Autopilot storage is not configured")
		return
	}
	p := r.Context().Value(identityKey{}).(domain.Principal)
	row, err := store.AutopilotPlan(r.Context(), p.TenantID, r.PathValue("id"), "", "")
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "Autopilot plan was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "Autopilot lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"plan": autopilotResponse(row)})
}
func (a API) approveAutopilotPlan(w http.ResponseWriter, r *http.Request) {
	store, ok := a.optimization()
	if !ok {
		writeError(w, 501, "autopilot_unavailable", "Autopilot storage is not configured")
		return
	}
	var body map[string]json.RawMessage
	if !decodeMutationBody(w, r, &body) {
		return
	}
	if len(body) != 0 {
		writeError(w, 400, "invalid_request", "request body must be an empty object")
		return
	}
	p := r.Context().Value(identityKey{}).(domain.Principal)
	row, err := store.ApproveAutopilotPlan(r.Context(), p.TenantID, r.PathValue("id"), p.Name)
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, 409, "invalid_state", "only advisory plans can be approved")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "plan approval failed")
		return
	}
	writeJSON(w, 200, map[string]any{"plan": autopilotResponse(row), "mutation": "none", "next": "create and validate a candidate explicitly"})
}
func autopilotResponse(row domain.AutopilotPlan) map[string]any {
	return map[string]any{"id": row.ID, "deployment": row.DeploymentName, "recommendation_id": row.RecommendationID, "status": row.Status, "objective": row.Objective, "candidates": json.RawMessage(row.CandidateJSON), "evidence": json.RawMessage(row.EvidenceJSON), "input_digest": row.InputDigest, "approved_by": row.ApprovedBy, "approved_at": row.ApprovedAt, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}
}

func (a API) captureReplay(w http.ResponseWriter, r *http.Request) {
	store, ok := a.intelligence()
	if !ok {
		writeError(w, http.StatusNotImplemented, "replay_unavailable", "replay storage is not configured")
		return
	}
	var request struct {
		WindowSeconds int `json:"window_seconds"`
		MaxRequests   int `json:"max_requests"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.WindowSeconds == 0 {
		request.WindowSeconds = 86400
	}
	if request.MaxRequests == 0 {
		request.MaxRequests = 1000
	}
	if request.WindowSeconds < 1 || request.WindowSeconds > 30*86400 || request.MaxRequests < 1 || request.MaxRequests > 10000 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "window_seconds must be 1..2592000 and max_requests 1..10000")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	trace, err := store.CaptureReplayTrace(r.Context(), principal.TenantID, r.PathValue("name"), time.Duration(request.WindowSeconds)*time.Second, request.MaxRequests)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "replay trace could not be captured")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"replay": replayTraceResponse(trace)})
}

func (a API) getReplay(w http.ResponseWriter, r *http.Request) {
	store, ok := a.intelligence()
	if !ok {
		writeError(w, 501, "replay_unavailable", "replay storage is not configured")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	trace, err := store.ReplayTrace(r.Context(), principal.TenantID, r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "replay trace was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "replay trace lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"replay": replayTraceResponse(trace)})
}

func replayTraceResponse(trace domain.ReplayTrace) map[string]any {
	return map[string]any{
		"id": trace.ID, "deployment_id": trace.DeploymentID, "deployment_name": trace.DeploymentName,
		"revision_id": trace.RevisionID, "schema_version": trace.SchemaVersion,
		"shape": json.RawMessage(trace.ShapeJSON), "summary": json.RawMessage(trace.SummaryJSON),
		"shape_digest": trace.ShapeDigest, "request_count": trace.RequestCount,
		"window_start": trace.WindowStart, "window_end": trace.WindowEnd, "created_at": trace.CreatedAt,
	}
}

func (a API) capacityIntelligence(w http.ResponseWriter, r *http.Request) {
	store, ok := a.intelligence()
	if !ok {
		writeError(w, 501, "capacity_intelligence_unavailable", "capacity intelligence storage is not configured")
		return
	}
	window := 30 * 24 * time.Hour
	if raw := r.URL.Query().Get("window_seconds"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > 365*86400 {
			writeError(w, 422, "validation_failed", "window_seconds must be 1..31536000")
			return
		}
		window = time.Duration(seconds) * time.Second
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := store.CapacityIntelligence(r.Context(), principal.TenantID, window)
	if err != nil {
		writeError(w, 500, "internal", "capacity intelligence lookup failed")
		return
	}
	if rows == nil {
		rows = make([]domain.CapacitySummary, 0)
	}
	writeJSON(w, 200, map[string]any{"capacity": rows, "evidence": "observed", "sample_scope": "tenant", "window_seconds": int(window.Seconds())})
}

func (a API) recordCacheObservation(w http.ResponseWriter, r *http.Request) {
	store, ok := a.intelligence()
	if !ok {
		writeError(w, 501, "artifact_cache_unavailable", "artifact cache storage is not configured")
		return
	}
	var request struct {
		Provider   string         `json:"provider"`
		Region     string         `json:"region"`
		Location   string         `json:"location"`
		State      string         `json:"state"`
		Source     string         `json:"source"`
		Evidence   map[string]any `json:"evidence"`
		TTLSeconds int            `json:"ttl_seconds"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.TTLSeconds == 0 {
		request.TTLSeconds = 300
	}
	evidence, _ := json.Marshal(request.Evidence)
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	observed := time.Now().UTC()
	row, err := store.RecordArtifactCacheObservation(r.Context(), principal.TenantID, domain.ArtifactCacheObservation{ModelArtifactID: r.PathValue("id"), Provider: request.Provider, Region: request.Region, Location: request.Location, State: request.State, Source: request.Source, EvidenceJSON: string(evidence), ObservedAt: observed, ExpiresAt: observed.Add(time.Duration(request.TTLSeconds) * time.Second)})
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "model artifact was not found")
		return
	}
	if err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"observation": artifactCacheObservationResponse(row)})
}

func (a API) artifactCacheState(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(artifactCacheStateStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "artifact_cache_unavailable", "artifact cache state is not configured")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	observations, prefetches, err := store.ArtifactCacheState(r.Context(), principal.TenantID, r.PathValue("id"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "model artifact was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "artifact cache state could not be read")
		return
	}
	observationData := make([]map[string]any, 0, len(observations))
	for _, row := range observations {
		observationData = append(observationData, artifactCacheObservationResponse(row))
	}
	prefetchData := make([]map[string]any, 0, len(prefetches))
	for _, row := range prefetches {
		prefetchData = append(prefetchData, artifactPrefetchResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"artifact_id": r.PathValue("id"), "observations": observationData, "prefetches": prefetchData, "execution_boundary": "provider_adapter"})
}

func (a API) requestPrefetch(w http.ResponseWriter, r *http.Request) {
	store, ok := a.intelligence()
	if !ok {
		writeError(w, 501, "artifact_prefetch_unavailable", "artifact prefetch storage is not configured")
		return
	}
	var request struct {
		Provider       string `json:"provider"`
		Region         string `json:"region"`
		Location       string `json:"location"`
		IdempotencyKey string `json:"idempotency_key"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	row, created, err := store.RequestArtifactPrefetch(r.Context(), principal.TenantID, domain.ArtifactPrefetch{ModelArtifactID: r.PathValue("id"), Provider: request.Provider, Region: request.Region, Location: request.Location, IdempotencyKey: request.IdempotencyKey})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, 409, "idempotency_conflict", "idempotency key already identifies another prefetch request")
		return
	}
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "model artifact was not found")
		return
	}
	if err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"prefetch": artifactPrefetchResponse(row), "created": created, "execution": "delegated_to_provider_adapter"})
}

func artifactCacheObservationResponse(row domain.ArtifactCacheObservation) map[string]any {
	return map[string]any{"id": row.ID, "model_artifact_id": row.ModelArtifactID, "provider": row.Provider, "region": row.Region, "location": row.Location, "state": row.State, "source": row.Source, "evidence": json.RawMessage(row.EvidenceJSON), "observed_at": row.ObservedAt, "expires_at": row.ExpiresAt, "created_at": row.CreatedAt}
}

func artifactPrefetchResponse(row domain.ArtifactPrefetch) map[string]any {
	return map[string]any{"id": row.ID, "model_artifact_id": row.ModelArtifactID, "provider": row.Provider, "region": row.Region, "location": row.Location, "status": row.Status, "idempotency_key": row.IdempotencyKey, "provider_operation_id": row.ProviderOperationID, "error_code": row.ErrorCode, "created_at": row.CreatedAt, "updated_at": row.UpdatedAt}
}

func (a API) runBenchmark(w http.ResponseWriter, r *http.Request) {
	if a.BenchmarkRunner == nil || a.GatewayURL == "" {
		writeError(w, http.StatusServiceUnavailable, "benchmark_unavailable", "AIPerf benchmark execution is not configured")
		return
	}
	var request struct {
		Requests     int    `json:"requests"`
		Concurrency  int    `json:"concurrency"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
		RandomSeed   int64  `json:"random_seed"`
		Revision     string `json:"revision"`
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
	if request.InputTokens == 0 {
		request.InputTokens = 128
	}
	if request.OutputTokens == 0 {
		request.OutputTokens = 32
	}
	if request.InputTokens < 1 || request.InputTokens > 1000000 || request.OutputTokens < 1 || request.OutputTokens > 1000000 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "input_tokens and output_tokens must be 1..1000000")
		return
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
	measured, err := a.BenchmarkRunner.Run(r.Context(), benchmark.Config{Binary: a.AIPerfBinary, Endpoint: endpoint, APIKey: credential, APIKeyEnv: apiKeyEnv, Model: model, Tokenizer: artifact.Repository, Requests: request.Requests, Concurrency: request.Concurrency, InputTokens: request.InputTokens, OutputTokens: request.OutputTokens, RandomSeed: request.RandomSeed})
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
	workload, _ := json.Marshal(map[string]any{"endpoint_type": "chat", "streaming": true, "request_count": request.Requests, "concurrency": request.Concurrency, "random_seed": request.RandomSeed, "input_tokens": request.InputTokens, "output_tokens": request.OutputTokens, "server_token_count": true, "revision_selector": selector, "direct_revision_validation": selectedRevisionID != deployment.ActiveRevisionID})
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

func (a API) captureRecipe(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(recipeLabStore)
	if !ok {
		writeError(w, 503, "recipe_store_unavailable", "recipe persistence is unavailable")
		return
	}
	var request struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		BenchmarkID string `json:"benchmark_id,omitempty"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	deploymentName := r.PathValue("name")
	resolved, err := a.Store.ResolveForTenant(r.Context(), principal.TenantID, deploymentName)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "deployment was not found")
		return
	}
	if err != nil || resolved.Deployment.ActiveRevisionID == "" {
		writeError(w, 409, "active_revision_required", "deployment must have an active revision")
		return
	}
	revisions, err := a.Store.Revisions(r.Context(), principal.TenantID, deploymentName)
	if err != nil {
		writeError(w, 500, "internal", "revision history lookup failed")
		return
	}
	var revision domain.DeploymentRevision
	for _, candidate := range revisions {
		if candidate.ID == resolved.Deployment.ActiveRevisionID {
			revision = candidate
			break
		}
	}
	if revision.ID == "" {
		writeError(w, 409, "active_revision_required", "active revision evidence is unavailable")
		return
	}
	artifact, err := a.Store.ModelArtifactForRevision(r.Context(), principal.TenantID, revision.ID)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 409, "immutable_artifact_required", "resolve an immutable model artifact before capturing a recipe")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "model artifact lookup failed")
		return
	}
	benchmarks, err := a.Store.BenchmarksForDeployment(r.Context(), principal.TenantID, deploymentName, 100)
	if err != nil {
		writeError(w, 500, "internal", "benchmark history lookup failed")
		return
	}
	var measured domain.BenchmarkResult
	for _, candidate := range benchmarks {
		if candidate.RevisionID == revision.ID && candidate.ModelIdentity == artifact.ModelIdentity && (request.BenchmarkID == "" || candidate.ID == request.BenchmarkID) {
			measured = candidate
			break
		}
	}
	if measured.ID == "" {
		writeError(w, 409, "measured_benchmark_required", "run an AIPerf benchmark for the active immutable revision before capturing a recipe")
		return
	}
	productVersion := a.ProductVersion
	if productVersion == "" {
		productVersion = "development"
	}
	value, err := recipe.Build(request.Name, request.Version, productVersion, artifact, revision, measured)
	if err != nil {
		writeError(w, 422, "invalid_recipe", err.Error())
		return
	}
	persisted, err := store.CreateModelRecipe(r.Context(), principal.TenantID, value)
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, 409, "recipe_immutable", "recipe name and version already identify different content")
		return
	}
	if err != nil {
		writeError(w, 500, "recipe_persist_failed", "recipe could not be persisted")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "recipe.capture", ResourceType: "recipe", ResourceName: persisted.Name + "@" + persisted.Version, Outcome: "succeeded", Payload: `{"digest":"` + persisted.Digest + `"}`})
	writeJSON(w, http.StatusCreated, map[string]any{"recipe": recipeResponse(persisted)})
}

func (a API) recipes(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(recipeLabStore)
	if !ok {
		writeError(w, 503, "recipe_store_unavailable", "recipe persistence is unavailable")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := store.ModelRecipes(r.Context(), principal.TenantID, r.URL.Query().Get("query"), limit)
	if err != nil {
		writeError(w, 500, "internal", "recipe search failed")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, recipeResponse(row))
	}
	writeJSON(w, 200, map[string]any{"data": data})
}
func (a API) getRecipe(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(recipeLabStore)
	if !ok {
		writeError(w, 503, "recipe_store_unavailable", "recipe persistence is unavailable")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	row, err := store.ModelRecipe(r.Context(), principal.TenantID, r.PathValue("name"), r.PathValue("version"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, 404, "not_found", "recipe was not found")
		return
	}
	if err != nil {
		writeError(w, 500, "internal", "recipe lookup failed")
		return
	}
	writeJSON(w, 200, map[string]any{"recipe": recipeResponse(row)})
}
func recipeResponse(row domain.ModelRecipe) map[string]any {
	return map[string]any{"id": row.ID, "name": row.Name, "version": row.Version, "digest": row.Digest, "payload": json.RawMessage(row.PayloadJSON), "provenance": json.RawMessage(row.ProvenanceJSON), "created_at": row.CreatedAt}
}

func (a API) evaluateLab(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(recipeLabStore)
	if !ok {
		writeError(w, 503, "lab_store_unavailable", "Inference Lab persistence is unavailable")
		return
	}
	var request struct {
		ModelIdentity  string   `json:"model_identity"`
		MaxTTFTP95MS   *float64 `json:"max_ttft_p95_ms,omitempty"`
		WorkloadDigest string   `json:"workload_digest,omitempty"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.ModelIdentity) == "" || request.MaxTTFTP95MS != nil && (*request.MaxTTFTP95MS < 0 || *request.MaxTTFTP95MS > 86400000) || request.WorkloadDigest != "" && (len(request.WorkloadDigest) != 64 || strings.Trim(request.WorkloadDigest, "0123456789abcdef") != "") {
		writeError(w, 422, "invalid_lab_input", "model identity, finite nonnegative SLO, and optional SHA-256 workload digest are required")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	evidence, err := store.BenchmarksForModel(r.Context(), principal.TenantID, request.ModelIdentity, 500)
	if err != nil {
		writeError(w, 500, "internal", "lab evidence lookup failed")
		return
	}
	value, err := lab.Evaluate(lab.Input{ModelIdentity: request.ModelIdentity, MaxTTFTP95MS: request.MaxTTFTP95MS, WorkloadDigest: request.WorkloadDigest}, evidence)
	if err != nil {
		writeError(w, 500, "internal", "lab evidence could not be canonicalized")
		return
	}
	persisted, err := store.RecordLabEvaluation(r.Context(), principal.TenantID, value)
	if err != nil {
		writeError(w, 500, "internal", "lab evaluation could not be persisted")
		return
	}
	var results json.RawMessage = json.RawMessage(persisted.ResultsJSON)
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: principal.TenantID, Actor: principal.Name, Action: "lab.evaluate", ResourceType: "model", ResourceName: request.ModelIdentity, Outcome: "succeeded", Payload: `{"input_digest":"` + persisted.InputDigest + `"}`})
	writeJSON(w, 201, map[string]any{"evaluation": map[string]any{"id": persisted.ID, "model_identity": persisted.ModelIdentity, "algorithm_version": persisted.AlgorithmVersion, "input": json.RawMessage(persisted.InputJSON), "input_digest": persisted.InputDigest, "results": results, "created_at": persisted.CreatedAt}})
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
	if !decodeMutationBody(w, r, &request) {
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
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.ID == "" {
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
	if !decodeMutationBody(w, r, &request) {
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

func (a API) principals(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(principalListStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "principal listing is not configured")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := store.PrincipalsForTenant(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "principal lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, principal := range items {
		data = append(data, map[string]any{
			"id": principal.ID, "tenant_id": principal.TenantID, "name": principal.Name,
			"role": principal.Role, "kind": principal.Kind, "scopes": principal.Scopes,
			"disabled": principal.Disabled, "created_at": principal.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
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
	if items == nil {
		items = make([]domain.SecretReference, 0)
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
	primaryTargets := 0
	for _, target := range resolved.Targets {
		if target.Provider == "openrouter" || target.Provider == "openai-compatible-external" {
			// Governed fallback is an alternate serving path, not desired primary
			// replica capacity. Its health is exposed in inspect and external policy
			// views but must not make a healthy primary deployment look degraded.
			if target.Health == "healthy" {
				status.ServingState = "serving"
			}
			continue
		}
		primaryTargets++
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
		status.DesiredReplicas = primaryTargets
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
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.Strategy == "" {
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
	if !decodeMutationBody(w, r, &request) {
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
func (a API) operations(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(operationListStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "operation listing is not configured")
		return
	}
	principal := r.Context().Value(identityKey{}).(domain.Principal)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	var before time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "before must be RFC3339")
			return
		}
		before = parsed
	}
	items, err := store.OperationsForTenant(r.Context(), principal.TenantID, before, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "operation lookup failed")
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, operation := range items {
		data = append(data, operationResponse(operation))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
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
