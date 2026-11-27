package provision_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/artifactcache"
	"github.com/infercrane/infercrane/internal/engineingress"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/support"
	"github.com/infercrane/infercrane/internal/testtools/providerfixture"
)

const testKubernetesImage = "vllm/vllm-openai@sha256:953d3a06d5e64ab582985cd7401289d3abf2a2c14ef2158e9a84313daeec77d7"

func testKubernetesProvider(runner provision.CommandRunner, api string) provision.Kubernetes {
	return provision.Kubernetes{Runner: runner, Context: "kind-infercrane", Namespace: "infercrane-system", WorkloadAPI: api, ServiceAccount: "infercrane-runtime", WorkerSecretName: "infercrane-worker", WorkerSecretKey: "api-key", ImageDigest: testKubernetesImage, GPUResource: "nvidia.com/gpu", GPUProductLabel: "nvidia.com/gpu.product"}
}

func testKubernetesSpec() provision.ReplicaSpec {
	return provision.ReplicaSpec{ExternalKey: "deployment-revision-r0", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", Cloud: "kubernetes", GPU: "NVIDIA-L40S", Port: 8000}
}

func TestKubernetesProviderAppliesValidatesAdoptsAndDeletesOwnerSet(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testKubernetesProvider(fixture, "deployment")
	spec := testKubernetesSpec()
	first, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || fixture.DryRuns != 1 || fixture.ApplyCalls != 1 || len(fixture.Objects) != 2 {
		t.Fatalf("handle=%#v dry=%d apply=%d objects=%#v err=%v", first, fixture.DryRuns, fixture.ApplyCalls, fixture.Objects, err)
	}
	second, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || first != second || fixture.ApplyCalls != 2 || len(fixture.Objects) != 2 {
		t.Fatalf("replay handle=%#v apply=%d err=%v", second, fixture.ApplyCalls, err)
	}
	manifest, err := json.Marshal(fixture.Objects)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), `"name":"VLLM_API_KEY"`) || !strings.Contains(string(manifest), `"args":["serve","Qwen/Qwen3-8B"`) || strings.Contains(string(manifest), `"--api-key"`) || strings.Contains(string(manifest), `"--model"`) {
		t.Fatalf("vLLM credential must be sourced from Secret-backed environment and absent from argv: %s", manifest)
	}
	observation, err := provider.ObserveReplica(context.Background(), second, 8000)
	if err != nil || !observation.Exists || observation.State != "ready" || !strings.Contains(observation.Endpoint, ".infercrane-system.svc:8000") || !strings.Contains(observation.Details, `"kind":"Deployment"`) {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	resources, err := provider.Inventory(context.Background(), provision.InventoryFilter{Prefix: "deployment"})
	if err != nil || len(resources) != 1 || resources[0].ExternalKey != spec.ExternalKey || resources[0].State != "ready" {
		t.Fatalf("resources=%#v err=%v", resources, err)
	}
	if err = provider.DeleteReplica(context.Background(), second); err != nil || fixture.DeleteCalls != 1 || len(fixture.Objects) != 0 {
		t.Fatalf("delete calls=%d objects=%#v err=%v", fixture.DeleteCalls, fixture.Objects, err)
	}
	if err = provider.DeleteReplica(context.Background(), second); err != nil || fixture.DeleteCalls != 1 {
		t.Fatalf("delete replay calls=%d err=%v", fixture.DeleteCalls, err)
	}
}

func TestKubernetesPortableWorkloadUsesExactTopologyAndExecutableArgv(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testKubernetesProvider(fixture, "deployment")
	spec := testKubernetesSpec()
	spec.GPU, spec.GPUCount, spec.Runtime = "H200", 4, "custom-oci"
	spec.Workload = runtimecontract.Workload{Image: "registry.example/runtime@sha256:" + strings.Repeat("b", 64), Command: []string{"vllm", "serve", "${MODEL}"}, Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 120}
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(fixture.Objects)
	for _, expected := range []string{`"nvidia.com/gpu":"4"`, `"command":["vllm"]`, `"args":["serve","Qwen/Qwen3-8B"]`, `"terminationGracePeriodSeconds":120`} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("manifest missing %q: %s", expected, encoded)
		}
	}
}

