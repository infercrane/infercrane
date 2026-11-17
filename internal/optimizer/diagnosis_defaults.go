package optimizer

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/optimizationreadiness"
	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/support"
)

// Accelerator memory classes that decide vLLM's batch defaults.
const (
	memorySmall = "small"
	memoryLarge = "large"
)

// reviewedEngineVersions are the engine versions whose flags and defaults
// were read from source. Another version has unknown defaults.
var reviewedEngineVersions = map[string]string{"vllm": support.DefaultRuntimeVersion, "sglang": support.SGLangRuntimeVersion}

// acceleratorMemoryClass applies vLLM 0.22.1's rule for batch defaults:
// 70 GiB or more of device memory, except A100, gets the larger defaults.
// The rule reads the device's memory and name on every platform, ROCm
// included (EngineArgs.get_batch_defaults), so a reviewed AMD accelerator's
// memory decides its class the same way.
func acceleratorMemoryClass(gpu string) (string, bool) {
	memory, known := provideridentity.AcceleratorMemoryGiB(gpu)
	if !known {
		return "", false
	}
	if memory >= 70 && !strings.Contains(provideridentity.NormalizedAccelerator(gpu), "a100") {
		return memoryLarge, true
	}
	return memorySmall, true
}

type engineDefault struct {
	value string
	// small and large replace value when the default depends on memory.
	small, large string
	sized        bool
}

// engineDefaults is what each reviewed engine version runs with when a flag
// is not set, read from its source. A flag that is not listed has an unknown
// default, so a setup changing it cannot be told apart from a no-op.
var engineDefaults = map[string]map[string]engineDefault{
	"vllm@" + support.DefaultRuntimeVersion: {
		"--max-num-batched-tokens": {sized: true, small: "2048", large: "8192"},
		"--max-num-seqs":           {sized: true, small: "256", large: "1024"},
		"--kv-cache-dtype":         {value: "auto"},
		"--quantization":           {value: ""},
		"--api-server-count":       {value: "1"},
		"--stream-interval":        {value: "1"},
		"--speculative-config":     {value: ""},
	},
	"sglang@" + support.SGLangRuntimeVersion: {
		"--kv-cache-dtype":               {value: "auto"},
		"--quantization":                 {value: ""},
		"--schedule-policy":              {value: "fcfs"},
		"--speculative-algorithm":        {value: ""},
		"--speculative-num-steps":        {value: ""},
		"--speculative-eagle-topk":       {value: ""},
		"--speculative-num-draft-tokens": {value: ""},
		// SGLang sizes these from GPU memory at start; the rule is not recorded.
		"--chunked-prefill-size": {sized: true},
		"--max-running-requests": {sized: true},
		"--cuda-graph-max-bs":    {sized: true},
	},
}

// withoutNoOps drops every argument equal to the engine's resolved default on
// this accelerator. It reports why the change cannot be planned when a
// default is unknown, or when nothing is left to change.
func withoutNoOps(engine, version, gpu string, args []runtimeArg) ([]runtimeArg, string) {
	defaults, reviewed := engineDefaults[engine+"@"+version]
	if !reviewed {
		return nil, fmt.Sprintf("InferCrane has not recorded %s %s's defaults, so it cannot tell whether this change does anything.", engineName(engine), version)
	}
	class, classKnown := acceleratorMemoryClass(gpu)
	var out []runtimeArg
	for _, arg := range args {
		resolved, listed := defaults[arg.flag]
		if !listed {
			return nil, fmt.Sprintf("InferCrane has not recorded %s %s's default for %s, so it cannot tell whether this change does anything.", engineName(engine), version, arg.flag)
		}
		value := resolved.value
		if resolved.sized {
			switch {
			case resolved.small == "" || resolved.large == "":
				return nil, fmt.Sprintf("%s %s sizes %s from GPU memory when it starts, and InferCrane has not recorded that rule, so it cannot tell whether this change does anything.", engineName(engine), version, arg.flag)
			case !classKnown:
				return nil, fmt.Sprintf("InferCrane does not know %s's memory, so it cannot tell %s's default for %s.", gpu, engineName(engine), arg.flag)
			case class == memoryLarge:
				value = resolved.large
			default:
				value = resolved.small
			}
		}
		if arg.value == value {
			continue
		}
		out = append(out, arg)
	}
	if len(out) == 0 {
		return nil, fmt.Sprintf("These are already %s %s's defaults on %s, so the setup would equal the starting setup.", engineName(engine), version, gpu)
	}
	return out, ""
}

// vLLM 0.22.1 captures CUDA graphs for batches of 1, 2, 4, then every 8 up to
// 256 and every 16 above, up to min(max_num_seqs x 2, 512) capped at the
// per-step token budget, which is also captured when it fits
// (config/vllm.py, VllmConfig._set_cudagraph_sizes). A larger batch runs
// without a full graph.
const vllmCaptureCeiling = 512

// captureSizeFlags change which batch sizes vLLM captures in ways the planner
// does not model: explicit size lists, compilation and optimization levels,
// performance modes, and speculation, which multiplies the decode batch.
var captureSizeFlags = []string{"--cudagraph-capture-sizes", "--compilation-config", "-cc", "--performance-mode", "--speculative-config"}

