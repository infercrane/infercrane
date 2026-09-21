// Package acceleratorlab coordinates profiler capture, isolated artifact
// building, and exact-accelerator qualification. It deliberately owns no
// provider credentials and cannot promote a deployment.
package acceleratorlab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/kernelplanner"
)

const (
	RequestSchemaV1 = "infercrane.accelerator-lab-request/v1"
	ResultSchemaV1  = "infercrane.accelerator-lab-result/v1"
)

var ErrExecutionAuthorityExpired = errors.New("accelerator lab execution authority expired")

type Modality string

const (
	ModalityText  Modality = "text"
	ModalityImage Modality = "image"
	ModalityVideo Modality = "video"
	ModalityAudio Modality = "audio"
)

type Topology struct {
	Mode             string `json:"mode"`
	Nodes            int    `json:"nodes"`
	Accelerators     int    `json:"accelerators"`
	TensorParallel   int    `json:"tensor_parallel"`
	PipelineParallel int    `json:"pipeline_parallel"`
	ExpertParallel   int    `json:"expert_parallel"`
	PrefillReplicas  int    `json:"prefill_replicas,omitempty"`
	DecodeReplicas   int    `json:"decode_replicas,omitempty"`
}

type MediaShape struct {
	ImagesPerRequest int `json:"images_per_request,omitempty"`
	ImageWidth       int `json:"image_width,omitempty"`
	ImageHeight      int `json:"image_height,omitempty"`
	VideoFrames      int `json:"video_frames,omitempty"`
	VideoFPS         int `json:"video_fps,omitempty"`
	AudioSeconds     int `json:"audio_seconds,omitempty"`
	AudioSampleRate  int `json:"audio_sample_rate,omitempty"`
}

type Workload struct {
	Digest       string              `json:"digest"`
	Modality     Modality            `json:"modality"`
	Phase        kernelplanner.Phase `json:"phase"`
	BatchSize    int                 `json:"batch_size"`
	Concurrency  int                 `json:"concurrency"`
	InputTokens  int                 `json:"input_tokens,omitempty"`
	OutputTokens int                 `json:"output_tokens,omitempty"`
	Media        MediaShape          `json:"media,omitzero"`
	QualitySuite string              `json:"quality_suite"`
	Replay       string              `json:"replay"`
}

type Policy struct {
	MaxCandidates          int       `json:"max_candidates"`
	MaxCostUSD             float64   `json:"max_cost_usd"`
	MinHotspotFraction     float64   `json:"min_hotspot_fraction"`
	MinAmdahlCeiling       float64   `json:"min_amdahl_ceiling"`
	TargetEndToEndSpeedup  float64   `json:"target_end_to_end_speedup"`
	MinKernelSpeedup       float64   `json:"min_kernel_speedup"`
	MaxMemoryRegressionPct float64   `json:"max_memory_regression_percent"`
	RequireSanitizer       bool      `json:"require_sanitizer"`
	RequireHiddenShapes    bool      `json:"require_hidden_shapes"`
	RequireServingReplay   bool      `json:"require_serving_replay"`
	RequireQuality         bool      `json:"require_quality"`
	AllowGeneratedKernels  bool      `json:"allow_generated_kernels"`
	AuthorizedUntil        time.Time `json:"authorized_until"`
}

type SourcePin struct {
	ImplementationID string     `json:"implementation_id"`
	Revision         string     `json:"revision"`
	License          string     `json:"license"`
	Artifacts        []Artifact `json:"artifacts,omitempty"`
}

type Request struct {
	SchemaVersion string                         `json:"schema_version"`
	TenantID      string                         `json:"tenant_id"`
	CampaignID    string                         `json:"campaign_id"`
	CandidateID   string                         `json:"candidate_id"`
	Model         kernelplanner.ModelIdentity    `json:"model"`
	Runtime       kernelplanner.RuntimeIdentity  `json:"runtime"`
	Hardware      kernelplanner.HardwareIdentity `json:"hardware"`
	Topology      Topology                       `json:"topology"`
	Workload      Workload                       `json:"workload"`
	Policy        Policy                         `json:"policy"`
	Sources       []SourcePin                    `json:"sources"`
}

