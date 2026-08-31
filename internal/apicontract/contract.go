// Package apicontract owns the public control-plane API description used by
// documentation, generated clients, and route-drift qualification.
package apicontract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const Version = "1.0.0"

type Route struct {
	Method      string
	Path        string
	OperationID string
	Tag         string
	Summary     string
	Request     string
	Response    string
	Status      int
	Idempotent  bool
}

// Routes is the complete authenticated /api/v1 contract. Keep route behavior
// in the handler and this metadata in lockstep; TestServerRouteCoverage makes
// drift a release failure.
var Routes = []Route{
	{"GET", "/operations/{id}", "getOperation", "Operations", "Inspect a durable operation", "", "Operation", 200, false},
	{"GET", "/operations", "listOperations", "Operations", "List durable operations for the authenticated organization", "", "ObjectList", 200, false},
	{"GET", "/doctor", "getDoctor", "System", "Run control-plane diagnostics", "", "Object", 200, false},
	{"GET", "/whoami", "getCurrentPrincipal", "Identity", "Inspect the authenticated principal", "", "Object", 200, false},
	{"GET", "/console/session", "getConsoleSession", "Identity", "Resolve the authenticated console organization and entitlements", "", "Object", 200, false},
	{"PUT", "/console/access", "configureConsoleAccess", "Identity", "Map a hosted identity and grant or revoke private-console access", "ConsoleIdentityProvisioning", "Object", 200, false},
	{"GET", "/console/access", "listConsoleAccess", "Identity", "List private-console members for the authenticated organization", "", "ObjectList", 200, false},
	{"GET", "/integrations", "listIntegrations", "System", "Inspect registered integration capabilities", "", "Object", 200, false},
	{"GET", "/catalog/models", "listCatalogModels", "Model catalog", "Search reviewed model starting points", "", "ObjectList", 200, false},
	{"GET", "/catalog/models/{name}", "getCatalogModel", "Model catalog", "Inspect one reviewed model and its serving profiles", "", "Object", 200, false},
	{"GET", "/model-api-catalog", "listModelAPICatalog", "Model APIs", "Browse supplier-neutral managed Model API identities", "", "ObjectList", 200, false},
	{"GET", "/model-api-catalog/{id}", "getModelAPICatalogEntry", "Model APIs", "Inspect one managed Model API identity without supplier disclosure", "", "Object", 200, false},
	{"GET", "/system/instances", "listControlPlaneInstances", "System", "List live control-plane instances and protocol compatibility", "", "ObjectList", 200, false},
	{"GET", "/environments", "listEnvironments", "Endpoints", "List endpoint environments", "", "ObjectList", 200, false},
	{"POST", "/environments", "createEnvironment", "Endpoints", "Create an endpoint environment", "EnvironmentCreate", "Object", 201, false},
	{"POST", "/environment-promotions", "stageEnvironmentPromotion", "Endpoints", "Stage one environment endpoint's immutable plan as another endpoint's candidate", "EnvironmentPromotionRequest", "Object", 201, true},
	{"GET", "/logical-models", "listLogicalModels", "Endpoints", "List logical model identities", "", "ObjectList", 200, false},
	{"POST", "/logical-models", "createLogicalModel", "Endpoints", "Create a logical model identity", "LogicalModelCreate", "Object", 201, false},
	{"GET", "/endpoints", "listEndpoints", "Endpoints", "List stable application endpoints", "", "ObjectList", 200, false},
	{"POST", "/endpoints", "createEndpoint", "Endpoints", "Create a stable application endpoint", "EndpointCreate", "Object", 201, false},
	{"POST", "/adoptions/endpoints", "adoptEndpoint", "Adoption", "Adopt an existing inference endpoint without transferring lifecycle ownership", "EndpointAdoption", "Object", 201, false},
	{"PUT", "/adoptions/endpoints/{name}/ownership", "promoteAdoptionOwnership", "Adoption", "Explicitly promote an adopted endpoint to traffic-managed ownership", "AdoptionOwnership", "Object", 200, false},
	{"GET", "/requests/{id}", "inspectRequest", "Diagnostics", "Inspect persisted request metadata without content", "", "Object", 200, false},
	{"GET", "/endpoints/{name}", "getEndpoint", "Endpoints", "Inspect an endpoint and its serving plans", "", "Object", 200, false},
	{"GET", "/endpoints/{name}/monitoring", "getEndpointMonitoring", "Monitoring", "Read bounded normalized inference and lifecycle evidence", "", "EndpointMonitoring", 200, false},
	{"POST", "/endpoints/{name}/doctor", "diagnoseEndpoint", "Diagnostics", "Persist deterministic endpoint findings", "Object", "ObjectList", 200, false},
	{"GET", "/endpoints/{name}/alerts", "listAlertPolicies", "Alerts", "List signed webhook alert policies", "", "ObjectList", 200, false},
	{"POST", "/endpoints/{name}/alerts", "createAlertPolicy", "Alerts", "Create a signed webhook alert policy", "AlertPolicyCreate", "Object", 201, false},
	{"POST", "/endpoints/{name}/alerts/evaluate", "evaluateAlerts", "Alerts", "Evaluate deterministic findings and deliver eligible alerts", "Object", "Object", 200, false},
	{"GET", "/endpoints/{name}/admission", "getAdmissionPolicy", "Admission", "Inspect bounded endpoint admission policy", "", "Object", 200, false},
	{"PUT", "/endpoints/{name}/admission", "setAdmissionPolicy", "Admission", "Set bounded endpoint admission policy", "AdmissionPolicy", "Object", 200, false},
	{"POST", "/endpoints/{name}/async", "submitAsyncInference", "Async inference", "Submit one encrypted durable inference job", "AsyncInferenceSubmit", "Object", 202, true},
	{"GET", "/async/jobs/{id}", "getAsyncInferenceJob", "Async inference", "Inspect an async job and its result when complete", "", "Object", 200, false},
	{"DELETE", "/async/jobs/{id}", "cancelAsyncInferenceJob", "Async inference", "Cancel queued or executing async inference", "", "Empty", 204, false},
	{"DELETE", "/endpoints/{name}", "deleteEndpoint", "Endpoints", "Delete an endpoint without deleting its bound deployment", "", "Object", 200, false},
	{"POST", "/endpoints/{name}/bindings", "createEndpointBinding", "Endpoints", "Attach a backend binding to an endpoint", "EndpointBindingCreate", "Object", 201, false},
	{"POST", "/endpoints/{name}/plans", "createServingPlan", "Endpoints", "Create and stage an immutable serving plan", "ServingPlanCreate", "Object", 201, false},
	{"PUT", "/endpoints/{name}/plans/{plan}/active", "activateServingPlan", "Endpoints", "Promote a serving plan for future requests", "Object", "Object", 200, false},
	{"PUT", "/endpoints/{name}/plans/{plan}/candidate", "stageServingPlan", "Endpoints", "Stage a candidate serving plan", "Object", "Object", 200, false},
	{"GET", "/endpoints/{name}/release-guard/policy", "getEndpointReleaseGuardPolicy", "Endpoints", "Inspect endpoint Release Guard policy", "", "Object", 200, false},
	{"PUT", "/endpoints/{name}/release-guard/policy", "setEndpointReleaseGuardPolicy", "Endpoints", "Set endpoint Release Guard policy", "EndpointReleaseGuardPolicy", "Object", 200, false},
	{"POST", "/endpoints/{name}/release-guard/evaluate", "evaluateEndpointReleaseGuard", "Endpoints", "Evaluate active and candidate serving plans", "EndpointGuardEvaluationRequest", "Object", 201, false},
	{"GET", "/endpoints/{name}/release-guard/evaluations", "listEndpointReleaseGuardEvaluations", "Endpoints", "List endpoint Release Guard decisions", "", "ObjectList", 200, false},
	{"GET", "/operations/{id}/events", "listOperationEvents", "Operations", "List durable operation events", "", "ObjectList", 200, false},
	{"POST", "/operations/{id}/cancel", "cancelOperation", "Operations", "Request cooperative operation cancellation", "", "Object", 202, false},
	{"POST", "/deployments/apply", "applyDeployment", "Deployments", "Apply a DeploymentSpec", "DeploymentSpec", "OperationEnvelope", 202, true},
	{"POST", "/deployments", "createDeployment", "Deployments", "Create a provider-managed deployment", "DeploymentCreate", "DeploymentOperationEnvelope", 202, true},
	{"DELETE", "/deployments/{name}", "deleteDeployment", "Deployments", "Delete a logical deployment", "", "OperationEnvelope", 202, true},
	{"GET", "/deployments", "listDeployments", "Deployments", "List logical deployments", "", "DeploymentList", 200, false},
	{"GET", "/deployments/{name}", "getDeployment", "Deployments", "Inspect deployment lifecycle state", "", "DeploymentView", 200, false},
	{"POST", "/deployments/{name}/measurements", "recordOperationalMeasurements", "Monitoring", "Attach bounded content-free operational measurements to the active revision", "OperationalMeasurementIngest", "ObjectList", 201, false},
	{"POST", "/deployments/{name}/cost-evidence", "recordCostEvidence", "FinOps", "Attach bounded currency-explicit cost evidence to the active revision", "CostEvidenceIngest", "ObjectList", 201, false},
	{"GET", "/deployments/{name}/events", "listDeploymentEvents", "Deployments", "List deployment events", "", "ObjectList", 200, false},
	{"POST", "/deployments/{name}/quality-evidence", "attachQualityEvidence", "Release evidence", "Attach signed aggregate semantic quality evidence to an immutable revision", "SignedEvidenceEnvelope", "Object", 201, true},
	{"GET", "/deployments/{name}/quality-evidence", "listQualityEvidence", "Release evidence", "List revision-bound semantic quality evidence without prompt or output content", "", "ObjectList", 200, false},
	{"POST", "/deployments/{name}/benchmarks", "runBenchmark", "Benchmarks", "Run and persist an AIPerf benchmark", "BenchmarkRequest", "Object", 201, false},
	{"GET", "/deployments/{name}/benchmarks", "listBenchmarks", "Benchmarks", "List benchmark history", "", "ObjectList", 200, false},
	{"POST", "/deployments/{name}/recipes", "captureRecipe", "Recipes", "Capture an immutable evidence-backed recipe", "RecipeCaptureRequest", "Object", 201, false},
	{"GET", "/recipes", "listRecipes", "Recipes", "Search immutable model recipes", "", "ObjectList", 200, false},
	{"GET", "/recipes/{name}/{version}", "getRecipe", "Recipes", "Get one immutable model recipe", "", "Object", 200, false},
	{"POST", "/lab/evaluations", "evaluateLab", "Inference Lab", "Compare persisted serving evidence", "LabEvaluationRequest", "Object", 201, false},
	{"POST", "/deployments/{name}/replays", "captureReplay", "Replay", "Capture a privacy-preserving production workload shape", "ReplayCaptureRequest", "Object", 201, false},
	{"GET", "/replays/{id}", "getReplay", "Replay", "Inspect a persisted workload-shape trace", "", "Object", 200, false},
	{"GET", "/capacity/intelligence", "capacityIntelligence", "Capacity intelligence", "Summarize tenant-scoped observed capacity evidence", "", "Object", 200, false},
	{"POST", "/artifacts/{id}/cache-observations", "recordArtifactCacheObservation", "Model artifacts", "Record bounded provider-native artifact cache evidence", "ArtifactCacheObservationRequest", "Object", 201, false},
	{"POST", "/artifacts/{id}/prefetches", "requestArtifactPrefetch", "Model artifacts", "Request provider-adapter artifact prefetch", "ArtifactPrefetchRequest", "Object", 201, true},
	{"GET", "/artifacts/{id}/cache", "inspectArtifactCache", "Model artifacts", "Inspect fresh and expired cache observations separately from prefetch intent", "", "Object", 200, false},
	{"POST", "/optimization/proposals", "proposeOptimization", "Inference optimization", "Generate bounded reviewed configuration candidates without provider mutation or performance claims", "OptimizationProposalRequest", "Object", 200, false},
	{"GET", "/optimization/campaigns", "listOptimizationCampaigns", "Inference optimization", "List durable evidence-gated optimization campaigns", "", "ObjectList", 200, false},
	{"POST", "/optimization/campaigns", "createOptimizationCampaign", "Inference optimization", "Persist a bounded immutable proposal without mutating provider infrastructure", "OptimizationCampaignCreate", "Object", 201, true},
	{"GET", "/optimization/campaigns/{id}", "getOptimizationCampaign", "Inference optimization", "Inspect predicted and measured campaign evidence", "", "Object", 200, false},
	{"POST", "/optimization/campaigns/{id}/approve", "approveOptimizationCampaign", "Inference optimization", "Validate bounded cost authority and queue durable optimization execution", "OptimizationCampaignApproval", "Object", 202, false},
	{"POST", "/optimization/campaigns/{id}/activate", "activateOptimizationCampaign", "Inference optimization", "Queue explicit activation or guarded promotion for a qualified candidate", "OptimizationCampaignActivation", "Object", 202, false},
	{"POST", "/optimization/campaigns/{id}/cancel", "cancelOptimizationCampaign", "Inference optimization", "Cancel future campaign work and require cleanup", "Object", "Object", 202, false},
	{"GET", "/optimized-artifacts", "listOptimizedArtifacts", "Inference optimization", "List immutable optimized artifact provenance", "", "ObjectList", 200, false},
	{"POST", "/optimized-artifacts", "createOptimizedArtifact", "Inference optimization", "Plan a digest-pinned external artifact build", "OptimizedArtifactPlan", "Object", 201, true},
	{"GET", "/optimized-artifacts/{id}", "getOptimizedArtifact", "Inference optimization", "Inspect optimized artifact provenance and evidence state", "", "Object", 200, false},
	{"POST", "/optimized-artifacts/{id}/build", "beginOptimizedArtifactBuild", "Inference optimization", "Record that the external digest-pinned builder started", "Object", "Object", 200, false},
	{"POST", "/optimized-artifacts/{id}/attest", "attestOptimizedArtifactBuild", "Inference optimization", "Attach immutable output identity or a stable external builder failure", "OptimizedArtifactAttestation", "Object", 200, false},
	{"POST", "/optimized-artifacts/{id}/qualify", "qualifyOptimizedArtifact", "Inference optimization", "Bind passing signed quality evidence from the exact candidate revision", "OptimizedArtifactQualification", "Object", 200, false},
	{"POST", "/sandboxes/references", "createSandboxReference", "Sandboxes", "Register an externally owned sandbox and issue an expiring endpoint-scoped credential", "SandboxReferenceCreate", "Object", 201, false},
	{"GET", "/sandboxes/references", "listSandboxReferences", "Sandboxes", "List external sandbox references without command, file, prompt, or output content", "", "ObjectList", 200, false},
	{"POST", "/sandboxes/references/{id}/credential/rotate", "rotateSandboxCredential", "Sandboxes", "Rotate the expiring endpoint-scoped credential for an active sandbox reference", "", "Object", 200, false},
	{"DELETE", "/sandboxes/references/{id}", "revokeSandboxReference", "Sandboxes", "Revoke InferCrane access without mutating the external sandbox", "", "Object", 200, false},
	{"POST", "/deployments/{name}/training-artifacts", "attachTrainingArtifact", "Training lineage", "Verify and attach a signed immutable checkpoint handoff", "SignedEvidenceEnvelope", "Object", 201, true},
	{"GET", "/deployments/{name}/training-artifacts", "listTrainingArtifacts", "Training lineage", "List content-free external training provenance for deployment revisions", "", "ObjectList", 200, false},
	{"POST", "/deployments/{name}/finops/reports", "createFinOpsReport", "FinOps", "Persist a sourced cost report without invented savings", "FinOpsReportRequest", "Object", 201, false},
	{"GET", "/deployments/{name}/finops/reports", "listFinOpsReports", "FinOps", "List persisted FinOps evidence", "", "ObjectList", 200, false},
	{"POST", "/deployments/{name}/autopilot/plans", "createAutopilotPlan", "Autopilot", "Create an advisory plan from a persisted recommendation", "AutopilotPlanRequest", "Object", 201, true},
	{"GET", "/autopilot/plans/{id}", "getAutopilotPlan", "Autopilot", "Inspect an immutable advisory plan", "", "Object", 200, false},
	{"POST", "/autopilot/plans/{id}/approve", "approveAutopilotPlan", "Autopilot", "Record human approval without mutating serving state", "Object", "Object", 200, true},
	{"POST", "/context-passports", "createContextPassport", "Context Passport", "Create bounded durable logical session identity", "ContextPassportRequest", "Object", 201, false},
	{"GET", "/context-passports/{id}", "getContextPassport", "Context Passport", "Inspect logical session identity and best-effort affinity", "", "Object", 200, false},
	{"POST", "/deployments/{name}/burst-guard/evaluate", "evaluateBurstGuard", "Burst Guard", "Persist a fresh budget-bounded overflow decision", "BurstGuardEvaluationRequest", "Object", 201, false},
	{"POST", "/deployments/{name}/passports", "createInferencePassport", "Release evidence", "Issue a signed Inference Passport", "Object", "Object", 201, false},
	{"GET", "/deployments/{name}/passports", "listInferencePassports", "Release evidence", "List signed Inference Passports", "", "ObjectList", 200, false},
	{"GET", "/deployments/{name}/slo-policy", "getSLOPolicy", "Inference decisions", "Inspect deterministic SLO policy", "", "SLOPolicyEnvelope", 200, false},
	{"PUT", "/deployments/{name}/slo-policy", "setSLOPolicy", "Inference decisions", "Set deterministic SLO policy", "SLOPolicy", "SLOPolicyEnvelope", 200, false},
	{"DELETE", "/deployments/{name}/slo-policy", "deleteSLOPolicy", "Inference decisions", "Delete deterministic SLO policy", "", "Empty", 204, false},
	{"POST", "/deployments/{name}/recommendations", "createRecommendation", "Inference decisions", "Evaluate and persist a recommendation", "RecommendationRequest", "RecommendationEnvelope", 201, false},
	{"GET", "/deployments/{name}/recommendations", "listRecommendations", "Inference decisions", "List persisted recommendation history", "", "ObjectList", 200, false},
	{"GET", "/deployments/{name}/revisions", "listRevisions", "Revisions", "List immutable deployment revisions", "", "ObjectList", 200, false},
	{"POST", "/deployments/{name}/rollouts", "createRollout", "Revisions", "Create a candidate revision", "Object", "OperationEnvelope", 202, true},
	{"POST", "/deployments/{name}/rollouts/guard/evaluate", "evaluateReleaseGuard", "Release Guard", "Evaluate active and candidate revisions", "Object", "OperationEnvelope", 202, true},
	{"GET", "/deployments/{name}/release-guard/policy", "getReleaseGuardPolicy", "Release Guard", "Inspect the persisted guard policy", "", "Object", 200, false},
	{"PUT", "/deployments/{name}/release-guard/policy", "setReleaseGuardPolicy", "Release Guard", "Set the persisted guard policy", "ReleaseGuardPolicy", "Object", 200, false},
	{"POST", "/deployments/{name}/rollouts/{revision}/promote", "promoteRollout", "Revisions", "Promote a validated candidate", "", "OperationEnvelope", 202, true},
	{"POST", "/deployments/{name}/rollouts/{revision}/provision", "provisionRollout", "Revisions", "Provision candidate capacity", "", "OperationEnvelope", 202, true},
	{"POST", "/deployments/{name}/rollouts/{revision}/reject", "rejectRollout", "Revisions", "Reject a candidate revision", "Object", "OperationEnvelope", 202, true},
	{"POST", "/deployments/{name}/rollback", "rollbackDeployment", "Revisions", "Roll back to a prior revision", "Object", "OperationEnvelope", 202, true},
	{"GET", "/deployments/{name}/scaling-decisions", "listScalingDecisions", "Scaling", "List persisted scaling decisions", "", "ObjectList", 200, false},
	{"PUT", "/deployments/{name}/route", "setDeploymentRoute", "Routing", "Set the active route target", "Object", "Object", 200, false},
	{"GET", "/targets", "listTargets", "Targets", "List inference targets", "", "ObjectList", 200, false},
	{"POST", "/targets", "createTarget", "Targets", "Register an inference target", "TargetCreate", "Object", 201, false},
	{"GET", "/provider-connections", "listProviderConnections", "Providers", "List reusable external provider connections", "", "ObjectList", 200, false},
	{"POST", "/provider-connections", "createProviderConnection", "Providers", "Create a secret-reference-only external provider connection", "ProviderConnectionCreate", "Object", 201, false},
	{"DELETE", "/provider-connections/{name}", "deleteProviderConnection", "Providers", "Delete provider configuration without mutating immutable endpoint bindings", "", "Object", 200, false},
	{"GET", "/billing/wallet", "getManagedWallet", "Billing", "Inspect prepaid managed Model API funds and active reservations", "", "ManagedWalletEnvelope", 200, false},
	{"GET", "/billing/ledger", "listManagedWalletLedger", "Billing", "List append-only managed Model API settlements and credits", "", "ManagedWalletLedgerList", 200, false},
	{"POST", "/billing/checkout-sessions", "createManagedCheckoutSession", "Billing", "Create a fixed-price hosted checkout without changing the prepaid balance", "ManagedCheckoutRequest", "ManagedCheckoutEnvelope", 201, false},
	{"POST", "/billing/webhooks/stripe", "processStripeBillingWebhook", "Billing", "Verify and atomically apply an idempotent Stripe prepaid payment event", "Object", "ManagedPaymentWebhookResponse", 200, false},
	{"POST", "/admin/billing/credits", "creditManagedWallet", "Billing administration", "Post an externally collected prepaid payment using bootstrap authority", "ManagedWalletCredit", "ManagedWalletEnvelope", 201, true},
	{"GET", "/admin/billing/reservations", "listManagedUsageReservations", "Billing administration", "List tenant reservations requiring operator reconciliation", "", "ManagedUsageReservationList", 200, false},
	{"POST", "/admin/billing/reservations/{id}/settlement", "settleManagedUsage", "Billing administration", "Settle a reservation from externally verified token usage", "ManagedUsageSettlement", "ManagedUsageReservationEnvelope", 200, true},
	{"POST", "/admin/billing/reservations/{id}/release", "releaseManagedUsage", "Billing administration", "Release a reservation after external non-billing is verified", "ManagedUsageRelease", "Object", 200, true},
	{"GET", "/orphans", "listOrphans", "Infrastructure", "List orphaned provider resources", "", "ObjectList", 200, false},
	{"GET", "/audit-events", "listAuditEvents", "Audit", "List tenant audit events", "", "ObjectList", 200, false},
	{"PUT", "/tenant/quota", "setTenantQuota", "Identity", "Set tenant safety quotas", "Object", "Empty", 204, false},
	{"POST", "/tenants", "createTenant", "Identity", "Create a tenant", "Object", "Object", 201, false},
	{"POST", "/principals", "createPrincipal", "Identity", "Create a scoped service account", "Object", "Object", 201, false},
	{"GET", "/principals", "listPrincipals", "Identity", "List service accounts for the authenticated organization", "", "ObjectList", 200, false},
	{"POST", "/principals/{id}/rotate", "rotatePrincipal", "Identity", "Rotate a service-account credential", "", "Object", 200, false},
	{"DELETE", "/principals/{id}", "revokePrincipal", "Identity", "Revoke a service account", "", "Empty", 204, false},
	{"GET", "/secrets", "listSecretReferences", "Secrets", "List secret references", "", "ObjectList", 200, false},
	{"POST", "/secrets", "createSecretReference", "Secrets", "Create a reference to an external secret", "Object", "Object", 201, false},
	{"DELETE", "/secrets/{id}", "deleteSecretReference", "Secrets", "Delete a secret reference", "", "Empty", 204, false},
	{"GET", "/deployments/{name}/external-policy", "getExternalPolicy", "External capacity", "Inspect governed external fallback", "", "ExternalPolicyEnvelope", 200, false},
	{"PUT", "/deployments/{name}/external-policy", "setExternalPolicy", "External capacity", "Configure governed external fallback", "ExternalPolicyRequest", "ExternalPolicyEnvelope", 200, false},
}

