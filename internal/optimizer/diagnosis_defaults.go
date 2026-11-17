package optimizer

import (
	"fmt"
	"strings"

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
