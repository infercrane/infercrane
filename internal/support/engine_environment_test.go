package support

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

func TestEngineEnvironmentAdmitsOnlyReviewedSwitchesForThePinnedImage(t *testing.T) {
	aiter := map[string]string{"VLLM_ROCM_USE_AITER": "1"}
	for _, gpu := range []string{"MI300X", "AMD Instinct MI300X OAM", "AMD_Instinct_MI300X_OAM", "MI325X", "MI350X", "MI355X"} {
		if err := ValidateRuntimeEnv("vllm", DefaultRuntimeVersion, gpu, runtimecontract.Workload{}, aiter); err != nil {
			t.Fatalf("%s: %v", gpu, err)
		}
		// The served workload filled in by provisioning is the same image.
		served := ServedWorkload("vllm", gpu, runtimecontract.Workload{})
		if err := ValidateRuntimeEnv("", "", gpu, served, map[string]string{"VLLM_ROCM_USE_AITER": "0"}); err != nil {
			t.Fatalf("%s with the served workload: %v", gpu, err)
		}
	}
	if err := ValidateRuntimeEnv("vllm", DefaultRuntimeVersion, "H100", runtimecontract.Workload{}, nil); err != nil {
		t.Fatalf("an empty env is always valid: %v", err)
	}
	for name, test := range map[string]struct {
		runtime, version, gpu string
		workload              runtimecontract.Workload
		env                   map[string]string
		want                  string
	}{
		"CUDA GPU":                {"vllm", DefaultRuntimeVersion, "H100", runtimecontract.Workload{}, aiter, "not reviewed for vllm 0.22.1 on H100 (platform cuda); reviewed: none"},
		"unreviewed GPU name":     {"vllm", DefaultRuntimeVersion, "MI250X", runtimecontract.Workload{}, aiter, "not reviewed"},
		"SGLang sets its own":     {"sglang", SGLangRuntimeVersion, "MI300X", runtimecontract.Workload{}, aiter, "not reviewed for sglang"},
		"another engine version":  {"vllm", "0.23.0", "MI300X", runtimecontract.Workload{}, aiter, "not reviewed for vllm 0.23.0"},
		"custom image":            {"vllm", DefaultRuntimeVersion, "MI300X", runtimecontract.Workload{Image: "registry.internal:5000/team/vllm@sha256:" + strings.Repeat("b", 64), Command: []string{"vllm", "serve"}, Protocol: "openai"}, aiter, "only for the pinned vllm 0.22.1 image"},
		"custom-oci runtime":      {"custom-oci", "", "MI300X", runtimecontract.Workload{}, aiter, "not reviewed for custom-oci"},
		"no GPU":                  {"vllm", DefaultRuntimeVersion, "", runtimecontract.Workload{}, aiter, "needs a GPU"},
		"unreviewed value":        {"vllm", DefaultRuntimeVersion, "MI300X", runtimecontract.Workload{}, map[string]string{"VLLM_ROCM_USE_AITER": "true"}, "must be one of 0, 1"},
		"credential":              {"vllm", DefaultRuntimeVersion, "MI300X", runtimecontract.Workload{}, map[string]string{"HF_TOKEN": "hf_abc"}, `"HF_TOKEN" is not reviewed`},
		"worker key override":     {"vllm", DefaultRuntimeVersion, "MI300X", runtimecontract.Workload{}, map[string]string{"VLLM_API_KEY": "x"}, "not reviewed"},
		"value in the name":       {"vllm", DefaultRuntimeVersion, "MI300X", runtimeWorkloadFor(t, "MI300X"), map[string]string{"hf_abc123": "1"}, "(not an environment variable name)"},
		"too many":                {"vllm", DefaultRuntimeVersion, "MI300X", runtimecontract.Workload{}, manyVariables(MaxRuntimeEnv + 1), "more than 8"},
		"sub-switch not reviewed": {"vllm", DefaultRuntimeVersion, "MI300X", runtimecontract.Workload{}, map[string]string{"VLLM_ROCM_USE_AITER_MHA": "0"}, "not reviewed"},
	} {
		err := ValidateRuntimeEnv(test.runtime, test.version, test.gpu, test.workload, test.env)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%s: err=%v, want %q", name, err, test.want)
		}
		for _, value := range test.env {
			if len(value) > 1 && strings.Contains(err.Error(), value) {
				t.Fatalf("%s: error echoes a refused value: %v", name, err)
			}
		}
		if strings.Contains(err.Error(), "hf_abc") {
			t.Fatalf("%s: error echoes a pasted credential: %v", name, err)
		}
	}
}