func Document() (map[string]any, error) {
	paths := map[string]any{}
	tags := map[string]bool{}
	seen := map[string]bool{}
	for _, route := range Routes {
		key := route.Method + " " + route.Path
		if seen[key] || route.OperationID == "" || route.Status == 0 {
			return nil, fmt.Errorf("invalid duplicate or incomplete API route %q", key)
		}
		seen[key], tags[route.Tag] = true, true
		documentPath := "/api/v1" + route.Path
		pathItem, _ := paths[documentPath].(map[string]any)
		if pathItem == nil {
			pathItem = map[string]any{}
			paths[documentPath] = pathItem
		}
		operation := map[string]any{
			"operationId": route.OperationID,
			"summary":     route.Summary,
			"tags":        []string{route.Tag},
			"security":    []map[string][]string{{"bearerAuth": {}}},
			"responses":   responses(route),
		}
		if route.Path == "/billing/webhooks/stripe" {
			operation["security"] = []any{}
		}
		parameters := pathParameters(route.Path)
		if route.Path == "/billing/webhooks/stripe" {
			parameters = append(parameters, map[string]any{"name": "Stripe-Signature", "in": "header", "required": true, "description": "Stripe webhook HMAC signature; the request is unauthenticated by bearer token and grants balance only after verification.", "schema": map[string]any{"type": "string"}})
		}
		if route.Idempotent {
			parameters = append(parameters, map[string]any{"name": "Idempotency-Key", "in": "header", "required": true, "description": "Stable key used to adopt the original durable operation after retries or disconnects.", "schema": map[string]any{"type": "string", "maxLength": 128}})
		}
		if len(parameters) > 0 {
			operation["parameters"] = parameters
		}
		if route.Request != "" {
			operation["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": ref(route.Request)}}}
		}
		pathItem[strings.ToLower(route.Method)] = operation
	}
	tags["Inference"] = true
	paths["/v1/models"] = map[string]any{"get": map[string]any{"operationId": "listInferenceModels", "summary": "List logical inference models", "tags": []string{"Inference"}, "security": []map[string][]string{{"bearerAuth": {}}}, "responses": map[string]any{"200": map[string]any{"description": "OpenAI-compatible model list", "content": map[string]any{"application/json": map[string]any{"schema": ref("Object")}}}, "default": map[string]any{"description": "Typed API error", "content": map[string]any{"application/json": map[string]any{"schema": ref("ErrorEnvelope")}}}}}}
	paths["/v1/chat/completions"] = map[string]any{"post": map[string]any{"operationId": "createChatCompletion", "summary": "Create a buffered or streaming chat completion", "description": "When stream=true, the response is an SSE sequence of data JSON events terminated by data: [DONE]. InferCrane never replays a partially transmitted stream.", "tags": []string{"Inference"}, "security": []map[string][]string{{"bearerAuth": {}}}, "requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": ref("ChatCompletionRequest")}}}, "responses": map[string]any{"200": map[string]any{"description": "Buffered JSON or streaming SSE response", "content": map[string]any{"application/json": map[string]any{"schema": ref("Object")}, "text/event-stream": map[string]any{"schema": map[string]any{"type": "string", "description": "SSE data events ending with [DONE]."}}}}, "default": map[string]any{"description": "Typed API error", "content": map[string]any{"application/json": map[string]any{"schema": ref("ErrorEnvelope")}}}}}}
	for path, operation := range map[string]string{"/v1/responses": "createResponse", "/v1/embeddings": "createEmbedding", "/v1/completions": "createCompletion", "/v1/chat/completions/batch": "createChatCompletionBatch"} {
		paths[path] = map[string]any{"post": map[string]any{"operationId": operation, "summary": "Proxy a capability-qualified OpenAI-compatible request", "description": "The selected endpoint must explicitly qualify this protocol. InferCrane rewrites only the logical model identity and otherwise preserves the request and response.", "tags": []string{"Inference"}, "security": []map[string][]string{{"bearerAuth": {}}}, "requestBody": map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": ref("Object")}}}, "responses": map[string]any{"200": map[string]any{"description": "Protocol-native JSON or streaming response", "content": map[string]any{"application/json": map[string]any{"schema": ref("Object")}, "text/event-stream": map[string]any{"schema": map[string]any{"type": "string"}}}}, "422": map[string]any{"description": "The selected endpoint has not qualified this protocol", "content": map[string]any{"application/json": map[string]any{"schema": ref("ErrorEnvelope")}}}, "default": map[string]any{"description": "Typed API error", "content": map[string]any{"application/json": map[string]any{"schema": ref("ErrorEnvelope")}}}}}}
	}
	tagList := make([]string, 0, len(tags))
	for tag := range tags {
		tagList = append(tagList, tag)
	}
	sort.Strings(tagList)
	tagObjects := make([]map[string]string, 0, len(tagList))
	for _, tag := range tagList {
		tagObjects = append(tagObjects, map[string]string{"name": tag})
	}
	return map[string]any{
		"openapi":    "3.1.0",
		"info":       map[string]any{"title": "InferCrane Control API", "version": Version, "description": "Durable, authenticated control-plane operations. Mutation responses may outlive the requesting client."},
		"servers":    []map[string]string{{"url": "http://127.0.0.1:18000", "description": "Local InferCrane API and gateway"}},
		"tags":       tagObjects,
		"paths":      paths,
		"components": map[string]any{"securitySchemes": map[string]any{"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"}}, "schemas": schemas()},
	}, nil
}

