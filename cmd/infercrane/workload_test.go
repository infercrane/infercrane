package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkloadCommandsAcceptDocumentedPathBeforeFlags(t *testing.T) {
	project := filepath.Join(t.TempDir(), "mistral")
	if err := workloadInitCommand([]string{project, "--recipe", "mistral-7b-instruct", "--name", "demo-model", "--output", "json"}); err != nil {
		t.Fatalf("init with leading path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, "infercrane.yaml")); err != nil {
		t.Fatalf("project spec: %v", err)
	}
	if err := workloadValidateCommand([]string{project, "--output", "json"}); err != nil {
		t.Fatalf("validate with leading path: %v", err)
	}
	if err := workloadBuildCommand(t.Context(), []string{project, "--tag", "example.invalid/demo:v1"}); err == nil || !strings.Contains(err.Error(), "requires a custom-oci") {
		t.Fatalf("standard runtime build boundary error=%v", err)
	}
}
