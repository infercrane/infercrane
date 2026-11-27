package provision

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/servingcontract"
	"github.com/infercrane/infercrane/internal/support"
)

// tunedSet is a set for the product's pinned vLLM on an H100 SXM, for
// Qwen3.6-35B-A3B-FP8's MoE shape.
func tunedSet(t *testing.T, version string) *domain.KernelConfigSet {
	t.Helper()
	shape := kerneltuning.FusedMoEShape{Experts: 256, Intermediate: 512, DType: "fp8_w8a8", BlockN: 128, BlockK: 128}
	file, err := kerneltuning.NewFile(shape, "NVIDIA_H100_80GB_HBM3", map[int]kerneltuning.TileConfig{
		128: {BlockSizeM: 16, BlockSizeN: 64, BlockSizeK: 128, GroupSizeM: 1, NumWarps: 4, NumStages: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	set := kerneltuning.NewSet("vllm", version, "NVIDIA_H100_80GB_HBM3", []domain.KernelConfigFile{file})
	return &set
}

func tunedReplicaSpec(t *testing.T, cloud string) ReplicaSpec {
	return ReplicaSpec{ExternalKey: "deployment-r0", Model: "Qwen/Qwen3.6-35B-A3B-FP8", ModelRevision: "immutable", Cloud: cloud, GPU: "H100", GPUCount: 1, Region: "eu-central-1",
		Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, KernelConfigs: tunedSet(t, support.DefaultRuntimeVersion)}
}

func kubernetesForKernelConfigs() Kubernetes {
	return Kubernetes{Context: "kind-infercrane", Namespace: "infercrane-system", WorkloadAPI: "deployment", ServiceAccount: "infercrane-runtime", WorkerSecretName: "infercrane-worker", WorkerSecretKey: "api-key",
		ImageDigest: "vllm/vllm-openai@sha256:" + strings.Repeat("a", 64), GPUResource: "nvidia.com/gpu", GPUProductLabel: "nvidia.com/gpu.product", ArtifactCachePolicy: "disabled"}
}

func TestKubernetesWritesTunedKernelFilesBeforeTheEngineStarts(t *testing.T) {
	provider := kubernetesForKernelConfigs()
	spec := tunedReplicaSpec(t, "kubernetes")
	if err := provider.validate(spec); err != nil {
		t.Fatal(err)
	}
	manifest, err := provider.manifest(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []struct {
			Kind string `json:"kind"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Args         []string         `json:"args"`
							Env          []map[string]any `json:"env"`
							VolumeMounts []map[string]any `json:"volumeMounts"`
						} `json:"containers"`
						Volumes []map[string]any `json:"volumes"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err = json.Unmarshal(manifest, &list); err != nil || len(list.Items) != 2 || list.Items[0].Kind != "Deployment" {
		t.Fatalf("manifest = %s, %v", manifest, err)
	}
	pod := list.Items[0].Spec.Template.Spec
	container := pod.Containers[0]
	if len(container.Args) < 5 || container.Args[0] != "/bin/sh" || container.Args[3] != "infercrane-serve" || container.Args[4] != "serve" {
		t.Fatalf("the engine command is not wrapped by the file step: %v", container.Args)
	}
	script := container.Args[2]
	file := spec.KernelConfigs.Files[0]
	for _, want := range []string{"sha256sum -c", "export VLLM_TUNED_CONFIG_FOLDER='/opt/infercrane/kernel-configs'", "'/opt/infercrane/kernel-configs/" + file.Name + "'", `exec "$@"`} {
		if !strings.Contains(script, want) {
			t.Fatalf("script misses %q:\n%s", want, script)
		}
	}
	carried := false
	for _, variable := range container.Env {
		if variable["name"] == "INFERCRANE_KERNEL_CONFIG_0" && variable["value"] == file.Content {
			carried = true
		}
	}
	mounted := false
	for _, mount := range container.VolumeMounts {
		if mount["mountPath"] == kerneltuning.MountPath && mount["name"] == "infercrane-kernel-configs" {
			mounted = true
		}
	}
	if !carried || !mounted || len(pod.Volumes) != 1 || pod.Volumes[0]["emptyDir"] == nil {
		t.Fatalf("env carried=%v mounted=%v volumes=%v", carried, mounted, pod.Volumes)
	}

	without := spec
	without.KernelConfigs = nil
	withHash, _ := provider.intentHash(spec)
	withoutHash, _ := provider.intentHash(without)
	if withHash == withoutHash {
		t.Fatal("intent hash ignores the tuned set")
	}
	if encoded, _ := json.Marshal(without); strings.Contains(string(encoded), "KernelConfigs") {
		t.Fatalf("an empty set changes existing intent hashes: %s", encoded)
	}
	if plain, _ := provider.manifest(without, ""); strings.Contains(string(plain), "kernel-configs") || strings.Contains(string(plain), "/bin/sh") {
		t.Fatalf("a replica without a set changed: %s", plain)
	}
}

func TestKernelConfigScriptWritesTheFileAndStopsOnAnotherOne(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum is not installed")
	}
	set := tunedSet(t, support.DefaultRuntimeVersion)
	folder := filepath.Join(t.TempDir(), "kernel-configs")
	script := strings.ReplaceAll(kernelConfigScript(*set), kerneltuning.MountPath, folder) + `printf '%s' "$VLLM_TUNED_CONFIG_FOLDER"` + "\n"
	run := func(content string) (string, error) {
		command := exec.Command("/bin/sh", "-c", script)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "INFERCRANE_KERNEL_CONFIG_0=" + content}
		output, err := command.CombinedOutput()
		return string(output), err
	}
	output, err := run(set.Files[0].Content)
	if err != nil || output != folder {
		t.Fatalf("script: %v %q", err, output)
	}
	written, err := os.ReadFile(filepath.Join(folder, set.Files[0].Name))
	if err != nil || string(written) != set.Files[0].Content {
		t.Fatalf("written = %q, %v", written, err)
	}
	if _, err = run(strings.Replace(set.Files[0].Content, `"num_stages":3`, `"num_stages":4`, 1)); err == nil {
		t.Fatal("a file that does not match its sha256 started the engine")
	}
}

func TestReplicaKernelConfigsMustFitTheEngine(t *testing.T) {
	provider := kubernetesForKernelConfigs()
	for name, change := range map[string]func(*ReplicaSpec){
		"a set for another engine release": func(spec *ReplicaSpec) { spec.KernelConfigs = tunedSet(t, support.LegacyVLLMRuntimeVersion) },
		// A revision on the older release must serve that release's image: a
		// workload carrying the current release's image is another engine.
		"an older release served from the current image": func(spec *ReplicaSpec) {
			spec.RuntimeVersion, spec.KernelConfigs = support.LegacyVLLMRuntimeVersion, tunedSet(t, support.LegacyVLLMRuntimeVersion)
			spec.Workload, _ = support.RuntimeWorkloadFor("vllm", support.DefaultRuntimeVersion, "cuda")
		},
		"another GPU": func(spec *ReplicaSpec) { spec.GPU = "H200" },
		"a custom image": func(spec *ReplicaSpec) {
			workload, _ := support.RuntimeWorkload("vllm", "cuda")
			workload.Image = "registry.example.com/vllm@sha256:" + strings.Repeat("b", 64)
			spec.Workload = workload
		},
		"a tampered file": func(spec *ReplicaSpec) {
			broken := *spec.KernelConfigs
			broken.Files = append([]domain.KernelConfigFile(nil), broken.Files...)
			broken.Files[0].Content = strings.Replace(broken.Files[0].Content, `"num_warps":4`, `"num_warps":8`, 1)
			spec.KernelConfigs = &broken
		},
	} {
		spec := tunedReplicaSpec(t, "kubernetes")
		change(&spec)
		if _, err := provider.manifest(spec, ""); !errors.Is(err, ErrInvalidReplicaSpec) {
			t.Errorf("%s: manifest err = %v", name, err)
		}
	}
}

func TestReplicaKernelConfigsFollowTheRevisionsRelease(t *testing.T) {
	provider := kubernetesForKernelConfigs()
	for _, version := range support.RuntimeVersions("vllm") {
		spec := tunedReplicaSpec(t, "kubernetes")
		spec.RuntimeVersion, spec.KernelConfigs = version, tunedSet(t, version)
		spec.Workload, _ = support.RuntimeWorkloadFor("vllm", version, "cuda")
		if _, err := provider.manifest(spec, ""); err != nil {
			t.Errorf("vllm %s with its own pinned image: %v", version, err)
		}
	}
	// An empty version is the current release.
	spec := tunedReplicaSpec(t, "kubernetes")
	spec.RuntimeVersion = ""
	if _, err := provider.manifest(spec, ""); err != nil {
		t.Errorf("current release by default: %v", err)
	}
}

func TestAdaptersThatCannotWriteTunedFilesRefuseThem(t *testing.T) {
	ctx := context.Background()
	refused := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, ErrInvalidReplicaSpec) || !strings.Contains(err.Error(), "tuned kernel configs") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	_, err := SkyPilot{}.EnsureReplica(ctx, tunedReplicaSpec(t, "aws"))
	refused("SkyPilot", err)
	_, err = RunPodPods{APIKey: "provider", WorkerAPIKey: "worker"}.EnsureReplica(ctx, tunedReplicaSpec(t, "runpod"))
	refused("RunPod", err)
	_, err = AWSEC2{}.EnsureReplica(ctx, tunedReplicaSpec(t, "aws"))
	refused("AWS EC2", err)
	_, err = GCPCompute{}.EnsureReplica(ctx, tunedReplicaSpec(t, "gcp"))
	refused("GCP", err)
	dynamo := KubernetesDynamo{Context: "kind-infercrane", Namespace: "infercrane-system", ServiceAccount: "infercrane-runtime",
		VLLMImageDigest: "nvcr.io/nvidia/ai-dynamo/vllm-runtime:1.4.0@sha256:" + strings.Repeat("a", 64), VLLMRuntimeVersion: "1.4.0",
		ModelSecretName: "huggingface-models", GPUResource: "nvidia.com/gpu", GPUProductLabel: "nvidia.com/gpu.product", Runner: refusingRunner{t}}
	spec := tunedReplicaSpec(t, "kubernetes")
	spec.Serving = servingcontract.Topology{Backend: servingcontract.BackendDynamo, Profile: "baseline", Mode: servingcontract.ModeAggregated, Routing: servingcontract.RoutingDirect, Worker: servingcontract.Pool{Replicas: 1, TensorParallelism: 1}}
	_, err = dynamo.EnsureReplica(ctx, spec)
	refused("Dynamo", err)
}
