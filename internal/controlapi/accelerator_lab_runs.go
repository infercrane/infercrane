package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kernelplanner"
	"github.com/infercrane/infercrane/internal/kernelresearch"
)

const (
	acceleratorLabResourceType    = "optimization_campaign"
	acceleratorLabRunBoundary     = "Lab runs are experiments on the target GPU. A qualified kernel still needs Release Guard and explicit promotion before it serves traffic."
	defaultAcceleratorLabRunLimit = 20
	maxAcceleratorLabRunLimit     = 100
	maxAcceleratorLabCampaignID   = 256
)

// acceleratorLabRunStore lists one tenant's operations of one kind on one
// resource type, newest first; an empty resource name lists them all.
type acceleratorLabRunStore interface {
	OperationsOfKind(ctx context.Context, tenant, kind, resourceType, resourceName string, limit int) ([]domain.Operation, error)
}

func (a API) acceleratorLabRuns(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(acceleratorLabRunStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "Accelerator Lab run history is not configured")
		return
	}
	query := r.URL.Query()
	limit := defaultAcceleratorLabRunLimit
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxAcceleratorLabRunLimit {
			writeError(w, http.StatusBadRequest, "invalid_query", "limit must be between 1 and 100")
			return
		}
		limit = parsed
	}
	campaignID := query.Get("campaign_id")
	if len(campaignID) > maxAcceleratorLabCampaignID {
		writeError(w, http.StatusBadRequest, "invalid_query", "campaign_id must be at most 256 bytes")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	operations, err := store.OperationsOfKind(r.Context(), actor.TenantID, acceleratorlab.ExecuteKind, acceleratorLabResourceType, campaignID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "Accelerator Lab runs could not be listed")
		return
	}
	runs := make([]acceleratorLabRun, 0, len(operations))
	for _, operation := range operations {
		// The store already filters; this keeps a store defect from
		// disclosing another tenant's run or an unrelated operation.
		if operation.TenantID != actor.TenantID || operation.Kind != acceleratorlab.ExecuteKind || (campaignID != "" && operation.ResourceName != campaignID) {
			continue
		}
		runs = append(runs, projectAcceleratorLabRun(operation))
		if len(runs) == limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": runs, "boundary": acceleratorLabRunBoundary})
}

// The projection copies stored values. It never fills in a missing
// measurement, and it leaves out artifact URIs and sizes, literature,
// credentials, tenant IDs, lease owners, and idempotency keys.

const (
	acceleratorLabResultPending    = "pending"
	acceleratorLabResultRecorded   = "recorded"
	acceleratorLabResultUnreadable = "unreadable"
)

type acceleratorLabRun struct {
	ID          string                   `json:"id"`
	CampaignID  string                   `json:"campaign_id"`
	CandidateID string                   `json:"candidate_id"`
	Status      string                   `json:"status"`
	Progress    int                      `json:"progress"`
	Message     string                   `json:"message"`
	ErrorCode   *string                  `json:"error_code"`
	CreatedAt   time.Time                `json:"created_at"`
	UpdatedAt   time.Time                `json:"updated_at"`
	CompletedAt *time.Time               `json:"completed_at"`
	Target      *acceleratorLabRunTarget `json:"target"`
	Policy      *acceleratorLabRunPolicy `json:"policy"`
	ResultState string                   `json:"result_state"`
	Result      *acceleratorLabRunResult `json:"result"`
}

type acceleratorLabRunTarget struct {
	Model    kernelplanner.ModelIdentity   `json:"model"`
	Runtime  kernelplanner.RuntimeIdentity `json:"runtime"`
	Hardware acceleratorLabRunHardware     `json:"hardware"`
	Topology acceleratorlab.Topology       `json:"topology"`
	Workload acceleratorLabRunWorkload     `json:"workload"`
}

type acceleratorLabRunHardware struct {
	Vendor            string `json:"vendor"`
	Accelerator       string `json:"accelerator"`
	ComputeCapability string `json:"compute_capability"`
}

type acceleratorLabRunWorkload struct {
	Digest       string                  `json:"digest"`
	Modality     acceleratorlab.Modality `json:"modality"`
	Phase        kernelplanner.Phase     `json:"phase"`
	BatchSize    int                     `json:"batch_size"`
	Concurrency  int                     `json:"concurrency"`
	InputTokens  int                     `json:"input_tokens"`
	OutputTokens int                     `json:"output_tokens"`
}

