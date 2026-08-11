package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryProducesDeterministicHonestSnapshot(t *testing.T) {
	registry, err := V02Catalog()
	if err != nil {
		t.Fatal(err)
	}
	first, err := json.Marshal(registry.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	second, _ := json.Marshal(registry.Snapshot())
	if string(first) != string(second) {
		t.Fatalf("registry snapshot is not deterministic:\n%s\n%s", first, second)
	}
	encoded := string(first)
	for _, required := range []string{ProviderContractV1, RuntimeContractV1, "runpod-serverless", "openai-compatible-external", "deferred"} {
		if !strings.Contains(encoded, required) {
			t.Fatalf("snapshot missing %q: %s", required, encoded)
		}
	}
	if strings.Contains(encoded, `"state":"real-qualified"`) {
		t.Fatalf("catalog fabricates real qualification: %s", encoded)
	}
}

func TestV06CatalogPublishesExactRuntimeCompatibility(t *testing.T) {
	registry, err := V06Catalog()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := registry.Snapshot()
	if len(snapshot.Compatibility) != 5 {
		t.Fatalf("compatibility=%#v", snapshot.Compatibility)
	}
	encoded, _ := json.Marshal(snapshot)
	for _, required := range []string{`"runtime":"sglang"`, `"runtime":"custom-oci"`, `"mode":"serverless"`, `"default_workload"`} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("missing %s: %s", required, encoded)
		}
	}
	if strings.Contains(string(encoded), `"state":"real-qualified"`) {
		t.Fatalf("fabricated real evidence: %s", encoded)
	}
}

func TestV09CatalogPublishesKubernetesWithoutAdvancedRoutingClaims(t *testing.T) {
	registry, err := V09Catalog()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := registry.Provider("kubernetes")
	if err != nil || profile.Cloud != "kubernetes" || len(profile.Modes) != 1 || profile.Modes[0] != ElasticMode {
		t.Fatalf("profile=%#v err=%v", profile, err)
	}
	encoded, _ := json.Marshal(registry.Snapshot())
	for _, required := range []string{`"kserve_standard"`, `"gateway_api_exposure"`, `"advanced_disaggregated_runtime","state":"unsupported"`, `"runtime":"vllm","cloud":"kubernetes","mode":"elastic","state":"simulated"`, `"runtime":"vllm","cloud":"runpod","mode":"elastic"`, `"runtime":"vllm","cloud":"aws","mode":"elastic"`} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("missing %s: %s", required, encoded)
		}
	}
	if strings.Contains(string(encoded), `"environment":"real-kubernetes-gpu","evidence"`) {
		t.Fatalf("fabricated real Kubernetes evidence: %s", encoded)
	}
}

func TestProfilesRejectInvalidOrUnsupportedClaims(t *testing.T) {
	provider := ProviderProfile{Adapter: "bad", Cloud: "cloud", ContractVersion: ProviderContractV1, AdapterVersion: "1", Modes: []ComputeMode{ElasticMode}, Qualification: []Qualification{{State: QualificationReal}}}
	if err := provider.Validate(); err == nil || !strings.Contains(err.Error(), "without evidence") {
		t.Fatalf("real qualification without evidence was accepted: %v", err)
	}
	provider.Qualification = []Qualification{{State: QualificationDeferred}}
	if err := provider.Validate(); err == nil || !strings.Contains(err.Error(), "requires a reason") {
		t.Fatalf("deferred qualification without reason was accepted: %v", err)
	}
	runtime := RuntimeProfile{Runtime: "test", ContractVersion: "future", AdapterVersion: "1", Protocol: "openai"}
	if err := runtime.Validate(); err == nil || !strings.Contains(err.Error(), "unsupported contract") {
		t.Fatalf("unsupported runtime contract was accepted: %v", err)
	}
}

func TestSupportedCapabilityRequiresEvidence(t *testing.T) {
	profile := ProviderProfile{Adapter: "provider", Cloud: "cloud", ContractVersion: ProviderContractV1, AdapterVersion: "1", Modes: []ComputeMode{ElasticMode}, Capabilities: []Capability{{Name: "adoption", State: CapabilitySupported}}}
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "without evidence") {
		t.Fatalf("unsupported evidence claim was accepted: %v", err)
	}
}

func TestRegistryRejectsDuplicateAdapters(t *testing.T) {
	registry := NewRegistry()
	profile := ProviderProfile{Adapter: "adapter", Cloud: "cloud", ContractVersion: ProviderContractV1, AdapterVersion: "1", Modes: []ComputeMode{ElasticMode}}
	if err := registry.RegisterProvider(profile); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterProvider(profile); err == nil {
		t.Fatal("duplicate provider adapter was accepted")
	}
}

