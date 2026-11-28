package integration

import (
	"slices"
	"testing"
)

// The catalog names what kubernetes-dynamo needs beside its cluster but cannot
// know whether a cluster runs it; only composition records that.
func TestDynamoDeclaresItsPrerequisiteAndOnlyCompositionConfiguresIt(t *testing.T) {
	registry, err := V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	dynamo, err := registry.Provider("kubernetes-dynamo")
	if err != nil {
		t.Fatal(err)
	}
	if dynamo.Prerequisite.Name != "NVIDIA Dynamo" || len(dynamo.Prerequisite.ConfiguredRuntimes) != 0 || dynamo.PrerequisiteConfigured("vllm") {
		t.Fatalf("catalog prerequisite=%+v", dynamo.Prerequisite)
	}
	plain, err := registry.Provider("kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Prerequisite.Name != "" || !plain.PrerequisiteConfigured("vllm") {
		t.Fatalf("plain Kubernetes needs nothing beside its cluster: %+v", plain.Prerequisite)
	}

	before := registry.Snapshot()
	if err = registry.ConfigurePrerequisite("kubernetes-dynamo", "vllm", "vllm"); err != nil {
		t.Fatal(err)
	}
	if err = registry.ConfigurePrerequisite("kubernetes-dynamo", "sglang"); err != nil {
		t.Fatal(err)
	}
	dynamo, _ = registry.Provider("kubernetes-dynamo")
	if !slices.Equal(dynamo.Prerequisite.ConfiguredRuntimes, []string{"sglang", "vllm"}) || !dynamo.PrerequisiteConfigured("sglang") || dynamo.PrerequisiteConfigured("custom-oci") {
		t.Fatalf("configured prerequisite=%+v", dynamo.Prerequisite)
	}
	if err = dynamo.Validate(); err != nil {
		t.Fatalf("a configured profile must still validate for its backends: %v", err)
	}
	for _, profile := range before.Providers {
		if profile.Adapter == "kubernetes-dynamo" && len(profile.Prerequisite.ConfiguredRuntimes) != 0 {
			t.Fatalf("an earlier snapshot changed: %+v", profile.Prerequisite)
		}
	}

	other := ProviderProfile{Adapter: "kubernetes-other", Cloud: "kubernetes", ContractVersion: ProviderContractV1, AdapterVersion: "builtin-v1", Modes: []ComputeMode{ElasticMode}, Prerequisite: Prerequisite{Name: "Other"}}
	ready, unnamed := other, other
	ready.Prerequisite.ConfiguredRuntimes = []string{"vllm"}
	unnamed.Prerequisite = Prerequisite{ConfiguredRuntimes: []string{"vllm"}}
	for name, configure := range map[string]func() error{
		"no prerequisite":    func() error { return registry.ConfigurePrerequisite("kubernetes", "vllm") },
		"unknown adapter":    func() error { return registry.ConfigurePrerequisite("kubernetes-other", "vllm") },
		"unknown runtime":    func() error { return registry.ConfigurePrerequisite("kubernetes-dynamo", "not-a-runtime") },
		"registered ready":   func() error { return registry.RegisterProvider(ready) },
		"unnamed configured": unnamed.Validate,
	} {
		if configure() == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if err = registry.RegisterProvider(other); err != nil {
		t.Fatalf("a declared, unconfigured prerequisite registers: %v", err)
	}
}
