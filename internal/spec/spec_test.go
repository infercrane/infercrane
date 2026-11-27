package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/support"
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

func TestLoadAdmitsDeclaredSkyPilotCloudOnlyByAdapter(t *testing.T) {
	loaded, err := Load(writeSpec(t, `
name: qwen-vast
model: {id: Qwen/Qwen3-0.6B}
resources: {gpu: L40S}
provider: {cloud: vast, adapter: skypilot-vast}
`))
	if err != nil || loaded.Provider.Cloud != "vast" || loaded.Provider.Adapter != "skypilot-vast" {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if _, err := Load(writeSpec(t, "name: qwen-vast\nmodel: {id: Qwen/Qwen3-0.6B}\nresources: {gpu: L40S}\nprovider: {cloud: vast}\n")); err == nil {
		t.Fatal("a SkyPilot cloud without its adapter identity was admitted")
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

func TestLoadPlacesAMDInstinctGPUsOnlyWhereTheirPlatformIsQualified(t *testing.T) {
	example, err := Load("../../examples/runpod-mi300x.yaml")
	if err != nil || example.Resources.GPU != "MI300X" || example.Provider.Cloud != "runpod" || example.Model.Revision == "main" {
		t.Fatalf("example=%#v err=%v", example, err)
	}
	for name, body := range map[string]string{
		"runpod":     "resources: {gpu: MI300X}\nprovider: {cloud: runpod}",
		"kubernetes": "resources: {gpu: AMD_Instinct_MI300X_OAM}\nprovider: {cloud: kubernetes}",
		"sglang":     "runtime: {engine: sglang}\nresources: {gpu: MI355X}\nprovider: {cloud: kubernetes}",
	} {
		loaded, err := Load(writeSpec(t, "name: qwen\nmodel: {id: Qwen/Qwen3-8B}\n"+body+"\n"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// SGLang stores its image in the spec, so it must be the gfx950 build.
		if name == "sglang" && !strings.Contains(loaded.Runtime.Workload.Image, "rocm720-mi35x@sha256:") {
			t.Fatalf("SGLang on MI355X stored %q", loaded.Runtime.Workload.Image)
		}
	}
	for name, body := range map[string]string{
		"aws":        "resources: {gpu: MI300X}\nprovider: {cloud: aws, region: eu-central-1}",
		"serverless": "resources: {gpu: MI300X}\nprovider: {cloud: runpod}\ncompute: {mode: serverless}\nscaling: {min_replicas: 0, max_replicas: 1}",
	} {
		if _, err := Load(writeSpec(t, "name: qwen\nmodel: {id: Qwen/Qwen3-8B}\n"+body+"\n")); err == nil || !strings.Contains(err.Error(), "rocm-gfx942 platform") {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

func TestLoadRefusesAcceleratorsThatAreNotQualified(t *testing.T) {
	// TPUs, Gaudi, and Intel GPUs resolve to their own platforms, which have
	// no selectable image or placement, so every spec naming one is refused
	// before anything is stored or provisioned, whatever the runtime.
	for name, body := range map[string]string{
		"gke tpu":          "resources: {gpu: tpu-v6e-slice}\nprovider: {cloud: kubernetes}",
		"custom image tpu": "runtime: {engine: custom-oci, workload: {image: \"vllm/vllm-tpu@sha256:450e37c04b8bf4dd2568e38672dcd94d0f22f131510962725d577eb249839440\", command: [vllm, serve, \"${MODEL}\"], port: 8000, readiness_path: /health}}\nresources: {gpu: tpu7x}\nprovider: {cloud: kubernetes}",
		"skypilot tpu":     "resources: {gpu: tpu-v6e-8}\nprovider: {cloud: gcp, region: us-east5}",
		"gaudi":            "resources: {gpu: Gaudi 3}\nprovider: {cloud: kubernetes}",
		"sglang gaudi":     "runtime: {engine: sglang}\nresources: {gpu: Gaudi 2}\nprovider: {cloud: kubernetes}",
		"intel gpu":        "resources: {gpu: Arc Pro B60}\nprovider: {cloud: kubernetes}",
	} {
		if _, err := Load(writeSpec(t, "name: qwen\nmodel: {id: Qwen/Qwen3-8B}\n"+body+"\n")); err == nil || !strings.Contains(err.Error(), "platform, which is not qualified") {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

func TestLoadAdmitsOnlyReviewedEngineEnvironment(t *testing.T) {
	base := "name: qwen\nmodel: {id: Qwen/Qwen3-8B}\nprovider: {cloud: runpod, adapter: runpod-pods}\n"
	loaded, err := Load(writeSpec(t, base+"resources: {gpu: MI300X}\nruntime: {engine: vllm, version: 0.22.1, env: {VLLM_ROCM_USE_AITER: \"1\"}}\n"))
	if err != nil || loaded.Runtime.Env["VLLM_ROCM_USE_AITER"] != "1" {
		t.Fatalf("spec=%#v err=%v", loaded.Runtime, err)
	}
	for name, body := range map[string]string{
		"CUDA GPU":   "resources: {gpu: H100}\nruntime: {env: {VLLM_ROCM_USE_AITER: \"1\"}}",
		"credential": "resources: {gpu: MI300X}\nruntime: {env: {HF_TOKEN: hf_pasted}}",
		"value":      "resources: {gpu: MI300X}\nruntime: {env: {VLLM_ROCM_USE_AITER: \"true\"}}",
	} {
		if _, err := Load(writeSpec(t, base+body+"\n")); err == nil || !strings.Contains(err.Error(), "runtime.env") || strings.Contains(err.Error(), "hf_pasted") {
			t.Fatalf("%s: err=%v", name, err)
		}
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

func TestLoadRefusesAnEngineReleaseWithoutAPinnedImage(t *testing.T) {
	base := "name: qwen\nmodel: {id: Qwen/Qwen3-8B, revision: 0123456789abcdef0123456789abcdef01234567}\nprovider: {cloud: runpod, adapter: runpod-pods}\nresources: {gpu: H100}\n"
	for _, version := range []string{"0.34.0", "v0.34.0", "0.22.1", ""} {
		if _, err := Load(writeSpec(t, base+"runtime: {engine: vllm, version: \""+version+"\"}\n")); err != nil {
			t.Fatalf("pinned release %q: %v", version, err)
		}
	}
	if _, err := Load(writeSpec(t, base+"runtime: {engine: vllm, version: 0.30.0}\n")); err == nil || !strings.Contains(err.Error(), "runtime vllm 0.30.0 is not pinned; pinned releases: 0.34.0, 0.22.1") {
		t.Fatalf("unpinned release err=%v", err)
	}
}

func TestLoadAdmitsOnlyATunedKernelSetThatFitsTheEngine(t *testing.T) {
	shape := kerneltuning.FusedMoEShape{Experts: 256, Intermediate: 512, DType: "fp8_w8a8", BlockN: 128, BlockK: 128}
	file, err := kerneltuning.NewFile(shape, "NVIDIA_H100_80GB_HBM3", map[int]kerneltuning.TileConfig{128: {BlockSizeM: 16, BlockSizeN: 64, BlockSizeK: 128, GroupSizeM: 1, NumWarps: 4, NumStages: 3}})
	if err != nil {
		t.Fatal(err)
	}
	set := kerneltuning.NewSet("vllm", support.DefaultRuntimeVersion, "NVIDIA_H100_80GB_HBM3", []domain.KernelConfigFile{file})
	encoded, _ := json.Marshal(set) // JSON is YAML flow style
	base := "name: qwen\nmodel: {id: Qwen/Qwen3.6-35B-A3B-FP8}\nprovider: {cloud: kubernetes, adapter: kubernetes}\n"
	loaded, err := Load(writeSpec(t, base+"resources: {gpu: H100}\nruntime: {engine: vllm, kernel_configs: "+string(encoded)+"}\n"))
	if err != nil || loaded.Runtime.KernelConfigs == nil || loaded.Runtime.KernelConfigs.Digest != set.Digest {
		t.Fatalf("spec=%#v err=%v", loaded.Runtime.KernelConfigs, err)
	}
	for name, body := range map[string]string{
		"another GPU":            "resources: {gpu: H200}\nruntime: {engine: vllm, kernel_configs: " + string(encoded) + "}",
		"another engine version": "resources: {gpu: H100}\nruntime: {engine: vllm, version: " + support.LegacyVLLMRuntimeVersion + ", kernel_configs: " + string(encoded) + "}",
		"tampered": "resources: {gpu: H100}\nruntime: {engine: vllm, kernel_configs: " +
			strings.Replace(string(encoded), `\"num_stages\":3`, `\"num_stages\":5`, 1) + "}",
	} {
		if _, err := Load(writeSpec(t, base+body+"\n")); err == nil || !strings.Contains(err.Error(), "runtime.kernel_configs") {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

func TestLoadChecksEngineArguments(t *testing.T) {
	base := "name: qwen\nmodel: {id: Qwen/Qwen3-8B, revision: 0123456789abcdef0123456789abcdef01234567}\nprovider: {cloud: runpod, adapter: runpod-pods}\nresources: {gpu: H100}\n"
	if _, err := Load(writeSpec(t, base+"runtime: {engine: vllm, args: [--max-model-len, \"8192\"]}\n")); err != nil {
		t.Fatalf("reviewed arguments: %v", err)
	}
	for args, want := range map[string]string{
		"[--trust-remote-code]":                "--trust-remote-code is refused",
		"[--tool-parser-plugin, /models/p.py]": "--tool-parser-plugin is not reviewed for vllm",
		"[--enable-custom-logit-processor]":    "is refused",
	} {
		if _, err := Load(writeSpec(t, base+"runtime: {engine: vllm, args: "+args+"}\n")); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err=%v, want %q", args, err, want)
		}
	}
}
