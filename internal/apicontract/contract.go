// Package apicontract owns the public control-plane API description used by
// documentation, generated clients, and route-drift qualification.
package apicontract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const Version = "1.4.0"

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
	{"GET", "/doctor", "getDoctor", "System", "Run control-plane diagnostics", "", "Object", 200, false},
	{"GET", "/whoami", "getCurrentPrincipal", "Identity", "Inspect the authenticated principal", "", "Object", 200, false},
	{"GET", "/integrations", "listIntegrations", "System", "Inspect registered integration capabilities", "", "Object", 200, false},
	{"GET", "/environments", "listEnvironments", "Endpoints", "List endpoint environments", "", "ObjectList", 200, false},
	{"POST", "/environments", "createEnvironment", "Endpoints", "Create an endpoint environment", "EnvironmentCreate", "Object", 201, false},
	{"GET", "/logical-models", "listLogicalModels", "Endpoints", "List logical model identities", "", "ObjectList", 200, false},
	{"POST", "/logical-models", "createLogicalModel", "Endpoints", "Create a logical model identity", "LogicalModelCreate", "Object", 201, false},
	{"GET", "/endpoints", "listEndpoints", "Endpoints", "List stable application endpoints", "", "ObjectList", 200, false},
	{"POST", "/endpoints", "createEndpoint", "Endpoints", "Create a stable application endpoint", "EndpointCreate", "Object", 201, false},
	{"POST", "/adoptions/endpoints", "adoptEndpoint", "Adoption", "Adopt an existing inference endpoint without transferring lifecycle ownership", "EndpointAdoption", "Object", 201, false},
	{"PUT", "/adoptions/endpoints/{name}/ownership", "promoteAdoptionOwnership", "Adoption", "Explicitly promote an adopted endpoint to traffic-managed ownership", "AdoptionOwnership", "Object", 200, false},
	{"GET", "/requests/{id}", "inspectRequest", "Diagnostics", "Inspect persisted request metadata without content", "", "Object", 200, false},
	{"GET", "/endpoints/{name}", "getEndpoint", "Endpoints", "Inspect an endpoint and its serving plans", "", "Object", 200, false},
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
	{"GET", "/deployments/{name}/events", "listDeploymentEvents", "Deployments", "List deployment events", "", "ObjectList", 200, false},
	{"POST", "/deployments/{name}/benchmarks", "runBenchmark", "Benchmarks", "Run and persist an AIPerf benchmark", "BenchmarkRequest", "Object", 201, false},
	{"GET", "/deployments/{name}/benchmarks", "listBenchmarks", "Benchmarks", "List benchmark history", "", "ObjectList", 200, false},
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
	{"PUT", "/deployments/{name}/release-guard/policy", "setReleaseGuardPolicy", "Release Guard", "Set the persisted guard policy", "Object", "Object", 200, false},
	{"POST", "/deployments/{name}/rollouts/{revision}/promote", "promoteRollout", "Revisions", "Promote a validated candidate", "", "OperationEnvelope", 202, true},
	{"POST", "/deployments/{name}/rollouts/{revision}/provision", "provisionRollout", "Revisions", "Provision candidate capacity", "", "OperationEnvelope", 202, true},
	{"POST", "/deployments/{name}/rollouts/{revision}/reject", "rejectRollout", "Revisions", "Reject a candidate revision", "Object", "OperationEnvelope", 202, true},
	{"POST", "/deployments/{name}/rollback", "rollbackDeployment", "Revisions", "Roll back to a prior revision", "Object", "OperationEnvelope", 202, true},
	{"GET", "/deployments/{name}/scaling-decisions", "listScalingDecisions", "Scaling", "List persisted scaling decisions", "", "ObjectList", 200, false},
	{"PUT", "/deployments/{name}/route", "setDeploymentRoute", "Routing", "Set the active route target", "Object", "Object", 200, false},
	{"GET", "/targets", "listTargets", "Targets", "List inference targets", "", "ObjectList", 200, false},
	{"POST", "/targets", "createTarget", "Targets", "Register an inference target", "TargetCreate", "Object", 201, false},
	{"GET", "/orphans", "listOrphans", "Infrastructure", "List orphaned provider resources", "", "ObjectList", 200, false},
	{"GET", "/audit-events", "listAuditEvents", "Audit", "List tenant audit events", "", "ObjectList", 200, false},
	{"PUT", "/tenant/quota", "setTenantQuota", "Identity", "Set tenant safety quotas", "Object", "Empty", 204, false},
	{"POST", "/tenants", "createTenant", "Identity", "Create a tenant", "Object", "Object", 201, false},
	{"POST", "/principals", "createPrincipal", "Identity", "Create a scoped service account", "Object", "Object", 201, false},
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
		parameters := pathParameters(route.Path)
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
		"Object":                         stringMap,
		"ObjectList":                     map[string]any{"type": "object", "required": []string{"data"}, "properties": map[string]any{"data": map[string]any{"type": "array", "items": stringMap}}},
		"Error":                          map[string]any{"type": "object", "required": []string{"code", "message"}, "properties": map[string]any{"code": map[string]any{"type": "string"}, "category": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string"}, "retryable": map[string]any{"type": "boolean"}, "remediation": map[string]any{"type": "string"}}},
		"ErrorEnvelope":                  map[string]any{"type": "object", "required": []string{"error"}, "properties": map[string]any{"error": ref("Error")}},
		"Operation":                      map[string]any{"type": "object", "required": []string{"id", "kind", "status", "progress"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "kind": map[string]any{"type": "string"}, "resource_type": map[string]any{"type": "string"}, "resource_name": map[string]any{"type": "string"}, "status": map[string]any{"type": "string", "enum": []string{"pending", "leased", "running", "waiting", "cancelling", "succeeded", "failed", "cancelled"}}, "progress": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}, "message": map[string]any{"type": "string"}, "error_code": map[string]any{"type": "string"}, "retryable": map[string]any{"type": "boolean"}, "cancel_requested": map[string]any{"type": "boolean"}, "attempt": map[string]any{"type": "integer"}, "max_attempts": map[string]any{"type": "integer"}, "created_at": map[string]any{"type": "string", "format": "date-time"}, "updated_at": map[string]any{"type": "string", "format": "date-time"}}},
		"OperationEnvelope":              map[string]any{"type": "object", "required": []string{"operation"}, "properties": map[string]any{"operation": ref("Operation"), "created": map[string]any{"type": "boolean"}}},
		"DeploymentOperationEnvelope":    map[string]any{"allOf": []map[string]any{ref("OperationEnvelope"), {"type": "object", "properties": map[string]any{"deployment": ref("Deployment")}}}},
		"Deployment":                     map[string]any{"type": "object", "required": []string{"name", "model", "runtime"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "runtime": map[string]any{"type": "string"}, "observed_state": map[string]any{"type": "string"}, "min_replicas": map[string]any{"type": "integer"}, "max_replicas": map[string]any{"type": "integer"}, "active_revision_id": map[string]any{"type": "string"}, "candidate_revision_id": map[string]any{"type": "string"}}},
		"DeploymentList":                 map[string]any{"type": "object", "required": []string{"data"}, "properties": map[string]any{"data": map[string]any{"type": "array", "items": ref("Deployment")}}},
		"DeploymentView":                 map[string]any{"type": "object", "required": []string{"deployment", "lifecycle_status"}, "properties": map[string]any{"deployment": ref("Deployment"), "lifecycle_status": stringMap, "active_operation": ref("Operation"), "targets": map[string]any{"type": "array", "items": stringMap}, "replicas": map[string]any{"type": "array", "items": stringMap}, "revisions": map[string]any{"type": "array", "items": stringMap}}},
		"RuntimeWorkload":                map[string]any{"type": "object", "required": []string{"image", "command", "protocol", "port", "readiness_path", "models_path", "metrics_path", "cancellation", "drain", "shutdown_grace_seconds"}, "properties": map[string]any{"image": map[string]any{"type": "string", "pattern": `@sha256:[a-f0-9]{64}$`}, "command": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "string"}}, "protocol": map[string]any{"type": "string", "enum": []string{"openai"}}, "port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}, "readiness_path": map[string]any{"type": "string", "enum": []string{"/health"}}, "models_path": map[string]any{"type": "string", "enum": []string{"/v1/models"}}, "metrics_path": map[string]any{"type": "string", "enum": []string{"/metrics"}}, "cancellation": map[string]any{"type": "string", "enum": []string{"http-disconnect"}}, "drain": map[string]any{"type": "string", "enum": []string{"connection"}}, "shutdown_grace_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 3600}}, "additionalProperties": false},
		"DeploymentCreate":               map[string]any{"type": "object", "required": []string{"name", "model"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "runtime": map[string]any{"type": "string", "default": "vllm", "enum": []string{"vllm", "sglang", "custom-oci"}}, "cloud": map[string]any{"type": "string"}, "compute_mode": map[string]any{"type": "string", "enum": []string{"elastic", "serverless"}}, "gpu": map[string]any{"type": "string"}, "region": map[string]any{"type": "string"}, "port": map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}, "min_replicas": map[string]any{"type": "integer", "minimum": 0}, "max_replicas": map[string]any{"type": "integer", "minimum": 0}, "runtime_version": map[string]any{"type": "string"}, "runtime_args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "workload": ref("RuntimeWorkload"), "model_revision": map[string]any{"type": "string"}}},
		"DeploymentSpec":                 stringMap,
		"TargetCreate":                   map[string]any{"type": "object", "required": []string{"name", "url", "provider", "runtime"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "url": map[string]any{"type": "string", "format": "uri"}, "provider": map[string]any{"type": "string"}, "runtime": map[string]any{"type": "string"}, "upstream_model": map[string]any{"type": "string"}}},
		"EnvironmentCreate":              map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "policy": stringMap}},
		"LogicalModelCreate":             map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "description": map[string]any{"type": "string", "maxLength": 4096}}},
		"EndpointCreate":                 map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "logical_model", "environment"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "logical_model": map[string]any{"type": "string"}, "environment": map[string]any{"type": "string"}}},
		"EndpointAdoption":               map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "logical_model", "url", "source", "ownership_mode", "runtime"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "logical_model": map[string]any{"type": "string"}, "upstream_model": map[string]any{"type": "string"}, "url": map[string]any{"type": "string", "format": "uri"}, "source": map[string]any{"type": "string", "enum": []string{"vllm", "openai-compatible"}}, "ownership_mode": map[string]any{"type": "string", "enum": []string{"observe-only", "traffic-managed"}}, "runtime": map[string]any{"type": "string"}}},
		"AdoptionOwnership":              map[string]any{"type": "object", "additionalProperties": false, "required": []string{"ownership_mode"}, "properties": map[string]any{"ownership_mode": map[string]any{"type": "string", "enum": []string{"traffic-managed"}}}},
		"AlertPolicyCreate":              map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "webhook_url", "secret_reference_id"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "webhook_url": map[string]any{"type": "string", "format": "uri", "pattern": "^https://"}, "secret_reference_id": map[string]any{"type": "string"}, "minimum_severity": map[string]any{"type": "string", "enum": []string{"info", "warning", "critical"}}, "enabled": map[string]any{"type": "boolean"}, "max_attempts": map[string]any{"type": "integer", "minimum": 1, "maximum": 5}}},
		"AdmissionPolicy":                map[string]any{"type": "object", "additionalProperties": false, "required": []string{"max_concurrency", "max_queue_depth", "queue_timeout_ms", "max_request_bytes", "max_output_tokens", "allowed_priorities", "retry_budget", "enabled"}, "properties": map[string]any{"max_concurrency": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}, "max_queue_depth": map[string]any{"type": "integer", "minimum": 0, "maximum": 100000}, "queue_timeout_ms": map[string]any{"type": "integer", "minimum": 1, "maximum": 300000}, "max_request_bytes": map[string]any{"type": "integer", "minimum": 1024, "maximum": 16777216}, "max_output_tokens": map[string]any{"type": "integer", "minimum": 1, "maximum": 1048576}, "allowed_priorities": map[string]any{"type": "array", "minItems": 1, "maxItems": 3, "uniqueItems": true, "items": map[string]any{"type": "string", "enum": []string{"low", "normal", "high"}}}, "retry_budget": map[string]any{"type": "integer", "minimum": 0, "maximum": 3}, "enabled": map[string]any{"type": "boolean"}}},
		"AsyncInferenceSubmit":           map[string]any{"type": "object", "additionalProperties": false, "required": []string{"input", "idempotency_key", "store_encrypted_content"}, "properties": map[string]any{"protocol": map[string]any{"type": "string", "enum": []string{"chat", "responses", "embeddings", "completions", "batch"}, "default": "chat"}, "input": map[string]any{"type": "object", "description": "Protocol-native request body; encrypted before persistence."}, "idempotency_key": map[string]any{"type": "string", "minLength": 1, "maxLength": 255}, "priority": map[string]any{"type": "integer", "minimum": -100, "maximum": 100}, "execution_deadline_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 86400}, "retention_seconds": map[string]any{"type": "integer", "minimum": 2, "maximum": 604800}, "webhook_url": map[string]any{"type": "string", "format": "uri", "pattern": "^https://"}, "webhook_secret_reference_id": map[string]any{"type": "string"}, "store_encrypted_content": map[string]any{"type": "boolean", "const": true}}},
		"EndpointBindingCreate":          map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "kind", "ownership_mode"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "kind": map[string]any{"type": "string", "enum": []string{"deployment", "external"}}, "ownership_mode": map[string]any{"type": "string", "enum": []string{"observe-only", "traffic-managed", "lifecycle-managed"}}, "deployment": map[string]any{"type": "string"}, "target": map[string]any{"type": "string"}, "config": stringMap}},
		"ServingPlanCreate":              map[string]any{"type": "object", "additionalProperties": false, "required": []string{"routing_policy", "bindings"}, "properties": map[string]any{"routing_policy": map[string]any{"type": "string", "enum": []string{"manual", "primary-fallback", "weighted"}}, "bindings": map[string]any{"type": "array", "minItems": 1, "maxItems": 32, "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"name", "priority", "weight"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "priority": map[string]any{"type": "integer", "minimum": 0}, "weight": map[string]any{"type": "integer", "minimum": 1, "maximum": 10000}}}}}},
		"EndpointReleaseGuardPolicy":     map[string]any{"type": "object", "additionalProperties": false, "required": []string{"enabled", "minimum_requests", "max_ttft_regression_percent", "max_latency_regression_percent", "max_error_rate_increase", "max_output_throughput_drop_percent", "require_compatibility_evidence"}, "properties": map[string]any{"enabled": map[string]any{"type": "boolean"}, "minimum_requests": map[string]any{"type": "integer", "minimum": 1, "maximum": 100000}, "max_ttft_regression_percent": map[string]any{"type": "number", "minimum": 0}, "max_latency_regression_percent": map[string]any{"type": "number", "minimum": 0}, "max_error_rate_increase": map[string]any{"type": "number", "minimum": 0, "maximum": 1}, "max_output_throughput_drop_percent": map[string]any{"type": "number", "minimum": 0}, "require_compatibility_evidence": map[string]any{"type": "boolean"}}},
		"EndpointGuardEvaluationRequest": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"window_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 2592000}}},
		"BenchmarkRequest":               map[string]any{"type": "object", "required": []string{"requests", "concurrency"}, "properties": map[string]any{"requests": map[string]any{"type": "integer", "minimum": 1, "maximum": 100000}, "concurrency": map[string]any{"type": "integer", "minimum": 1}, "random_seed": map[string]any{"type": "integer", "default": 17}, "revision": map[string]any{"type": "string"}}},
		"SLOPolicy":                      map[string]any{"type": "object", "minProperties": 1, "additionalProperties": false, "properties": map[string]any{"max_ttft_p95_ms": map[string]any{"type": "number", "minimum": 0}, "max_latency_p95_ms": map[string]any{"type": "number", "minimum": 0}, "max_error_rate": map[string]any{"type": "number", "minimum": 0, "maximum": 1}, "min_output_tokens_second": map[string]any{"type": "number", "minimum": 0}, "max_hourly_cost": map[string]any{"type": "number", "minimum": 0}}},
		"SLOPolicyEnvelope":              map[string]any{"type": "object", "required": []string{"policy"}, "properties": map[string]any{"policy": ref("SLOPolicy")}},
		"RecommendationRequest":          map[string]any{"type": "object", "maxProperties": 0, "additionalProperties": false},
		"RecommendationCandidate":        map[string]any{"type": "object", "required": []string{"evidence_id", "configuration", "eligible"}, "properties": map[string]any{"evidence_id": map[string]any{"type": "string"}, "configuration": map[string]any{"type": "string"}, "qualification_state": map[string]any{"type": "string"}, "capacity_state": map[string]any{"type": "string", "enum": []string{"available", "constrained", "unavailable", "unknown"}}, "capacity_source": map[string]any{"type": "string"}, "capacity_observed_at": map[string]any{"type": "string", "format": "date-time"}, "capacity_expires_at": map[string]any{"type": "string", "format": "date-time"}, "eligible": map[string]any{"type": "boolean"}, "missing": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "violations": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "disclosures": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "score": map[string]any{"type": "number"}}},
		"Recommendation":                 map[string]any{"type": "object", "required": []string{"id", "status", "algorithm_version", "reason", "input_snapshot", "input_digest", "created_at"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "status": map[string]any{"type": "string", "enum": []string{"recommended", "no_match", "unknown"}}, "algorithm_version": map[string]any{"type": "string"}, "selected_evidence_id": map[string]any{"type": "string"}, "reason": map[string]any{"type": "string"}, "missing": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "candidates": map[string]any{"type": "array", "items": ref("RecommendationCandidate")}, "input_snapshot": map[string]any{"type": "object", "additionalProperties": true}, "input_digest": map[string]any{"type": "string", "pattern": "^[a-f0-9]{64}$"}, "created_at": map[string]any{"type": "string", "format": "date-time"}}},
		"RecommendationEnvelope":         map[string]any{"type": "object", "required": []string{"recommendation"}, "properties": map[string]any{"recommendation": ref("Recommendation")}},
		"ExternalPolicyRequest":          map[string]any{"type": "object", "required": []string{"target", "adapter", "secret_reference_id", "enabled", "privacy_acknowledged", "request_limit", "cost_limit_microusd", "max_request_cost_microusd"}, "additionalProperties": false, "properties": map[string]any{"target": map[string]any{"type": "string", "minLength": 1}, "adapter": map[string]any{"type": "string", "enum": []string{"openrouter", "openai-compatible-external"}}, "secret_reference_id": map[string]any{"type": "string", "minLength": 1}, "enabled": map[string]any{"type": "boolean"}, "privacy_acknowledged": map[string]any{"type": "boolean"}, "request_limit": map[string]any{"type": "integer", "minimum": 1}, "cost_limit_microusd": map[string]any{"type": "integer", "minimum": 1}, "max_request_cost_microusd": map[string]any{"type": "integer", "minimum": 1}, "overflow_mode": map[string]any{"type": "string", "enum": []string{"health", "health_and_queue"}, "default": "health"}, "queue_threshold": map[string]any{"type": "number", "exclusiveMinimum": 0}, "breach_intervals": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 1}, "recovery_intervals": map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "default": 2}, "cooldown_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 86400, "default": 60}, "signal_max_age_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 600, "default": 30}}},
		"ExternalPolicy":                 map[string]any{"type": "object", "required": []string{"id", "deployment_id", "target_id", "adapter", "enabled", "privacy_acknowledged", "request_limit", "cost_limit_microusd", "max_request_cost_microusd", "overflow_mode"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "deployment_id": map[string]any{"type": "string"}, "target_id": map[string]any{"type": "string"}, "adapter": map[string]any{"type": "string"}, "secret_reference_id": map[string]any{"type": "string"}, "enabled": map[string]any{"type": "boolean"}, "privacy_acknowledged": map[string]any{"type": "boolean"}, "request_limit": map[string]any{"type": "integer"}, "requests_reserved": map[string]any{"type": "integer"}, "cost_limit_microusd": map[string]any{"type": "integer"}, "max_request_cost_microusd": map[string]any{"type": "integer"}, "cost_reserved_microusd": map[string]any{"type": "integer"}, "overflow_mode": map[string]any{"type": "string", "enum": []string{"health", "health_and_queue"}}, "queue_threshold": map[string]any{"type": "number"}, "breach_intervals": map[string]any{"type": "integer"}, "recovery_intervals": map[string]any{"type": "integer"}, "cooldown_seconds": map[string]any{"type": "integer"}, "signal_max_age_seconds": map[string]any{"type": "integer"}, "created_at": map[string]any{"type": "string", "format": "date-time"}, "updated_at": map[string]any{"type": "string", "format": "date-time"}}},
		"ExternalPolicyEnvelope":         map[string]any{"type": "object", "required": []string{"policy"}, "properties": map[string]any{"policy": ref("ExternalPolicy")}},
		"ChatCompletionRequest":          map[string]any{"type": "object", "required": []string{"model", "messages"}, "properties": map[string]any{"model": map[string]any{"type": "string", "description": "Stable InferCrane endpoint name; migrated v1 deployment aliases remain compatible endpoints."}, "messages": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "required": []string{"role", "content"}, "properties": map[string]any{"role": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}}}, "stream": map[string]any{"type": "boolean", "default": false}}, "additionalProperties": true},
		"Empty":                          map[string]any{"type": "null"},
	}
}
