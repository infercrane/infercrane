package gputrace

import (
	"strings"

	"github.com/infercrane/infercrane/internal/gpuprofile"
)

// Kernel names are matched on stable fragments. Communication comes first so
// an all-reduce fused with a norm counts as communication; attention comes
// before GEMM so attention kernels built on GEMM templates stay attention.
var (
	communicationFragments = []string{"nccl", "rccl", "allreduce", "all_reduce", "allgather", "all_gather", "reducescatter", "reduce_scatter", "cross_device_reduce", "custom_ar", "alltoall", "all_to_all"}
	attentionFragments     = []string{"attn", "attention", "fmha", "flash_fwd", "flash_bwd", "paged", "batchdecode", "batchprefill", "singleprefill", "reshape_and_cache", "merge_attn", "mla_", "mmha", "masked_multihead", "xqa"}
	gemmFragments          = []string{"gemm", "gemv", "matmul", "cutlass", "cublas", "xmma", "wgmma", "marlin", "machete", "nvjet", "fused_moe", "splitk", "cijk_"}
	memoryFragments        = []string{"memcpy", "memset"}
	normFragments          = []string{"rmsnorm", "rms_norm", "layernorm", "layer_norm", "norm_kernel"}
	activationFragments    = []string{"silu", "gelu", "swiglu", "act_and_mul", "relu", "activation"}
	samplingFragments      = []string{"sampl", "topk", "top_k", "topp", "top_p", "argmax", "multinomial", "gumbel", "softmax", "logits"}
	prefillFragments       = []string{"batchprefill", "singleprefill", "prefill", "context_attention", "fmha_v2", "extend_attention"}
	decodeFragments        = []string{"batchdecode", "decode", "paged_attention_v1", "paged_attention_v2", "mmha", "masked_multihead", "xqa", "splitkv_mla"}
	launchAPIs             = map[string]bool{
		"cudalaunchkernel": true, "cudalaunchkernelexc": true, "cudalaunchcooperativekernel": true, "cudalaunchcooperativekernelmultidevice": true,
		"culaunchkernel": true, "culaunchkernelex": true, "culaunchcooperativekernel": true, "cudagraphlaunch": true, "cugraphlaunch": true,
		"hiplaunchkernel": true, "hipextlaunchkernel": true, "hipmodulelaunchkernel": true, "hipextmodulelaunchkernel": true,
		"hiplaunchcooperativekernel": true, "hipgraphlaunch": true, "hipextlaunchmultikernelmultidevice": true,
	}
	// stepMarkers are host ranges that wrap one engine step. ProfilerStep#N
	// appears when vLLM profiles on a schedule.
	stepMarkers = []string{"profilerstep#", "execute_model", "gpu_model_runner: forward", "forward_batch_generation", "run_batch", "model_runner.forward"}
)

func containsAny(lower string, fragments []string) bool {
	for _, fragment := range fragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

// Category groups a kernel by its name into one of gpuprofile.Categories.
func Category(name string) string {
	lower := strings.ToLower(name)
	switch {
	case containsAny(lower, communicationFragments):
		return gpuprofile.CategoryCommunication
	case containsAny(lower, attentionFragments):
		return gpuprofile.CategoryAttention
	case containsAny(lower, gemmFragments):
		return gpuprofile.CategoryGEMM
	case containsAny(lower, memoryFragments):
		return gpuprofile.CategoryMemory
	case containsAny(lower, normFragments):
		return gpuprofile.CategoryNorm
	case containsAny(lower, activationFragments):
		return gpuprofile.CategoryActivation
	case containsAny(lower, samplingFragments):
		return gpuprofile.CategorySampling
	default:
		return gpuprofile.CategoryOther
	}
}

// DiagnosisClass folds a category into the four classes the optimization
// planner reads: attention, gemm, communication, and other.
func DiagnosisClass(name string) string {
	switch category := Category(name); category {
	case gpuprofile.CategoryAttention, gpuprofile.CategoryGEMM, gpuprofile.CategoryCommunication:
		return category
	default:
		return gpuprofile.CategoryOther
	}
}

// KernelPhase is the serving phase an attention kernel's name commits to,
// or "" when its name does not say. Kernels shared by both phases (for
// example FlashAttention's varlen kernels under chunked prefill) say nothing.
func KernelPhase(name string) string {
	if Category(name) != gpuprofile.CategoryAttention {
		return ""
	}
	lower := strings.ToLower(name)
	prefill, decode := containsAny(lower, prefillFragments), containsAny(lower, decodeFragments)
	switch {
	case prefill && !decode:
		return gpuprofile.PhasePrefill
	case decode && !prefill:
		return gpuprofile.PhaseDecode
	default:
		return ""
	}
}

// IsLaunchAPI reports whether a runtime or driver call enqueues a kernel or a
// graph.
func IsLaunchAPI(name string) bool {
	return launchAPIs[strings.ToLower(name)]
}

// IsGraphLaunch reports whether a launch call replays a CUDA or HIP graph.
func IsGraphLaunch(name string) bool {
	return strings.Contains(strings.ToLower(name), "graphlaunch")
}

// stepMarker returns the marker a host range name matches, or "".
func stepMarker(name string) string {
	lower := strings.ToLower(name)
	for _, marker := range stepMarkers {
		if strings.HasPrefix(lower, marker) || marker != "profilerstep#" && lower == marker {
			return marker
		}
	}
	return ""
}

// annotationPhase is the phase a step annotation's name states, or "".
func annotationPhase(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "prefill") || strings.Contains(lower, "extend"):
		return gpuprofile.PhasePrefill
	case strings.Contains(lower, "decode"):
		return gpuprofile.PhaseDecode
	default:
		return ""
	}
}

func isCompute(category string) bool {
	return category != gpuprofile.CategoryCommunication && category != gpuprofile.CategoryMemory
}