type acceleratorLabRunPolicy struct {
	MaxCostUSD             float64 `json:"max_cost_usd"`
	MinHotspotFraction     float64 `json:"min_hotspot_fraction"`
	MinAmdahlCeiling       float64 `json:"min_amdahl_ceiling"`
	TargetEndToEndSpeedup  float64 `json:"target_end_to_end_speedup"`
	MinKernelSpeedup       float64 `json:"min_kernel_speedup"`
	MaxMemoryRegressionPct float64 `json:"max_memory_regression_percent"`
	RequireHiddenShapes    bool    `json:"require_hidden_shapes"`
	RequireSanitizer       bool    `json:"require_sanitizer"`
	RequireServingReplay   bool    `json:"require_serving_replay"`
	RequireQuality         bool    `json:"require_quality"`
	AllowGeneratedKernels  bool    `json:"allow_generated_kernels"`
}

type acceleratorLabRunResult struct {
	SchemaVersion string                        `json:"schema_version"`
	InputDigest   string                        `json:"input_digest"`
	State         string                        `json:"state"`
	EvidenceState string                        `json:"evidence_state"`
	WinnerID      *string                       `json:"winner_id"`
	CostUSD       float64                       `json:"cost_usd"`
	Profile       acceleratorLabRunProfile      `json:"profile"`
	Plan          acceleratorLabRunPlan         `json:"plan"`
	Experiments   []acceleratorLabRunExperiment `json:"experiments"`
	Limitations   []string                      `json:"limitations"`
}

type acceleratorLabRunProfile struct {
	Tool               string                     `json:"tool"`
	ToolVersion        string                     `json:"tool_version"`
	CapturedAt         time.Time                  `json:"captured_at"`
	RuntimeImageDigest string                     `json:"runtime_image_digest"`
	ArtifactDigest     string                     `json:"artifact_digest"`
	Hotspots           []acceleratorLabRunHotspot `json:"hotspots"`
}

// acceleratorLabRunHotspot keeps the profiler's optional measurements null
// when the stored profile did not record them.
type acceleratorLabRunHotspot struct {
	ID                 string                       `json:"id"`
	Name               string                       `json:"name"`
	Family             kernelplanner.OperatorFamily `json:"family"`
	DeviceTimeFraction float64                      `json:"device_time_fraction"`
	MeanDurationUS     *float64                     `json:"mean_duration_us"`
	Invocations        *int                         `json:"invocations"`
	Shapes             []string                     `json:"shapes"`
	DTypes             []string                     `json:"dtypes"`
}

type acceleratorLabRunPlan struct {
	AlgorithmVersion  string                    `json:"algorithm_version"`
	RegistryVersion   string                    `json:"registry_version"`
	SelectionBoundary string                    `json:"selection_boundary"`
	Candidates        []kernelplanner.Candidate `json:"candidates"`
	Rejected          []kernelplanner.Rejection `json:"rejected"`
}

type acceleratorLabRunExperiment struct {
	CandidateID    string                          `json:"candidate_id"`
	OperatorFamily kernelplanner.OperatorFamily    `json:"operator_family"`
	State          string                          `json:"state"`
	FailureCode    *string                         `json:"failure_code"`
	Reasons        []string                        `json:"reasons"`
	Source         acceleratorLabRunSource         `json:"source"`
	Generated      bool                            `json:"generated"`
	Build          *acceleratorLabRunBuild         `json:"build"`
	Qualification  *acceleratorLabRunQualification `json:"qualification"`
	Research       *acceleratorLabRunResearch      `json:"research"`
}

type acceleratorLabRunSource struct {
	ImplementationID string `json:"implementation_id"`
	Revision         string `json:"revision"`
	License          string `json:"license"`
	Backend          string `json:"backend"`
}

type acceleratorLabRunBuild struct {
	Builder         string   `json:"builder"`
	BuilderVersion  string   `json:"builder_version"`
	SourceDigest    string   `json:"source_digest"`
	Status          string   `json:"status"`
	Checks          []string `json:"checks"`
	ReceiptDigest   string   `json:"receipt_digest"`
	ArtifactDigests []string `json:"artifact_digests"`
}

