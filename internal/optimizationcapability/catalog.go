package optimizationcapability

import "github.com/infercrane/infercrane/internal/support"

var reviewedModels = []string{
	"BAAI/bge-m3",
	"Qwen/Qwen2.5-Coder-7B-Instruct",
	"Qwen/Qwen2.5-VL-7B-Instruct",
	"Qwen/Qwen3-8B",
	"deepseek-ai/DeepSeek-R1-Distill-Qwen-7B",
	"google/gemma-3-4b-it",
	"ibm-granite/granite-3.3-8b-instruct",
	"meta-llama/Llama-3.1-8B-Instruct",
	"mistralai/Mistral-7B-Instruct-v0.3",
}

var commonAccelerators = []string{"A10G", "H100", "H200", "L4", "L40S"}

// schedulerAccelerators adds the reviewed AMD Instinct accelerators to the
// engine-scheduler mechanisms (continuous batching, prefix caching, the
// batched-token budget). Those flags configure the engine's scheduler, not a
// kernel, so the pinned ROCm builds of the same engine version accept them
// unchanged. Kernel-, precision-, and backend-level descriptors stay NVIDIA
// until an AMD run is recorded.
var schedulerAccelerators = append(append([]string(nil), commonAccelerators...), "MI300X", "MI325X", "MI350X", "MI355X")

