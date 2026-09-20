package kernelplanner

import (
	"reflect"
	"testing"
)

func TestDefaultRegistrySearchesExistingBeforeGeneratedKernels(t *testing.T) {
	registry := DefaultRegistry()
	request := SearchRequest{Family: AttentionDecode, Runtime: "vllm", HardwareVendor: "nvidia", ComputeCapability: "sm90", DTypes: []string{"bf16"}, Phase: PhaseDecode}
	first, err := registry.Search(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registry.Search(request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("registry search is not deterministic")
	}
	if len(first) < 4 || first[0].ImplementationID != "runtime-vllm" || first[1].ImplementationID != "flashinfer" || first[len(first)-1].ImplementationID != "cuda-cute-template" {
		t.Fatalf("unexpected search order: %+v", first)
	}
	if first[0].Compatibility != "exact-runtime-baseline" || first[1].Compatibility != "review-required" {
		t.Fatalf("search crossed compatibility boundary: %+v", first)
	}
}

func TestRegistryFiltersRuntimeHardwareAndDType(t *testing.T) {
	registry := DefaultRegistry()
	matches, err := registry.Search(SearchRequest{Family: QuantizedLinear, Runtime: "sglang", HardwareVendor: "nvidia", ComputeCapability: "sm80", DTypes: []string{"int4"}, Phase: PhaseDecode})
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		if match.ImplementationID == "runtime-vllm" || match.ImplementationID == "flashinfer" {
			t.Fatalf("incompatible source leaked into results: %+v", match)
		}
	}
	if len(matches) == 0 || matches[0].ImplementationID != "runtime-sglang" {
		t.Fatalf("missing exact runtime baseline: %+v", matches)
	}
	unsupported, err := registry.Search(SearchRequest{Family: ResidualRMSNorm, Runtime: "vllm", HardwareVendor: "amd", ComputeCapability: "gfx942", DTypes: []string{"bf16"}, Phase: PhaseDecode})
	if err != nil || len(unsupported) != 0 {
		t.Fatalf("unsupported hardware matched: %+v err=%v", unsupported, err)
	}
}

func TestDefaultRegistryCarriesProvenanceAndSafeExecutionBoundary(t *testing.T) {
	registry := DefaultRegistry()
	if err := registry.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, implementation := range registry.Implementations {
		if implementation.SourceURL == "" || implementation.License == "" || implementation.RevisionPolicy == "" || implementation.ExecutionBoundary == "" {
			t.Fatalf("implementation lacks provenance: %+v", implementation)
		}
	}
}
