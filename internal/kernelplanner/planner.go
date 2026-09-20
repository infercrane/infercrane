// Package kernelplanner turns measured operator hotspots into bounded,
// unmeasured custom-kernel experiments. It never treats model identity as a
// performance proxy: the same operator-level planner works for any pinned
// open-weight model whose target runtime can emit a profile.
package kernelplanner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

const (
	RequestSchemaV1  = "infercrane.kernel-opportunity-request/v1"
	PlanSchemaV1     = "infercrane.kernel-opportunity-plan/v1"
	AlgorithmVersion = "profiled-operator-amdahl-v2"
)

type Phase string

const (
	PhasePrefill Phase = "prefill"
	PhaseDecode  Phase = "decode"
	PhaseMixed   Phase = "mixed"
)

type OperatorFamily string

const (
	ResidualRMSNorm  OperatorFamily = "residual-rmsnorm"
	QuantizedLinear  OperatorFamily = "quantized-linear"
	SwiGLU           OperatorFamily = "swiglu"
	AttentionPrefill OperatorFamily = "attention-prefill"
	AttentionDecode  OperatorFamily = "attention-decode"
	KVCache          OperatorFamily = "kv-cache"
	MoERouting       OperatorFamily = "moe-routing"
	Sampling         OperatorFamily = "sampling"
)

type ModelIdentity struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

type RuntimeIdentity struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	ImageDigest string `json:"image_digest"`
}

type HardwareIdentity struct {
	Vendor            string `json:"vendor"`
	Accelerator       string `json:"accelerator"`
	ComputeCapability string `json:"compute_capability,omitempty"`
}

