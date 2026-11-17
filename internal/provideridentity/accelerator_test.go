package provideridentity

import "testing"

func TestAcceleratorMemoryGiB(t *testing.T) {
	for name, want := range map[string]int{"H100": 80, "NVIDIA H100-80GB": 80, "A100 40GB": 40, "A100-80GB": 80, "RTX PRO 6000": 96, "a40": 48, "H200 NVL": 141, "Custom 72GB": 72} {
		if got, ok := AcceleratorMemoryGiB(name); !ok || got != want {
			t.Fatalf("AcceleratorMemoryGiB(%q) = %d, %v; want %d", name, got, ok, want)
		}
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
