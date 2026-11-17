package optimizer

import (
	"fmt"
	"regexp"
	"strings"

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

// acceleratorMemoryGiB is the device memory of accelerators InferCrane
// plans on, keyed by normalized name. An absent name has unknown memory.
var acceleratorMemoryGiB = map[string]int{
	"h100": 80, "h100sxm": 80, "h100pcie": 80, "h10080gb": 80, "h100nvl": 94, "h800": 80, "h20": 96, "h200": 141, "gh200": 96,
	"b200": 180, "b300": 288, "gb200": 186, "gb300": 288, "rtxpro6000": 96,
	"a100": 40, "a10040gb": 40, "a10080gb": 80, "a100sxm4": 40, "a800": 80,
	"l40s": 48, "l40": 48, "l4": 24, "a10g": 24, "a10": 24, "a6000": 48, "rtxa6000": 48, "rtx6000ada": 48,
	"rtx4090": 24, "rtx5090": 32, "t4": 16, "v100": 16,
	"mi300x": 192, "mi325x": 256, "mi355x": 288,
}

var memorySuffix = regexp.MustCompile(`([0-9]+)gb$`)

// acceleratorMemoryClass applies vLLM 0.22.1's rule for batch defaults:
// 70 GiB or more of device memory, except A100, gets the larger defaults.
func acceleratorMemoryClass(gpu string) (string, bool) {
	name := normalizedAccelerator(gpu)
	memory, known := acceleratorMemoryGiB[name]
	if !known {
		if match := memorySuffix.FindStringSubmatch(name); match != nil {
			var parsed int
			if _, err := fmt.Sscanf(match[1], "%d", &parsed); err == nil && parsed > 0 {
				memory, known = parsed, true
			}
		}
	}
	if !known {
		return "", false
	}
	if memory >= 70 && !strings.Contains(name, "a100") {
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
