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

func TestWorkloadInitMissingServingSourceIsActionable(t *testing.T) {
	err := workloadInitCommand([]string{filepath.Join(t.TempDir(), "missing-model")})
	if err == nil {
		t.Fatal("workload init accepted a project without a model or recipe")
	}
	for _, expected := range []string{"--model MODEL", "--recipe NAME", "infercrane recipes curated"} {
		if !strings.Contains(err.Error(), expected) {
			t.Fatalf("error %q does not explain %q", err, expected)
		}
	}
}

func TestWorkloadInitAppliesSelectedServingProfile(t *testing.T) {
	project := filepath.Join(t.TempDir(), "mistral-interactive")
	if err := workloadInitCommand([]string{project, "--recipe", "mistral-7b-instruct", "--profile", "vllm-interactive", "--cloud", "aws", "--region", "eu-central-1", "--output", "json"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(project, "infercrane.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"c170c708c41dac9275d15a8fff4eca08d52bab71", `args: ["--enable-prefix-caching","--max-num-batched-tokens","2048"]`, "strategy: cache-aware", "max_replicas: 2"} {
		if !strings.Contains(string(content), expected) {
			t.Fatalf("generated profile missing %q:\n%s", expected, content)
		}
	}
	if err := workloadValidateCommand([]string{project}); err != nil {
		t.Fatalf("generated profile is not valid: %v", err)
	}
}

func TestWorkloadInitRejectsUnknownOrUnboundProfile(t *testing.T) {
	if err := workloadInitCommand([]string{filepath.Join(t.TempDir(), "unknown"), "--recipe", "mistral-7b-instruct", "--profile", "fastest"}); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("expected unknown-profile rejection, got %v", err)
	}
	if err := workloadInitCommand([]string{filepath.Join(t.TempDir(), "unbound"), "--model", "acme/model", "--profile", "vllm-balanced"}); err == nil || !strings.Contains(err.Error(), "requires --recipe") {
		t.Fatalf("expected unbound-profile rejection, got %v", err)
	}
	if err := workloadInitCommand([]string{filepath.Join(t.TempDir(), "runtime"), "--recipe", "mistral-7b-instruct", "--profile", "vllm-balanced", "--runtime", "sglang"}); err == nil || !strings.Contains(err.Error(), "requires runtime") {
		t.Fatalf("expected profile/runtime rejection, got %v", err)
	}
}

func TestWorkloadInitAllowsExplicitGPUOverrideOfProfileHint(t *testing.T) {
	project := filepath.Join(t.TempDir(), "mistral-h100")
	if err := workloadInitCommand([]string{project, "--recipe", "mistral-7b-instruct", "--profile", "vllm-throughput", "--gpu", "H100"}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(project, "infercrane.yaml"))
	if err != nil || !strings.Contains(string(content), "gpu: H100") {
		t.Fatalf("explicit GPU override was lost: err=%v spec=%s", err, content)
	}
}

func TestProviderPreflightCommandGuidesEachOwnershipBoundary(t *testing.T) {
	tests := map[string]string{
		"aws":               "infercrane doctor --aws",
		"gcp-compute":       "infercrane doctor --gcp",
		"kubernetes-dynamo": "infercrane doctor --kubernetes",
		"runpod-serverless": "infercrane doctor --serverless",
		"runpod":            "infercrane doctor --cloud",
		"custom-provider":   "infercrane doctor",
	}
	for provider, expected := range tests {
		if actual := providerPreflightCommand(provider); actual != expected {
			t.Fatalf("provider %q preflight=%q, want %q", provider, actual, expected)
		}
	}
}

func TestWorkloadInitPrintsZeroToInspectionJourney(t *testing.T) {
	project := filepath.Join(t.TempDir(), "aws-model")
	output, err := captureStdout(t, func() error {
		return workloadInitCommand([]string{project, "--recipe", "mistral-7b-instruct", "--name", "support-production", "--cloud", "aws", "--region", "eu-central-1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"infercrane workload validate",
		"infercrane doctor --aws",
		"infercrane workload plan",
		"infercrane workload deploy --wait",
		"infercrane request support-production",
		"infercrane doctor support-production",
		`model="support-production"`,
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("output missing %q:\n%s", expected, output)
		}
	}
}
