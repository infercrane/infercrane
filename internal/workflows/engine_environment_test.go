package workflows

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/support"
)

func TestConvergePassesRuntimeEnvOnlyThroughAdaptersThatDeclareIt(t *testing.T) {
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runtimeProfile, err := registry.Runtime(support.DefaultRuntime)
	if err != nil {
		t.Fatal(err)
	}
	podsProfile, err := registry.Provider("runpod-pods")
	if err != nil {
		t.Fatal(err)
	}
	converge := func(t *testing.T, profile integration.ProviderProfile) (*fakeReplicaProvider, *fakeCloudStore, error) {
		t.Helper()
		store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", Model: "Qwen/Qwen3-8B", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1}}
		provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready", Endpoint: "http://gpu:8000", Details: `{}`}}
		backends, err := NewReplicaBackends(ReplicaBackend{Name: "runpod-pods", Cloud: "runpod", Runtime: "vllm", Profile: profile, Provider: provider})
		if err != nil {
			t.Fatal(err)
		}
		runtimes, err := integration.NewRuntimeBackends(integration.RuntimeBackend{Profile: runtimeProfile, Inspector: fakeInspector{ready: true}})
		if err != nil {
			t.Fatal(err)
		}
		operation := domain.Operation{ID: "operation-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","provider_adapter":"runpod-pods","gpu":"MI300X","tenant_id":"global","min_replicas":1,"max_replicas":1,"runtime_env":{"VLLM_ROCM_USE_AITER":"1"}}`}
		_, err = CloudHandlersWithBackendsAndDrain(store, backends, runtimes, &fakeDrainTracker{current: true})[ConvergeKind](context.Background(), operation)
		return provider, store, err
	}
	provider, _, err := converge(t, podsProfile)
	pinned, _ := support.RuntimeWorkload("vllm", "rocm-gfx942")
	if err != nil || provider.ensureCalls != 1 || provider.lastSpec.RuntimeEnv["VLLM_ROCM_USE_AITER"] != "1" || provider.lastSpec.Workload.Image != pinned.Image {
		t.Fatalf("runpod-pods: ensure_calls=%d spec=%+v err=%v", provider.ensureCalls, provider.lastSpec, err)
	}
	// An adapter that does not declare the capability is refused before any
	// provider call, so no replica ever starts without its reviewed switch.
	provider, store, err := converge(t, testElasticProfile("runpod-pods", "runpod"))
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Code != "runtime_env_unsupported" || failure.Retryable || provider.ensureCalls != 0 || store.target.Name != "" {
		t.Fatalf("undeclared adapter: failure=%+v ensure_calls=%d target=%+v err=%v", failure, provider.ensureCalls, store.target, err)
	}
}

func TestEveryRegisteredAdapterPassesOrRefusesRuntimeEnv(t *testing.T) {
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"VLLM_ROCM_USE_AITER": "1"}
	passes := map[string]bool{}
	for _, profile := range registry.Snapshot().Providers {
		passes[profile.Adapter] = refuseUnpassedRuntimeEnv(profile.Adapter, profile, env) == nil
		if refuseUnpassedRuntimeEnv(profile.Adapter, profile, nil) != nil {
			t.Fatalf("%s refused a revision without runtime env", profile.Adapter)
		}
	}
	for adapter, want := range map[string]bool{"runpod-pods": true, "kubernetes": true, "aws-ec2": true, "gcp-compute": true, "skypilot": false, "runpod-serverless": false, "kubernetes-dynamo": false} {
		if got, registered := passes[adapter]; !registered || got != want {
			t.Fatalf("%s passes runtime env=%t registered=%t, want %t", adapter, got, registered, want)
		}
	}
	// A declared SkyPilot cloud refuses it too.
	skyPilot := integration.SkyPilotProviderProfile("lambda", "skypilot-lambda")
	if err := refuseUnpassedRuntimeEnv(skyPilot.Adapter, skyPilot, env); err == nil || !strings.Contains(err.Error(), "skypilot-lambda") {
		t.Fatalf("SkyPilot cloud err=%v", err)
	}
}

func TestCloudRequestAdmitsOnlyReviewedRuntimeEnv(t *testing.T) {
	request := func(gpu string, env map[string]string) CloudRequest {
		return CloudRequest{Name: "qwen", Model: "Qwen/Qwen3-8B", Cloud: "runpod", ProviderAdapter: "runpod-pods", GPU: gpu, MinReplicas: 1, MaxReplicas: 1, RuntimeEnv: env}
	}
	for _, gpu := range []string{"MI300X", "AMD Instinct MI300X OAM", "MI350X"} {
		valid := request(gpu, map[string]string{"VLLM_ROCM_USE_AITER": "1"})
		if err := valid.Validate(); err != nil {
			t.Fatalf("%s: %v", gpu, err)
		}
	}
	for name, test := range map[string]CloudRequest{
		"CUDA GPU":   request("H100", map[string]string{"VLLM_ROCM_USE_AITER": "1"}),
		"credential": request("MI300X", map[string]string{"HF_TOKEN": "hf_secret"}),
		"value":      request("MI300X", map[string]string{"VLLM_ROCM_USE_AITER": "yes"}),
	} {
		if err := test.Validate(); err == nil || strings.Contains(err.Error(), "hf_secret") {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	sglang := request("MI300X", map[string]string{"VLLM_ROCM_USE_AITER": "1"})
	sglang.Runtime, sglang.Cloud, sglang.ProviderAdapter = "sglang", "kubernetes", "kubernetes"
	if err := sglang.Validate(); err == nil {
		t.Fatal("SGLang accepted a vLLM switch")
	}
}

// A built-in vLLM request runs a pinned release: the current one when it
// names none, the legacy one when it records it, and never another release
// under the current image. Serverless and custom images bring their own.
func TestCloudRequestRunsOnlyPinnedEngineReleases(t *testing.T) {
	request := func(version string) CloudRequest {
		return CloudRequest{Name: "qwen", Model: "Qwen/Qwen3-8B", Cloud: "runpod", ProviderAdapter: "runpod-pods", GPU: "H100", MinReplicas: 1, MaxReplicas: 1, RuntimeVersion: version}
	}
	for version, want := range map[string]string{"": support.DefaultRuntimeVersion, "0.34.0": "0.34.0", "v0.34.0": "0.34.0", "0.22.1": support.LegacyVLLMRuntimeVersion} {
		valid := request(version)
		if err := valid.Validate(); err != nil || valid.RuntimeVersion != want {
			t.Fatalf("%q: version=%q err=%v", version, valid.RuntimeVersion, err)
		}
	}
	unpinned := request("0.30.0")
	if err := unpinned.Validate(); err == nil || !strings.Contains(err.Error(), "runtime vllm 0.30.0 is not pinned") {
		t.Fatalf("unpinned release err=%v", err)
	}
	serverless := request("0.10.2")
	serverless.ComputeMode, serverless.ProviderAdapter, serverless.MinReplicas = "serverless", "", 0
	if err := serverless.Validate(); err != nil {
		t.Fatalf("serverless runs the template's release: %v", err)
	}
	custom := request("2.0.0")
	custom.Runtime, custom.Workload = "custom-oci", support.VLLMWorkload()
	if err := custom.Validate(); err != nil || custom.RuntimeVersion != "2.0.0" {
		t.Fatalf("custom image version=%q err=%v", custom.RuntimeVersion, err)
	}
	// The release decides the image provisioning fills in; serverless takes
	// only the contract's paths from the current release.
	if legacy := request("0.22.1"); legacy.imageRelease() != "0.22.1" {
		t.Fatalf("legacy image release %q", legacy.imageRelease())
	}
	if serverless.imageRelease() != "" {
		t.Fatalf("serverless image release %q", serverless.imageRelease())
	}
}

// An SGLang request stores the image of its release: the current one when
// it names none, the legacy one when it records it; an unpinned release is
// refused instead of stored under another release's image.
func TestCloudRequestStoresTheSGLangReleaseImage(t *testing.T) {
	request := func(version string) CloudRequest {
		return CloudRequest{Name: "qwen", Model: "Qwen/Qwen3-8B", Runtime: "sglang", Cloud: "kubernetes", ProviderAdapter: "kubernetes", GPU: "H100", MinReplicas: 1, MaxReplicas: 1, RuntimeVersion: version}
	}
	for version, release := range map[string]string{"": support.SGLangRuntimeVersion, "v0.5.23": "0.5.23", "0.5.12": support.LegacySGLangRuntimeVersion} {
		valid := request(version)
		pinned, _ := support.PinnedImage("sglang", release, "cuda")
		if err := valid.Validate(); err != nil || valid.RuntimeVersion != release || valid.Workload.Image != pinned.Image {
			t.Fatalf("%q: version=%q image=%q err=%v", version, valid.RuntimeVersion, valid.Workload.Image, err)
		}
	}
	unpinned := request("0.5.30")
	if err := unpinned.Validate(); err == nil || !strings.Contains(err.Error(), "runtime sglang 0.5.30 is not pinned") {
		t.Fatalf("unpinned SGLang release err=%v", err)
	}
	// A stored 0.5.12 revision keeps its image on every later operation.
	legacy := request("0.5.12")
	stored, _ := support.RuntimeWorkloadFor("sglang", "0.5.12", "cuda")
	legacy.Workload = stored
	if err := legacy.Validate(); err != nil || legacy.Workload.Image != stored.Image {
		t.Fatalf("stored legacy workload image=%q err=%v", legacy.Workload.Image, err)
	}
}

func TestCloudRequestCarriesOnlyATunedSetThatFitsAndKeepsItsEvidenceApart(t *testing.T) {
	shape := kerneltuning.FusedMoEShape{Experts: 256, Intermediate: 512, DType: "fp8_w8a8", BlockN: 128, BlockK: 128}
	file, err := kerneltuning.NewFile(shape, "NVIDIA_H100_80GB_HBM3", map[int]kerneltuning.TileConfig{128: {BlockSizeM: 16, BlockSizeN: 64, BlockSizeK: 128, GroupSizeM: 1, NumWarps: 4, NumStages: 3}})
	if err != nil {
		t.Fatal(err)
	}
	set := kerneltuning.NewSet("vllm", support.DefaultRuntimeVersion, "NVIDIA_H100_80GB_HBM3", []domain.KernelConfigFile{file})
	request := CloudRequest{Name: "qwen", Model: "Qwen/Qwen3.6-35B-A3B-FP8", Cloud: "kubernetes", ProviderAdapter: "kubernetes", GPU: "H100", MinReplicas: 1, MaxReplicas: 1, KernelConfigs: &set}
	if err = request.Validate(); err != nil {
		t.Fatal(err)
	}
	untuned := request
	untuned.KernelConfigs = nil
	if runtimeDigest(request) == runtimeDigest(untuned) {
		t.Fatal("tuned and untuned replicas share capacity evidence")
	}
	if runtimeDigest(untuned) != support.RuntimeArgsDigest(untuned.RuntimeArgs, untuned.RuntimeEnv) {
		t.Fatal("a replica without a set changed its evidence digest")
	}
	other := request
	other.GPU = "H200"
	if err = other.Validate(); err == nil || !strings.Contains(err.Error(), "kernel configs") {
		t.Fatalf("a set tuned on an H100 was accepted for an H200: %v", err)
	}
}

// A deployment's extra engine arguments are a reviewed set on a built-in
// runtime's image; refused flags are refused on any image.
func TestCloudRequestChecksEngineArguments(t *testing.T) {
	request := func(args ...string) CloudRequest {
		return CloudRequest{Name: "qwen", Model: "Qwen/Qwen3-8B", Cloud: "runpod", ProviderAdapter: "runpod-pods", GPU: "H100", MinReplicas: 1, MaxReplicas: 1, RuntimeArgs: args}
	}
	if valid := request("--max-num-seqs", "256", "--enable-prefix-caching"); valid.Validate() != nil {
		t.Fatalf("reviewed arguments: %v", valid.Validate())
	}
	for args, want := range map[string]string{
		"--trust-remote-code":             "--trust-remote-code is refused",
		"--enable-custom-logit-processor": "is refused",
		"--host":                          "--host is not reviewed for vllm",
		"--load-format":                   "--load-format is not reviewed",
	} {
		refused := request(args)
		if err := refused.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err=%v, want %q", args, err, want)
		}
	}
	custom := request("--whatever-the-image-takes")
	custom.Runtime, custom.Workload = "custom-oci", support.VLLMWorkload()
	if err := custom.Validate(); err != nil {
		t.Fatalf("custom image arguments: %v", err)
	}
	custom.RuntimeArgs = []string{"--trust-remote-code"}
	if err := custom.Validate(); err == nil || !strings.Contains(err.Error(), "is refused") {
		t.Fatalf("custom image remote code err=%v", err)
	}
}

// An SGLang deployment's server starts with the worker key; a stored revision
// gets it the next time it is provisioned, and vLLM is unchanged.
func TestCloudRequestStartsSGLangWithTheWorkerKey(t *testing.T) {
	request := CloudRequest{Name: "qwen", Model: "Qwen/Qwen3-8B", Runtime: "sglang", Cloud: "kubernetes", ProviderAdapter: "kubernetes", GPU: "H100", MinReplicas: 1, MaxReplicas: 1}
	if err := request.Validate(); err != nil || !support.EngineKeyRequired(request.Workload) {
		t.Fatalf("new SGLang request command=%v err=%v", request.Workload.Command, err)
	}
	stored := request
	stored.Workload = support.SGLangWorkload()
	if err := stored.Validate(); err != nil || !support.EngineKeyRequired(stored.Workload) {
		t.Fatalf("stored SGLang revision command=%v err=%v", stored.Workload.Command, err)
	}
	if err := stored.Validate(); err != nil || strings.Count(strings.Join(stored.Workload.Command, " "), "infercrane-engine-key") != 1 {
		t.Fatalf("validated twice: %v err=%v", stored.Workload.Command, err)
	}
	// A tenant's own SGLang command behind a prefix gets the launcher, and the
	// command after it is unchanged.
	prefixed := request
	prefixed.Workload = support.SGLangWorkload()
	own := append([]string{"numactl", "--interleave=all"}, prefixed.Workload.Command...)
	prefixed.Workload.Command = own
	if err := prefixed.Validate(); err != nil || !support.EngineKeyRequired(prefixed.Workload) || strings.Join(prefixed.Workload.Command[4:], " ") != strings.Join(own, " ") {
		t.Fatalf("prefixed SGLang command=%v err=%v", prefixed.Workload.Command, err)
	}
	vllm := CloudRequest{Name: "qwen", Model: "Qwen/Qwen3-8B", Cloud: "kubernetes", ProviderAdapter: "kubernetes", GPU: "H100", MinReplicas: 1, MaxReplicas: 1}
	if err := vllm.Validate(); err != nil || support.EngineKeyRequired(vllm.Workload) {
		t.Fatalf("vLLM request command=%v err=%v", vllm.Workload.Command, err)
	}
}
