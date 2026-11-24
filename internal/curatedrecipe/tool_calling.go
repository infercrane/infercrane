package curatedrecipe

import (
	"slices"
	"strings"
)

const (
	// CapabilityToolCalling and TaskTools tell customers an entry calls tools.
	// Every profile of such an entry starts its engine with tool calling on.
	CapabilityToolCalling = "tool-calling"
	TaskTools             = "tools"

	// EngineVLLM and EngineSGLang name the inference engine a profile starts.
	EngineVLLM   = "vllm"
	EngineSGLang = "sglang"
)

// toolParsers names the parser each engine reads a model's tool calls with.
// The name depends on the format the model's own chat template asks the model
// to write calls in, and each engine names its parsers differently.
type toolParsers struct {
	vllm, sglang string
}

// withToolCalling turns tool calling on in every profile. vLLM accepts tool
// requests only with --enable-auto-tool-choice and a --tool-call-parser;
// SGLang needs only the parser. A profile on an engine the parsers do not name
// is a catalog error.
func withToolCalling(parsers toolParsers, profiles ...ServingProfile) []ServingProfile {
	out := make([]ServingProfile, 0, len(profiles))
	for _, profile := range profiles {
		var flags []string
		switch {
		case profile.Runtime == EngineVLLM && parsers.vllm != "":
			flags = []string{"--enable-auto-tool-choice", "--tool-call-parser", parsers.vllm}
		case profile.Runtime == EngineSGLang && parsers.sglang != "":
			flags = []string{"--tool-call-parser", parsers.sglang}
		default:
			panic("no reviewed tool-call parser for profile " + profile.Name)
		}
		profile.RuntimeArgs = append(append([]string(nil), profile.RuntimeArgs...), flags...)
		out = append(out, profile)
	}
	return out
}

// ProfileEngine names the inference engine a profile starts: the built-in
// runtime it names, or the engine its pinned custom image packages. It is
// empty for an image InferCrane does not recognize.
func ProfileEngine(profile ServingProfile) string {
	switch profile.Runtime {
	case EngineVLLM, EngineSGLang:
		return profile.Runtime
	}
	switch image := profile.Workload.Image; {
	case strings.HasPrefix(image, "vllm/vllm-openai"):
		return EngineVLLM
	case strings.HasPrefix(image, "lmsysorg/sglang"):
		return EngineSGLang
	}
	return ""
}

// LaunchArgs is the argv a profile starts its engine with: the custom image's
// command, or the arguments added to a built-in runtime's command.
func (p ServingProfile) LaunchArgs() []string {
	if !p.Workload.Empty() {
		return append([]string(nil), p.Workload.Command...)
	}
	return append([]string(nil), p.RuntimeArgs...)
}

// ToolCallingSetup is how a reviewed profile starts one engine to call tools.
type ToolCallingSetup struct {
	// Args are vLLM's switch, the reasoning parser the profile pairs with its
	// tool parser (the tool parser reads the answer after reasoning is split
	// off), and the tool-call parser.
	Args []string
	// Profile names the reviewed profile the flags come from.
	Profile string
	// EngineVersion is that profile's engine version. It is empty for a
	// built-in runtime, which runs the version InferCrane pins.
	EngineVersion string
}

// ToolCalling returns how a reviewed profile of this entry starts engine to
// call tools. A profile on the given engine version wins; otherwise the first
// profile on that engine does. ok is false when the entry claims no tool
// calling or none of its profiles runs that engine with a parser.
func (e Entry) ToolCalling(engine, version string) (ToolCallingSetup, bool) {
	if !slices.Contains(e.Capabilities, CapabilityToolCalling) {
		return ToolCallingSetup{}, false
	}
	var chosen *ServingProfile
	for index := range e.Profiles {
		profile := &e.Profiles[index]
		if ProfileEngine(*profile) != engine || flagValue(profile.LaunchArgs(), "--tool-call-parser") == "" {
			continue
		}
		if chosen == nil || (version != "" && profile.RuntimeVersion == version && chosen.RuntimeVersion != version) {
			chosen = profile
		}
	}
	if chosen == nil {
		return ToolCallingSetup{}, false
	}
	launch, setup := chosen.LaunchArgs(), ToolCallingSetup{Profile: chosen.Name, EngineVersion: chosen.RuntimeVersion}
	if engine == EngineVLLM {
		setup.Args = append(setup.Args, "--enable-auto-tool-choice")
	}
	if reasoning := flagValue(launch, "--reasoning-parser"); reasoning != "" {
		setup.Args = append(setup.Args, "--reasoning-parser", reasoning)
	}
	setup.Args = append(setup.Args, "--tool-call-parser", flagValue(launch, "--tool-call-parser"))
	return setup, true
}

// toolCallingFlags take a value; --enable-auto-tool-choice is a switch.
var toolCallingFlags = []string{"--tool-call-parser", "--reasoning-parser"}

// SplitToolCallingArgs separates a profile's tool-calling flags from its
// tuning flags. Tool calling is part of the model's API contract, reviewed
// with the catalog entry, not an optimization the planner compiles.
func SplitToolCallingArgs(args []string) (tuning, toolCalling []string) {
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--enable-auto-tool-choice":
			toolCalling = append(toolCalling, arg)
		case slices.Contains(toolCallingFlags, arg) && index+1 < len(args):
			toolCalling = append(toolCalling, arg, args[index+1])
			index++
		case slices.ContainsFunc(toolCallingFlags, func(flag string) bool { return strings.HasPrefix(arg, flag+"=") }):
			toolCalling = append(toolCalling, arg)
		default:
			tuning = append(tuning, arg)
		}
	}
	return tuning, toolCalling
}

// flagValue reads a flag's value from argv, written as "--flag value" or
// "--flag=value".
func flagValue(args []string, flag string) string {
	for index, arg := range args {
		if value, found := strings.CutPrefix(arg, flag+"="); found {
			return value
		}
		if arg == flag && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}
