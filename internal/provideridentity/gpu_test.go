package provideridentity

import "testing"

func TestRunPodAliasesResolveToExactProviderSKU(t *testing.T) {
	for input, expected := range map[string]string{
		"H100":        "NVIDIA H100 80GB HBM3",
		"H100 PCIe":   "NVIDIA H100 PCIe",
		"H100 NVL":    "NVIDIA H100 NVL",
		"A100 80GB":   "NVIDIA A100-SXM4-80GB",
		"A100 PCIe":   "NVIDIA A100 80GB PCIe",
		"L40S":        "NVIDIA L40S",
		"unknown-sku": "unknown-sku",
	} {
		if got := GPUTypeID("runpod", input); got != expected {
			t.Fatalf("GPUTypeID(runpod, %q)=%q, want %q", input, got, expected)
		}
	}
}

func TestProviderIdentityDoesNotGuessOtherClouds(t *testing.T) {
	if got := GPUTypeID("aws", "H100"); got != "H100" {
		t.Fatalf("unexpected non-RunPod mapping: %q", got)
	}
	if got := PriceRegion("runpod", "EU-RO-1"); got != "global" {
		t.Fatalf("RunPod price scope=%q", got)
	}
}