type ProfileIntent struct {
	InputDigest string                         `json:"input_digest"`
	Model       kernelplanner.ModelIdentity    `json:"model"`
	Runtime     kernelplanner.RuntimeIdentity  `json:"runtime"`
	Hardware    kernelplanner.HardwareIdentity `json:"hardware"`
	Topology    Topology                       `json:"topology"`
	Workload    Workload                       `json:"workload"`
}

type ProfileEvidence struct {
	InputDigest        string                         `json:"input_digest"`
	Tool               string                         `json:"tool"`
	ToolVersion        string                         `json:"tool_version"`
	Hardware           kernelplanner.HardwareIdentity `json:"hardware"`
	RuntimeImageDigest string                         `json:"runtime_image_digest"`
	Topology           Topology                       `json:"topology"`
	WorkloadDigest     string                         `json:"workload_digest"`
	CapturedAt         time.Time                      `json:"captured_at"`
	Hotspots           []kernelplanner.Hotspot        `json:"hotspots"`
	CostUSD            float64                        `json:"cost_usd"`
	ArtifactDigest     string                         `json:"artifact_digest"`
}

type Artifact struct {
	Kind   string `json:"kind"`
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type BuildRequest struct {
	InputDigest string                         `json:"input_digest"`
	CampaignID  string                         `json:"campaign_id"`
	CandidateID string                         `json:"candidate_id"`
	Experiment  kernelplanner.Candidate        `json:"experiment"`
	Source      SourcePin                      `json:"source"`
	Model       kernelplanner.ModelIdentity    `json:"model"`
	Runtime     kernelplanner.RuntimeIdentity  `json:"runtime"`
	Hardware    kernelplanner.HardwareIdentity `json:"hardware"`
}

type BuildEvidence struct {
	InputDigest    string     `json:"input_digest"`
	Builder        string     `json:"builder"`
	BuilderVersion string     `json:"builder_version"`
	Environment    string     `json:"environment_revision"`
	SourceDigest   string     `json:"source_digest"`
	Status         string     `json:"status"`
	Checks         []string   `json:"checks"`
	Artifacts      []Artifact `json:"artifacts"`
	ReceiptDigest  string     `json:"receipt_digest"`
	CostUSD        float64    `json:"cost_usd"`
}

// GenerationRequest is intentionally profile-bound. A generator never sees
// credentials and cannot select a different model, runtime, workload, or
// target after the request has been authorized.
type GenerationRequest struct {
	InputDigest string                         `json:"input_digest"`
	Candidate   kernelplanner.Candidate        `json:"candidate"`
	Profile     ProfileEvidence                `json:"profile"`
	Model       kernelplanner.ModelIdentity    `json:"model"`
	Runtime     kernelplanner.RuntimeIdentity  `json:"runtime"`
	Hardware    kernelplanner.HardwareIdentity `json:"hardware"`
	Workload    Workload                       `json:"workload"`
}

// GenerationEvidence points at immutable generated source. Generated code is
// still untrusted input: it must pass the same isolated build and exact-target
// qualification gates as reviewed source.
type GenerationEvidence struct {
	InputDigest           string    `json:"input_digest"`
	CandidateID           string    `json:"candidate_id"`
	Generator             string    `json:"generator"`
	GeneratorVersion      string    `json:"generator_version"`
	ProfileArtifactDigest string    `json:"profile_artifact_digest"`
	Source                SourcePin `json:"source"`
	GeneratedAt           time.Time `json:"generated_at"`
	CostUSD               float64   `json:"cost_usd"`
}

type QualificationRequest struct {
	InputDigest string                         `json:"input_digest"`
	Experiment  kernelplanner.Candidate        `json:"experiment"`
	Build       BuildEvidence                  `json:"build"`
	Model       kernelplanner.ModelIdentity    `json:"model"`
	Runtime     kernelplanner.RuntimeIdentity  `json:"runtime"`
	Hardware    kernelplanner.HardwareIdentity `json:"hardware"`
	Topology    Topology                       `json:"topology"`
	Workload    Workload                       `json:"workload"`
	Policy      Policy                         `json:"policy"`
}

type QualificationEvidence struct {
	InputDigest            string                         `json:"input_digest"`
	Hardware               kernelplanner.HardwareIdentity `json:"hardware"`
	RuntimeImageDigest     string                         `json:"runtime_image_digest"`
	Topology               Topology                       `json:"topology"`
	WorkloadDigest         string                         `json:"workload_digest"`
	Modality               Modality                       `json:"modality"`
	Media                  MediaShape                     `json:"media,omitzero"`
	Replay                 string                         `json:"replay"`
	QualitySuite           string                         `json:"quality_suite"`
	BuildArtifactDigest    string                         `json:"build_artifact_digest"`
	CorrectnessPassed      bool                           `json:"correctness_passed"`
	HiddenShapesPassed     bool                           `json:"hidden_shapes_passed"`
	SanitizerPassed        bool                           `json:"sanitizer_passed"`
	QualityPassed          bool                           `json:"quality_passed"`
	ServingReplayMeasured  bool                           `json:"serving_replay_measured"`
	KernelSpeedup          float64                        `json:"kernel_speedup"`
	EndToEndSpeedup        float64                        `json:"end_to_end_speedup"`
	MemoryRegressionPct    float64                        `json:"memory_regression_percent"`
	CostRegressionPct      float64                        `json:"cost_regression_percent"`
	ProfilerArtifactDigest string                         `json:"profiler_artifact_digest"`
	BenchmarkEvidenceID    string                         `json:"benchmark_evidence_id"`
	QualityEvidenceID      string                         `json:"quality_evidence_id"`
	CostUSD                float64                        `json:"cost_usd"`
	MeasuredAt             time.Time                      `json:"measured_at"`
}

type ExperimentResult struct {
	Candidate     kernelplanner.Candidate `json:"candidate"`
	Source        SourcePin               `json:"source"`
	Generation    *GenerationEvidence     `json:"generation,omitempty"`
	Build         *BuildEvidence          `json:"build,omitempty"`
	Qualification *QualificationEvidence  `json:"qualification,omitempty"`
	State         string                  `json:"state"`
	FailureCode   string                  `json:"failure_code,omitempty"`
	Reasons       []string                `json:"reasons,omitempty"`
}

type Result struct {
	SchemaVersion string             `json:"schema_version"`
	InputDigest   string             `json:"input_digest"`
	State         string             `json:"state"`
	EvidenceState string             `json:"evidence_state"`
	Profile       ProfileEvidence    `json:"profile"`
	Plan          kernelplanner.Plan `json:"plan"`
	Experiments   []ExperimentResult `json:"experiments"`
	WinnerID      string             `json:"winner_id,omitempty"`
	CostUSD       float64            `json:"cost_usd"`
	Limitations   []string           `json:"limitations"`
}

type Progress struct {
	Stage    string  `json:"stage"`
	Progress int     `json:"progress"`
	Message  string  `json:"message"`
	CostUSD  float64 `json:"cost_usd"`
}

type Profiler interface {
	Capture(context.Context, ProfileIntent) (ProfileEvidence, error)
}

type Generator interface {
	Generate(context.Context, GenerationRequest) (GenerationEvidence, error)
}

type Builder interface {
	Build(context.Context, BuildRequest) (BuildEvidence, error)
}

type Qualifier interface {
	Qualify(context.Context, QualificationRequest) (QualificationEvidence, error)
}

type Engine struct {
	Profiler     Profiler
	Generator    Generator
	Builder      Builder
	Qualifier    Qualifier
	Capabilities CapabilityCatalog
	Progress     func(context.Context, Progress) error
	Now          func() time.Time
}

func (e Engine) Run(ctx context.Context, request Request) (Result, error) {
	request = normalize(request)
	digest, err := validateAndDigest(request)
	if err != nil {
		return Result{}, err
	}
	if e.Profiler == nil || e.Builder == nil || e.Qualifier == nil {
		return Result{}, errors.New("accelerator lab requires profiler, isolated builder, and exact-target qualifier")
	}
	if err = e.ensureAuthority(request.Policy); err != nil {
		return Result{}, err
	}
	capabilities := e.Capabilities
	if capabilities.Version == "" && len(capabilities.Capabilities) == 0 {
		capabilities = DefaultCapabilityCatalog()
	}
	if err = capabilities.ValidateRequest(request); err != nil {
		return Result{}, fmt.Errorf("accelerator capability check: %w", err)
	}
	result := Result{SchemaVersion: ResultSchemaV1, InputDigest: digest, State: "profiling", EvidenceState: "unmeasured", Limitations: []string{"A qualified experiment is still a release candidate; production traffic requires the normal InferCrane Release Guard and explicit promotion."}}
	if err = e.report(ctx, Progress{Stage: "profile", Progress: 5, Message: "Capturing the baseline on the target accelerator."}); err != nil {
		return result, err
	}
	profile, err := e.Profiler.Capture(ctx, ProfileIntent{InputDigest: digest, Model: request.Model, Runtime: request.Runtime, Hardware: request.Hardware, Topology: request.Topology, Workload: request.Workload})
	if err != nil {
		return result, fmt.Errorf("capture target profile: %w", err)
	}
	if err = validateProfile(request, digest, profile); err != nil {
		return result, fmt.Errorf("reject profiler evidence: %w", err)
	}
	result.Profile, result.CostUSD = profile, profile.CostUSD
	if err = withinBudget(request.Policy.MaxCostUSD, result.CostUSD); err != nil {
		return result, err
	}
	if err = e.report(ctx, Progress{Stage: "search", Progress: 18, Message: "Searching reviewed implementations before generating code.", CostUSD: result.CostUSD}); err != nil {
		return result, err
	}
	plan, err := kernelplanner.Build(kernelplanner.Request{
		SchemaVersion: kernelplanner.RequestSchemaV1,
		Model:         request.Model, Runtime: request.Runtime, Hardware: request.Hardware,
		Workload: kernelplanner.Workload{Digest: request.Workload.Digest, Phase: request.Workload.Phase, BatchSize: request.Workload.BatchSize, Concurrency: request.Workload.Concurrency, InputTokens: max(request.Workload.InputTokens, 1), OutputTokens: max(request.Workload.OutputTokens, 1)},
		Profile:  kernelplanner.Profile{Tool: profile.Tool, ToolVersion: profile.ToolVersion, EvidenceClass: "measured", CapturedAt: profile.CapturedAt.UTC().Format(time.RFC3339Nano), Hotspots: profile.Hotspots},
		Policy:   kernelplanner.Policy{MinHotspotFraction: request.Policy.MinHotspotFraction, MinMaxEndToEndSpeedup: request.Policy.MinAmdahlCeiling, TargetEndToEndSpeedup: request.Policy.TargetEndToEndSpeedup, MaxCandidates: request.Policy.MaxCandidates, RequireMeasuredProfiler: true},
	})
	if err != nil {
		return result, fmt.Errorf("plan profiler-backed kernel search: %w", err)
	}
	result.Plan = plan
	pins := make(map[string]SourcePin, len(request.Sources))
	for _, source := range request.Sources {
		pins[source.ImplementationID] = source
	}
	for index, candidate := range plan.Candidates {
		experiment := ExperimentResult{Candidate: candidate, State: "blocked"}
		source, found := selectSource(candidate, pins)
		if !found {
			if !request.Policy.AllowGeneratedKernels || e.Generator == nil {
				experiment.FailureCode = "immutable_source_unavailable"
				experiment.Reasons = []string{"No approved immutable source revision was supplied and autonomous generation is unavailable."}
				result.Experiments = append(result.Experiments, experiment)
				continue
			}
			if err = e.ensureAuthority(request.Policy); err != nil {
				return result, err
			}
			if err = e.report(ctx, Progress{Stage: "search", Progress: 20 + index*5/max(len(plan.Candidates), 1), Message: "Generating one profile-bound kernel candidate in isolation.", CostUSD: result.CostUSD}); err != nil {
				return result, err
			}
			generated, generationErr := e.Generator.Generate(ctx, GenerationRequest{InputDigest: digest, Candidate: candidate, Profile: profile, Model: request.Model, Runtime: request.Runtime, Hardware: request.Hardware, Workload: request.Workload})
			if generationErr != nil {
				experiment.State, experiment.FailureCode = "inconclusive", "kernel_generation_failed"
				experiment.Reasons = []string{generationErr.Error()}
				result.Experiments = append(result.Experiments, experiment)
				continue
			}
			if generationErr = validateGeneration(digest, candidate, profile, generated); generationErr != nil {
				experiment.State, experiment.FailureCode = "rejected", "generation_evidence_invalid"
				experiment.Reasons = []string{generationErr.Error()}
				result.Experiments = append(result.Experiments, experiment)
				continue
			}
			experiment.Generation = &generated
			result.CostUSD += generated.CostUSD
			if err = withinBudget(request.Policy.MaxCostUSD, result.CostUSD); err != nil {
				return result, err
			}
			source, found = generated.Source, true
		}
		experiment.Source = source
		if err = e.ensureAuthority(request.Policy); err != nil {
			return result, err
		}
		progress := 25 + index*30/max(len(plan.Candidates), 1)
		if err = e.report(ctx, Progress{Stage: "build", Progress: progress, Message: "Building candidate " + fmt.Sprint(index+1) + " of " + fmt.Sprint(len(plan.Candidates)) + " in Brezel.", CostUSD: result.CostUSD}); err != nil {
			return result, err
		}
		built, buildErr := e.Builder.Build(ctx, BuildRequest{InputDigest: digest, CampaignID: request.CampaignID, CandidateID: request.CandidateID, Experiment: candidate, Source: source, Model: request.Model, Runtime: request.Runtime, Hardware: request.Hardware})
		if buildErr != nil {
			experiment.State, experiment.FailureCode = "rejected", "isolated_build_failed"
			experiment.Reasons = []string{buildErr.Error()}
			result.Experiments = append(result.Experiments, experiment)
			continue
		}
		if buildErr = validateBuild(digest, built); buildErr != nil {
			experiment.State, experiment.FailureCode = "rejected", "build_evidence_invalid"
			experiment.Reasons = []string{buildErr.Error()}
			result.Experiments = append(result.Experiments, experiment)
			continue
		}
		experiment.Build = &built
		result.CostUSD += built.CostUSD
		if err = withinBudget(request.Policy.MaxCostUSD, result.CostUSD); err != nil {
			return result, err
		}
		if err = e.report(ctx, Progress{Stage: "measure", Progress: progress + 12, Message: "Checking correctness and performance on " + request.Hardware.Accelerator + ".", CostUSD: result.CostUSD}); err != nil {
			return result, err
		}
		if err = e.ensureAuthority(request.Policy); err != nil {
			return result, err
		}
		qualified, qualifyErr := e.Qualifier.Qualify(ctx, QualificationRequest{InputDigest: digest, Experiment: candidate, Build: built, Model: request.Model, Runtime: request.Runtime, Hardware: request.Hardware, Topology: request.Topology, Workload: request.Workload, Policy: request.Policy})
		if qualifyErr != nil {
			experiment.State, experiment.FailureCode = "inconclusive", "target_qualification_failed"
			experiment.Reasons = []string{qualifyErr.Error()}
			result.Experiments = append(result.Experiments, experiment)
			continue
		}
		experiment.Qualification = &qualified
		result.CostUSD += qualified.CostUSD
		if err = withinBudget(request.Policy.MaxCostUSD, result.CostUSD); err != nil {
			return result, err
		}
		experiment.Reasons = qualificationReasons(request, built, qualified)
		if len(experiment.Reasons) > 0 {
			experiment.State, experiment.FailureCode = "rejected", "qualification_gate_failed"
		} else {
			experiment.State = "qualified"
		}
		result.Experiments = append(result.Experiments, experiment)
	}
	qualified := make([]ExperimentResult, 0, len(result.Experiments))
	for _, experiment := range result.Experiments {
		if experiment.State == "qualified" {
			qualified = append(qualified, experiment)
		}
	}
	if len(qualified) == 0 {
		result.State, result.EvidenceState = "inconclusive", "measured"
		_ = e.report(ctx, Progress{Stage: "prove", Progress: 100, Message: "No candidate passed every qualification gate.", CostUSD: result.CostUSD})
		return result, nil
	}
	sort.Slice(qualified, func(i, j int) bool {
		left, right := qualified[i].Qualification, qualified[j].Qualification
		if left.EndToEndSpeedup != right.EndToEndSpeedup {
			return left.EndToEndSpeedup > right.EndToEndSpeedup
		}
		if left.CostRegressionPct != right.CostRegressionPct {
			return left.CostRegressionPct < right.CostRegressionPct
		}
		return qualified[i].Candidate.ID < qualified[j].Candidate.ID
	})
	result.State, result.EvidenceState, result.WinnerID = "qualified", "qualified", qualified[0].Candidate.ID
	if err = e.report(ctx, Progress{Stage: "prove", Progress: 100, Message: "A measured candidate is ready for Release Guard review.", CostUSD: result.CostUSD}); err != nil {
		return result, err
	}
	return result, nil
}

func normalize(request Request) Request {
	request.SchemaVersion = strings.TrimSpace(request.SchemaVersion)
	request.TenantID = strings.TrimSpace(request.TenantID)
	request.CampaignID = strings.TrimSpace(request.CampaignID)
	request.CandidateID = strings.TrimSpace(request.CandidateID)
	request.Hardware.Vendor = strings.ToLower(strings.TrimSpace(request.Hardware.Vendor))
	request.Hardware.Accelerator = strings.ToUpper(strings.TrimSpace(request.Hardware.Accelerator))
	request.Topology.Mode = strings.ToLower(strings.TrimSpace(request.Topology.Mode))
	request.Workload.Modality = Modality(strings.ToLower(strings.TrimSpace(string(request.Workload.Modality))))
	request.Workload.QualitySuite = strings.TrimSpace(request.Workload.QualitySuite)
	request.Workload.Replay = strings.TrimSpace(request.Workload.Replay)
	if request.Policy.MaxCandidates == 0 {
		request.Policy.MaxCandidates = 3
	}
	if request.Policy.MinHotspotFraction == 0 {
		request.Policy.MinHotspotFraction = .05
	}
	if request.Policy.MinAmdahlCeiling == 0 {
		request.Policy.MinAmdahlCeiling = 1.05
	}
	if request.Policy.TargetEndToEndSpeedup == 0 {
		request.Policy.TargetEndToEndSpeedup = 1.02
	}
	if request.Policy.MinKernelSpeedup == 0 {
		request.Policy.MinKernelSpeedup = 1.01
	}
	if request.Topology.Mode == "" {
		request.Topology.Mode = "aggregated"
	}
	if request.Topology.Nodes == 0 {
		request.Topology.Nodes = 1
	}
	if request.Topology.Accelerators == 0 {
		request.Topology.Accelerators = 1
	}
	if request.Topology.TensorParallel == 0 {
		request.Topology.TensorParallel = 1
	}
	if request.Topology.PipelineParallel == 0 {
		request.Topology.PipelineParallel = 1
	}
	if request.Topology.ExpertParallel == 0 {
		request.Topology.ExpertParallel = 1
	}
	return request
}

func validateAndDigest(request Request) (string, error) {
	if request.SchemaVersion != RequestSchemaV1 || request.TenantID == "" || request.CampaignID == "" || request.CandidateID == "" {
		return "", errors.New("schema, tenant, campaign, and candidate identities are required")
	}
	if request.Model.Repository == "" || !hexRevision(request.Model.Revision) || request.Runtime.Name == "" || request.Runtime.Version == "" || !digest(request.Runtime.ImageDigest) {
		return "", errors.New("immutable model and runtime identities are required")
	}
	if request.Hardware.Vendor == "" || request.Hardware.Accelerator == "" {
		return "", errors.New("exact hardware vendor and accelerator are required")
	}
	if request.Topology.Nodes < 1 || request.Topology.Accelerators < 1 || request.Topology.TensorParallel < 1 || request.Topology.PipelineParallel < 1 || request.Topology.ExpertParallel < 1 {
		return "", errors.New("topology counts must be positive")
	}
	if request.Topology.Mode != "aggregated" && request.Topology.Mode != "disaggregated" {
		return "", errors.New("topology mode must be aggregated or disaggregated")
	}
	if request.Topology.Mode == "disaggregated" && (request.Topology.PrefillReplicas < 1 || request.Topology.DecodeReplicas < 1) {
		return "", errors.New("disaggregated topology requires prefill and decode replicas")
	}
	if !digest(request.Workload.Digest) || request.Workload.BatchSize < 1 || request.Workload.Concurrency < 1 || request.Workload.QualitySuite == "" || request.Workload.Replay == "" {
		return "", errors.New("workload digest, positive load shape, quality suite, and replay identity are required")
	}
	if err := validateMedia(request.Workload); err != nil {
		return "", err
	}
	if request.Policy.MaxCandidates < 1 || request.Policy.MaxCandidates > 20 || request.Policy.MaxCostUSD <= 0 || math.IsNaN(request.Policy.MaxCostUSD) || math.IsInf(request.Policy.MaxCostUSD, 0) || request.Policy.MinHotspotFraction <= 0 || request.Policy.MinHotspotFraction >= 1 || request.Policy.MinAmdahlCeiling <= 1 || request.Policy.TargetEndToEndSpeedup <= 1 || request.Policy.MinKernelSpeedup <= 1 || request.Policy.MaxMemoryRegressionPct < 0 || request.Policy.AuthorizedUntil.IsZero() {
		return "", errors.New("bounded candidate, cost, speed, hotspot, and memory policies are required")
	}
	seen := map[string]struct{}{}
	for _, source := range request.Sources {
		if source.ImplementationID == "" || !hexRevision(source.Revision) || source.License == "" {
			return "", errors.New("every source requires implementation, immutable revision, and license")
		}
		if _, ok := seen[source.ImplementationID]; ok {
			return "", errors.New("duplicate source implementation")
		}
		seen[source.ImplementationID] = struct{}{}
		for _, artifact := range source.Artifacts {
			if !validArtifact(artifact) {
				return "", errors.New("source artifacts require HTTPS or S3 identity, sha256 digest, and positive size")
			}
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func validateMedia(workload Workload) error {
	switch workload.Modality {
	case ModalityText:
		if workload.InputTokens < 1 || workload.OutputTokens < 1 {
			return errors.New("text workload requires positive input and output tokens")
		}
	case ModalityImage:
		if workload.Media.ImagesPerRequest < 1 || workload.Media.ImageWidth < 1 || workload.Media.ImageHeight < 1 || workload.OutputTokens < 1 {
			return errors.New("image workload requires count, dimensions, and output tokens")
		}
	case ModalityVideo:
		if workload.Media.VideoFrames < 1 || workload.Media.VideoFPS < 1 || workload.Media.ImageWidth < 1 || workload.Media.ImageHeight < 1 || workload.OutputTokens < 1 {
			return errors.New("video workload requires frames, fps, dimensions, and output tokens")
		}
	case ModalityAudio:
		if workload.Media.AudioSeconds < 1 || workload.Media.AudioSampleRate < 1 {
			return errors.New("audio workload requires duration and sample rate")
		}
	default:
		return errors.New("workload modality must be text, image, video, or audio")
	}
	return nil
}

func validateProfile(request Request, inputDigest string, profile ProfileEvidence) error {
	if profile.InputDigest != inputDigest || profile.Tool == "" || profile.ToolVersion == "" || profile.CapturedAt.IsZero() || len(profile.Hotspots) == 0 || profile.WorkloadDigest != request.Workload.Digest || profile.Hardware != request.Hardware || profile.RuntimeImageDigest != request.Runtime.ImageDigest || profile.Topology != request.Topology || !digest(profile.ArtifactDigest) || invalidCost(profile.CostUSD) {
		return errors.New("profile must be measured, immutable, and bound to the exact request, workload, and hardware")
	}
	return nil
}

func validateBuild(inputDigest string, build BuildEvidence) error {
	if build.InputDigest != inputDigest || build.Status != "passed" || build.Builder == "" || build.BuilderVersion == "" || build.Environment == "" || !digest(build.SourceDigest) || !digest(build.ReceiptDigest) || len(build.Checks) == 0 || len(build.Artifacts) == 0 || invalidCost(build.CostUSD) {
		return errors.New("build evidence is incomplete or not bound to the request")
	}
	for _, artifact := range build.Artifacts {
		if !validArtifact(artifact) {
			return errors.New("build artifact is not immutable and retrievable")
		}
	}
	return nil
}

func validateGeneration(inputDigest string, candidate kernelplanner.Candidate, profile ProfileEvidence, generated GenerationEvidence) error {
	if generated.InputDigest != inputDigest || generated.CandidateID != candidate.ID || generated.Generator == "" || generated.GeneratorVersion == "" || generated.GeneratedAt.IsZero() || generated.ProfileArtifactDigest != profile.ArtifactDigest || invalidCost(generated.CostUSD) {
		return errors.New("generated source is not bound to the request, candidate, and measured profile")
	}
	if generated.Source.ImplementationID == "" || !hexRevision(generated.Source.Revision) || generated.Source.License == "" || len(generated.Source.Artifacts) == 0 {
		return errors.New("generated source requires immutable identity, provenance, license, and artifacts")
	}
	for _, artifact := range generated.Source.Artifacts {
		if !validArtifact(artifact) {
			return errors.New("generated source artifact is not immutable and retrievable")
		}
	}
	return nil
}

func qualificationReasons(request Request, build BuildEvidence, evidence QualificationEvidence) []string {
	var reasons []string
	if evidence.InputDigest != build.InputDigest || evidence.Hardware != request.Hardware || evidence.RuntimeImageDigest != request.Runtime.ImageDigest || evidence.Topology != request.Topology || evidence.WorkloadDigest != request.Workload.Digest || evidence.Modality != request.Workload.Modality || evidence.Media != request.Workload.Media || evidence.Replay != request.Workload.Replay || evidence.QualitySuite != request.Workload.QualitySuite || !containsArtifact(build.Artifacts, evidence.BuildArtifactDigest) || !digest(evidence.ProfilerArtifactDigest) || evidence.MeasuredAt.IsZero() || invalidCost(evidence.CostUSD) {
		return []string{"qualification evidence identity does not match the build, runtime, workload, and target hardware"}
	}
	if !evidence.CorrectnessPassed {
		reasons = append(reasons, "reference correctness failed")
	}
	if request.Policy.RequireHiddenShapes && !evidence.HiddenShapesPassed {
		reasons = append(reasons, "hidden and adversarial shapes failed")
	}
	if request.Policy.RequireSanitizer && !evidence.SanitizerPassed {
		reasons = append(reasons, "memory and race sanitizer failed")
	}
	if evidence.KernelSpeedup < request.Policy.MinKernelSpeedup {
		reasons = append(reasons, "kernel did not beat the target-native baseline")
	}
	if request.Policy.RequireServingReplay && !evidence.ServingReplayMeasured {
		reasons = append(reasons, "end-to-end serving replay is missing")
	}
	if evidence.EndToEndSpeedup < request.Policy.TargetEndToEndSpeedup {
		reasons = append(reasons, "end-to-end speedup target was not reached")
	}
	if evidence.MemoryRegressionPct > request.Policy.MaxMemoryRegressionPct {
		reasons = append(reasons, "memory regression exceeds policy")
	}
	if request.Policy.RequireQuality && (!evidence.QualityPassed || evidence.QualityEvidenceID == "") {
		reasons = append(reasons, "semantic quality evidence is missing or failed")
	}
	if evidence.BenchmarkEvidenceID == "" {
		reasons = append(reasons, "serving benchmark evidence is missing")
	}
	return reasons
}

func selectSource(candidate kernelplanner.Candidate, pins map[string]SourcePin) (SourcePin, bool) {
	for _, implementation := range candidate.ExistingImplementations {
		if implementation.Kind == "runtime-bundled" {
			continue
		}
		if source, found := pins[implementation.ImplementationID]; found {
			return source, true
		}
	}
	for _, compiler := range candidate.CompilerChoices {
		if source, found := pins[compiler]; found {
			return source, true
		}
	}
	return SourcePin{}, false
}

func containsArtifact(artifacts []Artifact, wanted string) bool {
	for _, artifact := range artifacts {
		if strings.EqualFold(artifact.SHA256, wanted) {
			return true
		}
	}
	return false
}

func validArtifact(artifact Artifact) bool {
	return artifact.Kind != "" && (strings.HasPrefix(artifact.URI, "https://") || strings.HasPrefix(artifact.URI, "s3://") || strings.HasPrefix(artifact.URI, "gs://")) && digest(artifact.SHA256) && artifact.Size > 0
}

func withinBudget(limit, used float64) error {
	if invalidCost(used) || used > limit {
		return fmt.Errorf("accelerator lab cost $%.4f exceeds authorized maximum $%.4f", used, limit)
	}
	return nil
}

func invalidCost(value float64) bool { return value < 0 || math.IsNaN(value) || math.IsInf(value, 0) }
func digest(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}
func hexRevision(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
func (e Engine) report(ctx context.Context, progress Progress) error {
	if e.Progress == nil {
		return nil
	}
	return e.Progress(ctx, progress)
}

func (e Engine) ensureAuthority(policy Policy) error {
	now := time.Now().UTC()
	if e.Now != nil {
		now = e.Now().UTC()
	}
	if !policy.AuthorizedUntil.After(now) {
		return ErrExecutionAuthorityExpired
	}
	return nil
}

// ValidateRequest applies defaults and returns the immutable request digest
// without invoking a provider, builder, or accelerator worker.
func ValidateRequest(request Request) (Request, string, error) {
	request = normalize(request)
	digest, err := validateAndDigest(request)
	return request, digest, err
}
