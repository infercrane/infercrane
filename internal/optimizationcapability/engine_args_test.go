package optimizationcapability

import (
	"os"
	"strings"
	"testing"
)

func TestEngineArgsAdmitOnlyTheReviewedSet(t *testing.T) {
	for _, test := range []struct {
		runtime string
		args    []string
	}{
		{"vllm", nil},
		{"vllm", []string{"--max-model-len", "8192", "--tensor-parallel-size", "2", "--enable-prefix-caching"}},
		{"vllm", []string{"--speculative-config", `{"method":"mtp","num_speculative_tokens":3}`, "--max-num-seqs=512"}},
		{"vllm", []string{"-cc.cudagraph_mode=FULL", "-O3", "--compilation-config", `{"level":3}`}},
		{"vllm", []string{"--tool-call-parser", "qwen3_coder", "--enable-auto-tool-choice", "--reasoning-parser", "qwen3", "--mm-encoder-tp-mode", "data"}},
		{"sglang", []string{"--chunked-prefill-size", "-1", "--schedule-policy", "lpm", "--max-running-requests", "1024"}},
		{"sglang", []string{"--speculative-algorithm", "NEXTN", "--speculative-num-steps", "2", "--speculative-eagle-topk", "1", "--speculative-num-draft-tokens", "3"}},
		{"sglang", []string{"--enable-hierarchical-cache", "--hicache-ratio", "2", "--attention-backend", "fa3", "--speculative-adaptive"}},
		// A runtime without a reviewed set (a custom image) is not checked
		// against it.
		{"custom-oci", []string{"--anything", "goes"}},
	} {
		if err := ValidateEngineArgs(test.runtime, test.args); err != nil {
			t.Fatalf("%s %v: %v", test.runtime, test.args, err)
		}
	}
	for _, test := range []struct {
		runtime string
		args    []string
		want    string
	}{
		{"vllm", []string{"--trust-remote-code"}, "--trust-remote-code is refused"},
		{"sglang", []string{"--trust-remote-code=true"}, "--trust-remote-code is refused"},
		{"sglang", []string{"--enable-custom-logit-processor"}, "--enable-custom-logit-processor is refused"},
		{"vllm", []string{"--enable-custom-logit-processor"}, "is refused"},
		// Refused flags are refused even on a custom image.
		{"custom-oci", []string{"--trust-remote-code"}, "is refused"},
		// Flags the product sets itself, and flags that load code or files.
		{"vllm", []string{"--host", "0.0.0.0"}, "--host is not reviewed for vllm"},
		{"vllm", []string{"--api-key", "secret"}, "--api-key is not reviewed"},
		{"vllm", []string{"--tool-parser-plugin", "/models/x.py"}, "--tool-parser-plugin is not reviewed"},
		{"vllm", []string{"--load-format", "pt"}, "--load-format is not reviewed"},
		{"vllm", []string{"--middleware", "a.b.C"}, "not reviewed"},
		{"vllm", []string{"--enable-lora"}, "not reviewed"},
		{"sglang", []string{"--config", "/tmp/x.yaml"}, "--config is not reviewed for sglang"},
		{"sglang", []string{"--api-key=hf_secret123"}, "--api-key is not reviewed"},
		{"sglang", []string{"--model-path", "x"}, "not reviewed"},
		// A vLLM flag is not an SGLang flag.
		{"sglang", []string{"--max-num-seqs", "8"}, "not reviewed for sglang"},
		{"vllm", []string{"--bogus\n--x"}, "(not a flag name)"},
	} {
		err := ValidateEngineArgs(test.runtime, test.args)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s %v: err=%v, want %q", test.runtime, test.args, err, test.want)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Fatalf("error echoes a value: %v", err)
		}
	}
	if err := RefuseUnsafeEngineArgs([]string{"python3", "-m", "sglang.launch_server", "--enable-custom-logit-processor"}); err == nil {
		t.Fatal("a command that turns on custom logit processors passed")
	}
	if !ReviewedEngine("VLLM") || ReviewedEngine("custom-oci") || len(ReviewedEngineFlags("sglang")) == 0 {
		t.Fatal("reviewed engines")
	}
}

