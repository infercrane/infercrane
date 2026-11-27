package provideridentity

import (
	"strings"
	"testing"
)

func TestNotQualifiedAcceleratorsResolveToTheirOwnPlatformAndNothingElse(t *testing.T) {
	for _, test := range []struct {
		input, name, platform, resource string
		memory                          int
	}{
		{"TPU v5e", "TPU v5e", PlatformTPUV5E, DeviceResourceGoogleTPU, 16},
		{"v5e", "TPU v5e", PlatformTPUV5E, DeviceResourceGoogleTPU, 16},
		// The Cloud TPU accelerator type, SkyPilot's name, and GKE's label.
		{"v5litepod-8", "TPU v5e", PlatformTPUV5E, DeviceResourceGoogleTPU, 16},
		{"tpu-v5litepod-4", "TPU v5e", PlatformTPUV5E, DeviceResourceGoogleTPU, 16},
		{"tpu-v5-lite-podslice", "TPU v5e", PlatformTPUV5E, DeviceResourceGoogleTPU, 16},
		{"tpu-v5p-slice", "TPU v5p", PlatformTPUV5P, DeviceResourceGoogleTPU, 95},
		{"v5p-8", "TPU v5p", PlatformTPUV5P, DeviceResourceGoogleTPU, 95},
		{"Google Cloud TPU v6e", "TPU v6e", PlatformTPUV6E, DeviceResourceGoogleTPU, 32},
		{"tpu-v6e-slice", "TPU v6e", PlatformTPUV6E, DeviceResourceGoogleTPU, 32},
		{"tpu-v6e-8", "TPU v6e", PlatformTPUV6E, DeviceResourceGoogleTPU, 32},
		{"Trillium", "TPU v6e", PlatformTPUV6E, DeviceResourceGoogleTPU, 32},
		{"tpu7x", "TPU7x", PlatformTPUV7X, DeviceResourceGoogleTPU, 192},
		{"TPU v7x", "TPU7x", PlatformTPUV7X, DeviceResourceGoogleTPU, 192},
		{"Ironwood", "TPU7x", PlatformTPUV7X, DeviceResourceGoogleTPU, 192},
		{"tpu7x-16", "TPU7x", PlatformTPUV7X, DeviceResourceGoogleTPU, 192},
		{"Gaudi 2", "Gaudi 2", PlatformHPUGaudi2, DeviceResourceIntelGaudi, 96},
		{"Intel Gaudi 2 AI Accelerator", "Gaudi 2", PlatformHPUGaudi2, DeviceResourceIntelGaudi, 96},
		{"gaudi3", "Gaudi 3", PlatformHPUGaudi3, DeviceResourceIntelGaudi, 128},
		{"Intel Habana Gaudi 3", "Gaudi 3", PlatformHPUGaudi3, DeviceResourceIntelGaudi, 128},
		{"Intel Arc Pro B60 Graphics", "Arc Pro B60", PlatformXPUXe2, DeviceResourceIntelGPUXe, 24},
		{"arc-pro-b70", "Arc Pro B70", PlatformXPUXe2, DeviceResourceIntelGPUXe, 32},
		// Node label values cannot hold spaces; an operator writes underscores.
		{"Gaudi_3", "Gaudi 3", PlatformHPUGaudi3, DeviceResourceIntelGaudi, 128},
		{"Arc_Pro_B60", "Arc Pro B60", PlatformXPUXe2, DeviceResourceIntelGPUXe, 24},
	} {
		accelerator, ok := IdentifyAccelerator(test.input)
		if !ok || accelerator.Name != test.name || accelerator.Platform != test.platform || accelerator.DeviceResource != test.resource || accelerator.MemoryGiB != test.memory || accelerator.Status != AcceleratorNotQualified || accelerator.NativeFP8 {
			t.Fatalf("IdentifyAccelerator(%q)=%+v, %v", test.input, accelerator, ok)
		}
		if AcceleratorPlatform(test.input) != test.platform || ExpectedDeviceResource(test.input) != test.resource {
			t.Fatalf("%q resolved to platform %q resource %q", test.input, AcceleratorPlatform(test.input), ExpectedDeviceResource(test.input))
		}
		// Nothing that selects may see it: no serving lookup, no memory for
		// planning, no RunPod SKU.
		if _, serving := LookupAccelerator(test.input); serving {
			t.Fatalf("%q is served", test.input)
		}
		if _, known := AcceleratorMemoryGiB(test.input); known {
			t.Fatalf("%q has planning memory", test.input)
		}
		if GPUTypeID("runpod", test.input) != test.input {
			t.Fatalf("%q mapped to a RunPod SKU", test.input)
		}
	}
	// A slice size is digits only, and a name is never guessed from a
	// prefix or a generation number.
	for _, name := range []string{"tpu-v6e-8x", "v6", "tpu", "TPU v4", "v5", "tpu-v5-lite", "tpu7", "Ironwood GPU", "v6e8gb",
		// Gaudi 1 (AWS DL1) is not supported by the vLLM Gaudi plugin, and no
		// Intel GPU other than the Arc Pro B60 and B70 is reviewed.
		"Gaudi", "Gaudi 1", "gaudi4", "Gaudi 2 8", "Arc Pro B50", "Arc B580", "Data Center GPU Max 1550"} {
		if accelerator, ok := IdentifyAccelerator(name); ok {
			t.Fatalf("%q resolved to %+v", name, accelerator)
		}
	}
}

