package kernelplanner

import "strings"

type markerSet struct {
	family  OperatorFamily
	markers []string
}

// operatorMarkers is reviewed vocabulary from kernel names observed in real
// traces. Unmatched kernels stay in the denominator but never become hotspots.
// Order matters: the first family with a matching marker wins.
var operatorMarkers = []markerSet{
	{LinearRecurrence, []string{"deltanet", "delta_rule", "gdn", "linear_attn", "mamba"}},
	{StateCommit, []string{"state_commit", "state_scatter", "scatter_state"}},
	{ResidualRMSNorm, []string{"rmsnorm", "rms_norm", "layernorm", "norm_kernel"}},
	{MoERouting, []string{"fused_moe", "moe_", "expert"}},
	{QuantizedLinear, []string{"gemm", "matmul", "cutlass", "cublas", "wgmma", "nvjet"}},
	{SwiGLU, []string{"swiglu", "silu_and_mul"}},
	// vLLM's cache writes (reshape_and_cache_flash_kernel, concat_and_cache_mla_kernel).
	{KVCache, []string{"reshape_and_cache", "concat_and_cache"}},
	{Sampling, []string{"sampling", "topk", "topp", "softmax"}},
}

var attentionMarkers = []string{"attention", "flash_attn", "flashinfer", "fmha", "mha_"}

// kvCacheMarkers come after attention: attention kernels that read a paged
// cache name it (FlashInfer's BatchDecodeWithPagedKVCacheKernel) without
// writing it.
var kvCacheMarkers = markerSet{KVCache, []string{"kv_cache", "kvcache"}}

// ClassifyKernel names the reviewed operator family a GPU kernel belongs to.
// Attention kernels need the serving phase they ran in: a mixed or unknown
// phase cannot say which attention path a kernel was, so it reports false.
// Both the Accelerator Lab's own profiler and the endpoint screen use it, so
// a hotspot means the same thing in both.
func ClassifyKernel(name string, phase Phase) (OperatorFamily, bool) {
	lowered := strings.ToLower(name)
	for _, entry := range operatorMarkers {
		if entry.matches(lowered) {
			return entry.family, true
		}
	}
	for _, marker := range attentionMarkers {
		if strings.Contains(lowered, marker) {
			switch phase {
			case PhasePrefill:
				return AttentionPrefill, true
			case PhaseDecode:
				return AttentionDecode, true
			}
			return "", false
		}
	}
	if kvCacheMarkers.matches(lowered) {
		return kvCacheMarkers.family, true
	}
	return "", false
}

func (set markerSet) matches(lowered string) bool {
	for _, marker := range set.markers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}
