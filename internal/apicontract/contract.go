// Package apicontract owns the public control-plane API description used by
// documentation, generated clients, and route-drift qualification.
package apicontract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const Version = "0.4.0"

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
	{"GET", "/deployments/{name}/external-policy", "getExternalPolicy", "External capacity", "Inspect governed external fallback", "", "Object", 200, false},
	{"PUT", "/deployments/{name}/external-policy", "setExternalPolicy", "External capacity", "Configure governed external fallback", "Object", "Object", 200, false},
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
		"Object":                      stringMap,
		"ObjectList":                  map[string]any{"type": "object", "required": []string{"data"}, "properties": map[string]any{"data": map[string]any{"type": "array", "items": stringMap}}},
		"Error":                       map[string]any{"type": "object", "required": []string{"code", "message"}, "properties": map[string]any{"code": map[string]any{"type": "string"}, "category": map[string]any{"type": "string"}, "message": map[string]any{"type": "string"}, "request_id": map[string]any{"type": "string"}, "retryable": map[string]any{"type": "boolean"}, "remediation": map[string]any{"type": "string"}}},
		"ErrorEnvelope":               map[string]any{"type": "object", "required": []string{"error"}, "properties": map[string]any{"error": ref("Error")}},
		"Operation":                   map[string]any{"type": "object", "required": []string{"id", "kind", "status", "progress"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "kind": map[string]any{"type": "string"}, "resource_type": map[string]any{"type": "string"}, "resource_name": map[string]any{"type": "string"}, "status": map[string]any{"type": "string", "enum": []string{"pending", "leased", "running", "waiting", "cancelling", "succeeded", "failed", "cancelled"}}, "progress": map[string]any{"type": "integer", "minimum": 0, "maximum": 100}, "message": map[string]any{"type": "string"}, "error_code": map[string]any{"type": "string"}, "retryable": map[string]any{"type": "boolean"}, "cancel_requested": map[string]any{"type": "boolean"}, "attempt": map[string]any{"type": "integer"}, "max_attempts": map[string]any{"type": "integer"}, "created_at": map[string]any{"type": "string", "format": "date-time"}, "updated_at": map[string]any{"type": "string", "format": "date-time"}}},
		"OperationEnvelope":           map[string]any{"type": "object", "required": []string{"operation"}, "properties": map[string]any{"operation": ref("Operation"), "created": map[string]any{"type": "boolean"}}},
		"DeploymentOperationEnvelope": map[string]any{"allOf": []map[string]any{ref("OperationEnvelope"), {"type": "object", "properties": map[string]any{"deployment": ref("Deployment")}}}},
		"Deployment":                  map[string]any{"type": "object", "required": []string{"name", "model", "runtime"}, "properties": map[string]any{"id": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "runtime": map[string]any{"type": "string"}, "observed_state": map[string]any{"type": "string"}, "min_replicas": map[string]any{"type": "integer"}, "max_replicas": map[string]any{"type": "integer"}, "active_revision_id": map[string]any{"type": "string"}, "candidate_revision_id": map[string]any{"type": "string"}}},
		"DeploymentList":              map[string]any{"type": "object", "required": []string{"data"}, "properties": map[string]any{"data": map[string]any{"type": "array", "items": ref("Deployment")}}},
		"DeploymentView":              map[string]any{"type": "object", "required": []string{"deployment", "lifecycle_status"}, "properties": map[string]any{"deployment": ref("Deployment"), "lifecycle_status": stringMap, "active_operation": ref("Operation"), "targets": map[string]any{"type": "array", "items": stringMap}, "replicas": map[string]any{"type": "array", "items": stringMap}, "revisions": map[string]any{"type": "array", "items": stringMap}}},
		"DeploymentCreate":            map[string]any{"type": "object", "required": []string{"name", "model"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"}, "runtime": map[string]any{"type": "string", "default": "vllm"}, "cloud": map[string]any{"type": "string"}, "compute_mode": map[string]any{"type": "string", "enum": []string{"elastic", "serverless"}}, "gpu": map[string]any{"type": "string"}, "region": map[string]any{"type": "string"}, "min_replicas": map[string]any{"type": "integer", "minimum": 0}, "max_replicas": map[string]any{"type": "integer", "minimum": 0}, "runtime_version": map[string]any{"type": "string"}, "runtime_args": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "model_revision": map[string]any{"type": "string"}}},
		"DeploymentSpec":              stringMap,
		"TargetCreate":                map[string]any{"type": "object", "required": []string{"name", "url", "provider", "runtime"}, "properties": map[string]any{"name": map[string]any{"type": "string"}, "url": map[string]any{"type": "string", "format": "uri"}, "provider": map[string]any{"type": "string"}, "runtime": map[string]any{"type": "string"}, "upstream_model": map[string]any{"type": "string"}}},
		"BenchmarkRequest":            map[string]any{"type": "object", "required": []string{"requests", "concurrency"}, "properties": map[string]any{"requests": map[string]any{"type": "integer", "minimum": 1, "maximum": 100000}, "concurrency": map[string]any{"type": "integer", "minimum": 1}, "random_seed": map[string]any{"type": "integer", "default": 17}, "revision": map[string]any{"type": "string"}}},
		"ChatCompletionRequest":       map[string]any{"type": "object", "required": []string{"model", "messages"}, "properties": map[string]any{"model": map[string]any{"type": "string", "description": "Logical InferCrane deployment alias."}, "messages": map[string]any{"type": "array", "minItems": 1, "items": map[string]any{"type": "object", "required": []string{"role", "content"}, "properties": map[string]any{"role": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}}}, "stream": map[string]any{"type": "boolean", "default": false}}, "additionalProperties": true},
		"Empty":                       map[string]any{"type": "null"},
	}
}
