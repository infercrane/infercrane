package resultstore

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// KnobsFromArgs reads a setup's knobs from its engine flags: speculation
// (vLLM's --speculative-config, SGLang's --speculative-algorithm and
// --speculative-num-steps), --kv-cache-dtype and --quantization. Other flags
// are not knobs; a caller that changes them names them in Knobs.Flags, which
// makes the setup one no record measured unless one did.
func KnobsFromArgs(engine string, args []string) Knobs {
	knobs := Knobs{Engine: engine}
	values := FlagValues(args)
	if config, ok := values["--speculative-config"]; ok {
		var parsed struct {
			Method string `json:"method"`
			Model  string `json:"model"`
			Tokens int    `json:"num_speculative_tokens"`
		}
		if json.Unmarshal([]byte(config), &parsed) == nil {
			knobs.Speculation, knobs.Drafts = speculationMethod(parsed.Method, parsed.Model != ""), parsed.Tokens
		} else {
			knobs.Speculation = SpeculationUnknown
		}
	}
	if algorithm, ok := values["--speculative-algorithm"]; ok {
		knobs.Speculation = speculationMethod(algorithm, false)
		if steps, err := strconv.Atoi(values["--speculative-num-steps"]); err == nil {
			knobs.Drafts = steps
		}
	}
	knobs.KVCache = values["--kv-cache-dtype"]
	knobs.Quantization = values["--quantization"]
	return knobs.Normal()
}

func speculationMethod(method string, hasModel bool) string {
	switch strings.ToLower(strings.TrimSpace(method)) {
	case "mtp", "nextn", "deepseek_mtp", "qwen3_next_mtp", "qwen3_5_mtp":
		return SpeculationMTP
	case "ngram":
		return SpeculationNgram
	case "eagle", "eagle3":
		return SpeculationEagle
	case "draft_model", "":
		if hasModel {
			return SpeculationDraft
		}
	}
	return SpeculationUnknown
}

// FlagValues reads "--name value", "--name=value" and switches ("--name"
// followed by another flag) into a map; a switch's value is "".
func FlagValues(args []string) map[string]string {
	values := map[string]string{}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if !strings.HasPrefix(arg, "--") {
			continue
		}
		if name, value, ok := strings.Cut(arg, "="); ok {
			values[name] = value
			continue
		}
		if index+1 < len(args) && !strings.HasPrefix(args[index+1], "--") {
			values[arg] = args[index+1]
			index++
			continue
		}
		values[arg] = ""
	}
	return values
}

// knobFlags are the flags KnobsFromArgs reads.
var knobFlags = map[string]bool{"--speculative-config": true, "--speculative-algorithm": true, "--speculative-num-steps": true, "--speculative-eagle-topk": true,
	"--speculative-num-draft-tokens": true, "--kv-cache-dtype": true, "--quantization": true}

// KnobFlag reports whether KnobsFromArgs reads the flag.
func KnobFlag(name string) bool { return knobFlags[name] }

// SetupFromArgs describes a setup from its engine flags: speculation and
// draft tokens, KV cache format, weight quantization, and every other flag as
// "--name=value" ("--name" for a switch), sorted.
func SetupFromArgs(name, label, engine string, args []string) Setup {
	knobs := KnobsFromArgs(engine, args)
	setup := Setup{Name: name, Label: truncate(label, maxLabel), Speculation: knobs.Speculation, Drafts: knobs.Drafts, KVCache: knobs.KVCache, Quantization: knobs.Quantization}
	for flag, value := range FlagValues(args) {
		if knobFlags[flag] {
			continue
		}
		if value == "" {
			setup.Flags = append(setup.Flags, flag)
		} else {
			setup.Flags = append(setup.Flags, flag+"="+value)
		}
	}
	slices.Sort(setup.Flags)
	return setup
}
