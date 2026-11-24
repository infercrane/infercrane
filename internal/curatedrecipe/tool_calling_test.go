package curatedrecipe

import (
	"reflect"
	"slices"
	"testing"
)

// pinnedToolCallParsers lists the tool-call parsers the engines behind the
// built-in runtimes register. An unknown name stops the engine at start.
var pinnedToolCallParsers = map[string][]string{
	// vLLM 0.22.1, vllm/tool_parsers/__init__.py (_TOOL_PARSERS_TO_REGISTER):
	// https://github.com/vllm-project/vllm/blob/v0.22.1/vllm/tool_parsers/__init__.py
	EngineVLLM: {"apertus", "cohere_command3", "cohere_command4", "deepseek_v3", "deepseek_v31", "deepseek_v32", "deepseek_v4", "ernie45", "functiongemma", "gemma4", "gigachat3", "glm45", "glm47", "granite", "granite-20b-fc", "granite4", "hermes", "hunyuan_a13b", "hy_v3", "internlm", "jamba", "kimi_k2", "lfm2", "llama3_json", "llama4_json", "llama4_pythonic", "longcat", "mimo", "minimax", "minimax_m2", "mistral", "olmo3", "openai", "phi4_mini_json", "poolside_v1", "pythonic", "qwen3_coder", "qwen3_xml", "seed_oss", "step3", "step3p5", "xlam"},
	// SGLang 0.5.12, FunctionCallParser.ToolCallParserEnum:
	// https://github.com/sgl-project/sglang/blob/v0.5.12/python/sglang/srt/function_call/function_call_parser.py
	EngineSGLang: {"deepseekv3", "deepseekv31", "deepseekv32", "deepseekv4", "gemma4", "gigachat3", "glm", "glm45", "glm47", "gpt-oss", "hermes", "hunyuan", "interns1", "kimi_k2", "lfm2", "llama3", "mimo", "minimax-m2", "mistral", "poolside_v1", "pythonic", "qwen", "qwen25", "qwen3_coder", "step3", "step3p5", "trinity"},
}

// An entry that tells customers it calls tools must start every engine with
// tool calling on. vLLM refuses tool requests unless it runs with
// --enable-auto-tool-choice and a --tool-call-parser, and refuses to start
// with the first flag alone; SGLang needs only the parser.
func TestEntriesThatClaimToolCallingStartEveryEngineWithIt(t *testing.T) {
	for _, entry := range All() {
		claims := slices.Contains(entry.Capabilities, CapabilityToolCalling) || slices.Contains(entry.Tasks, TaskTools)
		for _, profile := range entry.Profiles {
			engine, args := ProfileEngine(profile), profile.LaunchArgs()
			parser := flagValue(args, "--tool-call-parser")
			if claims && parser == "" {
				t.Errorf("%s/%s claims tool calling but starts %s without a tool-call parser: %v", entry.Name, profile.Name, engine, args)
			}
			if engine == EngineVLLM && (parser != "") != slices.Contains(args, "--enable-auto-tool-choice") {
				t.Errorf("%s/%s: vLLM needs --enable-auto-tool-choice and --tool-call-parser together: %v", entry.Name, profile.Name, args)
			}
			if parser == "" || profile.Runtime == "custom-oci" {
				continue
			}
			if registered := pinnedToolCallParsers[profile.Runtime]; !slices.Contains(registered, parser) {
				t.Errorf("%s/%s: the pinned %s does not register tool-call parser %q", entry.Name, profile.Name, profile.Runtime, parser)
			}
		}
	}
}

