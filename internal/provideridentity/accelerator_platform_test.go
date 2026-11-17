package provideridentity

import "testing"

func TestReviewedAMDAcceleratorsResolveEverySpellingToOneIdentity(t *testing.T) {
	for _, test := range []struct {
		input, name, platform, target string
		memory                        int
	}{
		{"MI300X", "MI300X", PlatformROCmGFX942, "gfx942", 192},
		{"mi300x", "MI300X", PlatformROCmGFX942, "gfx942", 192},
		{"AMD MI300X", "MI300X", PlatformROCmGFX942, "gfx942", 192},
		{"AMD Instinct MI300X", "MI300X", PlatformROCmGFX942, "gfx942", 192},
		// RunPod's GPU type ID and the AMD node labeller's product-name label.
		{"AMD Instinct MI300X OAM", "MI300X", PlatformROCmGFX942, "gfx942", 192},
		{"AMD_Instinct_MI300X_OAM", "MI300X", PlatformROCmGFX942, "gfx942", 192},
		{"MI325X", "MI325X", PlatformROCmGFX942, "gfx942", 256},
		{"AMD Instinct MI325X OAM", "MI325X", PlatformROCmGFX942, "gfx942", 256},
		{"MI350X", "MI350X", PlatformROCmGFX950, "gfx950", 288},
		{"AMD Instinct MI350 OAM", "MI350X", PlatformROCmGFX950, "gfx950", 288},
		{"MI355X", "MI355X", PlatformROCmGFX950, "gfx950", 288},
		{"AMD_Instinct_MI355X", "MI355X", PlatformROCmGFX950, "gfx950", 288},
	} {
		accelerator, ok := LookupAccelerator(test.input)
		if !ok || accelerator.Name != test.name || accelerator.Platform != test.platform || accelerator.Target != test.target || accelerator.MemoryGiB != test.memory || accelerator.Vendor != "amd" || accelerator.DeviceResource != DeviceResourceAMD || !accelerator.NativeFP8 {
			t.Fatalf("LookupAccelerator(%q)=%+v, %v", test.input, accelerator, ok)
		}
		if AcceleratorPlatform(test.input) != test.platform || ExpectedDeviceResource(test.input) != DeviceResourceAMD {
			t.Fatalf("platform or device resource of %q is wrong", test.input)
		}
		if len(accelerator.Sources) < 2 {
			t.Fatalf("%s has no cited sources", accelerator.Name)
		}
	}
}

func TestUnreviewedNamesStayOnTheDefaultPlatform(t *testing.T) {
	// A name outside the reviewed list is never guessed from a model number:
	// it keeps today's CUDA images and NVIDIA device resource.
	for _, name := range []string{"H100", "NVIDIA H100 80GB HBM3", "L40S", "MI250X", "MI300A", "MI3000X", "AMD", "", "  "} {
		if _, ok := LookupAccelerator(name); ok {
			t.Fatalf("%q resolved to a reviewed accelerator", name)
		}
		if AcceleratorPlatform(name) != PlatformCUDA || ExpectedDeviceResource(name) != DeviceResourceNVIDIA {
			t.Fatalf("%q left the default platform", name)
		}
	}
}

func TestAcceleratorsReturnsACopy(t *testing.T) {
	list := Accelerators()
	if len(list) != 4 || list[0].Name != "MI300X" || list[3].Name != "MI355X" {
		t.Fatalf("accelerators=%+v", list)
	}
	list[0].Sources[0] = "mutated"
	list[0].MemoryGiB = 1
	if again := Accelerators(); again[0].Sources[0] == "mutated" || again[0].MemoryGiB != 192 {
		t.Fatal("the reviewed catalog can be mutated through its accessor")
	}
}

func TestKnownDeviceResources(t *testing.T) {
	if !KnownDeviceResource("amd.com/gpu") || !KnownDeviceResource(" nvidia.com/gpu ") || KnownDeviceResource("nvidia.com/mig-1g.10gb") {
		t.Fatal("device resource recognition changed")
	}
}

func TestRunPodAMDAliasesResolveToExactProviderSKU(t *testing.T) {
	for input, expected := range map[string]string{
		"MI300X":                  "AMD Instinct MI300X OAM",
		"AMD Instinct MI300X":     "AMD Instinct MI300X OAM",
		"AMD Instinct MI300X OAM": "AMD Instinct MI300X OAM",
		"MI350X":                  "AMD Instinct MI350 OAM",
		"AMD Instinct MI350 OAM":  "AMD Instinct MI350 OAM",
		// RunPod lists no MI325X or MI355X: the name passes through unchanged
		// and finds no price rather than another GPU's.
		"MI325X": "MI325X",
		"MI355X": "MI355X",
	} {
		if got := GPUTypeID("runpod", input); got != expected {
			t.Fatalf("GPUTypeID(runpod, %q)=%q, want %q", input, got, expected)
		}
	}
	if !MatchesGPU("runpod", "AMD Instinct MI300X OAM", "MI300X") || MatchesGPU("runpod", "AMD Instinct MI300X OAM", "MI325X") {
		t.Fatal("RunPod MI300X price filter does not match exactly")
	}
	if got := GPUTypeID("aws", "MI300X"); got != "MI300X" {
		t.Fatalf("AWS mapping invented an AMD SKU: %q", got)
	}
}