type acceleratorLabRunQualification struct {
	CorrectnessPassed      bool                      `json:"correctness_passed"`
	HiddenShapesPassed     bool                      `json:"hidden_shapes_passed"`
	SanitizerPassed        bool                      `json:"sanitizer_passed"`
	QualityPassed          bool                      `json:"quality_passed"`
	ServingReplayMeasured  bool                      `json:"serving_replay_measured"`
	KernelSpeedup          float64                   `json:"kernel_speedup"`
	EndToEndSpeedup        float64                   `json:"end_to_end_speedup"`
	MemoryRegressionPct    float64                   `json:"memory_regression_percent"`
	CostRegressionPct      float64                   `json:"cost_regression_percent"`
	BuildArtifactDigest    string                    `json:"build_artifact_digest"`
	ProfilerArtifactDigest string                    `json:"profiler_artifact_digest"`
	BenchmarkEvidenceID    string                    `json:"benchmark_evidence_id"`
	QualityEvidenceID      string                    `json:"quality_evidence_id"`
	MeasuredAt             time.Time                 `json:"measured_at"`
	Hardware               acceleratorLabRunHardware `json:"hardware"`
	RuntimeImageDigest     string                    `json:"runtime_image_digest"`
	WorkloadDigest         string                    `json:"workload_digest"`
	CostUSD                float64                   `json:"cost_usd"`
}

type acceleratorLabRunResearch struct {
	State               string                     `json:"state"`
	BestSourceRevision  string                     `json:"best_source_revision"`
	BestKernelSpeedup   float64                    `json:"best_kernel_speedup"`
	BestEndToEndSpeedup float64                    `json:"best_end_to_end_speedup"`
	ReceiptDigest       string                     `json:"receipt_digest"`
	Attempts            []acceleratorLabRunAttempt `json:"attempts"`
}

type acceleratorLabRunAttempt struct {
	Iteration         int                        `json:"iteration"`
	SourceRevision    string                     `json:"source_revision"`
	Backend           string                     `json:"backend"`
	Correctness       kernelresearch.Correctness `json:"correctness"`
	BaselineMedianUS  float64                    `json:"baseline_median_us"`
	CandidateMedianUS float64                    `json:"candidate_median_us"`
	KernelSpeedup     float64                    `json:"kernel_speedup"`
	EndToEndSpeedup   float64                    `json:"end_to_end_speedup"`
	Decision          string                     `json:"decision"`
	Reason            string                     `json:"reason"`
	CompletedAt       time.Time                  `json:"completed_at"`
}

func projectAcceleratorLabRun(operation domain.Operation) acceleratorLabRun {
	run := acceleratorLabRun{
		ID: operation.ID, CampaignID: operation.ResourceName, Status: operation.Status, Progress: operation.Progress,
		Message: redactURIs(operation.Message), ErrorCode: optionalText(operation.ErrorCode),
		CreatedAt: operation.CreatedAt, UpdatedAt: operation.UpdatedAt, CompletedAt: operation.CompletedAt,
	}
	var request acceleratorlab.Request
	if json.Unmarshal([]byte(operation.RequestJSON), &request) == nil && request.SchemaVersion == acceleratorlab.RequestSchemaV1 {
		run.CandidateID = request.CandidateID
		run.Target = &acceleratorLabRunTarget{
			Model: request.Model, Runtime: request.Runtime, Hardware: projectHardware(request.Hardware), Topology: request.Topology,
			Workload: acceleratorLabRunWorkload{
				Digest: request.Workload.Digest, Modality: request.Workload.Modality, Phase: request.Workload.Phase,
				BatchSize: request.Workload.BatchSize, Concurrency: request.Workload.Concurrency,
				InputTokens: request.Workload.InputTokens, OutputTokens: request.Workload.OutputTokens,
			},
		}
		policy := request.Policy
		run.Policy = &acceleratorLabRunPolicy{
			MaxCostUSD: policy.MaxCostUSD, MinHotspotFraction: policy.MinHotspotFraction, MinAmdahlCeiling: policy.MinAmdahlCeiling,
			TargetEndToEndSpeedup: policy.TargetEndToEndSpeedup, MinKernelSpeedup: policy.MinKernelSpeedup, MaxMemoryRegressionPct: policy.MaxMemoryRegressionPct,
			RequireHiddenShapes: policy.RequireHiddenShapes, RequireSanitizer: policy.RequireSanitizer, RequireServingReplay: policy.RequireServingReplay,
			RequireQuality: policy.RequireQuality, AllowGeneratedKernels: policy.AllowGeneratedKernels,
		}
	}
	run.ResultState, run.Result = projectAcceleratorLabResult(operation.ResultJSON)
	return run
}

