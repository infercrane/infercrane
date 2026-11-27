package support

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

// MaxRuntimeEnv bounds the engine environment variables one revision carries.
const MaxRuntimeEnv = 8

var environmentName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// EngineEnvironmentVariable is one engine switch a deployment may set through
// the environment, reviewed for one engine version on the platforms listed.
// Every entry is a fixed set of values read from the engine's source, so an
// entry can never carry a credential or free text.
type EngineEnvironmentVariable struct {
	Runtime       string   `json:"runtime"`
	EngineVersion string   `json:"engine_version"`
	Platforms     []string `json:"platforms"`
	Name          string   `json:"name"`
	// Label names what a value other than Default turns on, for setup names.
	Label  string   `json:"label"`
	Values []string `json:"values"`
	// Default is the value the pinned engine runs with when the variable is
	// unset: read from the engine's source and the pinned image's
	// configuration, both listed in Sources.
	Default     string   `json:"default"`
	Description string   `json:"description"`
	Sources     []string `json:"sources"`
}

const (
	// vLLM 0.22.1 reads VLLM_ROCM_USE_AITER as true only for "1" or "true"
	// and defaults it to False; the AITER sub-switches for linear layers,
	// MoE, RMSNorm, MLA, and MHA default to on once it is set.
	sourceVLLMEnvs = "https://github.com/vllm-project/vllm/blob/v0.22.1/vllm/envs.py"
	// The pinned vllm/vllm-openai-rocm:v0.22.1 image (sha256:368b2992…)
	// sets AITER_ROCM_ARCH=gfx942;gfx950 and does not set
	// VLLM_ROCM_USE_AITER (image configuration read on 2026-11-17).
	sourceVLLMROCmImage = "https://hub.docker.com/layers/vllm/vllm-openai-rocm/v0.22.1/images/sha256-368b2992c776ad653e40e9e00d38aa51cf1e9a16558b05d07c42818abd9ec031"
	// vLLM 0.34.0 reads VLLM_ROCM_USE_AITER the same way ("1" or "true",
	// default False), and VLLM_USE_RUST_FRONTEND with int(), default 0: "1"
	// starts the vllm-rs HTTP server shipped in the package instead of the
	// Python one.
	sourceVLLM034Envs = "https://github.com/vllm-project/vllm/blob/v0.34.0/vllm/envs.py"
	// The pinned vllm/vllm-openai-rocm:v0.34.0 image (sha256:3cb773d2…) sets
	// AITER_ROCM_ARCH=gfx942;gfx950 and does not set VLLM_ROCM_USE_AITER
	// (image configuration read on 2026-11-26).
	sourceVLLM034ROCmImage = "https://hub.docker.com/layers/vllm/vllm-openai-rocm/v0.34.0/images/sha256-3cb773d23f063afa86291ef75773cdd486154ddea94c213300f4ed7cc42b7af1"
	// The Rust front end's routes, its API key check, and its access log.
	sourceVLLM034RustServer = "https://github.com/vllm-project/vllm/tree/v0.34.0/rust/src/server/src"
	// On one H100 with the pinned 0.34.0 CUDA image, a server with
	// VLLM_USE_RUST_FRONTEND=1 started, refused a request without its key,
	// answered, called a tool, and exported every engine metric family the
	// control plane reads.
	sourceVLLM034RustCheck = "docs/testing/evidence/qwen38-27b-fp8-h100-vllm034-serving-check-2026-11-23.json"
	// AMD: AITER provides fused kernels for MI300X/MI325X and MI350 Series
	// GPUs in vLLM V1, enabled by the master switch VLLM_ROCM_USE_AITER=1.
	sourceROCmVLLMOptimization = "https://rocm.docs.amd.com/en/latest/how-to/rocm-for-ai/inference-optimization/vllm-optimization.html"
	// vLLM recipes: the AMD hardware override sets VLLM_ROCM_USE_AITER "1".
	sourceVLLMRecipesAMD = "https://github.com/vllm-project/recipes/blob/fa02ae800b4998a7985e3654116b602fb6c139df/SCHEMA.md"
)

var engineEnvironment = []EngineEnvironmentVariable{
	{
		Runtime: DefaultRuntime, EngineVersion: DefaultRuntimeVersion,
		Platforms: []string{provideridentity.PlatformROCmGFX942, provideridentity.PlatformROCmGFX950},
		Name:      "VLLM_ROCM_USE_AITER", Label: "AMD AITER kernels", Values: []string{"0", "1"}, Default: "0",
		Description: "Turns on AMD's AITER kernels for attention, linear layers, MoE, and RMSNorm in vLLM on AMD Instinct GPUs.",
		Sources:     []string{sourceVLLM034Envs, sourceVLLM034ROCmImage, sourceROCmVLLMOptimization, sourceVLLMRecipesAMD},
	},
	{
		Runtime: DefaultRuntime, EngineVersion: DefaultRuntimeVersion,
		Platforms: []string{provideridentity.PlatformCUDA},
		Name:      "VLLM_USE_RUST_FRONTEND", Label: "Rust front end", Values: []string{"0", "1"}, Default: "0",
		Description: "Serves vLLM's HTTP API from its Rust server instead of the Python one. Same paths, metrics, and API key; its access log lines differ, and it refuses the Anthropic Messages and batch routes.",
		Sources:     []string{sourceVLLM034Envs, sourceVLLM034RustServer, sourceVLLM034RustCheck},
	},
	{
		Runtime: DefaultRuntime, EngineVersion: LegacyVLLMRuntimeVersion,
		Platforms: []string{provideridentity.PlatformROCmGFX942, provideridentity.PlatformROCmGFX950},
		Name:      "VLLM_ROCM_USE_AITER", Label: "AMD AITER kernels", Values: []string{"0", "1"}, Default: "0",
		Description: "Turns on AMD's AITER kernels for attention, linear layers, MoE, and RMSNorm in vLLM on AMD Instinct GPUs.",
		Sources:     []string{sourceVLLMEnvs, sourceVLLMROCmImage, sourceROCmVLLMOptimization, sourceVLLMRecipesAMD},
	},
}