type Workload struct {
	Digest       string `json:"digest"`
	Phase        Phase  `json:"phase"`
	BatchSize    int    `json:"batch_size"`
	Concurrency  int    `json:"concurrency"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
}

type Hotspot struct {
	ID                 string         `json:"id"`
	Name               string         `json:"name"`
	Family             OperatorFamily `json:"family"`
	DeviceTimeFraction float64        `json:"device_time_fraction"`
	MeanDurationUS     float64        `json:"mean_duration_us,omitempty"`
	Invocations        int            `json:"invocations,omitempty"`
	Shapes             []string       `json:"shapes,omitempty"`
	DTypes             []string       `json:"dtypes,omitempty"`
}

type Profile struct {
	Tool          string    `json:"tool"`
	ToolVersion   string    `json:"tool_version"`
	EvidenceClass string    `json:"evidence_class"`
	CapturedAt    string    `json:"captured_at,omitempty"`
	Hotspots      []Hotspot `json:"hotspots"`
}

type Policy struct {
	MinHotspotFraction      float64 `json:"min_hotspot_fraction,omitempty"`
	MinMaxEndToEndSpeedup   float64 `json:"min_max_end_to_end_speedup,omitempty"`
	TargetEndToEndSpeedup   float64 `json:"target_end_to_end_speedup,omitempty"`
	MaxCandidates           int     `json:"max_candidates,omitempty"`
	RequireMeasuredProfiler bool    `json:"require_measured_profiler,omitempty"`
}

type Request struct {
	SchemaVersion string           `json:"schema_version"`
	Model         ModelIdentity    `json:"model"`
	Runtime       RuntimeIdentity  `json:"runtime"`
	Hardware      HardwareIdentity `json:"hardware"`
	Workload      Workload         `json:"workload"`
	Profile       Profile          `json:"profile"`
	Policy        Policy           `json:"policy"`
}

type Candidate struct {
	ID                      string         `json:"id"`
	Rank                    int            `json:"rank"`
	Status                  string         `json:"status"`
	HotspotID               string         `json:"hotspot_id"`
	OperatorFamily          OperatorFamily `json:"operator_family"`
	Template                string         `json:"template"`
	CompilerChoices         []string       `json:"compiler_choices"`
	ExistingImplementations []KernelMatch  `json:"existing_implementations"`
	SearchBoundary          string         `json:"search_boundary"`
	DeviceTimeFraction      float64        `json:"device_time_fraction"`
	MaxEndToEndSpeedup      float64        `json:"max_end_to_end_speedup"`
	TargetEndToEndSpeedup   float64        `json:"target_end_to_end_speedup"`
	RequiredKernelSpeedup   float64        `json:"required_kernel_speedup"`
	LocalProof              []string       `json:"local_proof"`
	TargetGPUProof          []string       `json:"target_gpu_proof"`
	ProductionQualification []string       `json:"production_qualification"`
	Limitations             []string       `json:"limitations"`
}

type Rejection struct {
	HotspotID string `json:"hotspot_id"`
	Reason    string `json:"reason"`
}

type Plan struct {
	SchemaVersion     string      `json:"schema_version"`
	AlgorithmVersion  string      `json:"algorithm_version"`
	RegistryVersion   string      `json:"registry_version"`
	InputDigest       string      `json:"input_digest"`
	EvidenceClass     string      `json:"evidence_class"`
	Candidates        []Candidate `json:"candidates"`
	Rejected          []Rejection `json:"rejected,omitempty"`
	SelectionBoundary string      `json:"selection_boundary"`
}

type template struct {
	name      string
	compilers []string
}

var templates = map[OperatorFamily]template{
	ResidualRMSNorm:  {"fused-residual-rmsnorm", []string{"triton", "cuda-cute"}},
	QuantizedLinear:  {"fused-dequant-matmul", []string{"cutlass", "cuda-cute", "triton"}},
	SwiGLU:           {"fused-swiglu", []string{"triton", "cuda-cute"}},
	AttentionPrefill: {"flash-attention-prefill", []string{"flashinfer", "triton", "cuda-cute"}},
	AttentionDecode:  {"paged-gqa-decode", []string{"flashinfer", "triton", "cuda-cute"}},
	KVCache:          {"fused-kv-append-quantize", []string{"flashinfer", "triton"}},
	MoERouting:       {"fused-router-topk", []string{"flashinfer", "cutlass", "triton"}},
	Sampling:         {"sorting-free-sampling", []string{"flashinfer", "triton"}},
}

// Build creates experiments, not deployable kernels or performance claims.
// Profiling and qualification remain separate evidence stages.
func Build(request Request) (Plan, error) {
	request = normalize(request)
	if err := validate(request); err != nil {
		return Plan{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Plan{}, err
	}
	digest := sha256.Sum256(body)
	plan := Plan{
		SchemaVersion:     PlanSchemaV1,
		AlgorithmVersion:  AlgorithmVersion,
		RegistryVersion:   RegistryVersion,
		InputDigest:       "sha256:" + hex.EncodeToString(digest[:]),
		EvidenceClass:     request.Profile.EvidenceClass + "-unmeasured",
		SelectionBoundary: "local checks establish semantic parity and memory safety only; target-GPU microbenchmarks plus end-to-end AIPerf, quality, SLO, cost, and Release Guard evidence are required before promotion",
	}
	for _, hotspot := range request.Profile.Hotspots {
		tmpl, supported := templates[hotspot.Family]
		if !supported {
			plan.Rejected = append(plan.Rejected, Rejection{HotspotID: hotspot.ID, Reason: "operator family has no reviewed kernel template"})
			continue
		}
		maxSpeedup := 1 / (1 - hotspot.DeviceTimeFraction)
		if hotspot.DeviceTimeFraction < request.Policy.MinHotspotFraction {
			plan.Rejected = append(plan.Rejected, Rejection{HotspotID: hotspot.ID, Reason: fmt.Sprintf("device-time fraction %.4f is below policy minimum %.4f", hotspot.DeviceTimeFraction, request.Policy.MinHotspotFraction)})
			continue
		}
		if maxSpeedup < request.Policy.MinMaxEndToEndSpeedup {
			plan.Rejected = append(plan.Rejected, Rejection{HotspotID: hotspot.ID, Reason: fmt.Sprintf("Amdahl ceiling %.4fx is below policy minimum %.4fx", maxSpeedup, request.Policy.MinMaxEndToEndSpeedup)})
			continue
		}
		if request.Policy.TargetEndToEndSpeedup >= maxSpeedup {
			plan.Rejected = append(plan.Rejected, Rejection{HotspotID: hotspot.ID, Reason: fmt.Sprintf("target end-to-end speedup %.4fx is not reachable below Amdahl ceiling %.4fx", request.Policy.TargetEndToEndSpeedup, maxSpeedup)})
			continue
		}
		target := request.Policy.TargetEndToEndSpeedup
		required := requiredKernelSpeedup(hotspot.DeviceTimeFraction, target)
		matches, searchErr := DefaultRegistry().Search(SearchRequest{Family: hotspot.Family, Runtime: request.Runtime.Name, HardwareVendor: request.Hardware.Vendor, ComputeCapability: request.Hardware.ComputeCapability, DTypes: hotspot.DTypes, Phase: request.Workload.Phase})
		if searchErr != nil {
			return Plan{}, fmt.Errorf("search kernel registry for hotspot %s: %w", hotspot.ID, searchErr)
		}
		candidate := Candidate{
			Status:                  "proposed-unmeasured",
			HotspotID:               hotspot.ID,
			OperatorFamily:          hotspot.Family,
			Template:                tmpl.name,
			CompilerChoices:         append([]string(nil), tmpl.compilers...),
			ExistingImplementations: matches,
			SearchBoundary:          "inspect and benchmark pinned runtime, vendor, and open-source implementations before adapting or generating custom code",
			DeviceTimeFraction:      hotspot.DeviceTimeFraction,
			MaxEndToEndSpeedup:      maxSpeedup,
			TargetEndToEndSpeedup:   target,
			RequiredKernelSpeedup:   required,
			LocalProof:              []string{"reference parity on randomized and adversarial shapes", "Triton interpreter or CUDA simulator check", "bounds and memory-access sanitizer", "compile-only artifact and source digest"},
			TargetGPUProof:          []string{"exact-SM compile", "randomized correctness against runtime reference", "warmup plus CUDA-event microbenchmark", "Nsight Compute roofline and memory-traffic evidence"},
			ProductionQualification: []string{"same-image runtime integration", "paired AIPerf workload replay", "semantic quality and numerical tolerance", "TTFT, TPOT, goodput, error-rate, and landed-cost gates"},
			Limitations:             []string{"Amdahl values are ceilings derived from the supplied profile, not predicted performance.", "A custom kernel must beat the pinned vendor/runtime kernel on the exact shapes and accelerator.", "Changing model revision, runtime, precision, workload digest, or accelerator invalidates the result."},
		}
		candidate.ID = candidateID(plan.InputDigest, request, hotspot, candidate)
		plan.Candidates = append(plan.Candidates, candidate)
	}
	sort.Slice(plan.Candidates, func(i, j int) bool {
		if plan.Candidates[i].DeviceTimeFraction != plan.Candidates[j].DeviceTimeFraction {
			return plan.Candidates[i].DeviceTimeFraction > plan.Candidates[j].DeviceTimeFraction
		}
		return plan.Candidates[i].ID < plan.Candidates[j].ID
	})
	if len(plan.Candidates) > request.Policy.MaxCandidates {
		for _, candidate := range plan.Candidates[request.Policy.MaxCandidates:] {
			plan.Rejected = append(plan.Rejected, Rejection{HotspotID: candidate.HotspotID, Reason: "candidate budget exceeded"})
		}
		plan.Candidates = plan.Candidates[:request.Policy.MaxCandidates]
	}
	for index := range plan.Candidates {
		plan.Candidates[index].Rank = index + 1
	}
	sort.Slice(plan.Rejected, func(i, j int) bool { return plan.Rejected[i].HotspotID < plan.Rejected[j].HotspotID })
	return plan, nil
}

func normalize(request Request) Request {
	request.SchemaVersion = strings.TrimSpace(request.SchemaVersion)
	request.Model.Repository = strings.TrimSpace(request.Model.Repository)
	request.Model.Revision = strings.ToLower(strings.TrimSpace(request.Model.Revision))
	request.Runtime.Name = strings.ToLower(strings.TrimSpace(request.Runtime.Name))
	request.Runtime.Version = strings.TrimSpace(request.Runtime.Version)
	request.Runtime.ImageDigest = strings.ToLower(strings.TrimSpace(request.Runtime.ImageDigest))
	request.Hardware.Vendor = strings.ToLower(strings.TrimSpace(request.Hardware.Vendor))
	request.Hardware.Accelerator = strings.ToUpper(strings.TrimSpace(request.Hardware.Accelerator))
	request.Hardware.ComputeCapability = strings.ToLower(strings.TrimSpace(request.Hardware.ComputeCapability))
	request.Workload.Digest = strings.ToLower(strings.TrimSpace(request.Workload.Digest))
	request.Profile.Tool = strings.TrimSpace(request.Profile.Tool)
	request.Profile.ToolVersion = strings.TrimSpace(request.Profile.ToolVersion)
	request.Profile.EvidenceClass = strings.ToLower(strings.TrimSpace(request.Profile.EvidenceClass))
	if request.Policy.MinHotspotFraction == 0 {
		request.Policy.MinHotspotFraction = .05
	}
	if request.Policy.MinMaxEndToEndSpeedup == 0 {
		request.Policy.MinMaxEndToEndSpeedup = 1.05
	}
	if request.Policy.TargetEndToEndSpeedup == 0 {
		request.Policy.TargetEndToEndSpeedup = 1.02
	}
	if request.Policy.MaxCandidates == 0 {
		request.Policy.MaxCandidates = 3
	}
	for index := range request.Profile.Hotspots {
		hotspot := &request.Profile.Hotspots[index]
		hotspot.ID = strings.TrimSpace(hotspot.ID)
		hotspot.Name = strings.TrimSpace(hotspot.Name)
		hotspot.Family = OperatorFamily(strings.ToLower(strings.TrimSpace(string(hotspot.Family))))
		sort.Strings(hotspot.Shapes)
		sort.Strings(hotspot.DTypes)
	}
	sort.Slice(request.Profile.Hotspots, func(i, j int) bool { return request.Profile.Hotspots[i].ID < request.Profile.Hotspots[j].ID })
	return request
}

func validate(request Request) error {
	if request.SchemaVersion != RequestSchemaV1 {
		return fmt.Errorf("schema_version must be %q", RequestSchemaV1)
	}
	if request.Model.Repository == "" || !immutableRevision(request.Model.Revision) {
		return errors.New("model repository and immutable 40 to 64 character hex revision are required")
	}
	if request.Runtime.Name == "" || request.Runtime.Version == "" || !digest(request.Runtime.ImageDigest) {
		return errors.New("runtime name, exact version, and sha256 image digest are required")
	}
	if request.Hardware.Vendor == "" || request.Hardware.Accelerator == "" {
		return errors.New("exact hardware vendor and accelerator are required")
	}
	if request.Hardware.Vendor != "nvidia" {
		return errors.New("the initial kernel planner has reviewed compiler backends only for NVIDIA hardware")
	}
	if request.Workload.Phase != PhasePrefill && request.Workload.Phase != PhaseDecode && request.Workload.Phase != PhaseMixed {
		return errors.New("workload phase must be prefill, decode, or mixed")
	}
	if !digest(request.Workload.Digest) || request.Workload.BatchSize < 1 || request.Workload.Concurrency < 1 || request.Workload.InputTokens < 1 || request.Workload.OutputTokens < 1 {
		return errors.New("workload requires a sha256 digest and positive shape values")
	}
	if request.Profile.Tool == "" || request.Profile.ToolVersion == "" || (request.Profile.EvidenceClass != "measured" && request.Profile.EvidenceClass != "fixture") || len(request.Profile.Hotspots) == 0 {
		return errors.New("profile requires exact tool identity, measured or fixture evidence, and hotspots")
	}
	if request.Policy.RequireMeasuredProfiler && request.Profile.EvidenceClass != "measured" {
		return errors.New("policy requires a measured profiler capture")
	}
	if request.Policy.MinHotspotFraction <= 0 || request.Policy.MinHotspotFraction >= 1 || request.Policy.MinMaxEndToEndSpeedup <= 1 || request.Policy.TargetEndToEndSpeedup <= 1 || request.Policy.MaxCandidates < 1 || request.Policy.MaxCandidates > 100 {
		return errors.New("kernel policy thresholds or candidate bound are invalid")
	}
	seen := map[string]struct{}{}
	total := 0.0
	for _, hotspot := range request.Profile.Hotspots {
		if hotspot.ID == "" || hotspot.Name == "" || hotspot.Family == "" || hotspot.DeviceTimeFraction <= 0 || hotspot.DeviceTimeFraction >= 1 || math.IsNaN(hotspot.DeviceTimeFraction) || math.IsInf(hotspot.DeviceTimeFraction, 0) || hotspot.MeanDurationUS < 0 || hotspot.Invocations < 0 {
			return fmt.Errorf("hotspot %q is incomplete or invalid", hotspot.ID)
		}
		if _, duplicate := seen[hotspot.ID]; duplicate {
			return fmt.Errorf("duplicate hotspot %q", hotspot.ID)
		}
		seen[hotspot.ID] = struct{}{}
		total += hotspot.DeviceTimeFraction
	}
	if total > 1.000001 {
		return fmt.Errorf("hotspot device-time fractions sum to %.6f, greater than one", total)
	}
	return nil
}

func requiredKernelSpeedup(fraction, targetEndToEnd float64) float64 {
	denominator := 1/targetEndToEnd - (1 - fraction)
	if denominator <= 0 {
		return math.Inf(1)
	}
	return fraction / denominator
}

func candidateID(inputDigest string, request Request, hotspot Hotspot, candidate Candidate) string {
	body, _ := json.Marshal(struct {
		InputDigest string           `json:"input_digest"`
		Hardware    HardwareIdentity `json:"hardware"`
		Hotspot     Hotspot          `json:"hotspot"`
		Template    string           `json:"template"`
	}{inputDigest, request.Hardware, hotspot, candidate.Template})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func immutableRevision(value string) bool {
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != 71 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}