// projectAcceleratorLabResult reads the worker's stored result. The column
// defaults to an empty object until the worker records one.
func projectAcceleratorLabResult(raw string) (string, *acceleratorLabRunResult) {
	var fields map[string]json.RawMessage
	if strings.TrimSpace(raw) == "" || (json.Unmarshal([]byte(raw), &fields) == nil && len(fields) == 0) {
		return acceleratorLabResultPending, nil
	}
	var stored acceleratorlab.Result
	if json.Unmarshal([]byte(raw), &stored) != nil || stored.SchemaVersion != acceleratorlab.ResultSchemaV1 {
		return acceleratorLabResultUnreadable, nil
	}
	// kernelplanner.Hotspot drops a zero duration or invocation count, so
	// absence is read separately and stays null instead of becoming zero.
	var recorded struct {
		Profile struct {
			Hotspots []struct {
				MeanDurationUS *float64 `json:"mean_duration_us"`
				Invocations    *int     `json:"invocations"`
			} `json:"hotspots"`
		} `json:"profile"`
	}
	if json.Unmarshal([]byte(raw), &recorded) != nil || len(recorded.Profile.Hotspots) != len(stored.Profile.Hotspots) {
		return acceleratorLabResultUnreadable, nil
	}
	result := &acceleratorLabRunResult{
		SchemaVersion: stored.SchemaVersion, InputDigest: stored.InputDigest, State: stored.State, EvidenceState: stored.EvidenceState,
		WinnerID: optionalText(stored.WinnerID), CostUSD: stored.CostUSD,
		Profile: acceleratorLabRunProfile{
			Tool: stored.Profile.Tool, ToolVersion: stored.Profile.ToolVersion, CapturedAt: stored.Profile.CapturedAt,
			RuntimeImageDigest: stored.Profile.RuntimeImageDigest, ArtifactDigest: stored.Profile.ArtifactDigest,
			Hotspots: make([]acceleratorLabRunHotspot, 0, len(stored.Profile.Hotspots)),
		},
		Plan: acceleratorLabRunPlan{
			AlgorithmVersion: stored.Plan.AlgorithmVersion, RegistryVersion: stored.Plan.RegistryVersion, SelectionBoundary: stored.Plan.SelectionBoundary,
			Candidates: make([]kernelplanner.Candidate, 0, len(stored.Plan.Candidates)), Rejected: emptyIfNil(stored.Plan.Rejected),
		},
		Experiments: make([]acceleratorLabRunExperiment, 0, len(stored.Experiments)),
		Limitations: emptyIfNil(stored.Limitations),
	}
	for index, hotspot := range stored.Profile.Hotspots {
		result.Profile.Hotspots = append(result.Profile.Hotspots, acceleratorLabRunHotspot{
			ID: hotspot.ID, Name: hotspot.Name, Family: hotspot.Family, DeviceTimeFraction: hotspot.DeviceTimeFraction,
			MeanDurationUS: recorded.Profile.Hotspots[index].MeanDurationUS, Invocations: recorded.Profile.Hotspots[index].Invocations,
			Shapes: emptyIfNil(hotspot.Shapes), DTypes: emptyIfNil(hotspot.DTypes),
		})
	}
	for _, candidate := range stored.Plan.Candidates {
		result.Plan.Candidates = append(result.Plan.Candidates, candidateWithEmptyLists(candidate))
	}
	for _, experiment := range stored.Experiments {
		result.Experiments = append(result.Experiments, projectAcceleratorLabExperiment(experiment))
	}
	return acceleratorLabResultRecorded, result
}