func TestServingAndNotQualifiedCatalogsAreDisjointCopies(t *testing.T) {
	serving, notQualified := Accelerators(), NotQualifiedAccelerators()
	if len(notQualified) == 0 {
		t.Fatal("no accelerator is recorded as not qualified")
	}
	platforms := map[string]string{}
	names := map[string]bool{}
	for _, accelerator := range append(serving, notQualified...) {
		if names[strings.ToLower(accelerator.Name)] {
			t.Fatalf("%s is listed twice", accelerator.Name)
		}
		names[strings.ToLower(accelerator.Name)] = true
		if status, seen := platforms[accelerator.Platform]; seen && status != accelerator.Status {
			t.Fatalf("platform %s mixes statuses", accelerator.Platform)
		}
		platforms[accelerator.Platform] = accelerator.Status
		if accelerator.Vendor == "" || accelerator.Product == "" || accelerator.Target == "" || accelerator.DeviceResource == "" || accelerator.MemoryGiB <= 0 || len(accelerator.Sources) < 2 {
			t.Fatalf("%s is incomplete: %+v", accelerator.Name, accelerator)
		}
		if !KnownDeviceResource(accelerator.DeviceResource) {
			t.Fatalf("%s's device resource %s is not known", accelerator.Name, accelerator.DeviceResource)
		}
		// Every spelling resolves through one identity.
		if identified, ok := IdentifyAccelerator(accelerator.Name); !ok || identified.Name != accelerator.Name {
			t.Fatalf("%s does not resolve to itself: %+v", accelerator.Name, identified)
		}
	}
	for _, accelerator := range serving {
		if accelerator.Status != AcceleratorServing {
			t.Fatalf("serving list holds %+v", accelerator)
		}
	}
	for _, accelerator := range notQualified {
		if accelerator.Status != AcceleratorNotQualified || accelerator.NativeFP8 {
			t.Fatalf("not-qualified list holds %+v", accelerator)
		}
	}
	notQualified[0].Sources[0] = "mutated"
	if NotQualifiedAccelerators()[0].Sources[0] == "mutated" {
		t.Fatal("the not-qualified catalog can be mutated through its accessor")
	}
}

func TestDeviceResourcesOfEveryReviewedPlatformAreChecked(t *testing.T) {
	// A cluster configured for TPUs refuses a GPU, and one configured for
	// NVIDIA refuses a TPU: both resources are known.
	for gpu, resource := range map[string]string{"tpu-v6e-slice": "google.com/tpu", "Gaudi 3": "habana.ai/gaudi", "Arc Pro B60": "gpu.intel.com/xe"} {
		if !KnownDeviceResource(resource) || ExpectedDeviceResource(gpu) != resource {
			t.Fatalf("%s: device resource %s is not checked", gpu, resource)
		}
	}
	// The i915 driver's resource serves GPUs that are not reviewed, so it
	// stays unchecked.
	if KnownDeviceResource("gpu.intel.com/i915") || ExpectedDeviceResource("H100") != DeviceResourceNVIDIA {
		t.Fatal("device resource recognition changed")
	}
}