func Marshal() ([]byte, error) {
	doc, err := Document()
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(doc, "", "  ")
}

func responses(route Route) map[string]any {
	result := map[string]any{fmt.Sprint(route.Status): map[string]any{"description": httpDescription(route.Status)}}
	if route.Response != "Empty" {
		result[fmt.Sprint(route.Status)].(map[string]any)["content"] = map[string]any{"application/json": map[string]any{"schema": ref(route.Response)}}
	}
	result["default"] = map[string]any{"description": "Typed API error", "content": map[string]any{"application/json": map[string]any{"schema": ref("ErrorEnvelope")}}}
	return result
}

func httpDescription(status int) string {
	switch status {
	case 200:
		return "Success"
	case 201:
		return "Created"
	case 202:
		return "Durable operation accepted"
	case 204:
		return "No content"
	}
	return "Response"
}

func pathParameters(path string) []map[string]any {
	parameters := []map[string]any{}
	for _, part := range strings.Split(path, "/") {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			name := strings.TrimSuffix(strings.TrimPrefix(part, "{"), "}")
			parameters = append(parameters, map[string]any{"name": name, "in": "path", "required": true, "schema": map[string]any{"type": "string", "minLength": 1}})
		}
	}
	return parameters
}

func ref(name string) map[string]any { return map[string]any{"$ref": "#/components/schemas/" + name} }

