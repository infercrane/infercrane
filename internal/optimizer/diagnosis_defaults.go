package optimizer

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/optimizationreadiness"
	"github.com/infercrane/infercrane/internal/provideridentity"
)

// Accelerator memory classes that decide vLLM's batch defaults.
const (
	memorySmall = "small"
	memoryLarge = "large"
	// memoryHuge is vLLM 0.34.0's tier for 160 GiB or more; 0.22.1 has no
	// such tier and treats those GPUs as large.
	memoryHuge = "huge"
)

// The vLLM releases whose flags and defaults were read from source. They are
// written out rather than taken from support's pins so the table names
// exactly what was read; a test checks that the pinned release is one of them.
const (
	vllm034    = "0.34.0"
	vllm0221   = "0.22.1"
	sglang0523 = "0.5.23"
	sglang0512 = "0.5.12"
)

// reviewedEngineVersions are the engine versions whose flags and defaults
// were read from source, newest first. Another version has unknown defaults.
var reviewedEngineVersions = map[string][]string{
	"vllm":   {vllm034, vllm0221},
	"sglang": {sglang0523, sglang0512},
}

// acceleratorMemoryClass applies one vLLM release's rule for batch defaults
// (EngineArgs.get_batch_defaults). vLLM 0.22.1: 70 GiB or more of device
// memory, except A100, is large. vLLM 0.34.0 adds a huge tier at 160 GiB or
// more and keeps A100 (and GB10, which InferCrane does not list) small below
// it. The rule reads the device's memory and name on every platform, ROCm
// included, so a reviewed AMD accelerator's memory decides its class the
// same way.
func acceleratorMemoryClass(gpu, version string) (string, bool) {
	memory, known := provideridentity.AcceleratorMemoryGiB(gpu)
	if !known {
		return "", false
	}
	if memory >= 160 && version != vllm0221 {
		return memoryHuge, true
	}
	if memory >= 70 && !strings.Contains(provideridentity.NormalizedAccelerator(gpu), "a100") {
		return memoryLarge, true
	}
	return memorySmall, true
}

type engineDefault struct {
	value string
	// small, large and huge replace value when the default depends on
	// memory; a release without a huge tier leaves it empty.
	small, large, huge string
	sized              bool
}

// sizedValue is the default for one memory class; "" when not recorded.
func (d engineDefault) sizedValue(class string) string {
	switch class {
	case memoryHuge:
		if d.huge != "" {
			return d.huge
		}
		return d.large
	case memoryLarge:
		return d.large
	default:
		return d.small
	}
}

