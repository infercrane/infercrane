package optimizer

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
)

// DiagnosisSourceEndpointReadiness marks a diagnosis read from an endpoint's
// optimization-readiness assessment: gateway request stages, the serving
// engine's own metrics, and GPU profiling fields when they were fresh.
const DiagnosisSourceEndpointReadiness = "endpoint_readiness"

// Kernel classes a GPU profile summary attributes sampled kernel time to.
const (
	KernelClassAttention     = "attention"
	KernelClassGEMM          = "gemm"
	KernelClassCommunication = "communication"
	KernelClassOther         = "other"
)

// GPUProfileMaxAge bounds how old a capture may be before a diagnosis stops
// using it. An older capture is left out rather than presented as current.
const GPUProfileMaxAge = 7 * 24 * time.Hour

const (
	maxDiagnosisFindings = 32
	maxDiagnosisSignals  = 64
)

var (
	diagnosisName  = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	diagnosisState = map[string]bool{
		optimizationreadiness.StateCollecting:         true,
		optimizationreadiness.StateProfileReady:       true,
		optimizationreadiness.StateReliabilityBlocked: true,
	}
	diagnosisComparisons = map[string]bool{">=": true, "<=": true, ">": true, "<": true}
	kernelClasses        = []string{KernelClassAttention, KernelClassGEMM, KernelClassCommunication, KernelClassOther}
)

// Diagnosis is the content-free bottleneck evidence a proposal plans from. It
// is part of the proposal input, so the input digest binds the candidate order
// to the exact evidence that chose it. A planner never reads a value that is
// not here: a signal the window did not report is absent, never zero.
type Diagnosis struct {
	Source            string             `json:"source"`
	Endpoint          string             `json:"endpoint"`
	AlgorithmVersion  string             `json:"algorithm_version"`
	WindowStart       time.Time          `json:"window_start"`
	WindowEnd         time.Time          `json:"window_end"`
	State             string             `json:"state"`
	PrimaryBottleneck string             `json:"primary_bottleneck"`
	Confidence        string             `json:"confidence"`
	Findings          []DiagnosisFinding `json:"findings"`
	// Signals holds every signal the assessment reported as available, by
	// name. Unavailable signals are omitted.
	Signals    map[string]float64 `json:"signals"`
	GPUProfile *GPUProfileSummary `json:"gpu_profile,omitempty"`
}

// DiagnosisFinding is one readiness rule that fired, with the observed value
// and the explicit threshold it crossed.
type DiagnosisFinding struct {
	Rule       string  `json:"rule"`
	Signal     string  `json:"signal"`
	Observed   float64 `json:"observed"`
	Comparison string  `json:"comparison"`
	Threshold  float64 `json:"threshold"`
	Unit       string  `json:"unit"`
}

// GPUProfileSummary is where sampled kernel time went in the newest GPU
// profile capture of the endpoint's active revision. Shares are fractions of
// kernel-attributed time; time with no kernel frame is not counted.
type GPUProfileSummary struct {
	CaptureID     string             `json:"capture_id"`
	Tool          string             `json:"tool"`
	Revision      int                `json:"revision"`
	CapturedAt    time.Time          `json:"captured_at"`
	KernelSeconds float64            `json:"kernel_seconds"`
	Shares        map[string]float64 `json:"shares"`
}

// DiagnosisFromAssessment keeps the parts of a readiness assessment a planner
// uses: state, primary bottleneck, fired findings, and available signal values.
func DiagnosisFromAssessment(assessment optimizationreadiness.Assessment, windowStart, windowEnd time.Time) Diagnosis {
	diagnosis := Diagnosis{
		Source: DiagnosisSourceEndpointReadiness, Endpoint: assessment.Endpoint, AlgorithmVersion: assessment.AlgorithmVersion,
		WindowStart: windowStart, WindowEnd: windowEnd, State: assessment.State,
		PrimaryBottleneck: assessment.PrimaryBottleneck, Confidence: assessment.Confidence,
		Findings: []DiagnosisFinding{}, Signals: map[string]float64{},
	}
	for _, finding := range assessment.Findings {
		diagnosis.Findings = append(diagnosis.Findings, DiagnosisFinding{Rule: finding.Rule, Signal: finding.Signal, Observed: finding.Observed, Comparison: finding.Comparison, Threshold: finding.Threshold, Unit: finding.Unit})
	}
	for _, signal := range assessment.Signals {
		if signal.Availability == "available" && signal.Value != nil && finite(*signal.Value) {
			diagnosis.Signals[signal.Name] = *signal.Value
		}
	}
	return normalizeDiagnosis(diagnosis)
}

// SummarizeGPUProfile attributes each sampled stack's GPU time to the deepest
// kernel frame on it and groups kernels by name into attention, GEMM,
// communication, and other. It returns nil when the capture attributes no
// time to a kernel, is older than maxAge at now, or has no identity.
func SummarizeGPUProfile(profile gpuprofile.Profile, now time.Time, maxAge time.Duration) *GPUProfileSummary {
	if profile.Capture.ID == "" || profile.Capture.EndedAt.IsZero() || now.Sub(profile.Capture.EndedAt) > maxAge {
		return nil
	}
	byClass := map[string]int64{}
	var total int64
	for _, stack := range profile.Stacks {
		kernel := ""
		for _, index := range stack.Frames {
			if index >= 0 && index < len(profile.Frames) && profile.Frames[index].Kind == gpuprofile.KindKernel {
				kernel = profile.Frames[index].Name
			}
		}
		if kernel == "" || stack.GPUNS <= 0 {
			continue
		}
		byClass[KernelClass(kernel)] += stack.GPUNS
		total += stack.GPUNS
	}
	if total == 0 {
		return nil
	}
	summary := &GPUProfileSummary{
		CaptureID: profile.Capture.ID, Tool: profile.Capture.Tool, Revision: profile.Revision,
		CapturedAt: profile.Capture.EndedAt, KernelSeconds: round(float64(total)/1e9, 3), Shares: map[string]float64{},
	}
	for _, class := range kernelClasses {
		summary.Shares[class] = round(float64(byClass[class])/float64(total), 4)
	}
	return summary
}

