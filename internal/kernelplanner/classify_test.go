package kernelplanner

import "testing"

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
		"void vllm::fused_add_rms_norm_kernel<c10::BFloat16, 8>":                         ResidualRMSNorm,
		"sm90_xmma_gemm_bf16bf16_bf16f32":                                                QuantizedLinear,
		"act_and_mul_kernel<silu_and_mul>":                                               SwiGLU,
		"reshape_and_cache_flash_kernel_kv_cache":                                        KVCache,
		"fused_moe_kernel":                                                               MoEExperts,
		"void vllm::moe::topkGatingSoftmax<8, 256, 4, 16, 32, int, float>(float const*)": MoERouting,
		"void vllm::moe::moe_align_block_size_kernel<int>(int const*)":                   MoERouting,
		"void deep_gemm::sm90_m_grouped_gemm_fp8_kernel":                                 MoEExperts,
		"void deep_gemm::sm90_fp8_gemm_1d2d_impl<128u>":                                  QuantizedLinear,
		"chunk_gated_delta_rule_fwd_kernel":                                              LinearRecurrence,
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
