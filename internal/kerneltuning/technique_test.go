package kerneltuning

import (
	"context"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizedartifact"
	"github.com/infercrane/infercrane/internal/technique"
)

type staticSets map[string]domain.KernelConfigSet

func (s staticSets) Find(_ context.Context, model, revision, runtime, version, device string) (*domain.KernelConfigSet, error) {
	if set, ok := s[strings.Join([]string{model, revision, runtime, version, device}, "|")]; ok {
		return &set, nil
	}
	return nil, nil
}

func TestTechniquePlugsIntoTheContract(t *testing.T) {
	set := testSet(t) // vLLM 0.34.0, NVIDIA_H100_80GB_HBM3
	sets := staticSets{strings.Join([]string{"Qwen/Qwen3.6-35B-A3B-FP8", "c653bba4", "vllm", "0.34.0", h100}, "|"): set}
	registry, err := technique.NewRegistry(Technique{Sets: sets, Accelerators: []string{"H100", "L40S"}})
	if err != nil {
		t.Fatal(err)
	}
	manifest := registry.Manifests()[0]
	if manifest.Artifact.RunsIn != technique.RunsInDisposableContainer || manifest.Artifact.UsesCustomerContent || manifest.Fallback.Serves != technique.FallbackEngineDefault ||
		!manifest.ChangesAnswers || manifest.Relieves[0] != technique.LimitKernelTime {
		t.Fatalf("manifest = %+v", manifest)
	}
	setup := technique.Setup{Model: "Qwen/Qwen3.6-35B-A3B-FP8", ModelRevision: "c653bba4", ModelFeatures: []string{ModelFeatureFusedMoEFP8Block},
		Engine: "vllm", EngineVersion: "0.34.0", Accelerator: "H100", GPUCount: 1, Args: []string{"--trust-remote-code"}}
	candidates, skipped, err := registry.Candidates(context.Background(), setup)
	if err != nil || len(candidates) != 1 || len(skipped) != 0 {
		t.Fatalf("candidates = %+v, skipped = %+v, %v", candidates, skipped, err)
	}
	candidate := candidates[0]
	if candidate.Artifact.Kind != optimizedartifact.KindKernelConfigs || candidate.Artifact.Digest != set.Digest || strings.Join(candidate.Args, " ") != "--trust-remote-code" {
		t.Fatalf("candidate = %+v", candidate)
	}

	for name, change := range map[string]func(*technique.Setup){
		"a dense model":         func(s *technique.Setup) { s.ModelFeatures = nil },
		"an unreviewed GPU":     func(s *technique.Setup) { s.Accelerator = "L4" },
		"an unreviewed engine":  func(s *technique.Setup) { s.EngineVersion = "0.20.2" },
		"no set for this model": func(s *technique.Setup) { s.ModelRevision = "other" },
		"a set for another GPU": func(s *technique.Setup) { s.Accelerator = "L40S" },
	} {
		other := setup
		change(&other)
		candidates, skipped, err = registry.Candidates(context.Background(), other)
		if err != nil || len(candidates) != 0 || len(skipped) != 1 || skipped[0].Reason == "" {
			t.Errorf("%s: candidates = %+v, skipped = %+v, %v", name, candidates, skipped, err)
		}
	}
}

func TestModelFeaturesAndArtifactPlan(t *testing.T) {
	qwen := `{"text_config":{"num_experts":256,"moe_intermediate_size":512},"quantization_config":{"quant_method":"fp8","weight_block_size":[128,128]}}`
	if features := ModelFeatures([]byte(qwen)); len(features) != 1 || features[0] != ModelFeatureFusedMoEFP8Block {
		t.Fatalf("features = %v", features)
	}
	for _, config := range []string{`{"hidden_size":4096}`, `{"num_experts":8,"intermediate_size":14336}`, `{"num_experts":8,"intermediate_size":14336,"quantization_config":{"quant_method":"fp8"}}`} {
		if features := ModelFeatures([]byte(config)); len(features) != 0 {
			t.Errorf("%s: features = %v", config, features)
		}
	}
	plan, err := ArtifactPlan(testSet(t), "artifact-1", "Qwen/Qwen3.6-35B-A3B-FP8", "c653bba46dfa06e9658dcc0b0dc87fe4685f14a2",
		"sha256:20fdb089454afbb324efa4110a2c025ff7b27cc1a086e07aa580b18df8ad1ce1")
	if err != nil || plan.Kind != optimizedartifact.KindKernelConfigs || !plan.RequiresQualityReview || !strings.Contains(string(plan.Configuration), "c653bba4") ||
		!strings.Contains(string(plan.HardwareConstraints), h100) {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if _, err = optimizedartifact.InputDigest(plan); err != nil {
		t.Fatal(err)
	}
}
