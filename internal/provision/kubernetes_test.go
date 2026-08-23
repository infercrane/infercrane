package provision_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/provision"
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

func jsonMarshalFixtureObjects(objects map[string]map[string]any) (string, error) {
	body, err := json.Marshal(objects)
	return string(body), err
}

func resourceNameFromHandle(handle provision.ProviderHandle) string {
	parts := strings.Split(handle.ResourceID, "/")
	return parts[len(parts)-1]
}
