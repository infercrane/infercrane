package kerneltuning

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizedartifact"
	"github.com/infercrane/infercrane/internal/technique"
)

const (
	// TechniqueID registers "kernels tuned for your shapes" with the
	// technique contract (internal/technique).
	TechniqueID = "tuned-moe-kernels"
	// TechniqueVersion changes when the tuning method does.
	TechniqueVersion = "0.1.0"
	// EvidenceKernelInvocation names the evidence every result carries: each
	// tuned start's engine log read every tuned file from the mount, and each
	// control start's reported the engine's default for it.
	EvidenceKernelInvocation = "kernel-invocation-proof"
	// ModelFeatureFusedMoEFP8Block marks a model whose MoE layers run vLLM's
	// block-FP8 fused-MoE kernel (ModelFeatures).
	ModelFeatureFusedMoEFP8Block = "fused_moe_fp8_block"
	// ArtifactTool is the builder recorded on a set's artifact plan.
	ArtifactTool   = "infercrane-kernel-tune"
	artifactFormat = "vllm-fused-moe-tuned-configs"
)

// ModelFeatures are the technique-contract model features a model's
// config.json declares for this package: block-FP8 fused MoE on one GPU.
func ModelFeatures(config []byte) []string {
	shape, ok, err := ShapeFromModelConfig(config, 1)
	if err != nil || !ok || shape.DType != "fp8_w8a8" || shape.BlockN == 0 {
		return nil
	}
	return []string{ModelFeatureFusedMoEFP8Block}
}

// SetSource finds the tuned set built for a model revision on an engine
// version and GPU (engine file-name form), or nil when none was built.
type SetSource interface {
	Find(ctx context.Context, model, modelRevision, runtime, runtimeVersion, device string) (*domain.KernelConfigSet, error)
}

// Technique is the contract's view of tuned kernel sets: given a starting
// setup on a reviewed engine and GPU with a block-FP8 MoE model, it proposes
// that setup with the set the source holds for it, unchanged otherwise.
type Technique struct {
	Sets SetSource
	// Accelerators are the GPUs, by the product's names, sets are built and
	// reviewed on; a set is per device, so the list is exact.
	Accelerators []string
}

// Manifest declares the technique. It changes answers as far as the
// contract is concerned: new tile settings can change the last digits of a
// sum, so the task-score and divergence gates apply until a measured run
// shows identical answers on that GPU.
func (t Technique) Manifest() technique.Manifest {
	return technique.Manifest{
		SchemaVersion: technique.SchemaV1, ID: TechniqueID, Version: TechniqueVersion, Label: "Kernels tuned for your shapes", License: "Apache-2.0",
		Requires: technique.Requires{Engines: []technique.Engine{{Name: "vllm", Versions: reviewedVersions("vllm")}}, Accelerators: slices.Clone(t.Accelerators),
			ModelFeatures: []string{ModelFeatureFusedMoEFP8Block}},
		Relieves:   []technique.Limit{technique.LimitKernelTime},
		Costs:      []technique.Limit{technique.LimitBuildTime},
		Prediction: technique.Method{Metric: "output_tokens_per_second", Basis: "amdahl"},
		Artifact: &technique.Build{Kind: optimizedartifact.KindKernelConfigs, RunsIn: technique.RunsInDisposableContainer, UsesCustomerContent: false,
			Produces: "the engine's fused-MoE tile files for one GPU and the model's MoE shape, pinned to the model revision, engine version and GPU"},
		ChangesAnswers: true,
		Runtime:        technique.Runtime{Policy: technique.RuntimeStatic},
		Evidence:       RequiredEvidence(),
		Fallback: technique.Fallback{Serves: technique.FallbackEngineDefault,
			Reason: "vLLM reads its own file or built-in default for every shape the set does not name, and the set keeps the engine's default at every step size tuning did not beat"},
	}
}

// RequiredEvidence is the manifest's evidence list, for the judge that
// writes this technique's results.
func RequiredEvidence() []string {
	return []string{EvidenceKernelInvocation}
}

func reviewedVersions(runtime string) []string {
	var versions []string
	for _, engine := range reviewedEngines {
		if engine.runtime == runtime {
			versions = append(versions, engine.version)
		}
	}
	return versions
}

// Candidates proposes the starting setup with the tuned set, or says why not.
func (t Technique) Candidates(ctx context.Context, setup technique.Setup) ([]technique.Candidate, string, error) {
	if t.Sets == nil {
		return nil, "no tuned sets are available", nil
	}
	device := DeviceForGPU(setup.Accelerator)
	set, err := t.Sets.Find(ctx, setup.Model, setup.ModelRevision, setup.Engine, setup.EngineVersion, device)
	if err != nil {
		return nil, "", err
	}
	if set == nil {
		return nil, fmt.Sprintf("no tuned set was built for %s on %s %s and %s", setup.Model, setup.Engine, setup.EngineVersion, device), nil
	}
	if err = ValidateForRevision(*set, setup.Engine, setup.EngineVersion, setup.Accelerator); err != nil {
		return nil, "the tuned set does not fit this setup: " + err.Error(), nil
	}
	names := make([]string, len(set.Files))
	for index, file := range set.Files {
		names[index] = file.Name
	}
	return []technique.Candidate{{
		Technique: TechniqueID, TechniqueVersion: TechniqueVersion, Label: "Kernels tuned for your shapes",
		Args: slices.Clone(setup.Args), Env: cloneEnv(setup.Env),
		Artifact:       &technique.ArtifactRef{ID: "kernel-configs-" + strings.TrimPrefix(set.Digest, "sha256:")[:16], Kind: optimizedartifact.KindKernelConfigs, Digest: set.Digest},
		ChangesAnswers: true,
		Reason: fmt.Sprintf("The engine runs its default MoE tile settings on %s for %s; a set tuned on this GPU type holds faster settings at the measured step sizes and keeps the default everywhere else.",
			device, strings.Join(names, ", ")),
	}}, "", nil
}

func cloneEnv(env map[string]string) map[string]string {
	if env == nil {
		return nil
	}
	out := make(map[string]string, len(env))
	for name, value := range env {
		out[name] = value
	}
	return out
}

// ArtifactPlan is a set's optimized-artifact plan: what it was built from
// and for, so the artifact is pinned to the model revision, engine version
// and GPU and refused anywhere else.
func ArtifactPlan(set domain.KernelConfigSet, baseModelArtifactID, model, modelRevision, builderImageDigest string) (optimizedartifact.Plan, error) {
	if err := Validate(set); err != nil {
		return optimizedartifact.Plan{}, err
	}
	names := make([]string, len(set.Files))
	for index, file := range set.Files {
		names[index] = file.Name
	}
	configuration, _ := json.Marshal(map[string]any{"operator": set.Operator, "runtime": set.Runtime, "runtime_version": set.RuntimeVersion,
		"model": model, "model_revision": modelRevision, "files": names, "set_digest": set.Digest, "fallback": technique.FallbackEngineDefault})
	constraints, _ := json.Marshal(map[string]any{"device": set.Device})
	plan := optimizedartifact.Plan{BaseModelArtifactID: baseModelArtifactID, Kind: optimizedartifact.KindKernelConfigs, Format: artifactFormat,
		Tool: ArtifactTool, ToolVersion: set.Tuner, Algorithm: "coordinate-search-keep-if-faster", BuilderImageDigest: builderImageDigest,
		LicenseSPDX: "Apache-2.0", Configuration: configuration, HardwareConstraints: constraints, RequiresQualityReview: true}
	return plan, optimizedartifact.ValidatePlan(plan)
}
