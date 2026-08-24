package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCheckRoleAllowsOnlyReadAccessForArtifactPVCs(t *testing.T) {
	var role map[string]any
	if err := yaml.Unmarshal([]byte(`
kind: Role
rules:
  - apiGroups: [""]
    resources: ["persistentvolumeclaims"]
    verbs: ["get", "list"]
`), &role); err != nil {
		t.Fatal(err)
	}
	resources, err := checkRole(role)
	if err != nil || len(resources) != 1 || resources[0] != "persistentvolumeclaims" {
		t.Fatalf("resources=%v err=%v", resources, err)
	}
	role["rules"].([]any)[0].(map[string]any)["verbs"] = []any{"get", "list", "delete"}
	if _, err = checkRole(role); err == nil || !strings.Contains(err.Error(), "must not include delete") {
		t.Fatalf("mutating PVC permission accepted: %v", err)
	}
}
