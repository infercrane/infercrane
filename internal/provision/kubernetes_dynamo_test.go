package provision_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/servingcontract"
	"github.com/infercrane/infercrane/internal/testtools/providerfixture"
)

const testDynamoImage = "nvcr.io/nvidia/ai-dynamo/vllm-runtime:1.4.0@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const testDynamoSGLangImage = "nvcr.io/nvidia/ai-dynamo/sglang-runtime:1.4.0@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func testDynamoProvider(runner provision.CommandRunner) provision.KubernetesDynamo {
	return provision.KubernetesDynamo{
		Runner: runner, Context: "kind-infercrane", Namespace: "infercrane-system",
		ServiceAccount: "infercrane-runtime", VLLMImageDigest: testDynamoImage, VLLMRuntimeVersion: "1.4.0",
		SGLangImageDigest: testDynamoSGLangImage, SGLangRuntimeVersion: "1.4.0",
		ModelSecretName: "huggingface-models", GPUResource: "nvidia.com/gpu",
		GPUProductLabel: "nvidia.com/gpu.product",
	}
}

func testDynamoSpec() provision.ReplicaSpec {
	return provision.ReplicaSpec{
		ExternalKey: "deployment-revision-r0", Model: "meta-llama/Llama-3.1-8B-Instruct",
		ModelRevision: "immutable", Cloud: "kubernetes", GPU: "NVIDIA-L40S", Runtime: "vllm", Port: 8000,
		Serving: servingcontract.Topology{
			Backend: servingcontract.BackendDynamo, Profile: "baseline",
			Mode: servingcontract.ModeAggregated, Routing: servingcontract.RoutingDirect,
			Worker: servingcontract.Pool{Replicas: 1, TensorParallelism: 1},
		},
	}
}

func TestKubernetesDynamoLifecycleIsReplaySafeAndOwnsOneParent(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testDynamoProvider(fixture)
	if err := provider.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	spec := testDynamoSpec()
	handle, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || fixture.DryRuns != 1 || fixture.ApplyCalls != 1 || len(fixture.Objects) != 1 {
		t.Fatalf("handle=%#v dry=%d apply=%d objects=%#v err=%v", handle, fixture.DryRuns, fixture.ApplyCalls, fixture.Objects, err)
	}
	replayed, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || replayed != handle || fixture.ApplyCalls != 2 || len(fixture.Objects) != 1 {
		t.Fatalf("replayed=%#v apply=%d objects=%#v err=%v", replayed, fixture.ApplyCalls, fixture.Objects, err)
	}
	observation, err := provider.ObserveReplica(context.Background(), handle, 8000)
	if err != nil || observation.State != "ready" || observation.Endpoint == "" || !strings.Contains(observation.Endpoint, "-frontend.infercrane-system.svc:8000") {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	resources, err := provider.Inventory(context.Background(), provision.InventoryFilter{Prefix: "deployment"})
	if err != nil || len(resources) != 1 || resources[0].State != "ready" {
		t.Fatalf("resources=%#v err=%v", resources, err)
	}
	if err = provider.DeleteReplica(context.Background(), handle); err != nil || len(fixture.Objects) != 0 {
		t.Fatalf("objects=%#v err=%v", fixture.Objects, err)
	}
	if err = provider.DeleteReplica(context.Background(), handle); err != nil || fixture.DeleteCalls != 1 {
		t.Fatalf("delete replay calls=%d err=%v", fixture.DeleteCalls, err)
	}
}

func TestKubernetesDynamoAdoptsLostApplyResponseAndRejectsStaleReadiness(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	fixture.FailAfterApplyOnce = true
	provider := testDynamoProvider(fixture)
	spec := testDynamoSpec()
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || len(fixture.Objects) != 1 {
		t.Fatalf("lost response was not reproduced: objects=%#v err=%v", fixture.Objects, err)
	}
	handle, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || len(fixture.Objects) != 1 {
		t.Fatalf("adoption failed: objects=%#v err=%v", fixture.Objects, err)
	}
	name := resourceNameFromHandle(handle)
	fixture.Objects["dynamographdeployment/"+name]["metadata"].(map[string]any)["generation"] = float64(2)
	observation, err := provider.ObserveReplica(context.Background(), handle, 8000)
	if err != nil || observation.State != "provisioning" || observation.Endpoint != "" {
		t.Fatalf("stale status accepted: observation=%#v err=%v", observation, err)
	}
}

func TestKubernetesDynamoFailureDominatesStaleReadyCondition(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testDynamoProvider(fixture)
	spec := testDynamoSpec()
	handle, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	name := resourceNameFromHandle(handle)
	status := fixture.Objects["dynamographdeployment/"+name]["status"].(map[string]any)
	status["state"] = "failed"
	observation, err := provider.ObserveReplica(context.Background(), handle, 8000)
	if err != nil || observation.State != "failed" || observation.Endpoint != "" {
		t.Fatalf("conflicting terminal state was routed: observation=%#v err=%v", observation, err)
	}
}

func TestKubernetesDynamoRejectsChangedIntentForSameDurableKey(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testDynamoProvider(fixture)
	spec := testDynamoSpec()
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	spec.Model = "Qwen/Qwen3-14B"
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "different immutable intent") {
		t.Fatalf("changed intent was accepted: %v", err)
	}
	if fixture.ApplyCalls != 1 {
		t.Fatalf("changed intent reached Kubernetes: apply calls=%d", fixture.ApplyCalls)
	}
}

