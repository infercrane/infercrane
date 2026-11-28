package integration

import (
	"slices"
	"strings"
	"testing"
)

// dynamoSettings are the settings a control plane names for its Dynamo images.
var dynamoSettings = map[string][]string{
	"vllm":   {"VLLM_IMAGE", "VLLM_VERSION"},
	"sglang": {"SGLANG_IMAGE", "SGLANG_VERSION"},
}

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
	// The catalog cannot know, so it never says a runtime is missing.
	if dynamo.Prerequisite.Known() || dynamo.PrerequisiteMissing("vllm") || dynamo.PrerequisiteGap("vllm") != "" {
		t.Fatalf("the catalog claims to know the control plane's configuration: %+v", dynamo.Prerequisite)
	}
	plain, err := registry.Provider("kubernetes")
	if err != nil {
		t.Fatal(err)
	}
	if plain.Prerequisite.Name != "" || !plain.PrerequisiteConfigured("vllm") {
		t.Fatalf("plain Kubernetes needs nothing beside its cluster: %+v", plain.Prerequisite)
	}

	before := registry.Snapshot()
	if err = registry.ConfigurePrerequisite("kubernetes-dynamo", map[string][]string{"vllm": dynamoSettings["vllm"]}, "vllm", "vllm"); err != nil {
		t.Fatal(err)
	}
	if err = registry.ConfigurePrerequisite("kubernetes-dynamo", map[string][]string{"sglang": dynamoSettings["sglang"]}, "sglang"); err != nil {
		t.Fatal(err)
	}
	dynamo, _ = registry.Provider("kubernetes-dynamo")
	if !slices.Equal(dynamo.Prerequisite.ConfiguredRuntimes, []string{"sglang", "vllm"}) || !dynamo.PrerequisiteConfigured("sglang") || dynamo.PrerequisiteConfigured("custom-oci") {
		t.Fatalf("configured prerequisite=%+v", dynamo.Prerequisite)
	}
	if !slices.Equal(dynamo.Prerequisite.Settings["vllm"], dynamoSettings["vllm"]) || !slices.Equal(dynamo.Prerequisite.Settings["sglang"], dynamoSettings["sglang"]) {
		t.Fatalf("settings were not merged: %+v", dynamo.Prerequisite.Settings)
	}
	if err = dynamo.Validate(); err != nil {
		t.Fatalf("a configured profile must still validate for its backends: %v", err)
	}
	for _, profile := range before.Providers {
		if profile.Adapter == "kubernetes-dynamo" && (len(profile.Prerequisite.ConfiguredRuntimes) != 0 || profile.Prerequisite.Settings != nil) {
			t.Fatalf("an earlier snapshot changed: %+v", profile.Prerequisite)
		}
	}

	other := ProviderProfile{Adapter: "kubernetes-other", Cloud: "kubernetes", ContractVersion: ProviderContractV1, AdapterVersion: "builtin-v1", Modes: []ComputeMode{ElasticMode}, Prerequisite: Prerequisite{Name: "Other"}}
	ready, named, unnamed, unexplained := other, other, other, other
	ready.Prerequisite.ConfiguredRuntimes = []string{"vllm"}
	named.Prerequisite.Settings = dynamoSettings
	unnamed.Prerequisite = Prerequisite{ConfiguredRuntimes: []string{"vllm"}}
	unexplained.Prerequisite = Prerequisite{Name: "Other", ConfiguredRuntimes: []string{"vllm"}, Settings: map[string][]string{"sglang": {"SGLANG_IMAGE"}}}
	configure := registry.ConfigurePrerequisite
	for name, refused := range map[string]func() error{
		"no prerequisite":     func() error { return configure("kubernetes", dynamoSettings, "vllm") },
		"unknown adapter":     func() error { return configure("kubernetes-other", dynamoSettings, "vllm") },
		"unknown runtime":     func() error { return configure("kubernetes-dynamo", map[string][]string{"not-a-runtime": {"X"}}) },
		"no settings":         func() error { return configure("kubernetes-dynamo", nil) },
		"empty settings":      func() error { return configure("kubernetes-dynamo", map[string][]string{"vllm": nil}) },
		"configured unnamed":  func() error { return configure("kubernetes-dynamo", dynamoSettings, "custom-oci") },
		"registered ready":    func() error { return registry.RegisterProvider(ready) },
		"registered settings": func() error { return registry.RegisterProvider(named) },
		"unnamed configured":  unnamed.Validate,
		"configured unset":    unexplained.Validate,
	} {
		if refused() == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	if err = registry.RegisterProvider(other); err != nil {
		t.Fatalf("a declared, unconfigured prerequisite registers: %v", err)
	}
}