// KernelClass groups a CUDA kernel by its name. Names are matched on stable
// fragments shared by the common attention (FlashAttention, FlashInfer,
// paged and Triton attention), matrix-multiply (cuBLAS, CUTLASS, Marlin,
// MoE), and collective (NCCL, custom all-reduce) kernels.
func KernelClass(name string) string {
	lower := strings.ToLower(name)
	has := func(fragments ...string) bool {
		for _, fragment := range fragments {
			if strings.Contains(lower, fragment) {
				return true
			}
		}
		return false
	}
	switch {
	case has("nccl", "allreduce", "all_reduce", "allgather", "all_gather", "reducescatter", "reduce_scatter", "cross_device_reduce", "custom_ar"):
		return KernelClassCommunication
	case has("attn", "attention", "fmha", "flash_fwd", "flash_bwd", "paged", "batchdecode", "batchprefill", "reshape_and_cache", "merge_attn", "mla_"):
		return KernelClassAttention
	case has("gemm", "gemv", "matmul", "cutlass", "cublas", "xmma", "wgmma", "marlin", "machete", "nvjet", "fused_moe", "splitk"):
		return KernelClassGEMM
	default:
		return KernelClassOther
	}
}

func normalizeDiagnosis(diagnosis Diagnosis) Diagnosis {
	diagnosis.Source = strings.ToLower(strings.TrimSpace(diagnosis.Source))
	diagnosis.Endpoint = strings.TrimSpace(diagnosis.Endpoint)
	diagnosis.WindowStart = diagnosis.WindowStart.UTC().Truncate(time.Second)
	diagnosis.WindowEnd = diagnosis.WindowEnd.UTC().Truncate(time.Second)
	if diagnosis.Findings == nil {
		diagnosis.Findings = []DiagnosisFinding{}
	}
	if diagnosis.Signals == nil {
		diagnosis.Signals = map[string]float64{}
	}
	if diagnosis.GPUProfile != nil {
		profile := *diagnosis.GPUProfile
		profile.CapturedAt = profile.CapturedAt.UTC().Truncate(time.Second)
		diagnosis.GPUProfile = &profile
	}
	return diagnosis
}

func validateDiagnosis(diagnosis Diagnosis) error {
	if diagnosis.Source != DiagnosisSourceEndpointReadiness {
		return fmt.Errorf("diagnosis source must be %s", DiagnosisSourceEndpointReadiness)
	}
	if diagnosis.Endpoint == "" || len(diagnosis.Endpoint) > 255 || diagnosis.AlgorithmVersion == "" || len(diagnosis.AlgorithmVersion) > 128 {
		return errors.New("diagnosis requires an endpoint and the readiness algorithm version")
	}
	if !diagnosisState[diagnosis.State] {
		return errors.New("diagnosis state must be collecting, profile-ready, or reliability-blocked")
	}
	if !diagnosisName.MatchString(diagnosis.PrimaryBottleneck) || len(diagnosis.Confidence) > 64 {
		return errors.New("diagnosis primary bottleneck or confidence is malformed")
	}
	if diagnosis.WindowStart.IsZero() || diagnosis.WindowEnd.Before(diagnosis.WindowStart) {
		return errors.New("diagnosis window must have a start no later than its end")
	}
	if len(diagnosis.Findings) > maxDiagnosisFindings || len(diagnosis.Signals) > maxDiagnosisSignals {
		return fmt.Errorf("diagnosis is limited to %d findings and %d signals", maxDiagnosisFindings, maxDiagnosisSignals)
	}
	for _, finding := range diagnosis.Findings {
		if !diagnosisName.MatchString(finding.Rule) || !diagnosisName.MatchString(finding.Signal) || !diagnosisComparisons[finding.Comparison] || len(finding.Unit) > 32 || !finite(finding.Observed) || !finite(finding.Threshold) {
			return errors.New("diagnosis finding is malformed")
		}
	}
	for name, value := range diagnosis.Signals {
		if !diagnosisName.MatchString(name) || !finite(value) {
			return errors.New("diagnosis signal is malformed")
		}
	}
	if profile := diagnosis.GPUProfile; profile != nil {
		if profile.CaptureID == "" || len(profile.CaptureID) > 128 || len(profile.Tool) > 64 || profile.Revision < 0 || profile.CapturedAt.IsZero() || !finite(profile.KernelSeconds) || profile.KernelSeconds <= 0 || len(profile.Shares) > len(kernelClasses) {
			return errors.New("diagnosis GPU profile summary is malformed")
		}
		for class, share := range profile.Shares {
			if !contains(kernelClasses, class) || !finite(share) || share < 0 || share > 1 {
				return errors.New("diagnosis GPU profile share is malformed")
			}
		}
	}
	return nil
}

func (d Diagnosis) finding(rule string) (DiagnosisFinding, bool) {
	for _, finding := range d.Findings {
		if finding.Rule == rule {
			return finding, true
		}
	}
	return DiagnosisFinding{}, false
}

func (d Diagnosis) signal(name string) (float64, bool) {
	value, ok := d.Signals[name]
	return value, ok
}

func (d Diagnosis) kernelShare(class string) (float64, bool) {
	if d.GPUProfile == nil {
		return 0, false
	}
	value, ok := d.GPUProfile.Shares[class]
	return value, ok
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func round(value float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(value*scale) / scale
}
