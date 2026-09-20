package main

import (
	"strings"
	"testing"
)

func TestGeneratedClientsHaveTypedResponsesAndEscapedPaths(t *testing.T) {
	python := pythonAPI()
	for _, fragment := range []string{
		"def get_operation(self, id: str) -> OperationData:",
		"def plan_intent(self, *, body: dict[str, Any]) -> IntentPlanEnvelope:",
		"quote(id, safe='')",
		"cast(OperationData, self._transport.request",
	} {
		if !strings.Contains(python, fragment) {
			t.Errorf("Python client is missing %q", fragment)
		}
	}
	typeScript := typeScriptAPI()
	for _, fragment := range []string{
		"getOperation(id: string): Promise<Operation>",
		"planIntent(body: JsonValue): Promise<IntentPlanEnvelope>",
		"encodeURIComponent(id)",
		"getSandboxPreview(sandbox: string, token: string, path: string)",
		"const requestPath = `/sandbox-previews/",
		"as Promise<Operation>",
	} {
		if !strings.Contains(typeScript, fragment) {
			t.Errorf("TypeScript client is missing %q", fragment)
		}
	}
	if strings.Contains(typeScript, "const path =") {
		t.Fatal("TypeScript generated route variable collides with path parameters")
	}
}

func TestResponseTypeMappings(t *testing.T) {
	if got := pythonResponseType("Empty"); got != "None" {
		t.Fatalf("Python empty response type = %q", got)
	}
	if got := typeScriptResponseType("DeploymentView"); got != "DeploymentView" {
		t.Fatalf("TypeScript deployment response type = %q", got)
	}
	if got := pythonResponseType("IntentPlanEnvelope"); got != "IntentPlanEnvelope" {
		t.Fatalf("Python intent-plan response type = %q", got)
	}
	if got := typeScriptResponseType("IntentPlanEnvelope"); got != "IntentPlanEnvelope" {
		t.Fatalf("TypeScript intent-plan response type = %q", got)
	}
}
