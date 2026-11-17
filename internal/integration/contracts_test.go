package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/support"
)

func TestRegistryProducesDeterministicHonestSnapshot(t *testing.T) {
	registry, err := BaseCatalog()
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

func TestRequestSurvivalRequiresDelegationAndQualification(t *testing.T) {
	if err := (RequestSurvivalContract{State: CapabilitySupported, Mechanism: "backend-migration", Evidence: "test", Qualification: QualificationRegistered}).Validate(); err == nil {
		t.Fatal("unqualified survival accepted")
	}
	if err := (RequestSurvivalContract{State: CapabilitySupported, Mechanism: "backend-migration", Evidence: "qualified-suite", Qualification: QualificationLocal}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPortableCatalogPublishesExactRuntimeCompatibility(t *testing.T) {
	registry, err := PortableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := registry.Snapshot()
	if len(snapshot.Compatibility) != 8 {
		t.Fatalf("compatibility=%#v", snapshot.Compatibility)
	}
	encoded, _ := json.Marshal(snapshot)
	for _, required := range []string{`"runtime":"sglang"`, `"runtime":"custom-oci"`, `"adapter":"runpod-pods"`, `"adapter":"modal"`, `"adapter":"runpod-serverless-api"`, `"adapter":"fly-io"`, `"mode":"serverless"`, `"default_workload"`} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("missing %s: %s", required, encoded)
		}
	}
	if strings.Contains(string(encoded), `"state":"real-qualified"`) {
		t.Fatalf("fabricated real evidence: %s", encoded)
	}
}

func TestKubernetesCatalogPublishesKubernetesWithoutAdvancedRoutingClaims(t *testing.T) {
	registry, err := KubernetesCatalog()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := registry.Provider("kubernetes")
	if err != nil || profile.Cloud != "kubernetes" || len(profile.Modes) != 1 || profile.Modes[0] != ElasticMode {
		t.Fatalf("profile=%#v err=%v", profile, err)
	}
	encoded, _ := json.Marshal(registry.Snapshot())
	for _, required := range []string{`"kserve_standard"`, `"gateway_api_exposure"`, `"advanced_disaggregated_runtime","state":"unsupported"`, `"runtime":"vllm","adapter":"kubernetes","cloud":"kubernetes","mode":"elastic","state":"simulated"`, `"runtime":"vllm","adapter":"skypilot","cloud":"runpod","mode":"elastic"`, `"runtime":"vllm","adapter":"aws-ec2","cloud":"aws","mode":"elastic"`} {
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

func TestCompositionProfilesRequireEvidenceAndKnownKinds(t *testing.T) {
	profile := CompositionProfile{Adapter: "unsafe", Kind: CompositionCache, ContractVersion: CompositionV1, Ownership: "external", Capabilities: []Capability{{Name: "cache", State: CapabilitySupported}}, Qualification: []Qualification{{State: QualificationRegistered}}}
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "supported without evidence") {
		t.Fatalf("unsupported composition claim accepted: %v", err)
	}
	profile.Kind = "magic"
	profile.Capabilities[0].Evidence = "test"
	if err := profile.Validate(); err == nil || !strings.Contains(err.Error(), "composition kind") {
		t.Fatalf("unknown composition kind accepted: %v", err)
	}
}

func TestV1CatalogSeparatesProviderProfilesWithoutFabricatingQualification(t *testing.T) {
	registry, err := V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := registry.Snapshot()
	required := map[string]bool{"aws-ec2": false, "aws-asg": false, "aws-eks": false, "aws-sagemaker": false, "aws-bedrock": false, "gcp-compute": false, "gcp-mig": false, "gcp-gke": false, "gcp-vertex": false, "coreweave-cks": false}
	for _, profile := range snapshot.Providers {
		if _, ok := required[profile.Adapter]; ok {
			required[profile.Adapter] = true
		}
		if profile.Adapter != "aws-ec2" && profile.Adapter != "gcp-compute" && required[profile.Adapter] {
			for _, qualification := range profile.Qualification {
				if qualification.State == QualificationLocal || qualification.State == QualificationReal {
					t.Fatalf("profile %s fabricated qualification: %#v", profile.Adapter, qualification)
				}
			}
		}
	}
	for adapter, found := range required {
		if !found {
			t.Errorf("missing provider profile %s", adapter)
		}
	}
}

func TestV1CatalogPublishesDynamoAsSeparateLocallySimulatedBackend(t *testing.T) {
	registry, err := V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := registry.Provider("kubernetes-dynamo")
	if err != nil || profile.Cloud != "kubernetes" {
		t.Fatalf("profile=%#v err=%v", profile, err)
	}
	encoded, _ := json.Marshal(profile)
	for _, required := range []string{`"dgd_parent_lifecycle","state":"supported"`, `"disaggregated_serving","state":"unsupported"`, `"dynamo_planner_autoscaling","state":"unsupported"`, `"environment":"real-dynamo-gpu-kubernetes","reason"`} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("profile missing %s: %s", required, encoded)
		}
	}
	if strings.Contains(string(encoded), `"state":"real-qualified"`) {
		t.Fatalf("Dynamo profile fabricated real qualification: %s", encoded)
	}
}