func projectAcceleratorLabExperiment(experiment acceleratorlab.ExperimentResult) acceleratorLabRunExperiment {
	out := acceleratorLabRunExperiment{
		CandidateID: experiment.Candidate.ID, OperatorFamily: experiment.Candidate.OperatorFamily, State: experiment.State,
		FailureCode: optionalText(experiment.FailureCode), Reasons: redactAllURIs(experiment.Reasons),
		Source:    acceleratorLabRunSource{ImplementationID: experiment.Source.ImplementationID, Revision: experiment.Source.Revision, License: experiment.Source.License, Backend: experiment.Source.Backend},
		Generated: experiment.Generation != nil,
	}
	if build := experiment.Build; build != nil {
		digests := make([]string, 0, len(build.Artifacts))
		for _, artifact := range build.Artifacts {
			digests = append(digests, artifact.SHA256)
		}
		out.Build = &acceleratorLabRunBuild{
			Builder: build.Builder, BuilderVersion: build.BuilderVersion, SourceDigest: build.SourceDigest, Status: build.Status,
			Checks: emptyIfNil(build.Checks), ReceiptDigest: build.ReceiptDigest, ArtifactDigests: digests,
		}
	}
	if evidence := experiment.Qualification; evidence != nil {
		out.Qualification = &acceleratorLabRunQualification{
			CorrectnessPassed: evidence.CorrectnessPassed, HiddenShapesPassed: evidence.HiddenShapesPassed, SanitizerPassed: evidence.SanitizerPassed,
			QualityPassed: evidence.QualityPassed, ServingReplayMeasured: evidence.ServingReplayMeasured,
			KernelSpeedup: evidence.KernelSpeedup, EndToEndSpeedup: evidence.EndToEndSpeedup,
			MemoryRegressionPct: evidence.MemoryRegressionPct, CostRegressionPct: evidence.CostRegressionPct,
			BuildArtifactDigest: evidence.BuildArtifactDigest, ProfilerArtifactDigest: evidence.ProfilerArtifactDigest,
			BenchmarkEvidenceID: evidence.BenchmarkEvidenceID, QualityEvidenceID: evidence.QualityEvidenceID, MeasuredAt: evidence.MeasuredAt,
			Hardware: projectHardware(evidence.Hardware), RuntimeImageDigest: evidence.RuntimeImageDigest, WorkloadDigest: evidence.WorkloadDigest, CostUSD: evidence.CostUSD,
		}
	}
	if experiment.Generation != nil && experiment.Generation.Research != nil {
		receipt := experiment.Generation.Research
		research := &acceleratorLabRunResearch{
			State: receipt.State, BestSourceRevision: receipt.BestSourceRevision, BestKernelSpeedup: receipt.BestKernelSpeedup,
			BestEndToEndSpeedup: receipt.BestEndToEndSpeedup, ReceiptDigest: receipt.ReceiptDigest,
			Attempts: make([]acceleratorLabRunAttempt, 0, len(receipt.Observations)),
		}
		for _, observation := range receipt.Observations {
			research.Attempts = append(research.Attempts, acceleratorLabRunAttempt{
				Iteration: observation.Iteration, SourceRevision: observation.SourceRevision, Backend: observation.Backend, Correctness: observation.Correctness,
				BaselineMedianUS: observation.BaselineMedianUS, CandidateMedianUS: observation.CandidateMedianUS,
				KernelSpeedup: observation.KernelSpeedup, EndToEndSpeedup: observation.EndToEndSpeedup,
				Decision: observation.Decision, Reason: redactURIs(observation.Reason), CompletedAt: observation.CompletedAt,
			})
		}
		out.Research = research
	}
	return out
}

func projectHardware(hardware kernelplanner.HardwareIdentity) acceleratorLabRunHardware {
	return acceleratorLabRunHardware{Vendor: hardware.Vendor, Accelerator: hardware.Accelerator, ComputeCapability: hardware.ComputeCapability}
}

// candidateWithEmptyLists keeps the planner's candidate as recorded and only
// turns absent lists into empty ones.
func candidateWithEmptyLists(candidate kernelplanner.Candidate) kernelplanner.Candidate {
	candidate.CompilerChoices = emptyIfNil(candidate.CompilerChoices)
	candidate.LocalProof = emptyIfNil(candidate.LocalProof)
	candidate.TargetGPUProof = emptyIfNil(candidate.TargetGPUProof)
	candidate.ProductionQualification = emptyIfNil(candidate.ProductionQualification)
	candidate.Limitations = emptyIfNil(candidate.Limitations)
	matches := make([]kernelplanner.KernelMatch, 0, len(candidate.ExistingImplementations))
	for _, match := range candidate.ExistingImplementations {
		match.Reasons = emptyIfNil(match.Reasons)
		matches = append(matches, match)
	}
	candidate.ExistingImplementations = matches
	return candidate
}

// Worker failures can wrap an HTTP client error that names the artifact or
// worker URL. Free text keeps its meaning without the URI.
var uriPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)

func redactURIs(text string) string { return uriPattern.ReplaceAllString(text, "[uri redacted]") }

func redactAllURIs(texts []string) []string {
	out := make([]string, 0, len(texts))
	for _, text := range texts {
		out = append(out, redactURIs(text))
	}
	return out
}

func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func emptyIfNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