func schemas() map[string]any {
	stringMap := map[string]any{"type": "object", "additionalProperties": true}
	return map[string]any{
		"Object":                          stringMap,
		"ObjectList":                      map[string]any{"type": "object", "required": []string{"data"}, "properties": map[string]any{"data": map[string]any{"type": "array", "items": stringMap}}},
		"Error":                           map[string]any{"type": "object", "required": []string{"code", "message"}, "properties": map[string]any{"code": map[string]any{"type": "string"}, "category": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string"}, "retryable": map[string]any{"type": "boolean"}, "remediation": map[string]any{"type": "string"}}},
		"ErrorEnvelope":                   map[string]any{"type": "object", "required": []string{"error"}, "properties": map[string]any{"error": ref("Error")}},
		"ConsoleIdentityProvisioning":     map[string]any{"type": "object", "additionalProperties": false, "required": []string{"provider", "external_user_id", "external_organization_id", "display_name", "role", "access"}, "properties": map[string]any{"provider": map[string]any{"type": "string", "enum": []string{"clerk"}}, "external_user_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "external_organization_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "display_name": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "role": map[string]any{"type": "string", "enum": []string{"viewer", "operator", "admin"}}, "scopes": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 32}, "access": map[string]any{"type": "boolean"}}},
		"Operation":                       map[string]any{"type": "object", "required": []string{"id", "kind", "status", "progress"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "kind": map[string]any{"type": "string"}, "resource_type": map[string]any{"type": "string"}, "resource_name": map[string]any{"type": "string"}, "status": map[string]any{"type": "string", "enum": []string{"pending", "leased", "running", "waiting", "cancelling", "succeeded", "failed", "cancelled"}}, "progress": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}, "message": map[string]any{"type": "string"}, "current_step": map[string]any{"type": "string"}, "current_step_status": map[string]any{"type": "string", "enum": []string{"pending", "running", "waiting", "succeeded", "failed", "cancelled"}}, "error_code": map[string]any{"type": "string"}, "retryable": map[string]any{"type": "boolean"}, "cancel_requested": map[string]any{"type": "boolean"}, "attempt": map[string]any{"type": "integer"}, "max_attempts": map[string]any{"type": "integer"}, "result": stringMap, "created_at": map[string]any{"type": "string", "format": "date-time"}, "updated_at": map[string]any{"type": "string", "format": "date-time"}}},
		"OperationEnvelope":               map[string]any{"type": "object", "required": []string{"operation"}, "properties": map[string]any{"operation": ref("Operation"), "created": map[string]any{"type": "boolean"}}},
		"DeploymentOperationEnvelope":     map[string]any{"allOf": []map[string]any{ref("OperationEnvelope"), {"type": "object", "properties": map[string]any{"deployment": ref("Deployment")}}}},
		"Deployment":                      map[string]any{"type": "object", "required": []string{"name", "model", "runtime"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"}, "endpoint_names": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "model": map[string]any{"type": "string"}, "runtime": map[string]any{"type": "string"}, "observed_state": map[string]any{"type": "string"}, "min_replicas": map[string]any{"type": "integer"}, "max_replicas": map[string]any{"type": "integer"}, "active_revision_id": map[string]any{"type": "string"}, "candidate_revision_id": map[string]any{"type": "string"}}},
		"DeploymentList":                  map[string]any{"type": "object", "required": []string{"data"}, "properties": map[string]any{"data": map[string]any{"type": "array", "items": ref("Deployment")}}},
		"DeploymentView":                  map[string]any{"type": "object", "required": []string{"deployment", "lifecycle_status"}, "properties": map[string]any{"deployment": ref("Deployment"), "lifecycle_status": stringMap, "active_operation": ref("Operation"), "targets": map[string]any{"type": "array", "items": stringMap}, "replicas": map[string]any{"type": "array", "items": stringMap}, "revisions": map[string]any{"type": "array", "items": stringMap}}},
		"RuntimeWorkload":                 map[string]any{"type": "object", "required": []string{"image", "command", "protocol", "port", "readiness_path", "models_path", "metrics_path", "cancellation", "drain", "shutdown_grace_seconds"}, "properties": map[string]any{"image": map[string]any{"type": "string", "pattern": `@sha256:[a-f0-9]{64}$`}, "command": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}}, "protocol": map[string]any{"type": "string", "enum": []string{"openai"}}, "port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}, "readiness_path": map[string]any{"type": "string", "enum": []string{"/health"}}, "models_path": map[string]any{"type": "string", "enum": []string{"/v1/models"}}, "metrics_path": map[string]any{"type": "string", "enum": []string{"/metrics"}}, "cancellation": map[string]any{"type": "string", "enum": []string{"http-disconnect"}}, "drain": map[string]any{"type": "string", "enum": []string{"connection"}}, "shutdown_grace_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 3600}}, "additionalProperties": false},
		"ServingPool":                     map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"replicas": map[string]any{"type": "integer", "minimum": 0, "maximum": 10000}, "tensor_parallelism": map[string]any{"type": "integer", "minimum": 0, "maximum": 1024}}},
		"ServingTopology":                 map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"schema_version": map[string]any{"type": "string", "enum": []string{"infercrane.serving/v1"}}, "backend": map[string]any{"type": "string", "enum": []string{"dynamo"}}, "profile": map[string]any{"type": "string", "enum": []string{"baseline", "custom"}}, "mode": map[string]any{"type": "string", "enum": []string{"aggregated", "disaggregated"}}, "routing": map[string]any{"type": "string", "enum": []string{"direct", "kv-aware"}}, "worker": ref("ServingPool"), "prefill": ref("ServingPool"), "decode": ref("ServingPool"), "autoscaling": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"owner": map[string]any{"type": "string", "enum": []string{"disabled", "dynamo-planner", "external"}}, "min": map[string]any{"type": "integer", "minimum": 0, "maximum": 10000}, "max": map[string]any{"type": "integer", "minimum": 0, "maximum": 10000}}}, "cache": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"backend": map[string]any{"type": "string", "enum": []string{"none", "kvbm", "lmcache", "hicache"}}, "host_gib": map[string]any{"type": "integer", "minimum": 0}, "disk_gib": map[string]any{"type": "integer", "minimum": 0}, "memory_gib": map[string]any{"type": "integer", "minimum": 0}, "storage_claim": map[string]any{"type": "string"}, "configuration_ref": map[string]any{"type": "string"}, "metrics": map[string]any{"type": "boolean"}}}}},
		"DeploymentCreate":                map[string]any{"type": "object", "required": []string{"name", "model"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "endpoint_name": map[string]any{"type": "string", "description": "Stable application endpoint alias. Defaults to the deployment name.", "pattern": `^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`}, "model": map[string]any{"type": "string"}, "runtime": map[string]any{"type": "string", "default": "vllm", "enum": []string{"vllm", "sglang", "custom-oci"}}, "cloud": map[string]any{"type": "string"}, "provider_adapter": map[string]any{"type": "string", "description": "Exact provider profile for this immutable revision; omit only when the cloud/runtime default is unambiguous."}, "compute_mode": map[string]any{"type": "string", "enum": []string{"elastic", "serverless"}}, "gpu": map[string]any{"type": "string"}, "gpu_count": map[string]any{"type": "integer", "minimum": 1, "maximum": 1024, "default": 1, "description": "Accelerators allocated to each runtime replica."}, "region": map[string]any{"type": "string"}, "port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}, "min_replicas": map[string]any{"type": "integer", "minimum": 0}, "max_replicas": map[string]any{"type": "integer", "minimum": 0}, "runtime_version": map[string]any{"type": "string"}, "runtime_args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "workload": ref("RuntimeWorkload"), "serving": ref("ServingTopology"), "model_revision": map[string]any{"type": "string"}}},
		"DeploymentSpec":                  map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "model", "targets"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "endpoint_name": map[string]any{"type": "string", "description": "Stable application endpoint alias. Defaults to the deployment name.", "pattern": `^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`}, "model": map[string]any{"type": "string"}, "targets": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}}, "routing_strategy": map[string]any{"type": "string"}, "min_replicas": map[string]any{"type": "integer", "minimum": 0}, "max_replicas": map[string]any{"type": "integer", "minimum": 0}, "autoscaling_enabled": map[string]any{"type": "boolean"}}},
		"TargetCreate":                    map[string]any{"type": "object", "required": []string{"name", "url", "provider", "runtime"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "url": map[string]any{"type": "string", "format": "uri"}, "provider": map[string]any{"type": "string"}, "runtime": map[string]any{"type": "string"}, "upstream_model": map[string]any{"type": "string"}}},
		"ProviderConnectionCreate":        map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "adapter", "target", "secret_reference_id"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "adapter": map[string]any{"type": "string", "enum": []string{"openrouter", "openai-compatible-external", "modal", "runpod-serverless-api", "fly-io"}}, "target": map[string]any{"type": "string"}, "secret_reference_id": map[string]any{"type": "string"}}},
		"ManagedWallet":                   map[string]any{"type": "object", "additionalProperties": false, "required": []string{"tenant_id", "currency", "balance_microusd", "reserved_microusd", "available_microusd", "updated_at"}, "properties": map[string]any{"tenant_id": map[string]any{"type": "string"}, "currency": map[string]any{"type": "string", "const": "USD"}, "balance_microusd": map[string]any{"type": "integer"}, "reserved_microusd": map[string]any{"type": "integer", "minimum": 0}, "available_microusd": map[string]any{"type": "integer"}, "updated_at": map[string]any{"type": "string", "format": "date-time"}}},
		"ManagedWalletEnvelope":           map[string]any{"type": "object", "required": []string{"data", "funding_mode", "funding_available", "funding_provider", "checkout_amounts_microusd"}, "properties": map[string]any{"data": ref("ManagedWallet"), "funding_mode": map[string]any{"type": "string", "const": "prepaid"}, "funding_available": map[string]any{"type": "boolean"}, "funding_provider": map[string]any{"type": "string", "enum": []string{"", "stripe"}}, "checkout_amounts_microusd": map[string]any{"type": "array", "items": map[string]any{"type": "integer", "enum": []int64{25000000, 50000000, 100000000, 250000000, 500000000}}}, "payment_collection": map[string]any{"type": "string"}, "credit_id": map[string]any{"type": "string"}, "payment_collected_by_infercrane": map[string]any{"type": "boolean"}}},
		"ManagedCheckoutRequest":          map[string]any{"type": "object", "additionalProperties": false, "required": []string{"amount_microusd"}, "properties": map[string]any{"amount_microusd": map[string]any{"type": "integer", "enum": []int64{25000000, 50000000, 100000000, 250000000, 500000000}}}},
		"ManagedCheckoutSession":          map[string]any{"type": "object", "additionalProperties": false, "required": []string{"provider", "provider_id", "url", "amount_microusd", "currency", "expires_at"}, "properties": map[string]any{"provider": map[string]any{"type": "string", "const": "stripe"}, "provider_id": map[string]any{"type": "string", "minLength": 1}, "url": map[string]any{"type": "string", "format": "uri"}, "amount_microusd": map[string]any{"type": "integer", "minimum": 1}, "currency": map[string]any{"type": "string", "const": "USD"}, "expires_at": map[string]any{"type": "string", "format": "date-time"}}},
		"ManagedCheckoutEnvelope":         map[string]any{"type": "object", "additionalProperties": false, "required": []string{"checkout", "balance_changed", "credit_authority"}, "properties": map[string]any{"checkout": ref("ManagedCheckoutSession"), "balance_changed": map[string]any{"type": "boolean", "const": false}, "credit_authority": map[string]any{"type": "string", "const": "verified_provider_webhook"}}},
		"ManagedPaymentWebhookResponse":   map[string]any{"type": "object", "additionalProperties": false, "required": []string{"received", "status", "credit_applied"}, "properties": map[string]any{"received": map[string]any{"type": "boolean", "const": true}, "status": map[string]any{"type": "string", "enum": []string{"ignored", "applied"}}, "credit_applied": map[string]any{"type": "boolean"}}},
		"ManagedWalletCredit":             map[string]any{"type": "object", "additionalProperties": false, "required": []string{"tenant_id", "amount_microusd", "description"}, "properties": map[string]any{"tenant_id": map[string]any{"type": "string", "minLength": 1}, "amount_microusd": map[string]any{"type": "integer", "minimum": 1}, "description": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}}},
		"ManagedWalletLedgerEntry":        map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id", "tenant_id", "kind", "currency", "amount_microusd", "description", "created_at"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "tenant_id": map[string]any{"type": "string"}, "reservation_id": map[string]any{"type": "string"}, "kind": map[string]any{"type": "string", "enum": []string{"credit", "settlement"}}, "currency": map[string]any{"type": "string", "const": "USD"}, "amount_microusd": map[string]any{"type": "integer"}, "description": map[string]any{"type": "string"}, "created_at": map[string]any{"type": "string", "format": "date-time"}}},
		"ManagedWalletLedgerList":         map[string]any{"type": "object", "required": []string{"data"}, "properties": map[string]any{"data": map[string]any{"type": "array", "items": ref("ManagedWalletLedgerEntry")}, "content_recorded": map[string]any{"type": "boolean"}}},
		"ManagedUsageReservation":         map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id", "tenant_id", "binding_id", "supplier", "model", "reserved_microusd", "actual_microusd", "state", "resolution", "created_at", "updated_at"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "tenant_id": map[string]any{"type": "string"}, "binding_id": map[string]any{"type": "string"}, "supplier": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "reserved_microusd": map[string]any{"type": "integer", "minimum": 0}, "actual_microusd": map[string]any{"type": "integer", "minimum": 0}, "input_tokens": map[string]any{"type": "integer", "minimum": 0}, "output_tokens": map[string]any{"type": "integer", "minimum": 0}, "state": map[string]any{"type": "string", "enum": []string{"reserved", "settled", "released", "pending_reconciliation"}}, "resolution": map[string]any{"type": "string"}, "created_at": map[string]any{"type": "string", "format": "date-time"}, "updated_at": map[string]any{"type": "string", "format": "date-time"}}},
		"ManagedUsageReservationList":     map[string]any{"type": "object", "required": []string{"data"}, "properties": map[string]any{"data": map[string]any{"type": "array", "items": ref("ManagedUsageReservation")}, "content_recorded": map[string]any{"type": "boolean"}}},
		"ManagedUsageReservationEnvelope": map[string]any{"type": "object", "required": []string{"data"}, "properties": map[string]any{"data": ref("ManagedUsageReservation"), "content_recorded": map[string]any{"type": "boolean"}}},
		"ManagedUsageSettlement":          map[string]any{"type": "object", "additionalProperties": false, "required": []string{"tenant_id", "input_tokens", "output_tokens"}, "properties": map[string]any{"tenant_id": map[string]any{"type": "string", "minLength": 1}, "input_tokens": map[string]any{"type": "integer", "minimum": 0}, "output_tokens": map[string]any{"type": "integer", "minimum": 0}}},
		"ManagedUsageRelease":             map[string]any{"type": "object", "additionalProperties": false, "required": []string{"tenant_id", "reason"}, "properties": map[string]any{"tenant_id": map[string]any{"type": "string", "minLength": 1}, "reason": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}}},
		"EnvironmentCreate":               map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "policy": stringMap}},
		"EnvironmentPromotionRequest":     map[string]any{"type": "object", "additionalProperties": false, "required": []string{"source_endpoint", "destination_endpoint", "idempotency_key"}, "properties": map[string]any{"source_endpoint": map[string]any{"type": "string", "minLength": 1}, "destination_endpoint": map[string]any{"type": "string", "minLength": 1}, "idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}}},
		"LogicalModelCreate":              map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "description": map[string]any{"type": "string", "maxLength": 4096}}},
		"EndpointCreate":                  map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "logical_model", "environment"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "logical_model": map[string]any{"type": "string"}, "environment": map[string]any{"type": "string"}}},
		"EndpointAdoption":                map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "logical_model", "url", "source", "ownership_mode", "runtime"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "logical_model": map[string]any{"type": "string"}, "upstream_model": map[string]any{"type": "string"}, "url": map[string]any{"type": "string", "format": "uri"}, "source": map[string]any{"type": "string", "enum": []string{"vllm", "openai-compatible"}}, "ownership_mode": map[string]any{"type": "string", "enum": []string{"observe-only", "traffic-managed"}}, "runtime": map[string]any{"type": "string"}, "discover": map[string]any{"type": "boolean", "description": "Ask the control plane to verify /v1/models and resolve the selected model before adoption."}, "connector": map[string]any{"type": "string", "enum": []string{"auto", "vllm", "litellm", "openai-compatible"}}}},
		"MeasurementEvidence":             measurementEvidenceSchema(),
		"MonitoringSummary":               monitoringSummarySchema(),
		"MonitoringBucket":                monitoringBucketSchema(),
		"MonitoringBreakdown":             monitoringBreakdownSchema(),
		"MonitoringEvent":                 monitoringEventSchema(),
		"MonitoringEvidence":              monitoringEvidenceSchema(),
		"AdmissionMonitoring":             map[string]any{"type": "object", "additionalProperties": false, "required": []string{"capacity_state", "scope", "instance_id", "active", "waiting", "max_concurrent", "max_queue_depth", "rejected", "queue_timeouts", "observed_at"}, "properties": map[string]any{"capacity_state": map[string]any{"type": "string", "enum": []string{"accepting", "queueing", "saturated"}}, "scope": map[string]any{"type": "string", "enum": []string{"gateway_instance"}}, "instance_id": map[string]any{"type": "string"}, "active": map[string]any{"type": "integer", "minimum": 0}, "waiting": map[string]any{"type": "integer", "minimum": 0}, "max_concurrent": map[string]any{"type": "integer", "minimum": 1}, "max_queue_depth": map[string]any{"type": "integer", "minimum": 0}, "rejected": map[string]any{"type": "integer", "minimum": 0}, "queue_timeouts": map[string]any{"type": "integer", "minimum": 0}, "observed_at": map[string]any{"type": "string", "format": "date-time"}}},
		"EndpointMonitoring":              map[string]any{"type": "object", "additionalProperties": false, "required": []string{"endpoint", "logical_model", "environment", "window_start", "window_end", "bucket_seconds", "summary", "series", "breakdowns", "events", "evidence"}, "properties": map[string]any{"endpoint": map[string]any{"type": "string"}, "logical_model": map[string]any{"type": "string"}, "environment": map[string]any{"type": "string"}, "window_start": map[string]any{"type": "string", "format": "date-time"}, "window_end": map[string]any{"type": "string", "format": "date-time"}, "bucket_seconds": map[string]any{"type": "integer", "minimum": 60}, "summary": ref("MonitoringSummary"), "series": map[string]any{"type": "array", "maxItems": 500, "items": ref("MonitoringBucket")}, "breakdowns": map[string]any{"type": "array", "maxItems": 50, "items": ref("MonitoringBreakdown")}, "events": map[string]any{"type": "array", "maxItems": 200, "items": ref("MonitoringEvent")}, "evidence": ref("MonitoringEvidence"), "admission": ref("AdmissionMonitoring")}},
		"OperationalMeasurementValue":     map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "value", "unit", "sample_count"}, "properties": map[string]any{"name": map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9_]{0,63}$"}, "value": map[string]any{"type": "number"}, "unit": map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9_]{0,63}$"}, "sample_count": map[string]any{"type": "integer", "minimum": 1}}},
		"OperationalMeasurementIngest":    map[string]any{"type": "object", "additionalProperties": false, "required": []string{"source", "evidence_class", "observed_at", "valid_until", "measurements"}, "properties": map[string]any{"source": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "evidence_class": map[string]any{"type": "string", "enum": []string{"measured", "provider_reported"}}, "replica_id": map[string]any{"type": "string", "maxLength": 255}, "observed_at": map[string]any{"type": "string", "format": "date-time"}, "valid_until": map[string]any{"type": "string", "format": "date-time"}, "measurements": map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": ref("OperationalMeasurementValue")}}},
		"CostEvidenceValue":               map[string]any{"type": "object", "additionalProperties": false, "required": []string{"scope", "resource", "billing_unit", "amount", "window_start", "window_end"}, "properties": map[string]any{"scope": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "resource": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "billing_unit": map[string]any{"type": "string", "enum": []string{"hour"}}, "amount": map[string]any{"type": "number", "minimum": 0}, "window_start": map[string]any{"type": "string", "format": "date-time"}, "window_end": map[string]any{"type": "string", "format": "date-time"}}},
		"CostEvidenceIngest":              map[string]any{"type": "object", "additionalProperties": false, "required": []string{"source", "currency", "evidence_class", "observed_at", "valid_until", "allocations"}, "properties": map[string]any{"source": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "currency": map[string]any{"type": "string", "pattern": "^[A-Z]{3}$"}, "evidence_class": map[string]any{"type": "string", "enum": []string{"measured", "provider_reported"}}, "observed_at": map[string]any{"type": "string", "format": "date-time"}, "valid_until": map[string]any{"type": "string", "format": "date-time"}, "allocations": map[string]any{"type": "array", "minItems": 1, "maxItems": 128, "items": ref("CostEvidenceValue")}}},
		"AdoptionOwnership":               map[string]any{"type": "object", "additionalProperties": false, "required": []string{"ownership_mode"}, "properties": map[string]any{"ownership_mode": map[string]any{"type": "string", "enum": []string{"traffic-managed"}}}},
		"AlertPolicyCreate":               map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "webhook_url", "secret_reference_id"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "webhook_url": map[string]any{"type": "string", "format": "uri", "pattern": "^https://"}, "secret_reference_id": map[string]any{"type": "string"}, "minimum_severity": map[string]any{"type": "string", "enum": []string{"info", "warning", "critical"}}, "enabled": map[string]any{"type": "boolean"}, "max_attempts": map[string]any{"type": "integer", "minimum": 1, "maximum": 5}}},
		"AdmissionPolicy":                 map[string]any{"type": "object", "additionalProperties": false, "required": []string{"max_concurrency", "max_queue_depth", "queue_timeout_ms", "request_timeout_ms", "max_request_bytes", "max_output_tokens", "allowed_priorities", "retry_budget", "enabled"}, "properties": map[string]any{"max_concurrency": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}, "max_queue_depth": map[string]any{"type": "integer", "minimum": 0, "maximum": 100000}, "queue_timeout_ms": map[string]any{"type": "integer", "minimum": 1, "maximum": 300000}, "request_timeout_ms": map[string]any{"type": "integer", "minimum": 1000, "maximum": 3600000, "description": "Absolute end-to-end deadline from gateway arrival through admission, retries, and response streaming."}, "max_request_bytes": map[string]any{"type": "integer", "minimum": 1024, "maximum": 16777216}, "max_output_tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 1048576}, "allowed_priorities": map[string]any{"type": "array", "minItems": 1, "maxItems": 3, "uniqueItems": true, "items": map[string]any{"type": "string", "enum": []string{"low", "normal", "high"}}}, "retry_budget": map[string]any{"type": "integer", "minimum": 0, "maximum": 3}, "enabled": map[string]any{"type": "boolean"}}},
		"AsyncInferenceSubmit":            map[string]any{"type": "object", "additionalProperties": false, "required": []string{"input", "idempotency_key", "store_encrypted_content"}, "properties": map[string]any{"protocol": map[string]any{"type": "string", "enum": []string{"chat", "responses", "embeddings", "completions", "batch"}, "default": "chat"}, "input": map[string]any{"type": "object", "description": "Protocol-native request body; encrypted before persistence."}, "idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "priority": map[string]any{"type": "integer", "minimum": -100, "maximum": 100}, "execution_deadline_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400}, "retention_seconds": map[string]any{"type": "integer", "minimum": 2, "maximum": 604800}, "webhook_url": map[string]any{"type": "string", "format": "uri", "pattern": "^https://"}, "webhook_secret_reference_id": map[string]any{"type": "string"}, "store_encrypted_content": map[string]any{"type": "boolean", "const": true}}},
		"EndpointBindingCreate":           map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "kind", "ownership_mode"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "kind": map[string]any{"type": "string", "enum": []string{"deployment", "external"}}, "ownership_mode": map[string]any{"type": "string", "enum": []string{"observe-only", "traffic-managed", "lifecycle-managed"}}, "deployment": map[string]any{"type": "string"}, "target": map[string]any{"type": "string"}, "provider_connection": map[string]any{"type": "string"}, "config": stringMap}},
		"ServingPlanCreate":               map[string]any{"type": "object", "additionalProperties": false, "required": []string{"routing_policy", "bindings"}, "properties": map[string]any{"routing_policy": map[string]any{"type": "string", "enum": []string{"manual", "primary-fallback", "weighted"}}, "bindings": map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "priority", "weight"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "priority": map[string]any{"type": "integer", "minimum": 0}, "weight": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}}}}}},
		"EndpointReleaseGuardPolicy":      map[string]any{"type": "object", "additionalProperties": false, "required": []string{"enabled", "minimum_requests", "max_ttft_regression_percent", "max_latency_regression_percent", "max_error_rate_increase", "max_output_throughput_drop_percent", "require_compatibility_evidence"}, "properties": map[string]any{"enabled": map[string]any{"type": "boolean"}, "minimum_requests": map[string]any{"type": "integer", "minimum": 1, "maximum": 100000}, "max_ttft_regression_percent": map[string]any{"type": "number", "minimum": 0}, "max_latency_regression_percent": map[string]any{"type": "number", "minimum": 0}, "max_error_rate_increase": map[string]any{"type": "number", "minimum": 0, "maximum": 1}, "max_output_throughput_drop_percent": map[string]any{"type": "number", "minimum": 0}, "require_compatibility_evidence": map[string]any{"type": "boolean"}}},
		"ReleaseGuardPolicy":              map[string]any{"type": "object", "additionalProperties": false, "required": []string{"enabled", "minimum_requests", "require_compatibility_evidence", "require_synthetic_evidence", "require_quality_evidence"}, "properties": map[string]any{"enabled": map[string]any{"type": "boolean"}, "minimum_requests": map[string]any{"type": "integer", "minimum": 1}, "max_ttft_regression_percent": map[string]any{"type": "number", "minimum": 0}, "max_latency_regression_percent": map[string]any{"type": "number", "minimum": 0}, "max_error_rate_increase": map[string]any{"type": "number", "minimum": 0, "maximum": 1}, "max_output_throughput_drop_percent": map[string]any{"type": "number", "minimum": 0}, "require_compatibility_evidence": map[string]any{"type": "boolean"}, "require_synthetic_evidence": map[string]any{"type": "boolean"}, "max_cost_regression_percent": map[string]any{"type": "number", "minimum": 0}, "auto_rollback_enabled": map[string]any{"type": "boolean"}, "auto_rollback_window_seconds": map[string]any{"type": "integer", "minimum": 0}, "validation_max_requests": map[string]any{"type": "integer", "minimum": 1}, "validation_max_concurrency": map[string]any{"type": "integer", "minimum": 1}, "require_quality_evidence": map[string]any{"type": "boolean"}, "minimum_quality_score": map[string]any{"type": "number", "minimum": 0, "maximum": 1}, "max_quality_regression_percent": map[string]any{"type": "number", "minimum": 0, "maximum": 100}}},
		"EndpointGuardEvaluationRequest":  map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"window_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 2592000}}},
		"SignedEvidenceEnvelope":          map[string]any{"type": "object", "additionalProperties": false, "required": []string{"payload", "digest", "signature", "public_key", "algorithm", "key_id"}, "properties": map[string]any{"payload": map[string]any{"type": "string", "description": "Canonical aggregate evidence JSON. Prompt and output content are not accepted by the v1 schema."}, "digest": map[string]any{"type": "string"}, "signature": map[string]any{"type": "string"}, "public_key": map[string]any{"type": "string"}, "algorithm": map[string]any{"type": "string", "enum": []string{"Ed25519-SHA256"}}, "key_id": map[string]any{"type": "string"}}},
		"BenchmarkRequest": map[string]any{"type": "object", "required": []string{"requests", "concurrency"}, "properties": map[string]any{
			"requests": map[string]any{"type": "integer", "minimum": 1, "maximum": 100000}, "concurrency": map[string]any{"type": "integer", "minimum": 1}, "input_tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000000, "default": 128}, "output_tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000000, "default": 32}, "random_seed": map[string]any{"type": "integer", "default": 17}, "revision": map[string]any{"type": "string"}, "streaming": map[string]any{"type": "boolean", "default": true},
			"profile": map[string]any{"type": "string", "enum": []string{"balanced", "buffered", "interactive", "long-context", "long-generation", "overload", "throughput"}}, "profile_version": map[string]any{"type": "string", "enum": []string{"benchmark-profile-v1"}},
			"ttft_slo_ms": map[string]any{"type": "number", "minimum": 0}, "tpot_slo_ms": map[string]any{"type": "number", "minimum": 0}, "latency_slo_ms": map[string]any{"type": "number", "minimum": 0},
		}},
		"ReplayCaptureRequest":            map[string]any{"type": "object", "properties": map[string]any{"window_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 2592000, "default": 86400}, "max_requests": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000, "default": 1000}}},
		"ArtifactCacheObservationRequest": map[string]any{"type": "object", "required": []string{"provider", "location", "state", "source"}, "properties": map[string]any{"provider": map[string]any{"type": "string"}, "region": map[string]any{"type": "string"}, "location": map[string]any{"type": "string"}, "state": map[string]any{"type": "string", "enum": []string{"present", "prefetching", "missing", "unknown"}}, "source": map[string]any{"type": "string"}, "evidence": map[string]any{"type": "object"}, "ttl_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400, "default": 300}}},
		"ArtifactPrefetchRequest":         map[string]any{"type": "object", "required": []string{"provider", "location", "idempotency_key"}, "properties": map[string]any{"provider": map[string]any{"type": "string"}, "region": map[string]any{"type": "string"}, "location": map[string]any{"type": "string"}, "idempotency_key": map[string]any{"type": "string", "minLength": 1}}},
		"OptimizationProposalRequest": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"model_identity", "provider", "gpu"}, "properties": map[string]any{
			"model_identity": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "provider": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "region": map[string]any{"type": "string", "maxLength": 128}, "gpu": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "runtimes": map[string]any{"type": "array", "maxItems": 16, "uniqueItems": true, "items": map[string]any{"type": "string", "minLength": 1, "maxLength": 64}}, "objective": map[string]any{"type": "string", "enum": []string{"interactive", "latency", "throughput", "cost-efficiency"}, "default": "interactive"}, "workload_profile": map[string]any{"type": "string", "enum": []string{"balanced", "buffered", "interactive", "long-context", "long-generation", "overload", "throughput"}}, "max_ttft_p95_ms": map[string]any{"type": "number", "minimum": 0}, "max_tpot_p95_ms": map[string]any{"type": "number", "minimum": 0}, "max_error_rate": map[string]any{"type": "number", "minimum": 0, "maximum": 1}, "min_goodput": map[string]any{"type": "number", "minimum": 0}, "min_output_tokens_second": map[string]any{"type": "number", "minimum": 0}, "max_hourly_cost": map[string]any{"type": "number", "minimum": 0}, "include_simulated": map[string]any{"type": "boolean", "default": false}, "workload_fingerprint": map[string]any{"type": "string", "maxLength": 256}, "target_concurrency": map[string]any{"type": "number", "exclusiveMinimum": 0}, "max_candidates": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 10},
		}},
		"OptimizationCampaignCreate":     map[string]any{"type": "object", "additionalProperties": false, "required": []string{"proposal"}, "properties": map[string]any{"proposal": map[string]any{"type": "object", "description": "A complete infercrane.optimizer.proposal/v1 document whose normalized digest is revalidated by the server."}, "intent": map[string]any{"type": "string", "enum": []string{"new_endpoint", "evolve_endpoint"}, "default": "new_endpoint"}, "target_deployment": map[string]any{"type": "string", "description": "Required only for evolve_endpoint campaigns; names the stable deployment whose candidate revision will be compared."}}},
		"OptimizationCampaignApproval":   map[string]any{"type": "object", "additionalProperties": false, "required": []string{"max_cost_usd", "expires_in_seconds"}, "properties": map[string]any{"max_cost_usd": map[string]any{"type": "number", "exclusiveMinimum": 0, "maximum": 1000000}, "expires_in_seconds": map[string]any{"type": "integer", "minimum": 60, "maximum": 86400}}},
		"OptimizationCampaignActivation": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"candidate_id"}, "properties": map[string]any{"candidate_id": map[string]any{"type": "string", "minLength": 1}}},
		"OptimizedArtifactPlan": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"base_model_artifact_id", "kind", "format", "tool", "tool_version", "algorithm", "builder_image_digest", "license_spdx", "configuration", "hardware_constraints", "requires_quality_review"}, "properties": map[string]any{
			"base_model_artifact_id": map[string]any{"type": "string", "minLength": 1}, "kind": map[string]any{"type": "string", "enum": []string{"quantized_checkpoint", "speculator_checkpoint", "tensorrt_engine"}}, "format": map[string]any{"type": "string", "minLength": 1}, "tool": map[string]any{"type": "string", "enum": []string{"llm-compressor", "modelopt", "vllm-speculators", "tensorrt-llm"}}, "tool_version": map[string]any{"type": "string", "minLength": 1}, "algorithm": map[string]any{"type": "string", "minLength": 1}, "builder_image_digest": map[string]any{"type": "string", "pattern": "^sha256:[a-fA-F0-9]{64}$"}, "calibration_digest": map[string]any{"type": "string", "pattern": "^sha256:[a-fA-F0-9]{64}$"}, "license_spdx": map[string]any{"type": "string", "minLength": 1}, "configuration": map[string]any{"type": "object"}, "hardware_constraints": map[string]any{"type": "object"}, "requires_quality_review": map[string]any{"type": "boolean", "const": true},
		}},
		"OptimizedArtifactAttestation":   map[string]any{"type": "object", "additionalProperties": false, "required": []string{"state", "attestation"}, "properties": map[string]any{"state": map[string]any{"type": "string", "enum": []string{"ready", "failed"}}, "attestation": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"build_evidence"}, "properties": map[string]any{"output_repository": map[string]any{"type": "string"}, "output_immutable_revision": map[string]any{"type": "string"}, "output_digest": map[string]any{"type": "string", "pattern": "^sha256:[a-fA-F0-9]{64}$"}, "build_evidence": map[string]any{"type": "object"}, "failure_code": map[string]any{"type": "string"}}}}},
		"OptimizedArtifactQualification": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"candidate_run_id", "quality_evidence_id"}, "properties": map[string]any{"candidate_run_id": map[string]any{"type": "string", "minLength": 1}, "quality_evidence_id": map[string]any{"type": "string", "minLength": 1}}},
		"SandboxReferenceCreate":         map[string]any{"type": "object", "additionalProperties": false, "required": []string{"provider", "external_id", "endpoint"}, "properties": map[string]any{"provider": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "external_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "external_revision": map[string]any{"type": "string", "maxLength": 256}, "endpoint": map[string]any{"type": "string", "minLength": 1, "maxLength": 256}, "ttl_seconds": map[string]any{"type": "integer", "minimum": 60, "maximum": 86400, "default": 1800}, "metadata": map[string]any{"type": "object", "description": "Bounded content-free labels. Commands, files, prompts, outputs, credentials, and secrets are rejected."}}},
		"FinOpsReportRequest":            map[string]any{"type": "object", "properties": map[string]any{"window_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 31536000, "default": 2592000}}},
		"AutopilotPlanRequest":           map[string]any{"type": "object", "required": []string{"objective"}, "properties": map[string]any{"objective": map[string]any{"type": "string", "enum": []string{"minimize_cost"}}}},
		"ContextPassportRequest":         map[string]any{"type": "object", "required": []string{"deployment"}, "properties": map[string]any{"deployment": map[string]any{"type": "string", "minLength": 1}, "ttl_seconds": map[string]any{"type": "integer", "minimum": 60, "maximum": 2592000, "default": 3600}, "preferred_binding_id": map[string]any{"type": "string"}, "preferred_target_id": map[string]any{"type": "string"}, "cache_hints": map[string]any{"type": "object"}, "metadata": map[string]any{"type": "object"}}},
		"BurstGuardEvaluationRequest":    map[string]any{"type": "object", "required": []string{"max_incremental_cost_microusd_hour", "observed_at"}, "properties": map[string]any{"enabled": map[string]any{"type": "boolean"}, "queue_threshold": map[string]any{"type": "integer", "minimum": 0}, "breach_intervals": map[string]any{"type": "integer", "minimum": 1}, "recovery_intervals": map[string]any{"type": "integer", "minimum": 1}, "cooldown_seconds": map[string]any{"type": "integer", "minimum": 1}, "signal_max_age_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 300}, "max_incremental_cost_microusd_hour": map[string]any{"type": "integer", "minimum": 1}, "queue_depth": map[string]any{"type": "integer", "minimum": 0}, "incremental_cost_microusd_hour": map[string]any{"type": "integer", "minimum": 0}, "external_healthy": map[string]any{"type": "boolean"}, "observed_at": map[string]any{"type": "string", "format": "date-time"}}},
		"SLOPolicy":                      map[string]any{"type": "object", "minProperties": 1, "additionalProperties": false, "properties": map[string]any{"max_ttft_p95_ms": map[string]any{"type": "number", "minimum": 0}, "max_latency_p95_ms": map[string]any{"type": "number", "minimum": 0}, "max_error_rate": map[string]any{"type": "number", "minimum": 0, "maximum": 1}, "min_output_tokens_second": map[string]any{"type": "number", "minimum": 0}, "max_hourly_cost": map[string]any{"type": "number", "minimum": 0}}},
		"SLOPolicyEnvelope":              map[string]any{"type": "object", "required": []string{"policy"}, "properties": map[string]any{"policy": ref("SLOPolicy")}},
		"RecommendationRequest":          map[string]any{"type": "object", "maxProperties": 0, "additionalProperties": false},
		"RecommendationCandidate":        map[string]any{"type": "object", "required": []string{"evidence_id", "configuration", "eligible"}, "properties": map[string]any{"evidence_id": map[string]any{"type": "string"}, "configuration": map[string]any{"type": "string"}, "qualification_state": map[string]any{"type": "string"}, "capacity_state": map[string]any{"type": "string", "enum": []string{"available", "constrained", "unavailable", "unknown"}}, "capacity_source": map[string]any{"type": "string"}, "capacity_observed_at": map[string]any{"type": "string", "format": "date-time"}, "capacity_expires_at": map[string]any{"type": "string", "format": "date-time"}, "eligible": map[string]any{"type": "boolean"}, "missing": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "violations": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "disclosures": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "score": map[string]any{"type": "number"}}},
		"Recommendation":                 map[string]any{"type": "object", "required": []string{"id", "status", "algorithm_version", "reason", "input_snapshot", "input_digest", "created_at"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "status": map[string]any{"type": "string", "enum": []string{"recommended", "no_match", "unknown"}}, "algorithm_version": map[string]any{"type": "string"}, "selected_evidence_id": map[string]any{"type": "string"}, "reason": map[string]any{"type": "string"}, "missing": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "candidates": map[string]any{"type": "array", "items": ref("RecommendationCandidate")}, "input_snapshot": map[string]any{"type": "object", "additionalProperties": true}, "input_digest": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}, "created_at": map[string]any{"type": "string", "format": "date-time"}}},
		"RecommendationEnvelope":         map[string]any{"type": "object", "required": []string{"recommendation"}, "properties": map[string]any{"recommendation": ref("Recommendation")}},
		"RecipeCaptureRequest":           map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "version"}, "properties": map[string]any{"name": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}, "version": map[string]any{"type": "string", "minLength": 1, "maxLength": 64}, "benchmark_id": map[string]any{"type": "string"}}},
		"LabEvaluationRequest":           map[string]any{"type": "object", "additionalProperties": false, "required": []string{"model_identity"}, "properties": map[string]any{"model_identity": map[string]any{"type": "string", "minLength": 1}, "objective": map[string]any{"type": "string", "default": "latency", "enum": []string{"interactive", "latency", "throughput", "cost-efficiency"}}, "workload_profile": map[string]any{"type": "string", "enum": []string{"balanced", "buffered", "interactive", "long-context", "long-generation", "overload", "throughput"}}, "max_ttft_p95_ms": map[string]any{"type": "number", "minimum": 0}, "max_tpot_p95_ms": map[string]any{"type": "number", "minimum": 0}, "max_error_rate": map[string]any{"type": "number", "minimum": 0, "maximum": 1}, "min_goodput": map[string]any{"type": "number", "minimum": 0}, "min_output_tokens_second": map[string]any{"type": "number", "minimum": 0}, "max_hourly_cost": map[string]any{"type": "number", "minimum": 0}, "region": map[string]any{"type": "string", "maxLength": 128}, "max_gpu_count": map[string]any{"type": "integer", "minimum": 1, "maximum": 1024}, "workload_digest": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}}},
		"ExternalPolicyRequest":          map[string]any{"type": "object", "required": []string{"target", "adapter", "secret_reference_id", "enabled", "privacy_acknowledged", "request_limit", "cost_limit_microusd", "max_request_cost_microusd"}, "additionalProperties": false, "properties": map[string]any{"target": map[string]any{"type": "string", "minLength": 1}, "adapter": map[string]any{"type": "string", "enum": []string{"openrouter", "openai-compatible-external", "modal", "runpod-serverless-api", "fly-io"}}, "secret_reference_id": map[string]any{"type": "string", "minLength": 1}, "enabled": map[string]any{"type": "boolean"}, "privacy_acknowledged": map[string]any{"type": "boolean"}, "request_limit": map[string]any{"type": "integer", "minimum": 1}, "cost_limit_microusd": map[string]any{"type": "integer", "minimum": 1}, "max_request_cost_microusd": map[string]any{"type": "integer", "minimum": 1}, "billing_mode": map[string]any{"type": "string", "enum": []string{"byoc", "customer_wallet"}}, "input_microusd_per_mtok": map[string]any{"type": "integer", "minimum": 0}, "output_microusd_per_mtok": map[string]any{"type": "integer", "minimum": 0}, "cost_basis_input_microusd_per_mtok": map[string]any{"type": "integer", "minimum": 0}, "cost_basis_output_microusd_per_mtok": map[string]any{"type": "integer", "minimum": 0}, "minimum_gross_margin_bps": map[string]any{"type": "integer", "minimum": 1500, "maximum": 9999}, "cost_basis_provenance": map[string]any{"type": "string", "minLength": 1}, "rate_card_valid_until": map[string]any{"type": "string", "format": "date-time"}, "overflow_mode": map[string]any{"type": "string", "enum": []string{"health", "health_and_queue"}, "default": "health"}, "queue_threshold": map[string]any{"type": "number", "exclusiveMinimum": 0}, "breach_intervals": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 1}, "recovery_intervals": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 2}, "cooldown_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 86400, "default": 60}, "signal_max_age_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 600, "default": 30}}},
		"ExternalPolicy":                 map[string]any{"type": "object", "required": []string{"id", "deployment_id", "target_id", "adapter", "enabled", "privacy_acknowledged", "request_limit", "cost_limit_microusd", "max_request_cost_microusd", "overflow_mode"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "deployment_id": map[string]any{"type": "string"}, "target_id": map[string]any{"type": "string"}, "adapter": map[string]any{"type": "string"}, "secret_reference_id": map[string]any{"type": "string"}, "enabled": map[string]any{"type": "boolean"}, "privacy_acknowledged": map[string]any{"type": "boolean"}, "request_limit": map[string]any{"type": "integer"}, "requests_reserved": map[string]any{"type": "integer"}, "cost_limit_microusd": map[string]any{"type": "integer"}, "max_request_cost_microusd": map[string]any{"type": "integer"}, "cost_reserved_microusd": map[string]any{"type": "integer"}, "overflow_mode": map[string]any{"type": "string", "enum": []string{"health", "health_and_queue"}}, "queue_threshold": map[string]any{"type": "number"}, "breach_intervals": map[string]any{"type": "integer"}, "recovery_intervals": map[string]any{"type": "integer"}, "cooldown_seconds": map[string]any{"type": "integer"}, "signal_max_age_seconds": map[string]any{"type": "integer"}, "created_at": map[string]any{"type": "string", "format": "date-time"}, "updated_at": map[string]any{"type": "string", "format": "date-time"}}},
		"ExternalPolicyEnvelope":         map[string]any{"type": "object", "required": []string{"policy"}, "properties": map[string]any{"policy": ref("ExternalPolicy")}},
		"ChatCompletionRequest":          map[string]any{"type": "object", "required": []string{"model", "messages"}, "properties": map[string]any{"model": map[string]any{"type": "string", "description": "Stable InferCrane endpoint name; migrated v1 deployment aliases remain compatible endpoints."}, "messages": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "required": []string{"role", "content"}, "properties": map[string]any{"role": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}}}, "stream": map[string]any{"type": "boolean", "default": false}}, "additionalProperties": true},
		"Empty":                          map[string]any{"type": "null"},
	}
}