func TestKubernetesAMDDevicePluginSchedulesAMDGPUsWithTheirROCmImage(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testKubernetesProvider(fixture, "deployment")
	provider.GPUResource, provider.GPUProductLabel = "amd.com/gpu", "amd.com/gpu.product-name"
	spec := testKubernetesSpec()
	// The AMD node labeller writes the sysfs product name with spaces as
	// underscores; that label value is the GPU the replica asks for.
	spec.GPU, spec.Runtime = "AMD_Instinct_MI300X_OAM", "vllm"
	spec.Workload, _ = support.RuntimeWorkload("vllm", "rocm-gfx942")
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(fixture.Objects)
	for _, expected := range []string{`"amd.com/gpu":"1"`, `"amd.com/gpu.product-name":"AMD_Instinct_MI300X_OAM"`, `"image":"vllm/vllm-openai-rocm@sha256:3cb773d2`, `"command":["vllm"]`} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("manifest missing %q: %s", expected, encoded)
		}
	}
	for _, test := range []struct{ resource, gpu string }{
		{"nvidia.com/gpu", "MI300X"},
		{"nvidia.com/gpu", "AMD_Instinct_MI355X"},
		{"amd.com/gpu", "NVIDIA-L40S"},
	} {
		fixture := providerfixture.NewKubernetesCLI()
		provider := testKubernetesProvider(fixture, "deployment")
		provider.GPUResource = test.resource
		spec := testKubernetesSpec()
		spec.GPU = test.gpu
		if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "INFERCRANE_KUBERNETES_GPU_RESOURCE") || fixture.ApplyCalls != 0 {
			t.Fatalf("%s on %s: err=%v apply=%d", test.gpu, test.resource, err, fixture.ApplyCalls)
		}
	}
	// A cluster whose device plugin exposes TPUs, Gaudi, or Intel GPUs still
	// refuses them: those platforms are not qualified. A GPU of another
	// platform is refused by the device check as before.
	for _, test := range []struct{ resource, gpu, want string }{
		{"google.com/tpu", "tpu-v6e-slice", "not qualified"},
		{"habana.ai/gaudi", "Gaudi 3", "not qualified"},
		{"gpu.intel.com/xe", "Arc Pro B60", "not qualified"},
		{"nvidia.com/gpu", "tpu7x", "not qualified"},
		{"google.com/tpu", "H100", "INFERCRANE_KUBERNETES_GPU_RESOURCE"},
		{"habana.ai/gaudi", "NVIDIA-L40S", "INFERCRANE_KUBERNETES_GPU_RESOURCE"},
	} {
		fixture := providerfixture.NewKubernetesCLI()
		provider := testKubernetesProvider(fixture, "deployment")
		provider.GPUResource = test.resource
		spec := testKubernetesSpec()
		spec.GPU = test.gpu
		if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || !strings.Contains(err.Error(), test.want) || fixture.ApplyCalls != 0 {
			t.Fatalf("%s on %s: err=%v apply=%d", test.gpu, test.resource, err, fixture.ApplyCalls)
		}
	}
	// A resource whose platform is not known, such as a MIG slice, is not
	// second-guessed.
	mig := testKubernetesProvider(providerfixture.NewKubernetesCLI(), "deployment")
	mig.GPUResource = "nvidia.com/mig-1g.10gb"
	if _, err := mig.EnsureReplica(context.Background(), testKubernetesSpec()); err != nil {
		t.Fatal(err)
	}
}

func TestKubernetesProviderAdoptsLostResponseAndRepairsPartialOwnerSet(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	fixture.FailAfterApplyOnce = true
	provider := testKubernetesProvider(fixture, "deployment")
	spec := testKubernetesSpec()
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || fixture.ApplyCalls != 1 || len(fixture.Objects) != 2 {
		t.Fatalf("expected lost response: applies=%d objects=%#v err=%v", fixture.ApplyCalls, fixture.Objects, err)
	}
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil || fixture.ApplyCalls != 2 || len(fixture.Objects) != 2 {
		t.Fatalf("adoption applied again: applies=%d err=%v", fixture.ApplyCalls, err)
	}
	name := resourceNameFromHandle(provider.Handle(spec.ExternalKey))
	delete(fixture.Objects, "service/"+name)
	resources, err := provider.Inventory(context.Background(), provision.InventoryFilter{})
	if err != nil || len(resources) != 1 || resources[0].State != "degraded" {
		t.Fatalf("partial inventory=%#v err=%v", resources, err)
	}
	if _, err = provider.EnsureReplica(context.Background(), spec); err != nil || fixture.ApplyCalls != 3 || len(fixture.Objects) != 2 {
		t.Fatalf("repair applies=%d objects=%#v err=%v", fixture.ApplyCalls, fixture.Objects, err)
	}
}