// engineDefaults is what each reviewed engine version runs with when a flag
// is not set, read from its source. A flag that is not listed has an unknown
// default, so a setup changing it cannot be told apart from a no-op.
var engineDefaults = map[string]map[string]engineDefault{
	"vllm@" + vllm0221: {
		"--max-num-batched-tokens": {sized: true, small: "2048", large: "8192"},
		"--max-num-seqs":           {sized: true, small: "256", large: "1024"},
		"--kv-cache-dtype":         {value: "auto"},
		"--quantization":           {value: ""},
		"--api-server-count":       {value: "1"},
		"--stream-interval":        {value: "1"},
		"--speculative-config":     {value: ""},
	},
	// vLLM 0.34.0, read from the v0.34.0 source (EngineArgs.get_batch_defaults,
	// CacheConfig, ModelConfig, SchedulerConfig, entrypoints/cli/serve.py) and
	// its own runs of 23 November: 2048 / 256 below 70 GiB and on A100, 8192 /
	// 1024 up to 160 GiB, 16384 / 1024 above; no speculation unless set; the
	// cache in the model's precision; weights as published; one result per
	// step; one API server per data-parallel rank, so one for a server on one
	// rank (the Rust front end always runs one).
	"vllm@" + vllm034: {
		"--max-num-batched-tokens": {sized: true, small: "2048", large: "8192", huge: "16384"},
		"--max-num-seqs":           {sized: true, small: "256", large: "1024", huge: "1024"},
		"--kv-cache-dtype":         {value: "auto"},
		"--quantization":           {value: ""},
		"--api-server-count":       {value: "1"},
		"--stream-interval":        {value: "1"},
		"--speculative-config":     {value: ""},
	},
	"sglang@" + sglang0512: {
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
	// SGLang 0.5.23, read at tag v0.5.23 (commit 297e54fd), fields in
	// python/sglang/srt/arg_groups/fields/: kv_cache_dtype "auto" (model.py:221),
	// quantization unset (model.py:184), schedule_policy "fcfs" (schedule.py:102),
	// no speculation (spec.py:31-69). The request limit, prefill chunk, and
	// decode graph size are sized from GPU memory at start (schedule.py:31-55,
	// exec_.py:629). 0.5.23 renamed --cuda-graph-max-bs to
	// --cuda-graph-max-bs-decode and -prefill.
	"sglang@" + sglang0523: {
		"--kv-cache-dtype":               {value: "auto"},
		"--quantization":                 {value: ""},
		"--schedule-policy":              {value: "fcfs"},
		"--speculative-algorithm":        {value: ""},
		"--speculative-num-steps":        {value: ""},
		"--speculative-eagle-topk":       {value: ""},
		"--speculative-num-draft-tokens": {value: ""},
		"--chunked-prefill-size":         {sized: true},
		"--max-running-requests":         {sized: true},
		"--cuda-graph-max-bs-decode":     {sized: true},
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
	class, classKnown := acceleratorMemoryClass(gpu, version)
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
			default:
				value = resolved.sizedValue(class)
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

// vLLM captures CUDA graphs for batches of 1, 2, 4, then every 8 up to 256
// and every 16 above, up to min(max_num_seqs x 2, the ceiling) capped at the
// per-step token budget, which is also captured when it fits
// (config/vllm.py, VllmConfig._set_cudagraph_sizes). A larger batch runs
// without a full graph. The ceiling is 512 in 0.22.1; 0.34.0 raises it to
// 1024 on data-center Blackwell (compute capability family 100) and there,
// unless the deprecated capture-size flags are set, captures a decode graph
// for every batch up to max_num_seqs within 8192 tokens
// (_split_cudagraph_capture_sizes).
const (
	vllmCaptureCeiling          = 512
	vllmBlackwellCaptureCeiling = 1024
	vllmBlackwellDecodeTokens   = 8192
)

// blackwellDataCenter lists the accelerators of compute capability family
// 100 that InferCrane names.
var blackwellDataCenter = []string{"b200", "b300", "gb200", "gb300"}

func dataCenterBlackwell(gpu string) bool {
	name := provideridentity.NormalizedAccelerator(gpu)
	for _, family := range blackwellDataCenter {
		if strings.HasPrefix(name, family) {
			return true
		}
	}
	return false
}

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
		// SGLang 0.5.23 sizes the decode graph with --cuda-graph-max-bs-decode.
		flag := "--cuda-graph-max-bs"
		if engine == "sglang" && version != sglang0512 {
			flag = "--cuda-graph-max-bs-decode"
		}
		if _, why := withoutNoOps(engine, version, gpu, []runtimeArg{{flag, ""}}); why != "" {
			return nil, why
		}
		return nil, fmt.Sprintf("No reviewed %s flag sizes CUDA graphs yet.", engineName(engine))
	}
	if !slices.Contains(reviewedEngineVersions[engine], version) {
		return nil, fmt.Sprintf("InferCrane has not recorded %s %s's defaults, so it cannot tell whether this change does anything.", engineName(engine), version)
	}
	size, seqs, tokens, why := vllmCaptureSize(current, version, gpu)
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

// vllmCaptureSize is the largest batch one vLLM release captures a CUDA
// graph for with these arguments on this accelerator, with the request and
// token limits it was derived from.
func vllmCaptureSize(args []string, version, gpu string) (size, seqs, tokens int, why string) {
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
	class, known := acceleratorMemoryClass(gpu, version)
	if !known {
		return 0, 0, 0, fmt.Sprintf("InferCrane does not know %s's memory, so it cannot tell vLLM's default batch limits.", gpu)
	}
	defaults, reviewed := engineDefaults["vllm@"+version]
	if !reviewed {
		return 0, 0, 0, fmt.Sprintf("InferCrane has not recorded vLLM %s's defaults, so it cannot tell which batch sizes it captures CUDA graphs for.", version)
	}
	resolved := func(flag string) (int, bool) {
		if value, set := argValue(args, flag); set {
			parsed, err := strconv.Atoi(value)
			return parsed, err == nil && parsed > 0
		}
		parsed, err := strconv.Atoi(defaults[flag].sizedValue(class))
		return parsed, err == nil
	}
	seqs, seqsOK := resolved("--max-num-seqs")
	tokens, tokensOK := resolved("--max-num-batched-tokens")
	if !seqsOK || !tokensOK {
		return 0, 0, 0, "The starting setup's batch limits could not be read, so InferCrane cannot tell which batch sizes vLLM captures CUDA graphs for."
	}
	ceiling := vllmCaptureCeiling
	if version != vllm0221 && dataCenterBlackwell(gpu) {
		ceiling = vllmBlackwellCaptureCeiling
	}
	limit := min(seqs*2, ceiling)
	if value, set := argValue(args, "--max-cudagraph-capture-size"); set {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			return 0, 0, 0, "The starting setup's CUDA graph capture size could not be read, so InferCrane cannot tell whether a larger capture does anything."
		}
		limit = parsed
	} else if version != vllm0221 && dataCenterBlackwell(gpu) {
		// Decode graphs cover every batch the server admits.
		decode := min(seqs, tokens, vllmBlackwellDecodeTokens)
		return decode, seqs, tokens, ""
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

// capBatchToCache keeps a larger batch inside what the engine's cache holds.
// Each running request holds at least one cache block, so a request limit
// above the blocks the engine reports never runs more requests, and an engine
// that keeps a recurrent state per request refuses to start with it. The
// limit is lowered to the blocks, a multiple of eight; when that is not above
// the setup's own limit, no larger batch fits and the change is not planned.
func capBatchToCache(args []runtimeArg, current []string, engine, version, gpu string, diagnosis Diagnosis) ([]runtimeArg, string) {
	limit, known := cacheBlockLimit(diagnosis)
	if !known {
		return args, ""
	}
	out := make([]runtimeArg, 0, len(args))
	for _, arg := range args {
		value, err := strconv.Atoi(arg.value)
		if arg.flag != "--max-num-seqs" && arg.flag != "--max-running-requests" || err != nil || value <= limit {
			out = append(out, arg)
			continue
		}
		now, nowKnown := requestLimit(current, arg.flag, engine, version, gpu)
		if !nowKnown || limit <= now {
			setup := "the setup's own limit"
			if nowKnown {
				setup = fmt.Sprintf("the setup's limit of %d", now)
			}
			return nil, fmt.Sprintf("The engine reports cache room for at most %d requests at once per server (one cache block each at least), which is not above %s, so no larger batch fits.", limit, setup)
		}
		out = append(out, runtimeArg{arg.flag, strconv.Itoa(limit)})
	}
	return out, ""
}

// requestLimit is the most requests the setup lets run at once: its own flag,
// else the engine's recorded default on this accelerator.
func requestLimit(current []string, flag, engine, version, gpu string) (int, bool) {
	if value, set := flagValues(current)[flag]; set {
		limit, err := strconv.Atoi(value)
		return limit, err == nil
	}
	resolved, listed := engineDefaults[engine+"@"+version][flag]
	if !listed {
		return 0, false
	}
	value := resolved.value
	if resolved.sized {
		class, known := acceleratorMemoryClass(gpu, version)
		if !known || resolved.small == "" || resolved.large == "" {
			return 0, false
		}
		value = resolved.sizedValue(class)
	}
	limit, err := strconv.Atoi(value)
	return limit, err == nil
}

// flagValues reads a runtime argument list as flag to value; a flag without
// a value maps to "true".
func flagValues(args []string) map[string]string {
	out := map[string]string{}
	for index := 0; index < len(args); index++ {
		flag := args[index]
		if !strings.HasPrefix(flag, "--") {
			continue
		}
		if name, value, inline := strings.Cut(flag, "="); inline {
			out[name] = value
			continue
		}
		if index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
			out[flag] = args[index+1]
			index++
			continue
		}
		out[flag] = "true"
	}
	return out
}