func TestKubernetesDynamoManifestMakesTopologyAndSecretsExplicit(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testDynamoProvider(fixture)
	spec := testDynamoSpec()
	spec.Serving.Routing = servingcontract.RoutingKVAware
	spec.Serving.Cache = servingcontract.Cache{
		Backend: servingcontract.CacheKVBM, HostGiB: 32, MemoryGiB: 64,
		DiskGiB: 64, StorageClaim: "llama-kv-cache", Metrics: true,
	}
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(fixture.Objects)
	encoded := string(body)
	for _, required := range []string{
		`"kind":"DynamoGraphDeployment"`, `"backendFramework":"vllm"`,
		`"runtimeVersionOverride":"1.4.0"`, `"dynamo.frontend"`, `"DYN_ROUTER_MODE","value":"kv"`,
		`"DYN_KVBM_CPU_CACHE_GB","value":"32"`, `"claimName":"llama-kv-cache"`,
		`"secretRef":{"name":"huggingface-models"}`, `"--served-model-name"`,
		`"image":"` + testDynamoImage + `"`,
	} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("manifest missing %s: %s", required, encoded)
		}
	}
	if strings.Contains(encoded, "hf_") || strings.Contains(encoded, "credential-value") {
		t.Fatalf("manifest persisted a secret value: %s", encoded)
	}
}

func TestKubernetesDynamoDisaggregatedVLLMAndSGLangFailClosedBeforeMutation(t *testing.T) {
	for _, runtimeName := range []string{"vllm", "sglang"} {
		t.Run(runtimeName, func(t *testing.T) {
			fixture := providerfixture.NewKubernetesCLI()
			provider := testDynamoProvider(fixture)
			spec := testDynamoSpec()
			spec.Runtime = runtimeName
			spec.Serving.Mode = servingcontract.ModeDisaggregated
			spec.Serving.Worker = servingcontract.Pool{}
			spec.Serving.Prefill = servingcontract.Pool{Replicas: 1, TensorParallelism: 1}
			spec.Serving.Decode = servingcontract.Pool{Replicas: 2, TensorParallelism: 1}
			if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "registered for argument translation") {
				t.Fatalf("%s disaggregation did not fail closed: %v", runtimeName, err)
			}
			if len(fixture.Objects) != 0 || fixture.ApplyCalls != 0 {
				t.Fatalf("%s deferred topology reached Kubernetes: objects=%#v apply=%d", runtimeName, fixture.Objects, fixture.ApplyCalls)
			}
		})
	}
}

func TestKubernetesDynamoFailsClosedForUnqualifiedOrUnsafeConfiguration(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testDynamoProvider(fixture)
	fixture.DynamoInstalled = false
	if err := provider.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing CRD error=%v", err)
	}
	fixture.DynamoInstalled = true
	provider.VLLMImageDigest = "nvcr.io/nvidia/ai-dynamo/vllm-runtime:latest"
	if _, err := provider.EnsureReplica(context.Background(), testDynamoSpec()); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("mutable image accepted: %v", err)
	}
	provider = testDynamoProvider(fixture)
	provider.SGLangImageDigest, provider.SGLangRuntimeVersion = "", ""
	missingRuntime := testDynamoSpec()
	missingRuntime.Runtime = "sglang"
	if _, err := provider.EnsureReplica(context.Background(), missingRuntime); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured runtime was accepted: %v", err)
	}
	provider = testDynamoProvider(fixture)
	spec := testDynamoSpec()
	spec.Serving.Autoscaling = servingcontract.Autoscaling{Owner: servingcontract.AutoscalingDynamoPlanner, Min: 1, Max: 4}
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("unqualified planner accepted: %v", err)
	}
	spec = testDynamoSpec()
	spec.Serving.Cache = servingcontract.Cache{Backend: servingcontract.CacheLMCache, ConfigurationRef: "lmcache-prod"}
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("unqualified LMCache accepted: %v", err)
	}
}