func nullableNumberSchema() map[string]any {
	return map[string]any{"type": []string{"number", "null"}}
}

func measurementEvidenceSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"name", "value", "unit", "availability", "evidence_class", "source", "observed_at", "fresh_until", "sample_count"},
		"properties": map[string]any{
			"name": map[string]any{"type": "string"}, "value": nullableNumberSchema(), "unit": map[string]any{"type": "string"},
			"availability":   map[string]any{"type": "string", "enum": []string{"available", "unavailable", "stale", "not_observed", "unsupported"}},
			"evidence_class": map[string]any{"type": "string", "enum": []string{"measured", "provider_reported", "modeled", "estimated"}},
			"source":         map[string]any{"type": "string"}, "observed_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"fresh_until": map[string]any{"type": []string{"string", "null"}, "format": "date-time"}, "sample_count": map[string]any{"type": "integer", "minimum": 0},
			"reason": map[string]any{"type": "string"},
		},
	}
}

func monitoringMetricProperties() map[string]any {
	return map[string]any{
		"requests": map[string]any{"type": "integer", "minimum": 0}, "errors": map[string]any{"type": "integer", "minimum": 0},
		"fallbacks": map[string]any{"type": "integer", "minimum": 0}, "retried": map[string]any{"type": "integer", "minimum": 0},
		"streaming": map[string]any{"type": "integer", "minimum": 0}, "token_usage_samples": map[string]any{"type": "integer", "minimum": 0},
		"input_token_samples": map[string]any{"type": "integer", "minimum": 0}, "output_token_samples": map[string]any{"type": "integer", "minimum": 0},
		"input_tokens": map[string]any{"type": "integer", "minimum": 0}, "output_tokens": map[string]any{"type": "integer", "minimum": 0},
		"requests_per_second": map[string]any{"type": "number", "minimum": 0}, "input_tokens_per_second": nullableNumberSchema(),
		"output_tokens_per_second": nullableNumberSchema(), "error_rate": nullableNumberSchema(), "fallback_rate": nullableNumberSchema(),
		"p50_latency_ms": nullableNumberSchema(), "p95_latency_ms": nullableNumberSchema(), "p50_ttft_ms": nullableNumberSchema(),
		"p95_ttft_ms": nullableNumberSchema(), "p95_queue_ms": nullableNumberSchema(), "p95_generation_ms": nullableNumberSchema(),
	}
}