func TestV1CatalogPublishesReplaceableOptimizationBoundariesWithoutExecutionClaims(t *testing.T) {
	registry, err := V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(registry.Snapshot())
	text := string(encoded)
	for _, required := range []string{
		`"adapter":"llm-compressor","kind":"artifact-builder"`,
		`"adapter":"modelopt","kind":"artifact-builder"`,
		`"adapter":"vllm-speculators","kind":"artifact-builder"`,
		`"adapter":"tensorrt-llm","kind":"artifact-builder"`,
		`"adapter":"lmcache","kind":"cache"`,
		`"adapter":"dynamo-nixl","kind":"orchestrator"`,
		`"adapter":"llm-d","kind":"orchestrator"`,
		`"adapter":"aibrix","kind":"orchestrator"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("catalog missing %s", required)
		}
	}
	if strings.Contains(text, `"runtime_execution","state":"supported"`) || strings.Contains(text, `"executable_lifecycle","state":"supported"`) {
		t.Fatalf("catalog fabricated executable optimization support: %s", text)
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
	registry, err := V1Catalog()
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

func TestCurrentCatalogIncludesExternalGatewayRuntimeProfiles(t *testing.T) {
	registry, err := V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"openai-compatible", "litellm"} {
		profile, lookupErr := registry.Runtime(name)
		if lookupErr != nil {
			t.Fatalf("runtime %s: %v", name, lookupErr)
		}
		capabilities := profile.ProtocolCapabilities()
		if !capabilities.ChatCompletions || !capabilities.Responses || !capabilities.Embeddings || !capabilities.Streaming {
			t.Fatalf("runtime %s capabilities = %+v", name, capabilities)
		}
	}
}

func TestProtocolCapabilitiesFailClosedAndProjectOnlySupportedClaims(t *testing.T) {
	profile := RuntimeProfile{Capabilities: []Capability{
		{Name: "buffered_chat", State: CapabilitySupported},
		{Name: "streaming_chat", State: CapabilitySupported},
		{Name: "responses", State: CapabilityUnknown},
		{Name: "embeddings", State: CapabilitySupported},
		{Name: "completions", State: CapabilityUnsupported},
	}}
	capabilities := profile.ProtocolCapabilities()
	if !capabilities.ChatCompletions || !capabilities.Streaming || !capabilities.Embeddings {
		t.Fatalf("supported capabilities were not projected: %+v", capabilities)
	}
	if capabilities.Responses || capabilities.Completions || capabilities.Batch {
		t.Fatalf("unknown or unsupported capabilities became routable: %+v", capabilities)
	}
}

func TestCapabilityEvidenceReferencesExistingTests(t *testing.T) {
	registry, err := V1Catalog()
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
	for _, composition := range registry.Snapshot().Compositions {
		capabilities = append(capabilities, composition.Capabilities...)
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

func TestSkyPilotProviderProfileDoesNotClaimGenericCapacityPreflight(t *testing.T) {
	profile := SkyPilotProviderProfile("lambda", "skypilot-lambda")
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if profile.Cloud != "lambda" || profile.Adapter != "skypilot-lambda" {
		t.Fatalf("unexpected profile: %#v", profile)
	}
	for _, capability := range profile.Capabilities {
		if capability.Name == "capacity_preflight" && capability.State != CapabilityUnknown {
			t.Fatalf("generic provider claimed capacity evidence: %#v", capability)
		}
	}
}

func TestSGLangQualificationCitesItsOnlyRecordedRealRun(t *testing.T) {
	registry, err := PortableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	profile, err := registry.Runtime("sglang")
	if err != nil {
		t.Fatal(err)
	}
	// Five-request AWS lifecycle runs are documented evidence, not retained
	// catalog evidence, so real-GPU qualification stays deferred and cites it.
	const evidence = "docs/testing/aws-real-evidence.md"
	cited := false
	for _, qualification := range profile.Qualification {
		if qualification.State == QualificationReal {
			t.Fatalf("SGLang catalog claims real qualification: %+v", qualification)
		}
		if qualification.State == QualificationDeferred && strings.Contains(qualification.Reason, evidence) && strings.Contains(qualification.Reason, "0.5.12") {
			cited = true
		}
	}
	body, err := os.ReadFile(filepath.Join("..", "..", evidence))
	if !cited || err != nil || !strings.Contains(string(body), "SGLang 0.5.12 | 5/5") {
		t.Fatalf("SGLang qualification does not match the recorded run: cited=%v err=%v", cited, err)
	}
}

// Measurement reads support.ServedWorkload to decide whether to scrape a
// replica's metrics; provisioning fills the registry's workload for the GPU.
// The two must name the same contract for every built-in runtime and GPU.
func TestServedWorkloadMatchesRegisteredRuntimeDefaults(t *testing.T) {
	registry, err := PortableCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{support.DefaultRuntime, "sglang", "custom-oci"} {
		profile, err := registry.Runtime(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, gpu := range []string{"", "H100", "MI300X", "AMD Instinct MI325X OAM", "MI355X"} {
			provisioned, err := profile.WorkloadFor(gpu)
			if err != nil {
				t.Fatalf("%s on %q: %v", name, gpu, err)
			}
			served := support.ServedWorkload(name, gpu, runtimecontract.Workload{})
			if served.Image != provisioned.Image || served.MetricsPath != provisioned.MetricsPath || served.Port != provisioned.Port {
				t.Fatalf("%s on %q: measurement contract %#v differs from the provisioned workload %#v", name, gpu, served, provisioned)
			}
		}
	}
}

func TestBuiltInRuntimesRecordEveryPinnedPlatformImage(t *testing.T) {
	registry, err := KubernetesCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{support.DefaultRuntime, "sglang"} {
		profile, err := registry.Runtime(name)
		if err != nil {
			t.Fatal(err)
		}
		var platforms []string
		for _, image := range profile.Images {
			platforms = append(platforms, image.Platform)
			if image.EngineVersion != profile.EngineVersion {
				t.Fatalf("%s %s image runs engine %s, profile %s", name, image.Platform, image.EngineVersion, profile.EngineVersion)
			}
		}
		if strings.Join(platforms, ",") != "cuda,rocm-gfx942,rocm-gfx950" || profile.DefaultWorkload.Image != profile.Images[0].Image {
			t.Fatalf("%s platforms=%v default=%q", name, platforms, profile.DefaultWorkload.Image)
		}
		// No real AMD run exists, so no platform image may claim one.
		for _, qualification := range profile.Qualification {
			if qualification.State == QualificationReal {
				t.Fatalf("%s claims real qualification: %+v", name, qualification)
			}
		}
		rocm, err := profile.WorkloadFor("MI300X")
		if err != nil || !strings.Contains(rocm.Image, "rocm") || strings.Join(rocm.Command, " ") != strings.Join(profile.DefaultWorkload.Command, " ") {
			t.Fatalf("%s MI300X workload=%#v err=%v", name, rocm, err)
		}
	}
}

func TestRuntimeProfileRejectsInconsistentPlatformImages(t *testing.T) {
	base := RuntimeProfile{Runtime: "vllm", ContractVersion: RuntimeContractV1, AdapterVersion: "builtin-v1", EngineVersion: support.DefaultRuntimeVersion, Protocol: "openai", DefaultWorkload: support.VLLMWorkload()}
	images := func() []support.RuntimeImage {
		var out []support.RuntimeImage
		for _, image := range support.RuntimeImages() {
			if image.Runtime == "vllm" {
				out = append(out, image)
			}
		}
		return out
	}
	valid := base
	valid.Images = images()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RuntimeProfile){
		"wrong engine":       func(p *RuntimeProfile) { p.Images[1].EngineVersion = "0.99.0" },
		"duplicate platform": func(p *RuntimeProfile) { p.Images[2].Platform = p.Images[1].Platform },
		"tag only":           func(p *RuntimeProfile) { p.Images[1].Image = "vllm/vllm-openai-rocm:v0.22.1" },
		"no CUDA default":    func(p *RuntimeProfile) { p.Images = p.Images[1:] },
		"other runtime":      func(p *RuntimeProfile) { p.Images[1].Runtime = "sglang" },
		"no default":         func(p *RuntimeProfile) { p.DefaultWorkload = runtimecontract.Workload{} },
	} {
		profile := base
		profile.Images = images()
		mutate(&profile)
		if err := profile.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	// A platform without an image is refused, never served the CUDA image.
	cudaOnly := base
	cudaOnly.Images = images()[:1]
	if _, err := cudaOnly.WorkloadFor("MI300X"); err == nil || !strings.Contains(err.Error(), "rocm-gfx942") {
		t.Fatalf("missing platform err=%v", err)
	}
	if workload, err := cudaOnly.WorkloadFor("H100"); err != nil || workload.Image != support.VLLMWorkload().Image {
		t.Fatalf("CUDA workload=%#v err=%v", workload, err)
	}
}