// cudaGraphArgs turns CUDA graphs back on when the starting setup turned them
// off. Otherwise, on vLLM, it raises the largest captured batch to cover the
// requests the server runs at once, only when that is above what vLLM
// captures by default for the starting setup.
func cudaGraphArgs(current []string, engine, version, gpu string, diagnosis Diagnosis) ([]string, string) {
	if flags := techniques[TechniqueCUDAGraphs].disables[engine]; len(flags) > 0 && hasAny(current, flags) {
		return withoutFlags(current, flags), ""
	}
	if engine != "vllm" {
		if _, why := withoutNoOps(engine, version, gpu, []runtimeArg{{"--cuda-graph-max-bs", ""}}); why != "" {
			return nil, why
		}
		return nil, fmt.Sprintf("No reviewed %s flag sizes CUDA graphs yet.", engineName(engine))
	}
	if version != reviewedEngineVersions[engine] {
		return nil, fmt.Sprintf("InferCrane has not recorded %s %s's defaults, so it cannot tell whether this change does anything.", engineName(engine), version)
	}
	size, seqs, tokens, why := vllmCaptureSize(current, gpu)
	if why != "" {
		return nil, why
	}
	running, ok := diagnosis.signal(optimizationreadiness.SignalRunningPerReplica)
	if !ok {
		return nil, "This needs the engine's count of running requests per server, which the diagnosis window did not report."
	}
	if running <= float64(size) {
		return nil, fmt.Sprintf("About %s ran at once per server, and vLLM %s already captures CUDA graphs for batches of up to %d here, so a larger capture changes nothing.", requestWords(running), version, size)
	}
	target := min(captureGridUp(min(int(math.Ceil(running*captureHeadroom)), seqs)), tokens)
	if target <= size {
		return nil, fmt.Sprintf("The starting setup runs at most %d requests per step, and vLLM %s already captures CUDA graphs for batches of up to %d here, so a larger capture changes nothing.", min(seqs, tokens), version, size)
	}
	derived, _ := withArgs(current, []runtimeArg{{"--max-cudagraph-capture-size", strconv.Itoa(target)}})
	return derived, ""
}

// vllmCaptureSize is the largest batch vLLM 0.22.1 captures a CUDA graph for
// with these arguments on this accelerator, with the request and token limits
// it was derived from.
func vllmCaptureSize(args []string, gpu string) (size, seqs, tokens int, why string) {
	for _, arg := range args {
		for _, flag := range captureSizeFlags {
			if arg == flag || strings.HasPrefix(arg, flag+"=") || strings.HasPrefix(arg, flag+".") {
				return 0, 0, 0, "The starting setup sets options that change which batch sizes vLLM captures CUDA graphs for, so InferCrane cannot tell whether a larger capture does anything."
			}
		}
		if strings.HasPrefix(arg, "-O") {
			return 0, 0, 0, "The starting setup sets an optimization level that changes which batch sizes vLLM captures CUDA graphs for, so InferCrane cannot tell whether a larger capture does anything."
		}
	}
	class, known := acceleratorMemoryClass(gpu)
	if !known {
		return 0, 0, 0, fmt.Sprintf("InferCrane does not know %s's memory, so it cannot tell vLLM's default batch limits.", gpu)
	}
	defaults := engineDefaults["vllm@"+reviewedEngineVersions["vllm"]]
	resolved := func(flag string) (int, bool) {
		if value, set := argValue(args, flag); set {
			parsed, err := strconv.Atoi(value)
			return parsed, err == nil && parsed > 0
		}
		value := defaults[flag].small
		if class == memoryLarge {
			value = defaults[flag].large
		}
		parsed, err := strconv.Atoi(value)
		return parsed, err == nil
	}
	seqs, seqsOK := resolved("--max-num-seqs")
	tokens, tokensOK := resolved("--max-num-batched-tokens")
	if !seqsOK || !tokensOK {
		return 0, 0, 0, "The starting setup's batch limits could not be read, so InferCrane cannot tell which batch sizes vLLM captures CUDA graphs for."
	}
	limit := min(seqs*2, vllmCaptureCeiling)
	if value, set := argValue(args, "--max-cudagraph-capture-size"); set {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return 0, 0, 0, "The starting setup's CUDA graph capture size could not be read, so InferCrane cannot tell whether a larger capture does anything."
		}
		limit = parsed
	}
	if limit >= tokens {
		return tokens, seqs, tokens, ""
	}
	return captureGridDown(limit), seqs, tokens, ""
}

// captureGridDown is the largest vLLM capture size at or below limit.
func captureGridDown(limit int) int {
	switch {
	case limit >= 256:
		return 256 + (limit-256)/16*16
	case limit >= 8:
		return limit / 8 * 8
	case limit >= 4:
		return 4
	default:
		return max(1, limit/2*2)
	}
}

// captureGridUp is the smallest vLLM capture size at or above size.
func captureGridUp(size int) int {
	switch {
	case size > 256:
		return 256 + (size-256+15)/16*16
	case size > 4:
		return (size + 7) / 8 * 8
	case size > 2:
		return 4
	default:
		return max(1, size)
	}
}

// argValue returns a flag's value written as "--flag value" or "--flag=value".
func argValue(args []string, flag string) (string, bool) {
	for index, arg := range args {
		if arg == flag && index+1 < len(args) {
			return args[index+1], true
		}
		if value, found := strings.CutPrefix(arg, flag+"="); found {
			return value, true
		}
	}
	return "", false
}

func hasAny(args, flags []string) bool {
	for _, flag := range flags {
		if contains(args, flag) {
			return true
		}
	}
	return false
}

// hasSpeculation reports a setup that sets any speculative decoding option.
func hasSpeculation(args []string) bool {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--speculative") {
			return true
		}
	}
	return false
}
