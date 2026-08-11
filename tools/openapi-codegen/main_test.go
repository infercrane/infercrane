package main

import (
	"strings"
	"testing"
)

func TestGeneratedClientsHaveTypedResponsesAndEscapedPaths(t *testing.T) {
	python := pythonAPI()
	for _, fragment := range []string{
		"def get_operation(self, id: str) -> OperationData:",
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
		"encodeURIComponent(id)",
		"as Promise<Operation>",
	} {
		if !strings.Contains(typeScript, fragment) {
			t.Errorf("TypeScript client is missing %q", fragment)
		}
	}
}

func TestResponseTypeMappings(t *testing.T) {
	if got := pythonResponseType("Empty"); got != "None" {
		t.Fatalf("Python empty response type = %q", got)
	}
	if got := typeScriptResponseType("DeploymentView"); got != "DeploymentView" {
		t.Fatalf("TypeScript deployment response type = %q", got)
	}
}