func runtimeWorkloadFor(t *testing.T, gpu string) runtimecontract.Workload {
	t.Helper()
	workload, ok := RuntimeWorkload("vllm", provideridentity.AcceleratorPlatform(gpu))
	if !ok {
		t.Fatalf("no vLLM workload for %s", gpu)
	}
	return workload
}

func manyVariables(count int) map[string]string {
	out := map[string]string{}
	for index := range count {
		out["VARIABLE_"+string(rune('A'+index))] = "1"
	}
	return out
}

func TestEngineEnvironmentRecordsReviewedDefaultsAndNeverManagedNames(t *testing.T) {
	variables := EngineEnvironment()
	if len(variables) == 0 {
		t.Fatal("no reviewed engine variables")
	}
	// Names InferCrane or a provider adapter sets itself.
	managed := []string{"INFERCRANE_", "VLLM_API_KEY", "HF_", "HUGGINGFACE_", "TRANSFORMERS_OFFLINE", "SGLANG_USE_AITER"}
	for _, variable := range variables {
		if !environmentName.MatchString(variable.Name) || len(variable.Values) == 0 || len(variable.Sources) < 2 || variable.Description == "" {
			t.Fatalf("incomplete reviewed variable %#v", variable)
		}
		found := false
		for _, value := range variable.Values {
			found = found || value == variable.Default
		}
		if !found {
			t.Fatalf("%s default %q is not one of its values", variable.Name, variable.Default)
		}
		for _, prefix := range managed {
			if strings.HasPrefix(variable.Name, prefix) {
				t.Fatalf("%s collides with a name InferCrane manages", variable.Name)
			}
		}
		for _, platform := range variable.Platforms {
			if _, ok := RuntimeWorkload(variable.Runtime, platform); !ok {
				t.Fatalf("%s is reviewed for %s on %s, which has no pinned image", variable.Name, variable.Runtime, platform)
			}
		}
		if engine := defaultEngineVersion(variable.Runtime); engine != variable.EngineVersion {
			t.Fatalf("%s is reviewed for %s %s, but the pinned engine is %s", variable.Name, variable.Runtime, variable.EngineVersion, engine)
		}
	}
	aiter, ok := LookupEngineEnvironment("vllm", "", provideridentity.PlatformROCmGFX942, "VLLM_ROCM_USE_AITER")
	if !ok || aiter.Default != "0" {
		t.Fatalf("vLLM 0.22.1 leaves AITER off by default: %#v", aiter)
	}
	// Callers cannot change the reviewed table through a returned copy.
	aiter.Values[0], variables[0].Platforms[0] = "x", "x"
	if again, _ := LookupEngineEnvironment("vllm", "", provideridentity.PlatformROCmGFX942, "VLLM_ROCM_USE_AITER"); again.Values[0] != "0" || EngineEnvironment()[0].Platforms[0] == "x" {
		t.Fatal("reviewed table was mutated through a copy")
	}
	if _, ok := LookupEngineEnvironment("vllm", "", provideridentity.PlatformCUDA, "VLLM_ROCM_USE_AITER"); ok {
		t.Fatal("AITER is reviewed on CUDA")
	}
}

func TestRuntimeEvidenceKeepsItsRecordedShapeWithoutEngineEnvironment(t *testing.T) {
	args := []string{"--max-num-seqs", "64"}
	body, _ := json.Marshal(args)
	sum := sha256.Sum256(body)
	recorded := "sha256:" + hex.EncodeToString(sum[:])
	if got := RuntimeArgsDigest(args, nil); got != recorded {
		t.Fatalf("digest without env=%s, want the recorded %s", got, recorded)
	}
	withEnv := RuntimeArgsDigest(args, map[string]string{"VLLM_ROCM_USE_AITER": "1"})
	if withEnv == recorded || withEnv != RuntimeArgsDigest(args, map[string]string{"VLLM_ROCM_USE_AITER": "1"}) || withEnv == RuntimeArgsDigest(args, map[string]string{"VLLM_ROCM_USE_AITER": "0"}) {
		t.Fatalf("digest with env=%s does not identify the switch", withEnv)
	}
	if config := BenchmarkRuntimeConfig(args, nil); len(config) != 1 || config["env"] != nil {
		t.Fatalf("config without env=%v", config)
	}
	if config := BenchmarkRuntimeConfig(args, map[string]string{"VLLM_ROCM_USE_AITER": "1"}); config["env"].(map[string]string)["VLLM_ROCM_USE_AITER"] != "1" {
		t.Fatalf("config with env=%v", config)
	}
}