// EngineEnvironment returns every reviewed engine environment variable.
func EngineEnvironment() []EngineEnvironmentVariable {
	out := make([]EngineEnvironmentVariable, len(engineEnvironment))
	for index, variable := range engineEnvironment {
		out[index] = cloneEngineVariable(variable)
	}
	return out
}

// LookupEngineEnvironment finds the reviewed variable for one runtime, engine
// version, and accelerator platform. Anything else is not reviewed.
func LookupEngineEnvironment(runtime, version, platform, name string) (EngineEnvironmentVariable, bool) {
	runtime = defaultRuntime(strings.TrimSpace(runtime))
	if version = NormalizeRuntimeVersion(version); version == "" {
		version = defaultEngineVersion(runtime)
	}
	for _, variable := range engineEnvironment {
		if variable.Runtime == runtime && variable.EngineVersion == version && variable.Name == name && slices.Contains(variable.Platforms, platform) {
			return cloneEngineVariable(variable), true
		}
	}
	return EngineEnvironmentVariable{}, false
}

// ValidateRuntimeEnv admits only reviewed engine variables, with reviewed
// values, for the runtime's pinned image on the GPU's platform. A refused
// value is never echoed back, so a credential pasted into the map does not
// reach logs or error responses.
func ValidateRuntimeEnv(runtime, version, gpu string, workload runtimecontract.Workload, env map[string]string) error {
	if len(env) == 0 {
		return nil
	}
	if len(env) > MaxRuntimeEnv {
		return fmt.Errorf("runtime env must not set more than %d variables", MaxRuntimeEnv)
	}
	runtime = defaultRuntime(strings.TrimSpace(runtime))
	if version = NormalizeRuntimeVersion(version); version == "" {
		version = defaultEngineVersion(runtime)
	}
	if strings.TrimSpace(gpu) == "" {
		return fmt.Errorf("runtime env needs a GPU, because each variable is reviewed per accelerator platform")
	}
	platform := provideridentity.AcceleratorPlatform(gpu)
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		variable, reviewed := LookupEngineEnvironment(runtime, version, platform, name)
		if !reviewed {
			return fmt.Errorf("runtime env variable %q is not reviewed for %s %s on %s (platform %s); reviewed: %s", boundedName(name), runtime, version, gpu, platform, reviewedNames(runtime, version, platform))
		}
		if !slices.Contains(variable.Values, env[name]) {
			return fmt.Errorf("runtime env variable %s must be one of %s", name, strings.Join(variable.Values, ", "))
		}
	}
	pinned, ok := RuntimeWorkloadFor(runtime, version, platform)
	if !ok || ServedWorkloadFor(runtime, version, gpu, workload).Image != pinned.Image {
		return fmt.Errorf("runtime env is reviewed only for the pinned %s %s image on platform %s", runtime, version, platform)
	}
	return nil
}

func reviewedNames(runtime, version, platform string) string {
	var names []string
	for _, variable := range engineEnvironment {
		if variable.Runtime == runtime && variable.EngineVersion == version && slices.Contains(variable.Platforms, platform) {
			names = append(names, variable.Name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// boundedName echoes only a short, well-formed variable name, so a value
// pasted into the name position is not repeated in the error either.
func boundedName(name string) string {
	if len(name) > 64 || !environmentName.MatchString(name) {
		return "(not an environment variable name)"
	}
	return name
}

func defaultEngineVersion(runtime string) string {
	switch runtime {
	case DefaultRuntime:
		return DefaultRuntimeVersion
	case "sglang":
		return SGLangRuntimeVersion
	}
	return ""
}

func cloneEngineVariable(variable EngineEnvironmentVariable) EngineEnvironmentVariable {
	variable.Platforms = slices.Clone(variable.Platforms)
	variable.Values = slices.Clone(variable.Values)
	variable.Sources = slices.Clone(variable.Sources)
	return variable
}

// RuntimeArgsDigest identifies a revision's engine arguments, and its engine
// environment when it has one, for capacity evidence. Without an environment
// the digest is the one recorded before environments existed.
func RuntimeArgsDigest(args []string, env map[string]string) string {
	var body []byte
	if len(env) == 0 {
		body, _ = json.Marshal(args)
	} else {
		body, _ = json.Marshal(struct {
			Args []string          `json:"args"`
			Env  map[string]string `json:"env"`
		}{args, env})
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// BenchmarkRuntimeConfig is the runtime configuration a benchmark records:
// the engine arguments and, only when set, the reviewed engine environment,
// so rows without one keep their recorded shape.
func BenchmarkRuntimeConfig(args []string, env map[string]string) map[string]any {
	config := map[string]any{"args": args}
	if len(env) > 0 {
		config["env"] = env
	}
	return config
}
