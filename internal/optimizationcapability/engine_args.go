package optimizationcapability

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Engine arguments a tenant or a candidate may add to a built-in runtime's
// command are a reviewed set per engine. Anything else is refused, so a new
// engine flag reaches a server only after review. Flags the product sets
// itself (host, port, model, revision, keys, LoRA, config) and flags that
// load code or files named in their value are not in the set.

// refusedEngineFlags are refused on every engine and in every argument list,
// with the reason given to the caller.
var refusedEngineFlags = map[string]string{
	"--trust-remote-code":             "runs code from the model repository, and InferCrane serves models only with the engine's own implementation",
	"--enable-custom-logit-processor": "lets a request carry code for the server to run",
}

// reviewedEngineFlags are the flags a tenant or candidate may set, per
// built-in runtime. A flag with a prefix entry (ending in ".") also admits
// its dotted sub-keys, for example -cc.cudagraph_mode=FULL.
var reviewedEngineFlags = map[string][]string{
	"vllm": {
		// Model shape and parallelism.
		"--max-model-len", "--tensor-parallel-size", "-tp", "--pipeline-parallel-size", "-pp",
		"--data-parallel-size", "-dp", "--enable-expert-parallel", "--dtype", "--seed",
		"--served-model-name", "--language-model-only", "--limit-mm-per-prompt", "--mm-encoder-tp-mode",
		// Batching and scheduling.
		"--max-num-seqs", "--max-num-batched-tokens", "--enable-chunked-prefill", "--no-enable-chunked-prefill",
		"--long-prefill-token-threshold", "--max-num-partial-prefills", "--async-scheduling", "--no-async-scheduling",
		"--stream-interval", "--api-server-count", "-asc", "--performance-mode",
		// Cache.
		"--enable-prefix-caching", "--no-enable-prefix-caching", "--prefix-caching-hash-algo",
		"--cache-aware-admission-window", "--cache-aware-admission-threshold", "--kv-cache-dtype", "--block-size",
		"--gpu-memory-utilization", "--kv-cache-memory-bytes", "--enable-extensible-kv-cache",
		"--no-enable-extensible-kv-cache", "--kv-offloading-size", "--kv-offloading-backend", "--swap-space", "--cpu-offload-gb",
		// Precision and kernels.
		"--quantization", "-q", "--attention-backend", "--moe-backend", "--all2all-backend", "--enable-eplb", "--enable-dbo",
		// Graphs and compilation.
		"--enforce-eager", "--max-cudagraph-capture-size", "--cudagraph-capture-sizes", "--compilation-config",
		"--compilation-config.", "-cc", "-cc.", "-O", "-O0", "-O1", "-O2", "-O3", "-O.",
		// Speculation and outputs.
		"--speculative-config", "-sc", "--max-logprobs",
		// Chat parsing.
		"--tool-call-parser", "--enable-auto-tool-choice", "--reasoning-parser",
		// Logging.
		"--disable-log-stats",
	},
	"sglang": {
		// Model shape and parallelism.
		"--context-length", "--tp-size", "--tp", "--dp-size", "--ep-size", "--attn-dp-size", "--enable-dp-attention",
		"--dtype", "--seed", "--served-model-name", "--language-model-only",
		// Batching and scheduling.
		"--max-running-requests", "--max-prefill-tokens", "--chunked-prefill-size", "--schedule-policy",
		"--disable-overlap-schedule", "--enable-mixed-chunk", "--stream-interval",
		// Cache.
		"--mem-fraction-static", "--kv-cache-dtype", "--page-size", "--disable-radix-cache",
		"--enable-hierarchical-cache", "--enable-hicache", "--hicache-ratio", "--hicache-size", "--hicache-write-policy",
		// Precision and kernels.
		"--quantization", "--attention-backend", "--decode-attention-backend", "--prefill-attention-backend",
		"--fp8-gemm-backend", "--moe-runner-backend", "--linear-attn-prefill-backend", "--linear-attn-decode-backend",
		"--enable-flashinfer-autotune", "--no-enable-flashinfer-autotune",
		// Graphs.
		"--disable-cuda-graph", "--disable-decode-cuda-graph", "--disable-prefill-cuda-graph", "--disable-piecewise-cuda-graph",
		"--cuda-graph-max-bs", "--cuda-graph-max-bs-decode", "--cuda-graph-max-bs-prefill", "--cuda-graph-bs",
		"--cuda-graph-bs-decode", "--cuda-graph-bs-prefill", "--cuda-graph-backend-decode", "--cuda-graph-backend-prefill",
		// Speculation.
		"--speculative-algorithm", "--speculative-num-steps", "--speculative-eagle-topk", "--speculative-num-draft-tokens",
		"--speculative-draft-model-path", "--speculative-draft-model-revision", "--speculative-draft-model-quantization",
		"--speculative-dflash-block-size", "--speculative-adaptive", "--enable-linear-replayssm-spec",
		// Chat parsing and metrics.
		"--tool-call-parser", "--reasoning-parser", "--enable-metrics",
	},
}