func monitoringSummarySchema() map[string]any {
	properties := monitoringMetricProperties()
	properties["retry_rate"] = nullableNumberSchema()
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
}

func monitoringBucketSchema() map[string]any {
	properties := monitoringMetricProperties()
	properties["started_at"] = map[string]any{"type": "string", "format": "date-time"}
	return map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
}

func monitoringBreakdownSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"binding": map[string]any{"type": "string"}, "deployment": map[string]any{"type": "string"}, "revision": map[string]any{"type": "string"},
		"provider": map[string]any{"type": "string"}, "runtime": map[string]any{"type": "string"}, "requests": map[string]any{"type": "integer", "minimum": 0},
		"errors": map[string]any{"type": "integer", "minimum": 0}, "fallbacks": map[string]any{"type": "integer", "minimum": 0},
		"error_rate": nullableNumberSchema(), "p95_latency_ms": nullableNumberSchema(), "p95_ttft_ms": nullableNumberSchema(),
		"last_seen_at": map[string]any{"type": "string", "format": "date-time"},
	}}
}

func monitoringEventSchema() map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
		"kind": map[string]any{"type": "string"}, "type": map[string]any{"type": "string"}, "summary": map[string]any{"type": "string"},
		"details": map[string]any{"type": "object", "additionalProperties": true}, "occurred_at": map[string]any{"type": "string", "format": "date-time"},
	}}
}

func monitoringEvidenceSchema() map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required": []string{"source", "semantic_convention_schema", "sample_count", "latest_request_at", "fresh", "content_recorded", "available", "unavailable", "measurements"},
		"properties": map[string]any{
			"source": map[string]any{"type": "string"}, "semantic_convention_schema": map[string]any{"type": "string", "format": "uri"},
			"sample_count": map[string]any{"type": "integer", "minimum": 0}, "latest_request_at": map[string]any{"type": []string{"string", "null"}, "format": "date-time"},
			"fresh": map[string]any{"type": "boolean"}, "content_recorded": map[string]any{"type": "boolean", "const": false},
			"available":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"unavailable":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"measurements": map[string]any{"type": "array", "maxItems": 32, "items": ref("MeasurementEvidence")},
		},
	}
}
