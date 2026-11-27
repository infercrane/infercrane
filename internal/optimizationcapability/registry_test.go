package optimizationcapability

import (
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/support"
)

func TestV1CompilesOnlyQualifiedExactTuples(t *testing.T) {
	registry, err := V1()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := registry.Compile(Request{Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "mistralai/Mistral-7B-Instruct-v0.3", ArtifactPrecision: "bf16", Accelerator: "L40S"})
	if err != nil || len(compiled.Arguments) != 1 || compiled.Arguments[0] != "--enable-prefix-caching" || compiled.DescriptorID == "" {
		t.Fatalf("compiled=%+v err=%v", compiled, err)
	}
	budget, err := registry.Compile(Request{Mechanism: ChunkedPrefill, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen2.5-Coder-7B-Instruct", ArtifactPrecision: "bf16", Accelerator: "H100", Parameters: map[string]string{"max_num_batched_tokens": "2048"}})
	if err != nil || strings.Join(budget.Arguments, " ") != "--max-num-batched-tokens 2048" {
		t.Fatalf("budget=%+v err=%v", budget, err)
	}
	kubernetesAlias, err := registry.Compile(Request{Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", ArtifactPrecision: "bf16", Accelerator: "NVIDIA-L40S"})
	if err != nil || kubernetesAlias.DescriptorID == "" {
		t.Fatalf("explicit Kubernetes GPU family alias did not normalize: compiled=%+v err=%v", kubernetesAlias, err)
	}
	// Both pinned vLLM releases compile the same reviewed flags, each under
	// its own descriptor.
	for _, version := range []string{support.DefaultRuntimeVersion, support.LegacyVLLMRuntimeVersion} {
		compiled, err := registry.Compile(Request{Mechanism: ChunkedPrefill, Runtime: "vllm", RuntimeVersion: version, Model: "Qwen/Qwen3-8B", ArtifactPrecision: "bf16", Accelerator: "H100", Parameters: map[string]string{"max_num_batched_tokens": "16384"}})
		if err != nil || compiled.DescriptorID != "vllm-"+version+"-chunked-prefill-budget-v1" || strings.Join(compiled.Arguments, " ") != "--max-num-batched-tokens 16384" {
			t.Fatalf("%s budget=%+v err=%v", version, compiled, err)
		}
	}
	for _, request := range []Request{
		{Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: "0.21.0", Model: "Qwen/Qwen3-8B", Accelerator: "L40S"},
		{Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: "0.30.0", Model: "Qwen/Qwen3-8B", Accelerator: "L40S"},
		{Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "unknown/model", Accelerator: "L40S"},
		{Mechanism: AttentionBackend, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", Accelerator: "L40S", Parameters: map[string]string{"backend": "flashinfer"}},
		{Mechanism: ChunkedPrefill, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", Accelerator: "L40S", Parameters: map[string]string{"max_num_batched_tokens": "64"}},
		{Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", Accelerator: "nvidia.com/gpu"},
	} {
		if _, err = registry.Compile(request); err == nil {
			t.Fatalf("expected fail-closed rejection for %+v", request)
		}
	}
}

func TestV1CompilesSchedulerMechanismsOnReviewedAMDAccelerators(t *testing.T) {
	registry, err := V1()
	if err != nil {
		t.Fatal(err)
	}
	for _, gpu := range []string{"MI300X", "AMD Instinct MI300X OAM", "AMD_Instinct_MI325X_OAM", "MI355X"} {
		for _, request := range []Request{
			{Mechanism: ContinuousBatching, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", ArtifactPrecision: "bf16", Accelerator: gpu},
			{Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", ArtifactPrecision: "bf16", Accelerator: gpu},
			{Mechanism: ChunkedPrefill, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", ArtifactPrecision: "bf16", Accelerator: gpu, Parameters: map[string]string{"max_num_batched_tokens": "8192"}},
			{Mechanism: ContinuousBatching, Runtime: "sglang", RuntimeVersion: support.SGLangRuntimeVersion, Model: "Qwen/Qwen3-8B", ArtifactPrecision: "bf16", Accelerator: gpu},
		} {
			if _, err := registry.Compile(request); err != nil {
				t.Fatalf("%s %s on %s: %v", request.Runtime, request.Mechanism, gpu, err)
			}
		}
	}
	// Kernel- and precision-level descriptors are not widened to AMD, and an
	// unreviewed AMD name matches nothing.
	for _, request := range []Request{
		{Mechanism: KVCachePrecision, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", ArtifactPrecision: "bf16", Accelerator: "MI300X", Parameters: map[string]string{"dtype": "fp8_e4m3"}},
		{Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", ArtifactPrecision: "bf16", Accelerator: "MI250X"},
		{Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", ArtifactPrecision: "bf16", Accelerator: "amd.com/gpu"},
	} {
		if _, err := registry.Compile(request); err == nil {
			t.Fatalf("expected fail-closed rejection for %+v", request)
		}
	}
}

func TestRegistryRejectsAmbiguousAndWildcardDescriptors(t *testing.T) {
	descriptor := Descriptor{ID: "one", Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: "1", Models: []string{"org/model"}, ArtifactPrecisions: []string{"bf16"}, Accelerators: []string{"H100"}, Compiler: "runtime-owned", State: LocalQualified, Evidence: "test", Upstream: "https://example.test", License: "apache-2.0"}
	if _, err := New(descriptor, descriptor); err == nil {
		t.Fatal("duplicate descriptors must fail")
	}
	descriptor.ID, descriptor.Models = "wildcard", nil
	if _, err := New(descriptor); err == nil {
		t.Fatal("unbounded model descriptors must fail")
	}
}

// Every descriptor names its own release in its ID and records a pinned
// release; 0.34.0's deferred LMCache tuple says its LMCache version is unchecked.
func TestV1DescriptorsNameTheirPinnedRelease(t *testing.T) {
	registry, err := V1()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, descriptor := range registry.descriptors {
		if descriptor.Runtime != "vllm" || strings.HasPrefix(descriptor.ID, "dynamo-") {
			continue
		}
		seen[descriptor.RuntimeVersion]++
		if !strings.HasPrefix(descriptor.ID, "vllm-"+descriptor.RuntimeVersion+"-") {
			t.Fatalf("%s records release %s", descriptor.ID, descriptor.RuntimeVersion)
		}
		if descriptor.ID == "vllm-0.34.0-lmcache-v1" && (descriptor.State != Deferred || !strings.Contains(strings.Join(descriptor.Limitations, " "), "not checked")) {
			t.Fatalf("0.34.0 LMCache tuple %+v", descriptor)
		}
	}
	if seen[support.DefaultRuntimeVersion] != 8 || seen[support.LegacyVLLMRuntimeVersion] != 8 || len(seen) != 2 {
		t.Fatalf("vLLM descriptors per release %v", seen)
	}
}

// SGLang 0.5.23's longest-prefix-match admission compiles to exactly its
// reviewed flag; HiCache, attention backends, and adaptive speculation are
// recorded with their flags but stay deferred, so they never compile.
func TestSGLangTuplesCompileOnlyTheirReviewedFlags(t *testing.T) {
	registry, err := V1()
	if err != nil {
		t.Fatal(err)
	}
	current := support.SGLangRuntimeVersion
	for _, gpu := range []string{"H100", "L4", "MI300X", "MI355X"} {
		compiled, err := registry.Compile(Request{Mechanism: SchedulePolicy, Runtime: "sglang", RuntimeVersion: current, Model: "Qwen/Qwen3-8B", ArtifactPrecision: "bf16", Accelerator: gpu, Parameters: map[string]string{"policy": "lpm"}})
		if err != nil || strings.Join(compiled.Arguments, " ") != "--schedule-policy lpm" || compiled.DescriptorID != "sglang-0.5.23-schedule-policy-lpm-v1" {
			t.Fatalf("%s lpm compiled=%+v err=%v", gpu, compiled, err)
		}
	}
	for name, request := range map[string]Request{
		"another policy":    {Mechanism: SchedulePolicy, Runtime: "sglang", RuntimeVersion: current, Model: "Qwen/Qwen3-8B", Accelerator: "H100", Parameters: map[string]string{"policy": "random"}},
		"legacy release":    {Mechanism: SchedulePolicy, Runtime: "sglang", RuntimeVersion: support.LegacySGLangRuntimeVersion, Model: "Qwen/Qwen3-8B", Accelerator: "H100", Parameters: map[string]string{"policy": "lpm"}},
		"HiCache deferred":  {Mechanism: KVReuse, Runtime: "sglang", RuntimeVersion: current, Model: "Qwen/Qwen3-8B", Accelerator: "H100"},
		"backend deferred":  {Mechanism: AttentionBackend, Runtime: "sglang", RuntimeVersion: current, Model: "Qwen/Qwen3-8B", Accelerator: "H100", Parameters: map[string]string{"backend": "fa3"}},
		"adaptive deferred": {Mechanism: SpeculativeDecode, Runtime: "sglang", RuntimeVersion: current, Model: "Qwen/Qwen3-8B", Accelerator: "H100"},
		"unreviewed model":  {Mechanism: SchedulePolicy, Runtime: "sglang", RuntimeVersion: current, Model: "unknown/model", Accelerator: "H100", Parameters: map[string]string{"policy": "lpm"}},
	} {
		if compiled, err := registry.Compile(request); err == nil {
			t.Fatalf("%s compiled %+v", name, compiled)
		}
	}
	// The deferred tuples name the exact flags and the limits read from source.
	byID := map[string]Descriptor{}
	for _, descriptor := range registry.descriptors {
		byID[descriptor.ID] = descriptor
	}
	if got := byID["sglang-0.5.23-attention-backend-v1"].Parameters["backend"]; !contains(got, "fa3") || !contains(got, "trtllm_mha") {
		t.Fatalf("attention backends %v", got)
	}
	if got := byID["sglang-0.5.23-hicache-v1"].Parameters["flags"]; !contains(got, "--enable-hierarchical-cache") {
		t.Fatalf("HiCache flags %v", got)
	}
	adaptive := byID["sglang-0.5.23-speculative-adaptive-v1"]
	if !contains(adaptive.Parameters["adaptive"], "--speculative-adaptive") || !strings.Contains(strings.Join(adaptive.Limitations, " "), "topk 1") {
		t.Fatalf("adaptive speculation %+v", adaptive)
	}
	// Both SGLang releases keep continuous batching, each under its own ID.
	for _, version := range []string{current, support.LegacySGLangRuntimeVersion} {
		if compiled, err := registry.Compile(Request{Mechanism: ContinuousBatching, Runtime: "sglang", RuntimeVersion: version, Model: "Qwen/Qwen3-8B", Accelerator: "H100"}); err != nil || compiled.DescriptorID != "sglang-"+version+"-continuous-batching-v1" {
			t.Fatalf("%s continuous batching %+v err=%v", version, compiled, err)
		}
	}
}