type contractRuntimeInspector struct{}

func (contractRuntimeInspector) Inspect(context.Context, string) (bool, map[string]struct{}) {
	return true, map[string]struct{}{}
}

func TestRuntimeBackendsBindValidatedProfileToImplementation(t *testing.T) {
	profile := RuntimeProfile{Runtime: "vllm", ContractVersion: RuntimeContractV1, AdapterVersion: "test", Protocol: "openai", Qualification: []Qualification{{State: QualificationSimulated, Environment: "hermetic"}}}
	backends, err := NewRuntimeBackends(RuntimeBackend{Profile: profile, Inspector: contractRuntimeInspector{}})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := backends.ForRuntime("vllm")
	if err != nil || backend.Profile.Runtime != "vllm" || backend.Inspector == nil {
		t.Fatalf("backend=%#v err=%v", backend, err)
	}
	if _, err := backends.ForRuntime("sglang"); err == nil {
		t.Fatal("expected unregistered runtime to be rejected")
	}
}

func TestRuntimeBackendsRejectInvalidComposition(t *testing.T) {
	profile := RuntimeProfile{Runtime: "vllm", ContractVersion: RuntimeContractV1, AdapterVersion: "test", Protocol: "openai", Qualification: []Qualification{{State: QualificationSimulated, Environment: "hermetic"}}}
	if _, err := NewRuntimeBackends(RuntimeBackend{Profile: profile}); err == nil {
		t.Fatal("expected missing inspector to be rejected")
	}
	if _, err := NewRuntimeBackends(
		RuntimeBackend{Profile: profile, Inspector: contractRuntimeInspector{}},
		RuntimeBackend{Profile: profile, Inspector: contractRuntimeInspector{}},
	); err == nil {
		t.Fatal("expected duplicate runtime composition to be rejected")
	}
}

func TestRegistryLooksUpProfilesWithoutExposingMutableMaps(t *testing.T) {
	registry, err := V09Catalog()
	if err != nil {
		t.Fatal(err)
	}
	provider, err := registry.Provider("skypilot")
	if err != nil || provider.Cloud != "runpod" || !HasMode(provider, ElasticMode) {
		t.Fatalf("provider=%+v err=%v", provider, err)
	}
	runtime, err := registry.Runtime("vllm")
	if err != nil || runtime.Protocol != "openai" {
		t.Fatalf("runtime=%+v err=%v", runtime, err)
	}
	if _, err = registry.Provider("missing"); err == nil {
		t.Fatal("missing provider lookup succeeded")
	}
}

func TestCapabilityEvidenceReferencesExistingTests(t *testing.T) {
	registry, err := V09Catalog()
	if err != nil {
		t.Fatal(err)
	}
	var capabilities []Capability
	for _, provider := range registry.Snapshot().Providers {
		capabilities = append(capabilities, provider.Capabilities...)
	}
	for _, runtime := range registry.Snapshot().Runtimes {
		capabilities = append(capabilities, runtime.Capabilities...)
	}
	for _, capability := range capabilities {
		if capability.State != CapabilitySupported {
			continue
		}
		if strings.HasPrefix(capability.Evidence, "script:") {
			path := strings.TrimPrefix(capability.Evidence, "script:")
			if _, err := os.Stat(filepath.Join("..", "..", path)); err != nil {
				t.Fatalf("supported capability %q references missing script %q", capability.Name, capability.Evidence)
			}
			continue
		}
		const prefix = "go:test/"
		if !strings.HasPrefix(capability.Evidence, prefix) || !strings.Contains(capability.Evidence, "#Test") {
			t.Fatalf("supported capability %q has non-test evidence %q", capability.Name, capability.Evidence)
		}
		parts := strings.SplitN(strings.TrimPrefix(capability.Evidence, prefix), "#", 2)
		packagePath, testName := parts[0], parts[1]
		files, err := filepath.Glob(filepath.Join("..", "..", packagePath, "*_test.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("evidence package %q for %q is unavailable: files=%v err=%v", packagePath, capability.Name, files, err)
		}
		found := false
		for _, file := range files {
			body, readErr := os.ReadFile(file)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if strings.Contains(string(body), "func "+testName+"(") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("supported capability %q references missing test %q", capability.Name, capability.Evidence)
		}
	}
}
