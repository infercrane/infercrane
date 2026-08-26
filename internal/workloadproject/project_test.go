package workloadproject

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
)

func TestInitDiscoverAndValidate(t *testing.T) {
	root := t.TempDir()
	path, err := Init(InitOptions{Directory: filepath.Join(root, "service"), Model: "meta-llama/Llama-3.1-8B-Instruct", Runtime: "vllm", Cloud: "aws", GPU: "L40S", Region: "eu-central-1"})
	if err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(filepath.Dir(path), "src", "nested")
	if err = os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	discovered, deployment, err := Validate(nested)
	if err != nil {
		t.Fatal(err)
	}
	if discovered != path || deployment.Model.ID != "meta-llama/Llama-3.1-8B-Instruct" || deployment.Provider.Region != "eu-central-1" {
		t.Fatalf("unexpected project: path=%q deployment=%+v", discovered, deployment)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(content), "# yaml-language-server: $schema=https://raw.githubusercontent.com/infercrane/infercrane/main/schemas/deployment-v1.schema.json\n") {
		t.Fatalf("editor schema directive missing: err=%v content=%q", err, content)
	}
	if _, err = Init(InitOptions{Directory: filepath.Dir(path), Model: "other/model"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected overwrite protection, got %v", err)
	}
}

func TestDeploymentSchemaIsValidJSONAndMatchesRuntimeEnums(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "schemas", "deployment-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err = json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("parse deployment schema: %v", err)
	}
	if schema["$id"] != "https://infercrane.dev/schemas/deployment-v1.schema.json" {
		t.Fatalf("unexpected schema identity: %v", schema["$id"])
	}
}

func TestInitRejectsUnsafeOrUnsupportedInputs(t *testing.T) {
	tests := []InitOptions{
		{Directory: t.TempDir(), Model: ""},
		{Directory: t.TempDir(), Model: "acme/model", Name: "../escape"},
		{Directory: t.TempDir(), Model: "acme/model", Runtime: "custom-oci"},
		{Directory: t.TempDir(), Model: "acme/model", Cloud: "aws"},
	}
	for _, test := range tests {
		if _, err := Init(test); err == nil {
			t.Fatalf("expected rejection for %+v", test)
		}
	}
}

