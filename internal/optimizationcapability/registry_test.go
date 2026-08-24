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
	for _, request := range []Request{
		{Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: "0.21.0", Model: "Qwen/Qwen3-8B", Accelerator: "L40S"},
		{Mechanism: PrefixCaching, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "unknown/model", Accelerator: "L40S"},
		{Mechanism: AttentionBackend, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", Accelerator: "L40S", Parameters: map[string]string{"backend": "flashinfer"}},
		{Mechanism: ChunkedPrefill, Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Model: "Qwen/Qwen3-8B", Accelerator: "L40S", Parameters: map[string]string{"max_num_batched_tokens": "64"}},
	} {
		if _, err = registry.Compile(request); err == nil {
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
