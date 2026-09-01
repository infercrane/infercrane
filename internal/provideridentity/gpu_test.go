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

func TestHyperscalerAliasesResolveToReviewedCatalogGPU(t *testing.T) {
	for _, test := range []struct{ provider, input, expected string }{
		{provider: "aws", input: "H100", expected: "NVIDIA H100 80GB HBM3"},
		{provider: "aws", input: "A100 40GB", expected: "NVIDIA A100-SXM4-40GB"},
		{provider: "azure", input: "H100", expected: "H100-80GB"},
		{provider: "azure", input: "H100 NVL", expected: "H100-NVL-94GB"},
		{provider: "azure", input: "A100", expected: "A100-80GB"},
	} {
		if got := GPUTypeID(test.provider, test.input); got != test.expected {
			t.Fatalf("GPUTypeID(%s, %q)=%q, want %q", test.provider, test.input, got, test.expected)
		}
	}
	if got := GPUTypeID("gcp", "H100"); got != "H100" {
		t.Fatalf("unreviewed provider mapping changed: %q", got)
	}
	if got := PriceRegion("runpod", "EU-RO-1"); got != "global" {
		t.Fatalf("RunPod price scope=%q", got)
	}
}

func TestVastAliasesResolveToExactMarketplaceName(t *testing.T) {
	for input, expected := range map[string]string{
		"H100":      "H100 SXM",
		"H100 PCIe": "H100 PCIE",
		"A100 80GB": "A100 SXM4",
		"T4":        "Tesla T4",
		"L40S":      "L40S",
	} {
		if got := GPUTypeID("vast", input); got != expected {
			t.Fatalf("GPUTypeID(vast, %q)=%q, want %q", input, got, expected)
		}
	}
	if got := PriceRegion("vast", "US"); got != "global" {
		t.Fatalf("Vast price scope=%q", got)
	}
}