// Every flag InferCrane itself puts in a built-in runtime's extra arguments
// (planner techniques, recipe search, intent drafts, kernel and speculation
// setups) is in the reviewed set, so a product-made setup is never refused.
func TestEngineArgsAdmitEveryFlagTheProductEmits(t *testing.T) {
	product := map[string][]string{
		"vllm": {"--max-num-seqs", "--max-num-batched-tokens", "--kv-cache-dtype", "--quantization", "--api-server-count",
			"--stream-interval", "--speculative-config", "--max-cudagraph-capture-size", "--enforce-eager", "--no-async-scheduling",
			"--no-enable-prefix-caching", "--enable-prefix-caching", "--max-model-len", "--tensor-parallel-size", "--gpu-memory-utilization",
			"--performance-mode", "--block-size", "--served-model-name", "--tool-call-parser", "--enable-auto-tool-choice",
			"--reasoning-parser", "--mm-encoder-tp-mode", "--enable-expert-parallel", "--language-model-only", "--seed", "--moe-backend",
			"--cudagraph-capture-sizes", "--compilation-config", "-cc", "-O3"},
		"sglang": {"--max-running-requests", "--chunked-prefill-size", "--schedule-policy", "--disable-radix-cache", "--quantization",
			"--kv-cache-dtype", "--speculative-algorithm", "--speculative-num-steps", "--speculative-eagle-topk",
			"--speculative-num-draft-tokens", "--disable-cuda-graph", "--disable-decode-cuda-graph", "--disable-overlap-schedule",
			"--cuda-graph-max-bs", "--cuda-graph-max-bs-decode", "--cuda-graph-bs-decode", "--cuda-graph-bs-prefill", "--tp-size", "--tp",
			"--context-length", "--mem-fraction-static", "--enable-metrics", "--served-model-name", "--tool-call-parser", "--reasoning-parser",
			"--max-prefill-tokens", "--fp8-gemm-backend", "--linear-attn-prefill-backend", "--linear-attn-decode-backend",
			"--no-enable-flashinfer-autotune", "--language-model-only", "--enable-linear-replayssm-spec", "--speculative-draft-model-path",
			"--speculative-draft-model-revision", "--speculative-draft-model-quantization", "--speculative-dflash-block-size",
			"--speculative-adaptive", "--enable-hierarchical-cache", "--disable-piecewise-cuda-graph", "--seed"},
	}
	for runtime, flags := range product {
		for _, flag := range flags {
			if err := ValidateEngineArgs(runtime, []string{flag}); err != nil {
				t.Errorf("%s: %v", runtime, err)
			}
		}
	}
}

// The deployment spec reference lists every reviewed argument, so the error
// that points at it is never wrong.
func TestDeploymentSpecListsEveryReviewedEngineArgument(t *testing.T) {
	body, err := os.ReadFile("../../docs/deployment-spec.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(body)
	for _, runtime := range []string{"vllm", "sglang"} {
		for _, flag := range ReviewedEngineFlags(runtime) {
			name := strings.TrimSuffix(flag, ".")
			if strings.HasPrefix(name, "-O") && len(name) == 3 {
				name = "-O0` to `-O3"
				if !strings.Contains(doc, name) {
					t.Errorf("%s: the reference does not list -O0 to -O3", runtime)
				}
				continue
			}
			if !strings.Contains(doc, "`"+name+"`") && !strings.Contains(doc, "(`"+name+"`") && !strings.Contains(doc, "`"+name+".<key>") {
				t.Errorf("%s: the reference does not list %s", runtime, flag)
			}
		}
	}
}

func TestContextLengthFlagIsAReviewedFlagOfItsRuntime(t *testing.T) {
	for _, runtime := range []string{"vllm", "sglang", " VLLM "} {
		flag := ContextLengthFlag(runtime)
		if flag == "" || ValidateEngineArgs(runtime, []string{flag, "8192"}) != nil {
			t.Fatalf("%q: context flag %q is not reviewed", runtime, flag)
		}
	}
	if flag := ContextLengthFlag("custom-oci"); flag != "" {
		t.Fatalf("custom image got a context flag %q", flag)
	}
}

func TestContextLengthReadsEitherRuntimesFlag(t *testing.T) {
	for _, test := range []struct {
		args  []string
		value string
		set   bool
	}{
		{nil, "", false},
		{[]string{"--enable-prefix-caching", "--max-num-batched-tokens", "2048"}, "", false},
		{[]string{"--enable-prefix-caching", "--max-model-len", "8192"}, "8192", true},
		{[]string{"--max-model-len=16384"}, "16384", true},
		{[]string{"--context-length", "32768", "--tp-size", "2"}, "32768", true},
		{[]string{"--max-model-len"}, "", true},
	} {
		value, set := ContextLength(test.args)
		if value != test.value || set != test.set {
			t.Fatalf("%v: got (%q, %t), want (%q, %t)", test.args, value, set, test.value, test.set)
		}
	}
}