func TestKubernetesProviderRefusesOwnershipAndFieldConflicts(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testKubernetesProvider(fixture, "deployment")
	spec := testKubernetesSpec()
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	name := resourceNameFromHandle(provider.Handle(spec.ExternalKey))
	metadata := fixture.Objects["deployment/"+name]["metadata"].(map[string]any)
	metadata["annotations"].(map[string]any)["infercrane.dev/external-key"] = "someone-else"
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "conflicts with durable key ownership") {
		t.Fatalf("ownership conflict err=%v", err)
	}
	delete(fixture.Objects, "deployment/"+name)
	delete(fixture.Objects, "service/"+name)
	fixture.ApplyFailure = true
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || strings.Contains(err.Error(), "field conflict") {
		t.Fatalf("field conflict must be classified and redacted: %v", err)
	}
}

func TestKubernetesKServeModeRequiresCRDAndUsesOneOwner(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	fixture.KServeInstalled = false
	provider := testKubernetesProvider(fixture, "kserve")
	if err := provider.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "discover KServe API") {
		t.Fatalf("missing CRD err=%v", err)
	}
	fixture.KServeInstalled = true
	if err := provider.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	handle, err := provider.EnsureReplica(context.Background(), testKubernetesSpec())
	if err != nil || len(fixture.Objects) != 1 || !strings.Contains(handle.ResourceID, "kubernetes:kserve") {
		t.Fatalf("handle=%#v objects=%#v err=%v", handle, fixture.Objects, err)
	}
	observation, err := provider.ObserveReplica(context.Background(), handle, 8000)
	if err != nil || observation.State != "ready" || !strings.HasPrefix(observation.Endpoint, "http://") {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
}

func TestKubernetesDoesNotAcceptReadinessFromStaleObservedGeneration(t *testing.T) {
	for _, api := range []string{"deployment", "kserve"} {
		t.Run(api, func(t *testing.T) {
			fixture := providerfixture.NewKubernetesCLI()
			provider := testKubernetesProvider(fixture, api)
			spec := testKubernetesSpec()
			handle, err := provider.EnsureReplica(context.Background(), spec)
			if err != nil {
				t.Fatal(err)
			}
			name := resourceNameFromHandle(handle)
			kind := "deployment"
			if api == "kserve" {
				kind = "inferenceservice"
			}
			fixture.Objects[kind+"/"+name]["metadata"].(map[string]any)["generation"] = float64(2)
			observation, err := provider.ObserveReplica(context.Background(), handle, 8000)
			if err != nil {
				t.Fatal(err)
			}
			if observation.State != "provisioning" || observation.Endpoint != "" {
				t.Fatalf("stale status was accepted as current readiness: %#v", observation)
			}
		})
	}
}

func TestKubernetesConfigurationAndManifestFailClosed(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testKubernetesProvider(fixture, "deployment")
	provider.Context = ""
	if _, err := provider.EnsureReplica(context.Background(), testKubernetesSpec()); err == nil {
		t.Fatal("missing context accepted")
	}
	provider = testKubernetesProvider(fixture, "deployment")
	provider.ImageDigest = "vllm/vllm-openai:latest"
	if _, err := provider.EnsureReplica(context.Background(), testKubernetesSpec()); err == nil {
		t.Fatal("mutable image accepted")
	}
	provider.ImageDigest = testKubernetesImage
	if _, err := provider.EnsureReplica(context.Background(), testKubernetesSpec()); err != nil {
		t.Fatal(err)
	}
	encoded, _ := jsonMarshalFixtureObjects(fixture.Objects)
	for _, required := range []string{testKubernetesImage, `"secretKeyRef"`, `"name":"infercrane-worker"`, `"nvidia.com/gpu":"1"`, `"strategy":{"type":"Recreate"}`} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("manifest missing %s: %s", required, encoded)
		}
	}
	if strings.Contains(encoded, "credential-value") {
		t.Fatalf("manifest contains secret value: %s", encoded)
	}
}