// A control plane that records its configuration with no runtime configured
// knows that every runtime is missing, and says which settings supply each.
func TestAKnownPrerequisiteNamesTheSettingsAMissingRuntimeNeeds(t *testing.T) {
	registry, err := V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.ConfigurePrerequisite("kubernetes-dynamo", dynamoSettings); err != nil {
		t.Fatalf("a control plane without a configured runtime records its settings: %v", err)
	}
	dynamo, _ := registry.Provider("kubernetes-dynamo")
	if !dynamo.Prerequisite.Known() || len(dynamo.Prerequisite.ConfiguredRuntimes) != 0 || !dynamo.PrerequisiteMissing("vllm") || !dynamo.PrerequisiteMissing("custom-oci") {
		t.Fatalf("prerequisite=%+v", dynamo.Prerequisite)
	}
	if err = dynamo.Validate(); err != nil {
		t.Fatal(err)
	}
	for runtimes, want := range map[string]string{
		"sglang":             "this control plane has no NVIDIA Dynamo image configured for sglang (set SGLANG_IMAGE and SGLANG_VERSION)",
		"vllm,sglang,vllm":   "this control plane has no NVIDIA Dynamo image configured for sglang or vllm (set SGLANG_IMAGE, SGLANG_VERSION, VLLM_IMAGE, and VLLM_VERSION)",
		"custom-oci":         "this control plane has no NVIDIA Dynamo image configured for custom-oci",
		"custom-oci,unknown": "this control plane has no NVIDIA Dynamo image configured for custom-oci or unknown",
	} {
		if got := dynamo.PrerequisiteGap(strings.Split(runtimes, ",")...); got != want {
			t.Fatalf("PrerequisiteGap(%s)=%q want %q", runtimes, got, want)
		}
	}

	if err = registry.ConfigurePrerequisite("kubernetes-dynamo", dynamoSettings, "vllm"); err != nil {
		t.Fatal(err)
	}
	dynamo, _ = registry.Provider("kubernetes-dynamo")
	if dynamo.PrerequisiteMissing("vllm") || dynamo.PrerequisiteGap("vllm") != "" || dynamo.PrerequisiteGap("vllm", "sglang") != "this control plane has no NVIDIA Dynamo image configured for sglang (set SGLANG_IMAGE and SGLANG_VERSION)" {
		t.Fatalf("configured vllm: %+v", dynamo.Prerequisite)
	}
	plain, _ := registry.Provider("kubernetes")
	if plain.PrerequisiteMissing("vllm") || plain.PrerequisiteGap("vllm") != "" {
		t.Fatal("an adapter without a prerequisite misses nothing")
	}
}

// A setup on an adapter that serves through software beside its cloud is not a
// plain server, even where that software runs; every other adapter's is.
func TestOnlyAnAdapterWithoutAPrerequisiteServesPlainServers(t *testing.T) {
	registry, err := V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.ConfigurePrerequisite("kubernetes-dynamo", dynamoSettings, "vllm"); err != nil {
		t.Fatal(err)
	}
	for _, profile := range registry.Snapshot().Providers {
		if profile.PlainServer() != (profile.Prerequisite.Name == "") {
			t.Fatalf("%s: plain=%v prerequisite=%+v", profile.Adapter, profile.PlainServer(), profile.Prerequisite)
		}
	}
	for adapter, plain := range map[string]bool{"kubernetes": true, "runpod-pods": true, "kubernetes-dynamo": false} {
		profile, providerErr := registry.Provider(adapter)
		if providerErr != nil || profile.PlainServer() != plain {
			t.Fatalf("%s: plain=%v err=%v", adapter, profile.PlainServer(), providerErr)
		}
	}
}
