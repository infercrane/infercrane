package apicontract

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestServerRouteCoverage(t *testing.T) {
	_, current, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(current), "..", "controlapi", "api.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	server := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "HandleFunc" {
			return true
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		value, err := strconv.Unquote(literal.Value)
		if err == nil && strings.Contains(value, " /api/v1/") {
			server[strings.TrimSpace(strings.Replace(value, "/api/v1", "", 1))] = true
		}
		return true
	})
	contract := map[string]bool{}
	operations := map[string]bool{}
	for _, route := range Routes {
		key := route.Method + " " + route.Path
		contract[key] = true
		if operations[route.OperationID] {
			t.Errorf("duplicate operationId %q", route.OperationID)
		}
		operations[route.OperationID] = true
	}
	for route := range server {
		if !contract[route] {
			t.Errorf("server route absent from OpenAPI contract: %s", route)
		}
	}
	for route := range contract {
		if !server[route] {
			t.Errorf("OpenAPI route absent from server: %s", route)
		}
	}
}

func TestDocumentBuilds(t *testing.T) {
	doc, err := Document()
	if err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("unexpected OpenAPI version: %v", doc["openapi"])
	}
	if len(doc["paths"].(map[string]any)) == 0 {
		t.Fatal("document has no paths")
	}
	paths := doc["paths"].(map[string]any)
	for _, path := range []string{"/api/v1/deployments", "/v1/models", "/v1/chat/completions"} {
		if paths[path] == nil {
			t.Errorf("document is missing %s", path)
		}
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	for _, route := range Routes {
		for _, name := range []string{route.Request, route.Response} {
			if name != "" && schemas[name] == nil {
				t.Errorf("%s %s references missing schema %q", route.Method, route.Path, name)
			}
		}
	}
}

func TestPublicGPUPricesAreDocumentedWithoutBearerAuthentication(t *testing.T) {
	doc, err := Document()
	if err != nil {
		t.Fatal(err)
	}
	operation := doc["paths"].(map[string]any)["/api/v1/public/catalog/gpu-prices"].(map[string]any)["get"].(map[string]any)
	security, ok := operation["security"].([]any)
	if !ok || len(security) != 0 {
		t.Fatalf("public GPU prices must not require bearer authentication: %#v", operation["security"])
	}
	parameters, ok := operation["parameters"].([]map[string]any)
	if !ok {
		t.Fatalf("public GPU price query parameters missing: %#v", operation["parameters"])
	}
	found := map[string]bool{}
	for _, parameter := range parameters {
		found[parameter["name"].(string)] = true
	}
	for _, name := range []string{"q", "provider", "gpu", "current", "sort", "limit", "offset"} {
		if !found[name] {
			t.Fatalf("public GPU price query parameter %q missing", name)
		}
	}
}

func TestIntentPlanningHasTypedEnvelopeAndDeploymentDraft(t *testing.T) {
	doc, err := Document()
	if err != nil {
		t.Fatal(err)
	}
	operation := doc["paths"].(map[string]any)["/api/v1/planning/intents"].(map[string]any)["post"].(map[string]any)
	response := operation["responses"].(map[string]any)["200"].(map[string]any)
	content := response["content"].(map[string]any)["application/json"].(map[string]any)
	schema := content["schema"].(map[string]any)
	if schema["$ref"] != "#/components/schemas/IntentPlanEnvelope" {
		t.Fatalf("intent plan response remains generic: %#v", schema)
	}
	schemas := doc["components"].(map[string]any)["schemas"].(map[string]any)
	envelope := schemas["IntentPlanEnvelope"].(map[string]any)
	properties := envelope["properties"].(map[string]any)
	if properties["plan"].(map[string]any)["$ref"] != "#/components/schemas/IntentPlan" || properties["deployment_draft"].(map[string]any)["$ref"] != "#/components/schemas/DeploymentCreate" {
		t.Fatalf("intent plan envelope refs=%#v", properties)
	}
	deployment := schemas["DeploymentCreate"].(map[string]any)
	if deployment["additionalProperties"] != false {
		t.Fatalf("deployment draft schema must reject unknown CloudRequest fields: %#v", deployment)
	}
}