func TestReviewedToolCallingSetups(t *testing.T) {
	wanted := map[string]map[string][]string{
		"qwen3-8b": {
			EngineVLLM:   {"--enable-auto-tool-choice", "--tool-call-parser", "hermes"},
			EngineSGLang: {"--tool-call-parser", "qwen"},
		},
		"mistral-7b-instruct": {
			EngineVLLM: {"--enable-auto-tool-choice", "--tool-call-parser", "mistral"},
		},
	}
	for name, byEngine := range wanted {
		entry, ok := Get(name)
		if !ok {
			t.Fatalf("%s is missing", name)
		}
		for _, profile := range entry.Profiles {
			want := byEngine[ProfileEngine(profile)]
			if want == nil {
				t.Fatalf("%s/%s runs an engine without a reviewed tool-calling setup", name, profile.Name)
			}
			args := profile.LaunchArgs()
			if len(args) < len(want) || !slices.Equal(args[len(args)-len(want):], want) {
				t.Fatalf("%s/%s starts with %v, want it to end with the tool-calling flags %v", name, profile.Name, args, want)
			}
		}
	}

	// vLLM's recommended Llama 3.1 chat template sits at a different path in
	// the NVIDIA and AMD images, and these profiles may run on either, so the
	// entry does not claim tool calling until a setup covers both.
	llama, _ := Get("llama-3.1-8b-instruct")
	if slices.Contains(llama.Capabilities, CapabilityToolCalling) || slices.Contains(llama.Tasks, TaskTools) {
		t.Fatalf("Llama 3.1 claims tool calling without a reviewed setup: %+v %+v", llama.Capabilities, llama.Tasks)
	}
	for _, profile := range llama.Profiles {
		if flagValue(profile.LaunchArgs(), "--tool-call-parser") != "" {
			t.Fatalf("Llama 3.1 starts a tool-call parser it does not claim: %v", profile.LaunchArgs())
		}
	}
}

func TestToolCallingCarriesTheReviewedParsersOfOneEngine(t *testing.T) {
	entry, _ := Get("qwen3.8-27b-fp8")
	setup, ok := entry.ToolCalling(EngineSGLang, "0.5.20")
	want := ToolCallingSetup{Args: []string{"--reasoning-parser", "qwen3", "--tool-call-parser", "qwen3_coder"}, Profile: "sglang-h200-nextn-k4-provisional", EngineVersion: "0.5.20"}
	if !ok || !reflect.DeepEqual(setup, want) {
		t.Fatalf("the profile on the same engine version wins: %+v ok=%v", setup, ok)
	}

	qwen3, _ := Get("qwen3-8b")
	setup, ok = qwen3.ToolCalling(EngineSGLang, "0.5.20")
	want = ToolCallingSetup{Args: []string{"--tool-call-parser", "qwen"}, Profile: "sglang-elastic"}
	if !ok || !reflect.DeepEqual(setup, want) {
		t.Fatalf("a profile on another SGLang version is the fallback, and says so: %+v ok=%v", setup, ok)
	}
	if setup, ok := qwen3.ToolCalling(EngineVLLM, ""); !ok || !slices.Equal(setup.Args, []string{"--enable-auto-tool-choice", "--tool-call-parser", "hermes"}) {
		t.Fatalf("vLLM setup=%+v ok=%v", setup, ok)
	}

	mistral, _ := Get("mistral-7b-instruct")
	if setup, ok := mistral.ToolCalling(EngineSGLang, "0.5.20"); ok || !reflect.DeepEqual(setup, ToolCallingSetup{}) {
		t.Fatalf("no reviewed SGLang setup means no parser: %+v", setup)
	}
	embeddings, _ := Get("bge-m3-embeddings")
	if _, ok := embeddings.ToolCalling(EngineVLLM, ""); ok {
		t.Fatal("a model that calls no tools has no tool-calling setup")
	}
}

func TestToolCallingDoesNotAliasTheCatalog(t *testing.T) {
	entry, _ := Get("qwen3-8b")
	setup, _ := entry.ToolCalling(EngineVLLM, "")
	setup.Args[len(setup.Args)-1] = "mutated"
	again, _ := Get("qwen3-8b")
	if fresh, _ := again.ToolCalling(EngineVLLM, ""); fresh.Args[len(fresh.Args)-1] != "hermes" {
		t.Fatalf("returned flags alias the reviewed catalog: %v", fresh.Args)
	}
}

func TestSplitToolCallingArgsKeepsTuningInOrder(t *testing.T) {
	tuning, toolCalling := SplitToolCallingArgs([]string{"--enable-prefix-caching", "--enable-auto-tool-choice", "--max-num-batched-tokens", "2048", "--tool-call-parser", "hermes", "--reasoning-parser=qwen3"})
	if !slices.Equal(tuning, []string{"--enable-prefix-caching", "--max-num-batched-tokens", "2048"}) {
		t.Fatalf("tuning=%v", tuning)
	}
	if !slices.Equal(toolCalling, []string{"--enable-auto-tool-choice", "--tool-call-parser", "hermes", "--reasoning-parser=qwen3"}) {
		t.Fatalf("tool calling=%v", toolCalling)
	}
	if tuning, toolCalling := SplitToolCallingArgs(nil); tuning != nil || toolCalling != nil {
		t.Fatalf("empty argv split into %v and %v", tuning, toolCalling)
	}
}