// vllmDescriptors are the reviewed tuples of one vLLM release. The flags the
// qualified tuples emit (--enable-prefix-caching, --max-num-batched-tokens)
// exist unchanged in 0.22.1 and 0.34.0; read from each tag's
// vllm/engine/arg_utils.py.
func vllmDescriptors(version string, speculation []string, lmcacheLimit string) []Descriptor {
	prefix := "vllm-" + version + "-"
	lmcacheLimits := []string(nil)
	if lmcacheLimit != "" {
		lmcacheLimits = []string{lmcacheLimit}
	}
	return []Descriptor{
		{ID: prefix + "continuous-batching-v1", Mechanism: ContinuousBatching, Runtime: "vllm", RuntimeVersion: version, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8", "awq", "gptq"}, Accelerators: schedulerAccelerators, Compiler: "runtime-owned", State: LocalQualified, Evidence: "go:test/internal/optimizationcapability#TestV1CompilesOnlyQualifiedExactTuples", Upstream: "https://docs.vllm.ai/en/latest/configuration/optimization/", License: "apache-2.0"},
		{ID: prefix + "prefix-cache-v1", Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: version, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16"}, Accelerators: schedulerAccelerators, Compiler: "vllm-prefix-cache", State: LocalQualified, Evidence: "go:test/internal/optimizationcapability#TestV1CompilesOnlyQualifiedExactTuples", Upstream: "https://docs.vllm.ai/en/latest/features/automatic_prefix_caching/", License: "apache-2.0", Limitations: []string{"Benefit requires repeated prompt prefixes and must be measured on the intended workload."}},
		{ID: prefix + "chunked-prefill-budget-v1", Mechanism: ChunkedPrefill, Runtime: "vllm", RuntimeVersion: version, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16"}, Accelerators: schedulerAccelerators, Parameters: map[string][]string{"max_num_batched_tokens": {"integer:256..1048576"}}, Compiler: "vllm-batch-token-budget", State: LocalQualified, Evidence: "go:test/internal/optimizationcapability#TestV1CompilesOnlyQualifiedExactTuples", Upstream: "https://docs.vllm.ai/en/latest/configuration/optimization/", License: "apache-2.0", Limitations: []string{"Token budget trades prefill throughput against decode latency and memory; benchmark each workload."}},
		{ID: prefix + "attention-backend-v1", Mechanism: AttentionBackend, Runtime: "vllm", RuntimeVersion: version, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: commonAccelerators, Parameters: map[string][]string{"backend": {"flashinfer", "flash-attention", "triton"}}, Compiler: "not-enabled", State: Deferred, Evidence: "docs:docs/features/optimization.mdx#upstream-composition-roadmap", Upstream: "https://docs.vllm.ai/en/latest/configuration/optimization/", License: "apache-2.0", Limitations: []string{"Backend choice is model, GPU, CUDA, and shape dependent; no universal default."}},
		{ID: prefix + "weight-precision-v1", Mechanism: WeightPrecision, Runtime: "vllm", RuntimeVersion: version, Models: reviewedModels, ArtifactPrecisions: []string{"fp8", "awq", "gptq"}, Accelerators: []string{"H100", "H200", "L40S"}, Parameters: map[string][]string{"format": {"fp8", "awq", "gptq"}}, Compiler: "artifact-required", State: Deferred, Evidence: "docs:docs/features/optimization.mdx#upstream-composition-roadmap", Upstream: "https://docs.vllm.ai/en/latest/features/quantization/", License: "apache-2.0"},
		{ID: prefix + "kv-cache-precision-v1", Mechanism: KVCachePrecision, Runtime: "vllm", RuntimeVersion: version, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: []string{"H100", "H200", "L40S"}, Parameters: map[string][]string{"dtype": {"fp8_e4m3", "fp8_e5m2"}}, Compiler: "quality-gate-required", State: Deferred, Evidence: "docs:docs/features/optimization.mdx#upstream-composition-roadmap", Upstream: "https://docs.vllm.ai/en/latest/features/quantization/quantized_kvcache/", License: "apache-2.0"},
		{ID: prefix + "speculative-v1", Mechanism: SpeculativeDecode, Runtime: "vllm", RuntimeVersion: version, Models: []string{"Qwen/Qwen3-8B", "meta-llama/Llama-3.1-8B-Instruct"}, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: []string{"H100", "H200", "L40S"}, Parameters: map[string][]string{"method": speculation}, Compiler: "speculator-artifact-required", State: Deferred, Evidence: "docs:docs/features/optimization.mdx#upstream-composition-roadmap", Upstream: "https://github.com/vllm-project/speculators", License: "apache-2.0"},
		{ID: prefix + "lmcache-v1", Mechanism: KVReuse, Runtime: "vllm", RuntimeVersion: version, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: commonAccelerators, Parameters: map[string][]string{"connector": {"LMCacheConnectorV1"}}, Compiler: "serving-contract-translation-deferred", State: Deferred, Evidence: "go:test/internal/servingcontract#TestDeferredLMCacheTranslationIsExactAndCannotCompile", Upstream: "https://docs.lmcache.ai/getting_started/quickstart/offload_kv_cache.html", License: "apache-2.0", Limitations: lmcacheLimits},
	}
}

// V1 returns only reviewed exact-version facts. Registered/deferred entries
// are visible to planning and diagnostics, but Compile refuses to emit them.
// vLLM 0.34.0 is the current release; the 0.22.1 tuples stay for revisions
// that record it (support.ImageLegacy).
func V1() (Registry, error) {
	var descriptors []Descriptor
	// vLLM 0.34.0 adds suffix decoding (config/speculative.py SpeculativeMethod).
	descriptors = append(descriptors, vllmDescriptors(support.DefaultRuntimeVersion, []string{"eagle3", "mtp", "dflash", "draft-model", "ngram", "suffix"},
		"vLLM 0.34.0 asks for lmcache 0.5.6 or newer when its KV connectors are installed (requirements/kv_connectors.txt); whether the pinned image carries LMCache, and which version, is not checked.")...)
	descriptors = append(descriptors, vllmDescriptors(support.LegacyVLLMRuntimeVersion, []string{"eagle3", "mtp", "dflash", "draft-model", "ngram"}, "")...)
	descriptors = append(descriptors,
		Descriptor{ID: "dynamo-prefill-decode-v1", Mechanism: Disaggregated, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: []string{"H100", "H200", "L40S"}, Parameters: map[string][]string{"transport": {"nixl"}}, Compiler: "serving-contract-translation-deferred", State: Deferred, Evidence: "go:test/internal/servingcontract#TestDeferredNIXLTranslationIsRuntimeSpecificAndCannotCompile", Upstream: "https://github.com/ai-dynamo/dynamo", License: "apache-2.0"},
		Descriptor{ID: "sglang-0.5.12-continuous-batching-v1", Mechanism: ContinuousBatching, Runtime: "sglang", RuntimeVersion: support.LegacySGLangRuntimeVersion, Models: []string{"Qwen/Qwen3-8B"}, ArtifactPrecisions: []string{"bf16", "fp16"}, Accelerators: schedulerAccelerators, Compiler: "runtime-owned", State: LocalQualified, Evidence: "go:test/internal/optimizationcapability#TestV1CompilesOnlyQualifiedExactTuples", Upstream: "https://docs.sglang.ai/", License: "apache-2.0"},
	)
	descriptors = append(descriptors, sglangDescriptors(support.SGLangRuntimeVersion)...)
	return New(descriptors...)
}

// sglangModels are the models whose SGLang tuples were reviewed.
var sglangModels = []string{"Qwen/Qwen3-8B"}

// sglangDescriptors are the reviewed tuples of SGLang 0.5.23, read from its
// source at tag v0.5.23 (commit 297e54fd). Flags live in
// python/sglang/srt/arg_groups/fields/: --schedule-policy (schedule.py, default
// fcfs, lpm among the choices), --enable-hierarchical-cache and --hicache-*
// (memory.py), --attention-backend (exec_.py, choices in arg_groups/choices.py),
// --speculative-algorithm and --speculative-adaptive (spec.py).
func sglangDescriptors(version string) []Descriptor {
	prefix := "sglang-" + version + "-"
	return []Descriptor{
		{ID: prefix + "continuous-batching-v1", Mechanism: ContinuousBatching, Runtime: "sglang", RuntimeVersion: version, Models: sglangModels, ArtifactPrecisions: []string{"bf16", "fp16"}, Accelerators: schedulerAccelerators, Compiler: "runtime-owned", State: LocalQualified, Evidence: "go:test/internal/optimizationcapability#TestV1CompilesOnlyQualifiedExactTuples", Upstream: "https://docs.sglang.io/", License: "apache-2.0"},
		// Longest-prefix-match admission is a scheduler flag, so it compiles
		// on the scheduler accelerators like the other scheduler tuples.
		{ID: prefix + "schedule-policy-lpm-v1", Mechanism: SchedulePolicy, Runtime: "sglang", RuntimeVersion: version, Models: sglangModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: schedulerAccelerators, Parameters: map[string][]string{"policy": {"lpm"}}, Compiler: "sglang-schedule-policy", State: LocalQualified, Evidence: "go:test/internal/optimizationcapability#TestSGLangTuplesCompileOnlyTheirReviewedFlags", Upstream: "https://docs.sglang.io/advanced_features/server_arguments.html", License: "apache-2.0", Limitations: []string{"Helps only traffic whose requests share prompt prefixes; the gain must be measured on the intended workload."}},
		{ID: prefix + "hicache-v1", Mechanism: KVReuse, Runtime: "sglang", RuntimeVersion: version, Models: sglangModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: commonAccelerators, Parameters: map[string][]string{"flags": {"--enable-hierarchical-cache"}, "hicache_ratio": {"number:1..8"}, "hicache_write_policy": {"write_back", "write_through", "write_through_selective"}}, Compiler: "not-enabled", State: Deferred, Evidence: "docs:docs/features/optimization.mdx#upstream-composition-roadmap", Upstream: "https://docs.sglang.io/advanced_features/hicache.html", License: "apache-2.0", Limitations: []string{"Host memory is sized from hicache_ratio (2.0 in cache mode when unset) or hicache_size; the server needs that much host RAM, and the gain depends on multi-turn reuse the replay must keep."}},
		{ID: prefix + "attention-backend-v1", Mechanism: AttentionBackend, Runtime: "sglang", RuntimeVersion: version, Models: sglangModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: commonAccelerators, Parameters: map[string][]string{"backend": {"fa3", "trtllm_mha", "flashinfer", "triton"}}, Compiler: "not-enabled", State: Deferred, Evidence: "docs:docs/features/optimization.mdx#upstream-composition-roadmap", Upstream: "https://docs.sglang.io/advanced_features/attention_backend.html", License: "apache-2.0", Limitations: []string{"Each backend runs only on the GPU generations it is built for, and the engine checks that at start; backend choice is model, GPU, and shape dependent."}},
		{ID: prefix + "speculative-adaptive-v1", Mechanism: SpeculativeDecode, Runtime: "sglang", RuntimeVersion: version, Models: sglangModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: []string{"H100", "H200", "L40S"}, Parameters: map[string][]string{"algorithm": {"EAGLE", "EAGLE3", "NEXTN"}, "adaptive": {"--speculative-adaptive"}}, Compiler: "speculator-artifact-required", State: Deferred, Evidence: "docs:docs/features/optimization.mdx#upstream-composition-roadmap", Upstream: "https://github.com/sgl-project/sglang/blob/v0.5.23/python/sglang/srt/speculative/adaptive_spec_params.py", License: "apache-2.0", Limitations: []string{"Adaptive steps run only with EAGLE or EAGLE3 (NEXTN resolves to EAGLE), speculative_eagle_topk 1, no attention data parallelism, and no multi-layer EAGLE; otherwise SGLang falls back to static steps with only a log warning, so a run must confirm it from the startup log.", "Cache room stays reserved for the largest step count, so it does not raise the batch the cache holds."}},
	}
}
