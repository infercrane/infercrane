package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "deployment.yaml")
	if err := os.WriteFile(filename, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestLoadServerlessArtifactAndRuntimeIdentity(t *testing.T) {
	loaded, err := Load(writeSpec(t, `
name: qwen-serverless
model:
  id: Qwen/Qwen3-8B
  revision: 0123456789abcdef0123456789abcdef01234567
runtime:
  engine: vllm
  version: 0.10.2
  args: [--enable-prefix-caching]
compute:
  mode: serverless
resources:
  gpu: L40S
provider:
  cloud: runpod
  adapter: runpod-serverless
scaling:
  min_replicas: 0
  max_replicas: 4
`))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Compute.Mode != "serverless" || loaded.Provider.Adapter != "runpod-serverless" || loaded.Scaling.MinReplicas != 0 || loaded.Scaling.MaxReplicas != 4 || loaded.Model.Revision == "" || loaded.Runtime.Version != "0.10.2" {
		t.Fatalf("loaded=%+v", loaded)
	}
}

func TestLoadElasticDefaults(t *testing.T) {
	loaded, err := Load(writeSpec(t, `
name: qwen
model: {id: Qwen/Qwen3-8B}
resources: {gpu: L40S}
provider: {cloud: runpod}
`))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.APIVersion != "infercrane.dev/v1" || loaded.Kind != "Deployment" || loaded.Compute.Mode != "elastic" || loaded.Runtime.Engine != "vllm" || loaded.Resources.GPUCount != 1 || loaded.Scaling.MinReplicas != 1 || loaded.Scaling.MaxReplicas != 1 || loaded.Routing.Strategy != "round-robin" {
		t.Fatalf("loaded=%+v", loaded)
	}
}

func TestLoadPreservesExactAcceleratorTopology(t *testing.T) {
	loaded, err := Load(writeSpec(t, `
name: multi-gpu
model: {id: acme/model}
runtime: {engine: custom-oci, workload: {image: "registry.example/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", command: [serve], protocol: openai, port: 8000, readiness_path: /health, models_path: /v1/models, metrics_path: /metrics, cancellation: http-disconnect, drain: connection, shutdown_grace_seconds: 30}}
resources: {gpu: H200, gpu_count: 4}
provider: {cloud: kubernetes}
`))
	if err != nil || loaded.Resources.GPUCount != 4 {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}

func TestLoadRejectsUnsupportedDeploymentSpecVersionAndKind(t *testing.T) {
	for _, header := range []string{"apiVersion: infercrane.dev/v2\nkind: Deployment", "apiVersion: infercrane.dev/v1\nkind: Workflow"} {
		_, err := Load(writeSpec(t, header+"\nname: qwen\nmodel: {id: org/model}\nresources: {gpu: L40S}\nprovider: {cloud: runpod}\n"))
		if err == nil || !strings.Contains(err.Error(), "apiVersion infercrane.dev/v1") {
			t.Fatalf("header=%q err=%v", header, err)
		}
	}
}

func TestLoadKubernetesElasticSpec(t *testing.T) {
	loaded, err := Load(writeSpec(t, `
name: qwen-kubernetes
model: {id: Qwen/Qwen3-8B}
resources: {gpu: NVIDIA-L40S}
provider: {cloud: kubernetes}
`))
	if err != nil || loaded.Provider.Cloud != "kubernetes" || loaded.Compute.Mode != "elastic" {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}

func TestLoadDynamoTopologyPreservesAdvancedIntentAndFailsClosed(t *testing.T) {
	base := `
name: llama-dynamo
model: {id: meta-llama/Llama-3.1-8B-Instruct}
runtime: {engine: vllm}
compute: {mode: elastic}
resources: {gpu: NVIDIA-L40S}
provider: {cloud: kubernetes, adapter: kubernetes-dynamo}
scaling: {min_replicas: 1, max_replicas: 1}
serving:
  backend: dynamo
  profile: custom
  mode: disaggregated
  routing: kv-aware
  prefill: {replicas: 1, tensor_parallelism: 1}
  decode: {replicas: 2, tensor_parallelism: 1}
  autoscaling: {owner: disabled}
  cache: {backend: none}
`
	loaded, err := Load(writeSpec(t, base))
	if err == nil || !strings.Contains(err.Error(), "registered for argument translation") || loaded.Serving.Backend != "dynamo" || loaded.Serving.Decode.Replicas != 2 || loaded.Serving.SchemaVersion == "" {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	aggregated := strings.Replace(base, "mode: disaggregated", "mode: aggregated", 1)
	aggregated = strings.Replace(aggregated, "routing: kv-aware", "routing: direct", 1)
	aggregated = strings.Replace(aggregated, "  prefill: {replicas: 1, tensor_parallelism: 1}\n  decode: {replicas: 2, tensor_parallelism: 1}", "  worker: {replicas: 1, tensor_parallelism: 1}", 1)
	_, err = Load(writeSpec(t, strings.Replace(aggregated, "max_replicas: 1", "max_replicas: 2", 1)))
	if err == nil || !strings.Contains(err.Error(), "outer replica bounds") {
		t.Fatalf("competing autoscaling owner accepted: %v", err)
	}
	_, err = Load(writeSpec(t, strings.Replace(base, "cache: {backend: none}", "cache: {backend: lmcache, configuration_ref: production}", 1)))
	if err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("unqualified disaggregated cache combination accepted: %v", err)
	}
}

func TestLoadRejectsNonzeroServerlessMinimum(t *testing.T) {
	_, err := Load(writeSpec(t, `
name: qwen
model: {id: Qwen/Qwen3-8B}
compute: {mode: serverless}
resources: {gpu: L40S}
provider: {cloud: runpod}
scaling: {min_replicas: 1, max_replicas: 4}
`))
	if err == nil {
		t.Fatal("expected serverless minimum validation error")
	}
}

func TestLoadRejectsExcludedRuntimeAndCloud(t *testing.T) {
	for name, override := range map[string]string{
		"runtime": "runtime: {engine: unknown-runtime}\nprovider: {cloud: runpod}",
		"cloud":   "runtime: {engine: vllm}\nprovider: {cloud: unqualified-cloud}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeSpec(t, "name: qwen\nmodel: {id: Qwen/Qwen3-8B}\nresources: {gpu: L40S}\n"+override+"\n"))
			if err == nil {
				t.Fatal("expected support-policy exclusion error")
			}
		})
	}
}

func TestLoadPortableRuntimeContracts(t *testing.T) {
	sglang, err := Load("../../examples/sglang.yaml")
	if err != nil || sglang.Runtime.Engine != "sglang" || sglang.Runtime.Workload.Image == "" {
		t.Fatalf("sglang=%#v err=%v", sglang.Runtime, err)
	}
	custom, err := Load("../../examples/custom-oci.yaml")
	if err != nil || custom.Runtime.Engine != "custom-oci" || custom.Runtime.Workload.Port != 8000 {
		t.Fatalf("custom=%#v err=%v", custom.Runtime, err)
	}
}

func TestLoadPinnedAWSModelQualificationExamples(t *testing.T) {
	for _, name := range []string{"aws-mistral-7b.yaml", "aws-deepseek-r1-distill-7b.yaml", "aws-granite-8b.yaml"} {
		deployment, err := Load("../../examples/" + name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if deployment.Provider.Cloud != "aws" || deployment.Provider.Region == "" || deployment.Model.Revision == "" || deployment.Model.Revision == "main" || deployment.Runtime.Engine != "vllm" || deployment.Resources.GPU != "L40S" {
			t.Fatalf("%s is not an immutable AWS qualification intent: %#v", name, deployment)
		}
	}
}

func TestLoadRejectsUnknownPortableWorkloadField(t *testing.T) {
	_, err := Load(writeSpec(t, `
name: custom
model: {id: org/model}
runtime:
  engine: custom-oci
  workload:
    image: registry.example/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    command: [serve]
    protocol: openai
    port: 8000
    readiness_path: /health
    models_path: /v1/models
    metrics_path: /metrics
    cancellation: http-disconnect
    drain: connection
    shutdown_grace_seconds: 30
    readyness_path: /typo
compute: {mode: elastic}
provider: {cloud: aws, region: eu-central-1}
resources: {gpu: L40S}
`))
	if err == nil || !strings.Contains(err.Error(), "readyness_path") {
		t.Fatalf("err=%v", err)
	}
}