func TestKubernetesRequiredImagePrewarmFailsClosedWithoutRegistryPull(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testKubernetesProvider(fixture, "deployment")
	provider.ImageCachePolicy = "required"
	if _, err := provider.EnsureReplica(context.Background(), testKubernetesSpec()); err != nil {
		t.Fatal(err)
	}
	encoded, _ := jsonMarshalFixtureObjects(fixture.Objects)
	if !strings.Contains(encoded, `"imagePullPolicy":"Never"`) {
		t.Fatalf("required prewarm policy can still pull from registry: %s", encoded)
	}
}

func TestKubernetesArtifactPVCIsVerifiedMountedAndObservable(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testKubernetesProvider(fixture, "deployment")
	spec := testKubernetesSpec()
	spec.ModelRevision = "0123456789abcdef0123456789abcdef01234567"
	identity := spec.Model + "@" + spec.ModelRevision
	claim := "qwen3-immutable-cache"
	provider.ArtifactCachePolicy = "required"
	provider.ArtifactPVCs = map[string]string{identity: claim}
	fixture.Objects["persistentvolumeclaim/"+claim] = map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": claim, "namespace": "infercrane-system", "annotations": map[string]any{"infercrane.dev/model-identity-digest": "sha256:b89562e9fdfc74f318d6870cc672993f6ededab8985a0689bc2c9f97b7414977"}},
		"spec":     map[string]any{"accessModes": []any{"ReadOnlyMany"}, "volumeName": "pvc-volume-qwen3"},
		"status":   map[string]any{"phase": "Bound", "capacity": map[string]any{"storage": "40Gi"}},
	}
	request := artifactcache.Request{ArtifactID: "artifact-qwen3", ModelIdentity: identity, Provider: "kubernetes", Region: provider.Context, Location: "kubernetes-pvc://" + claim, IdempotencyKey: "prefetch-qwen3"}
	operation, err := provider.Prefetch(context.Background(), request)
	if err != nil || operation.Status != "succeeded" || operation.ProviderOperationID != "infercrane-system/"+claim {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	observation, err := provider.Observe(context.Background(), request)
	if err != nil || observation.State != "present" || observation.Source != "kubernetes-pvc" || !strings.Contains(observation.EvidenceJSON, `"mount":"read-only"`) {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	if _, err = provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	encoded, _ := jsonMarshalFixtureObjects(fixture.Objects)
	for _, required := range []string{`"claimName":"qwen3-immutable-cache"`, `"mountPath":"/models"`, `"readOnly":true`, `"name":"HF_HOME"`, `"value":"/models/huggingface"`} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("cache mount missing %s: %s", required, encoded)
		}
	}
}

func TestKubernetesArtifactPVCFailsClosedBeforeWorkloadMutation(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testKubernetesProvider(fixture, "deployment")
	spec := testKubernetesSpec()
	spec.ModelRevision = "0123456789abcdef0123456789abcdef01234567"
	provider.ArtifactCachePolicy = "required"
	provider.ArtifactPVCs = map[string]string{spec.Model + "@" + spec.ModelRevision: "missing-cache"}
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || fixture.ApplyCalls != 0 {
		t.Fatalf("missing required PVC reached apply: calls=%d err=%v", fixture.ApplyCalls, err)
	}
	request := artifactcache.Request{ArtifactID: "artifact", ModelIdentity: spec.Model + "@" + spec.ModelRevision, Provider: "kubernetes", Region: provider.Context, Location: "kubernetes-pvc://other-cache", IdempotencyKey: "prefetch"}
	if _, err := provider.Prefetch(context.Background(), request); err == nil {
		t.Fatal("unconfigured cache location was adopted")
	} else if code, unknown := artifactcache.Classify(err); code != "artifact_cache_not_configured" || unknown {
		t.Fatalf("cache failure classification code=%q unknown=%t err=%v", code, unknown, err)
	}
}

