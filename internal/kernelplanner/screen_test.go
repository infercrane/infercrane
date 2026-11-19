package kernelplanner

import (
	"strings"
	"testing"
)

func TestClassifyAttentionNeedsAKnownPhase(t *testing.T) {
	if family, ok := ClassifyKernel("flash_attn_kernel", PhasePrefill); !ok || family != AttentionPrefill {
		t.Fatalf("prefill family=%v ok=%v", family, ok)
	}
	if family, ok := ClassifyKernel("flash_attn_kernel", PhaseDecode); !ok || family != AttentionDecode {
		t.Fatalf("decode family=%v ok=%v", family, ok)
	}
	if _, ok := ClassifyKernel("flash_attn_kernel", PhaseMixed); ok {
		t.Fatal("mixed trace attributed attention to one path")
	}
	if _, ok := ClassifyKernel("flash_attn_kernel", ""); ok {
		t.Fatal("unknown phase attributed attention to one path")
	}
}

func TestClassifyNamesReviewedFamiliesOnly(t *testing.T) {
	for name, want := range map[string]OperatorFamily{
		"void vllm::fused_add_rms_norm_kernel<c10::BFloat16, 8>": ResidualRMSNorm,
		"sm90_xmma_gemm_bf16bf16_bf16f32":                        QuantizedLinear,
		"act_and_mul_kernel<silu_and_mul>":                       SwiGLU,
		"reshape_and_cache_flash_kernel_kv_cache":                KVCache,
		"fused_moe_kernel":                                       MoERouting,
		"chunk_gated_delta_rule_fwd_kernel":                      LinearRecurrence,
	} {
		if family, ok := ClassifyKernel(name, PhaseDecode); !ok || family != want {
			t.Errorf("%s: family=%q ok=%v, want %q", name, family, ok, want)
		}
	}
	if family, ok := ClassifyKernel("ncclDevKernel_AllReduce_Sum_bf16_RING_LL", PhaseDecode); ok {
		t.Fatalf("communication kernel classified as %q", family)
	}
}

func TestClassifyAttentionThatReadsAPagedCacheIsAttention(t *testing.T) {
	decode := "void flashinfer::BatchDecodeWithPagedKVCacheKernel<(flashinfer::PosEncodingMode)0, 2u, 4u, 8u, 16u, 1u, 8u>"
	if family, ok := ClassifyKernel(decode, PhaseDecode); !ok || family != AttentionDecode {
		t.Fatalf("paged decode attention = %q %v", family, ok)
	}
	if _, ok := ClassifyKernel(decode, PhaseMixed); ok {
		t.Fatal("paged attention without a phase was attributed")
	}
	for _, write := range []string{"void vllm::reshape_and_cache_flash_kernel<__nv_bfloat16, __nv_bfloat16, (vllm::Fp8KVCacheDataType)0>", "vllm::concat_and_cache_mla_kernel", "fused_kv_cache_append"} {
		if family, ok := ClassifyKernel(write, PhaseMixed); !ok || family != KVCache {
			t.Fatalf("%s = %q %v", write, family, ok)
		}
	}
}

func TestScreenUsesBuildGates(t *testing.T) {
	hotspots := []Hotspot{
		{ID: "residual-rmsnorm", Name: "fused_add_rms_norm_kernel", Family: ResidualRMSNorm, DeviceTimeFraction: .12},
		{ID: "swiglu", Name: "silu_and_mul_kernel", Family: SwiGLU, DeviceTimeFraction: .03},
		{ID: "quantized-linear", Name: "sm90_gemm", Family: QuantizedLinear, DeviceTimeFraction: .55},
	}
	screened, err := Screen(ScreenRequest{Runtime: "vllm", Hardware: HardwareIdentity{Vendor: "nvidia", Accelerator: "H100"}, Phase: PhaseDecode, Hotspots: hotspots})
	if err != nil {
		t.Fatal(err)
	}
	if len(screened) != 3 || screened[0].HotspotID != "quantized-linear" || screened[2].HotspotID != "swiglu" {
		t.Fatalf("order = %+v", screened)
	}
	norm := screened[1]
	if !norm.Eligible || norm.Reason != "" || norm.RequiredKernelSpeedup == nil || norm.Template != "fused-residual-rmsnorm" {
		t.Fatalf("norm = %+v", norm)
	}
	if want := 1 / (1 - .12); norm.MaxEndToEndSpeedup != want {
		t.Fatalf("ceiling = %v, want %v", norm.MaxEndToEndSpeedup, want)
	}
	if len(norm.ExistingImplementations) == 0 || norm.ExistingImplementations[0].ImplementationID != "runtime-vllm" {
		t.Fatalf("the pinned runtime kernel must be the first implementation: %+v", norm.ExistingImplementations)
	}
	small := screened[2]
	if small.Eligible || small.RequiredKernelSpeedup != nil || !strings.Contains(small.Reason, "below policy minimum") || len(small.ExistingImplementations) != 0 {
		t.Fatalf("small hotspot = %+v", small)
	}
	// The same hotspots through Build turn down the same operator.
	plan, err := Build(Request{
		SchemaVersion: RequestSchemaV1,
		Model:         ModelIdentity{Repository: "Qwen/Qwen3-8B", Revision: strings.Repeat("a", 40)},
		Runtime:       RuntimeIdentity{Name: "vllm", Version: "0.22.1", ImageDigest: "sha256:" + strings.Repeat("b", 64)},
		Hardware:      HardwareIdentity{Vendor: "nvidia", Accelerator: "H100"},
		Workload:      Workload{Digest: "sha256:" + strings.Repeat("c", 64), Phase: PhaseDecode, BatchSize: 1, Concurrency: 1, InputTokens: 1, OutputTokens: 1},
		Profile:       Profile{Tool: "torch-profiler", ToolVersion: "2.9.0", EvidenceClass: "measured", Hotspots: hotspots},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rejected) != 1 || plan.Rejected[0].HotspotID != "swiglu" || plan.Rejected[0].Reason != small.Reason {
		t.Fatalf("Build rejected %+v, Screen said %q", plan.Rejected, small.Reason)
	}
}

func TestScreenRefusesUnknownVendorAndImpossibleShares(t *testing.T) {
	if _, err := Screen(ScreenRequest{Hardware: HardwareIdentity{Vendor: "intel"}, Hotspots: []Hotspot{{ID: "a", Family: SwiGLU, DeviceTimeFraction: .2}}}); err == nil {
		t.Fatal("unknown vendor screened")
	}
	if _, err := Screen(ScreenRequest{Hardware: HardwareIdentity{Vendor: "nvidia"}, Hotspots: []Hotspot{{ID: "a", Family: SwiGLU, DeviceTimeFraction: .6}, {ID: "b", Family: Sampling, DeviceTimeFraction: .6}}}); err == nil {
		t.Fatal("shares above one screened")
	}
	if _, err := Screen(ScreenRequest{Hardware: HardwareIdentity{Vendor: "nvidia"}, Hotspots: []Hotspot{{ID: "a", Family: SwiGLU, DeviceTimeFraction: 1}}}); err == nil {
		t.Fatal("a hotspot holding all device time has no finite ceiling")
	}
}