func TestInitPersistsReviewedServingProfile(t *testing.T) {
	root := t.TempDir()
	path, err := Init(InitOptions{
		Directory:     root,
		Model:         "mistralai/Mistral-7B-Instruct-v0.3",
		ModelRevision: "c170c708c41dac9275d15a8fff4eca08d52bab71",
		Runtime:       "vllm",
		Cloud:         "aws",
		GPU:           "L40S",
		Region:        "eu-central-1",
		ComputeMode:   "elastic",
		Routing:       "cache-aware",
		RuntimeArgs:   []string{"--enable-prefix-caching", "--max-num-batched-tokens", "2048"},
		MinReplicas:   1,
		MaxReplicas:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`args: ["--enable-prefix-caching","--max-num-batched-tokens","2048"]`,
		"min_replicas: 1", "max_replicas: 2", "strategy: cache-aware",
	} {
		if !strings.Contains(string(content), expected) {
			t.Fatalf("profile spec missing %q:\n%s", expected, content)
		}
	}
	if _, deployment, err := Validate(root); err != nil {
		t.Fatalf("profile spec is invalid: %v", err)
	} else if len(deployment.Runtime.Args) != 3 || deployment.Routing.Strategy != "cache-aware" || deployment.Scaling.MaxReplicas != 2 {
		t.Fatalf("profile intent was not preserved: %+v", deployment)
	}
}

func TestInitPersistsGeneralImmutableMultiGPUProfile(t *testing.T) {
	for _, name := range []string{"glm-5.3-flash", "qwen3.8-flash-next"} {
		t.Run(name, func(t *testing.T) {
			entry, ok := curatedrecipe.Get(name)
			if !ok {
				t.Fatal("reviewed multi-GPU recipe missing")
			}
			profile := entry.Profiles[0]
			root := t.TempDir()
			_, err := Init(InitOptions{Directory: root, Model: entry.Model, ModelRevision: entry.Revision, Runtime: profile.Runtime, RuntimeVersion: profile.RuntimeVersion, Cloud: profile.CloudHint, GPU: profile.GPUHint, GPUCount: profile.GPUCount, ComputeMode: profile.ComputeMode, RuntimeArgs: profile.RuntimeArgs, Workload: profile.Workload, MinReplicas: profile.MinReplicas, MaxReplicas: profile.MaxReplicas})
			if err != nil {
				t.Fatal(err)
			}
			_, deployment, err := Validate(root)
			if err != nil || deployment.Runtime.Engine != "custom-oci" || deployment.Runtime.Version != profile.RuntimeVersion || deployment.Runtime.Workload.Image != profile.Workload.Image || deployment.Resources.GPUCount != profile.GPUCount || deployment.Provider.Cloud != "kubernetes" {
				t.Fatalf("deployment=%+v err=%v", deployment, err)
			}
		})
	}
}

func TestInitRejectsInvalidReplicaBounds(t *testing.T) {
	_, err := Init(InitOptions{Directory: t.TempDir(), Model: "acme/model", MinReplicas: 2, MaxReplicas: 1})
	if err == nil || !strings.Contains(err.Error(), "0 <= min <= max") {
		t.Fatalf("expected replica-bound rejection, got %v", err)
	}
}

func TestPlanBuildUsesArgumentBoundariesAndRejectsEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(InitOptions{Directory: root, Model: "acme/model"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tag := "registry.example/acme/image:rc;touch-pwned"
	plan, err := PlanBuild(root, tag, "Dockerfile", "linux/amd64", true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Tag != tag || !containsExact(plan.Args, tag) || !containsExact(plan.Args, "linux/amd64") {
		t.Fatalf("arguments lost boundaries: %#v", plan.Args)
	}
	if _, err = PlanBuild(root, tag, "../Dockerfile", "", true); err == nil || !strings.Contains(err.Error(), "inside") {
		t.Fatalf("expected path escape rejection, got %v", err)
	}
}

func TestFinalizeBuildRequiresPushAndWritesImmutableImage(t *testing.T) {
	root := t.TempDir()
	specBody := `apiVersion: infercrane.dev/v1
kind: Deployment
name: custom
model:
  id: acme/model
runtime:
  engine: custom-oci
  workload:
    image: registry.example/acme/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    command: ["/serve"]
    protocol: openai
    port: 8000
    readiness_path: /health
    models_path: /v1/models
    metrics_path: /metrics
    cancellation: http-disconnect
    drain: connection
    shutdown_grace_seconds: 30
compute:
  mode: elastic
resources:
  gpu: L40S
provider:
  cloud: aws
  region: eu-central-1
scaling:
  min_replicas: 1
  max_replicas: 1
routing:
  strategy: round-robin
`
	if err := os.WriteFile(filepath.Join(root, SpecName), []byte(specBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanBuild(root, "registry.example/acme/image:rc", "Dockerfile", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, MetadataDir), 0o755); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("b", 64)
	metadata, _ := json.Marshal(map[string]any{"containerimage.digest": digest})
	if err = os.WriteFile(filepath.Join(root, MetadataDir, "buildx-metadata.json"), metadata, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := FinalizeBuild(plan)
	if err != nil {
		t.Fatal(err)
	}
	if result.Image != "registry.example/acme/image:rc@"+digest {
		t.Fatalf("image=%q", result.Image)
	}
	updated, err := os.ReadFile(filepath.Join(root, SpecName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), result.Image) {
		t.Fatalf("spec was not updated:\n%s", updated)
	}
	if _, _, err = Validate(root); err != nil {
		t.Fatalf("updated spec invalid: %v", err)
	}
	localPlan := plan
	localPlan.Push = false
	if _, err = FinalizeBuild(localPlan); err == nil || !strings.Contains(err.Error(), "requires --push") {
		t.Fatalf("expected local identity rejection, got %v", err)
	}
}

func TestDiscoverRejectsWrongFilenameAndMissingProject(t *testing.T) {
	root := t.TempDir()
	wrong := filepath.Join(root, "deployment.yaml")
	if err := os.WriteFile(wrong, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Discover(wrong); err == nil || !strings.Contains(err.Error(), SpecName) {
		t.Fatalf("expected wrong-name error, got %v", err)
	}
	if _, err := Discover(root); err == nil || !strings.Contains(err.Error(), "no "+SpecName) {
		t.Fatalf("expected missing-project error, got %v", err)
	}
}

func containsExact(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