func TestKubernetesRequiredArtifactCacheRejectsUnqualifiedCustomRuntime(t *testing.T) {
	provider := testKubernetesProvider(providerfixture.NewKubernetesCLI(), "deployment")
	provider.ArtifactCachePolicy = "required"
	provider.ArtifactPVCs = map[string]string{"Qwen/Qwen3-8B@0123456789abcdef0123456789abcdef01234567": "qwen3-cache"}
	spec := testKubernetesSpec()
	spec.ModelRevision = "0123456789abcdef0123456789abcdef01234567"
	spec.Runtime = "custom-oci"
	spec.Workload = runtimecontract.Workload{Image: "registry.example/runtime@sha256:" + strings.Repeat("b", 64), Command: []string{"serve", "${MODEL}"}, Port: 8000, ReadinessPath: "/health", ShutdownGraceSeconds: 30}
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "qualified only for vLLM and SGLang") {
		t.Fatalf("required cache accepted an unqualified custom runtime: %v", err)
	}
}

func jsonMarshalFixtureObjects(objects map[string]map[string]any) (string, error) {
	body, err := json.Marshal(objects)
	return string(body), err
}

func resourceNameFromHandle(handle provision.ProviderHandle) string {
	parts := strings.Split(handle.ResourceID, "/")
	return parts[len(parts)-1]
}