// contextLengthFlags is the reviewed flag that bounds each built-in
// runtime's context length.
var contextLengthFlags = map[string]string{"vllm": "--max-model-len", "sglang": "--context-length"}

// ContextLengthFlag returns the reviewed flag that bounds a built-in
// runtime's context length, or "" for any other runtime.
func ContextLengthFlag(runtime string) string {
	return contextLengthFlags[strings.ToLower(strings.TrimSpace(runtime))]
}

// ContextLength returns the value an argument list gives a built-in
// runtime's context-length flag, as written ("--max-model-len 8192" or
// "--context-length=8192"), and whether the list sets one at all.
func ContextLength(args []string) (string, bool) {
	for index := 0; index < len(args); index++ {
		name, value, inline := strings.Cut(strings.TrimSpace(args[index]), "=")
		if name != contextLengthFlags["vllm"] && name != contextLengthFlags["sglang"] {
			continue
		}
		if !inline && index+1 < len(args) {
			value = args[index+1]
		}
		return strings.TrimSpace(value), true
	}
	return "", false
}

// negativeNumber is a value such as -1 (SGLang disables chunked prefill
// with --chunked-prefill-size -1), not a flag.
var negativeNumber = regexp.MustCompile(`^-[0-9]+(\.[0-9]+)?$`)

// ReviewedEngine reports whether a runtime's extra arguments are checked
// against a reviewed set.
func ReviewedEngine(runtime string) bool {
	_, ok := reviewedEngineFlags[strings.ToLower(strings.TrimSpace(runtime))]
	return ok
}

// ValidateEngineArgs refuses the flags that are refused everywhere and, for a
// built-in runtime, every flag outside its reviewed set. Values are not
// echoed, so a credential pasted into an argument does not reach an error.
func ValidateEngineArgs(runtime string, args []string) error {
	if err := RefuseUnsafeEngineArgs(args); err != nil {
		return err
	}
	reviewed, ok := reviewedEngineFlags[strings.ToLower(strings.TrimSpace(runtime))]
	if !ok {
		return nil
	}
	for _, flag := range engineFlags(args) {
		if !admitted(reviewed, flag) {
			return fmt.Errorf("engine argument %s is not reviewed for %s; reviewed arguments are listed in the deployment spec reference", boundedFlag(flag), runtime)
		}
	}
	return nil
}

// RefuseUnsafeEngineArgs refuses the flags that are never allowed, whatever
// the engine or image.
func RefuseUnsafeEngineArgs(args []string) error {
	for _, flag := range engineFlags(args) {
		if reason, refused := refusedEngineFlags[flag]; refused {
			return fmt.Errorf("engine argument %s is refused: it %s", flag, reason)
		}
	}
	return nil
}

// ReviewedEngineFlags lists a built-in runtime's reviewed flags, sorted.
func ReviewedEngineFlags(runtime string) []string {
	out := slices.Clone(reviewedEngineFlags[strings.ToLower(strings.TrimSpace(runtime))])
	slices.Sort(out)
	return out
}

// engineFlags returns each flag in an argument list without its value:
// "--flag", "--flag=value", "-cc.key=value". A token that does not start with
// "-", or a negative number, is a value.
func engineFlags(args []string) []string {
	var out []string
	for _, arg := range args {
		arg = strings.TrimSpace(arg)
		if !strings.HasPrefix(arg, "-") || negativeNumber.MatchString(arg) {
			continue
		}
		name, _, _ := strings.Cut(arg, "=")
		out = append(out, name)
	}
	return out
}

func admitted(reviewed []string, flag string) bool {
	if slices.Contains(reviewed, flag) {
		return true
	}
	for _, entry := range reviewed {
		if strings.HasSuffix(entry, ".") && strings.HasPrefix(flag, entry) {
			return true
		}
	}
	return false
}

// boundedFlag echoes only a short, well-formed flag name.
func boundedFlag(flag string) string {
	if len(flag) > 64 || strings.ContainsFunc(flag, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.')
	}) {
		return "(not a flag name)"
	}
	return flag
}
