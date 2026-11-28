package provideridentity

import "testing"

func TestAcceleratorMemoryGiB(t *testing.T) {
	for name, want := range map[string]int{"H100": 80, "NVIDIA H100-80GB": 80, "A100 40GB": 40, "A100-80GB": 80, "RTX PRO 6000": 96, "a40": 48, "H200 NVL": 141, "Custom 72GB": 72} {
		if got, ok := AcceleratorMemoryGiB(name); !ok || got != want {
			t.Fatalf("AcceleratorMemoryGiB(%q) = %d, %v; want %d", name, got, ok, want)
		}
	}
	// A size is read from the name's own suffix, never from the model number
	// running into it once the separators are gone.
	for name, want := range map[string]int{"NVIDIA-A100-SXM4-40GB": 40, "NVIDIA-A100-PCIE-80GB": 80, "Custom_96GB": 96} {
		if got, ok := AcceleratorMemoryGiB(name); !ok || got != want {
			t.Fatalf("AcceleratorMemoryGiB(%q) = %d, %v; want %d", name, got, ok, want)
		}
	}
	if got, ok := AcceleratorMemoryGiB("Custom48GB"); ok {
		t.Fatalf("a size run into the model name was read: %d", got)
	}
	if _, ok := AcceleratorMemoryGiB("mystery"); ok {
		t.Fatal("an unknown accelerator reported memory")
	}
	for _, alias := range PlanningAccelerators() {
		if _, ok := AcceleratorMemoryGiB(alias); !ok {
			t.Fatalf("planning accelerator %q has no known memory", alias)
		}
	}
}

// Usable memory is the driver's total, below the nominal size, and known only
// where a published reading is recorded.
func TestAcceleratorUsableMemoryIsTheDriversTotal(t *testing.T) {
	for _, test := range []struct {
		gpu    string
		memory int
	}{
		{"NVIDIA-A10G", 23028}, {"nvidia-l4", 23034}, {"Tesla-T4", 15360}, {"L40S", 46068}, {"H100", 81559},
		{"NVIDIA-H100-80GB-HBM3", 81559}, {"A100 80GB", 81920}, {"NVIDIA-A100-SXM4-40GB", 40960}, {"H200", 143771},
		{"AMD Instinct MI300X", 196592},
	} {
		memory, known := AcceleratorUsableMemoryMiB(test.gpu)
		if !known || memory != test.memory {
			t.Fatalf("%s: usable memory (%d, %t), want %d MiB", test.gpu, memory, known, test.memory)
		}
	}
	for _, gpu := range []string{"RTX 5090", "H100 NVL", "RTX A6000", "MI325X", "custom-48GB", ""} {
		if memory, known := AcceleratorUsableMemoryMiB(gpu); known {
			t.Fatalf("%s: usable memory %d MiB without a published reading", gpu, memory)
		}
	}
	for name, usable := range acceleratorDriverMemoryMiB {
		nominal, known := acceleratorMemoryGiB[name]
		if accelerator, reviewed := LookupAccelerator(name); reviewed {
			nominal, known = accelerator.MemoryGiB, true
		}
		if known && (usable > nominal<<10 || usable < nominal<<10*9/10) {
			t.Fatalf("%s: usable %d MiB is not within 90%% of the nominal %d GiB", name, usable, nominal)
		}
	}
}