// An SGLang server's key launcher reaches the container unchanged: the
// script is one argument, Kubernetes leaves $(mktemp -d) alone because it is
// not a variable name, and the key comes from the worker Secret, not argv.
func TestKubernetesStartsSGLangThroughTheKeyLauncher(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testKubernetesProvider(fixture, "deployment")
	spec := testKubernetesSpec()
	spec.Runtime = "sglang"
	spec.Workload = support.WithEngineKey("sglang", support.SGLangWorkload())
	if _, err := provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Command []string `json:"command"`
						Args    []string `json:"args"`
						Env     []struct {
							Name      string         `json:"name"`
							ValueFrom map[string]any `json:"valueFrom"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	for _, object := range fixture.Objects {
		body, _ := json.Marshal(object)
		if strings.Contains(string(body), `"kind":"Deployment"`) {
			if err := json.Unmarshal(body, &manifest); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(manifest.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("no runtime container")
	}
	container := manifest.Spec.Template.Spec.Containers[0]
	argv := strings.Join(append(container.Command, container.Args...), "\x00")
	if !strings.Contains(argv, "infercrane-engine-key") || !strings.Contains(argv, `dir=$(mktemp -d)`) || !strings.Contains(argv, "sglang.launch_server") {
		t.Fatalf("container argv %q", argv)
	}
	if strings.Contains(argv, "--api-key") {
		t.Fatalf("the key is passed in argv: %q", argv)
	}
	secret := false
	for _, env := range container.Env {
		secret = secret || env.Name == "INFERCRANE_WORKER_API_KEY" && env.ValueFrom != nil
	}
	if !secret {
		t.Fatalf("worker key env %+v", container.Env)
	}
}

// With engine ingress sources configured, each replica gets a NetworkPolicy
// that admits only those sources, and only on the engine's port. The policy
// is created, observed and deleted with the Deployment and Service, and a
// replica created before the policy existed stays ready until the next
// ensure applies it.
func TestKubernetesEngineNetworkPolicyAdmitsOnlyConfiguredSources(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := testKubernetesProvider(fixture, "deployment")
	provider.EngineIngress = []engineingress.Peer{
		{PodLabels: map[string]string{"app.kubernetes.io/name": "infercrane"}},
		{NamespaceLabels: map[string]string{"kubernetes.io/metadata.name": "gateway"}, PodLabels: map[string]string{"app": "router"}},
		{CIDR: "10.8.0.0/16"},
	}
	// The preflight asks for the policy permission only when a policy is made.
	fixture.DeniedResources = []string{"networkpolicies.networking.k8s.io"}
	if err := provider.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "denies get networkpolicies.networking.k8s.io") {
		t.Fatalf("check without the policy permission: %v", err)
	}
	if err := testKubernetesProvider(fixture, "deployment").Check(context.Background()); err != nil {
		t.Fatalf("check without sources: %v", err)
	}
	fixture.DeniedResources = nil
	if err := provider.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	spec := testKubernetesSpec()
	handle, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || len(fixture.Objects) != 3 {
		t.Fatalf("handle=%#v objects=%d err=%v", handle, len(fixture.Objects), err)
	}
	name := resourceNameFromHandle(handle)
	policy := fixture.Objects["networkpolicy/"+name]
	if policy == nil {
		t.Fatalf("no NetworkPolicy among %v", fixture.Objects)
	}
	encoded, _ := json.Marshal(policy["spec"])
	want := `{"ingress":[{"from":[{"podSelector":{"matchLabels":{"app.kubernetes.io/name":"infercrane"}}},` +
		`{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"gateway"}},"podSelector":{"matchLabels":{"app":"router"}}},` +
		`{"ipBlock":{"cidr":"10.8.0.0/16"}}],"ports":[{"port":8000,"protocol":"TCP"}]}],` +
		`"podSelector":{"matchLabels":{"app.kubernetes.io/instance":"` + name + `"}},"policyTypes":["Ingress"]}`
	if string(encoded) != want {
		t.Fatalf("policy spec\n got %s\nwant %s", encoded, want)
	}
	observation, err := provider.ObserveReplica(context.Background(), handle, 8000)
	if err != nil || observation.State != "ready" {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	// A replica from before the policy is still a complete owner set; the
	// next ensure adds the policy.
	delete(fixture.Objects, "networkpolicy/"+name)
	if observation, err = provider.ObserveReplica(context.Background(), handle, 8000); err != nil || observation.State != "ready" {
		t.Fatalf("without policy: observation=%#v err=%v", observation, err)
	}
	if _, err = provider.EnsureReplica(context.Background(), spec); err != nil || fixture.Objects["networkpolicy/"+name] == nil {
		t.Fatalf("ensure did not restore the policy: objects=%v err=%v", fixture.Objects, err)
	}
	if err = provider.DeleteReplica(context.Background(), handle); err != nil || len(fixture.Objects) != 0 {
		t.Fatalf("delete objects=%v err=%v", fixture.Objects, err)
	}
	// KServe owns its own pods, and without sources no policy is made.
	for _, test := range []struct {
		api   string
		peers []engineingress.Peer
	}{{"deployment", nil}, {"kserve", provider.EngineIngress}} {
		fixture := providerfixture.NewKubernetesCLI()
		other := testKubernetesProvider(fixture, test.api)
		other.EngineIngress = test.peers
		if _, err := other.EnsureReplica(context.Background(), spec); err != nil {
			t.Fatalf("%s: %v", test.api, err)
		}
		for key := range fixture.Objects {
			if strings.HasPrefix(key, "networkpolicy/") {
				t.Fatalf("%s: unexpected policy %s", test.api, key)
			}
		}
	}
}

// A replica's Service address is known before it is ready for a Deployment,
// so a server that stopped serving can still be named; KServe assigns its
// own address, so none is given.
func TestServiceAddressIsFixedByTheExternalKey(t *testing.T) {
	deployments := provision.Kubernetes{Namespace: "models"}
	handle := provision.ProviderHandle{ExternalKey: "agent-serve:support-bot:0123456789abcdef:8000"}
	address := deployments.ServiceAddress(handle, 0)
	if !strings.HasPrefix(address, "http://infercrane-") || !strings.HasSuffix(address, ".models.svc:8000") || deployments.ServiceAddress(handle, 8000) != address || deployments.ServiceAddress(handle, 9000) == address {
		t.Fatalf("address = %q", address)
	}
	if other := deployments.ServiceAddress(provision.ProviderHandle{ExternalKey: "agent-serve:support-bot:fedcba9876543210:8000"}, 8000); other == address {
		t.Fatal("two external keys share an address")
	}
	if got := (provision.Kubernetes{Namespace: "models", WorkloadAPI: "kserve"}).ServiceAddress(handle, 8000); got != "" {
		t.Fatalf("kserve address = %q", got)
	}
	if got := deployments.ServiceAddress(provision.ProviderHandle{}, 8000); got != "" {
		t.Fatalf("empty key address = %q", got)
	}
}
