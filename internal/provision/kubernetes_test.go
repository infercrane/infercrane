package provision_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/artifactcache"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/testtools/providerfixture"
)

const testKubernetesImage = "vllm/vllm-openai@sha256:0fec7ec5f3e6bc168e54899935fb0557da908a4832a1dbc88e2debcf2f889416"

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
