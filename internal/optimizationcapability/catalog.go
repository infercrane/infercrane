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

// V1 returns only reviewed exact-version facts. Registered/deferred entries
// are visible to planning and diagnostics, but Compile refuses to emit them.
func V1() (Registry, error) {
	descriptors := []Descriptor{
		{ID: "vllm-0.22.0-continuous-batching-v1", Mechanism: ContinuousBatching, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8", "awq", "gptq"}, Accelerators: commonAccelerators, Compiler: "runtime-owned", State: LocalQualified, Evidence: "go:test/internal/optimizationcapability#TestV1CompilesOnlyQualifiedExactTuples", Upstream: "https://docs.vllm.ai/en/latest/configuration/optimization/", License: "apache-2.0"},
		{ID: "vllm-0.22.0-prefix-cache-v1", Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16"}, Accelerators: commonAccelerators, Compiler: "vllm-prefix-cache", State: LocalQualified, Evidence: "go:test/internal/optimizationcapability#TestV1CompilesOnlyQualifiedExactTuples", Upstream: "https://docs.vllm.ai/en/latest/features/automatic_prefix_caching/", License: "apache-2.0", Limitations: []string{"Benefit requires repeated prompt prefixes and must be measured on the intended workload."}},
		{ID: "vllm-0.22.0-chunked-prefill-budget-v1", Mechanism: ChunkedPrefill, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16"}, Accelerators: commonAccelerators, Parameters: map[string][]string{"max_num_batched_tokens": {"integer:256..1048576"}}, Compiler: "vllm-batch-token-budget", State: LocalQualified, Evidence: "go:test/internal/optimizationcapability#TestV1CompilesOnlyQualifiedExactTuples", Upstream: "https://docs.vllm.ai/en/latest/configuration/optimization/", License: "apache-2.0", Limitations: []string{"Token budget trades prefill throughput against decode latency and memory; benchmark each workload."}},
		{ID: "vllm-0.22.0-attention-backend-v1", Mechanism: AttentionBackend, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: commonAccelerators, Parameters: map[string][]string{"backend": {"flashinfer", "flash-attention", "triton"}}, Compiler: "not-enabled", State: Deferred, Evidence: "docs:docs/workplans/inference-optimization-execution-plan.md#M2", Upstream: "https://docs.vllm.ai/en/latest/configuration/optimization/", License: "apache-2.0", Limitations: []string{"Backend choice is model, GPU, CUDA, and shape dependent; no universal default."}},
		{ID: "vllm-0.22.0-weight-precision-v1", Mechanism: WeightPrecision, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Models: reviewedModels, ArtifactPrecisions: []string{"fp8", "awq", "gptq"}, Accelerators: []string{"H100", "H200", "L40S"}, Parameters: map[string][]string{"format": {"fp8", "awq", "gptq"}}, Compiler: "artifact-required", State: Deferred, Evidence: "docs:docs/workplans/inference-optimization-execution-plan.md#M4", Upstream: "https://docs.vllm.ai/en/latest/features/quantization/", License: "apache-2.0"},
		{ID: "vllm-0.22.0-kv-cache-precision-v1", Mechanism: KVCachePrecision, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: []string{"H100", "H200", "L40S"}, Parameters: map[string][]string{"dtype": {"fp8_e4m3", "fp8_e5m2"}}, Compiler: "quality-gate-required", State: Deferred, Evidence: "docs:docs/workplans/inference-optimization-execution-plan.md#M4", Upstream: "https://docs.vllm.ai/en/latest/features/quantization/quantized_kvcache/", License: "apache-2.0"},
		{ID: "vllm-0.22.0-speculative-v1", Mechanism: SpeculativeDecode, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Models: []string{"Qwen/Qwen3-8B", "meta-llama/Llama-3.1-8B-Instruct"}, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: []string{"H100", "H200", "L40S"}, Parameters: map[string][]string{"method": {"eagle3", "mtp", "dflash", "draft-model", "ngram"}}, Compiler: "speculator-artifact-required", State: Deferred, Evidence: "docs:docs/workplans/inference-optimization-execution-plan.md#M5", Upstream: "https://github.com/vllm-project/speculators", License: "apache-2.0"},
		{ID: "vllm-0.22.0-lmcache-mp-v1", Mechanism: KVReuse, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: commonAccelerators, Parameters: map[string][]string{"connector": {"LMCacheMPConnector"}}, Compiler: "lmcache-lifecycle-required", State: Deferred, Evidence: "docs:docs/workplans/inference-optimization-execution-plan.md#M6", Upstream: "https://docs.lmcache.ai/getting_started/quickstart.html", License: "apache-2.0"},
		{ID: "dynamo-prefill-decode-v1", Mechanism: Disaggregated, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Models: reviewedModels, ArtifactPrecisions: []string{"bf16", "fp16", "fp8"}, Accelerators: []string{"H100", "H200", "L40S"}, Parameters: map[string][]string{"transport": {"nixl"}}, Compiler: "dynamo-topology-required", State: Deferred, Evidence: "go:test/internal/provision#TestKubernetesDynamoDisaggregatedVLLMAndSGLangAreExplicit", Upstream: "https://github.com/ai-dynamo/dynamo", License: "apache-2.0"},
		{ID: "sglang-0.5.12-continuous-batching-v1", Mechanism: ContinuousBatching, Runtime: "sglang", RuntimeVersion: support.SGLangRuntimeVersion, Models: []string{"Qwen/Qwen3-8B"}, ArtifactPrecisions: []string{"bf16", "fp16"}, Accelerators: commonAccelerators, Compiler: "runtime-owned", State: LocalQualified, Evidence: "go:test/internal/optimizationcapability#TestV1CompilesOnlyQualifiedExactTuples", Upstream: "https://docs.sglang.ai/", License: "apache-2.0"},
	}
	return New(descriptors...)
}
