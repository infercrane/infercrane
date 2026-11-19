package optimizer

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/gputrace"
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
	// KernelLab is set by the control plane when it can run a custom-kernel
	// lane inside a run: an Accelerator Lab worker is configured and the
	// kernel lane is turned on. A client cannot set it.
	KernelLab bool `json:"kernel_lab,omitempty"`
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
	// Trace is what the analysis of the capture's engine trace measured. It
	// is absent for a call-stack-only (v1) capture and for an analysis
	// algorithm the planner has not reviewed.
	Trace *GPUTraceSummary `json:"trace,omitempty"`
}

// GPUTraceSummary is the part of a trace analysis a plan reads: the analysis
// rules that fired, each with its measured value and threshold, and the
// measured values those rules rest on. A value the trace could not measure is
// absent, never zero.
type GPUTraceSummary struct {
	AlgorithmVersion string             `json:"algorithm_version"`
	Bottleneck       string             `json:"bottleneck"`
	Devices          int                `json:"devices"`
	WindowMS         float64            `json:"window_ms"`
	Findings         []GPUTraceFinding  `json:"findings"`
	Measurements     map[string]float64 `json:"measurements"`
	// Operators is kernel time and efficiency per operator class, when the
	// analysis measured them.
	Operators []TraceOperator `json:"operators,omitempty"`
}

// TraceOperator is one operator class's share of kernel time and, when the
// trace's shapes covered enough of it, its efficiency: achieved throughput
// over what the roofline allows at its intensity. Absent efficiency is
// unknown, never zero.
type TraceOperator struct {
	Operator     string   `json:"operator"`
	Share        float64  `json:"share"`
	CoveredShare float64  `json:"covered_share"`
	Efficiency   *float64 `json:"efficiency,omitempty"`
}

// GPUTraceFinding is one trace analysis rule that fired.
type GPUTraceFinding struct {
	Rule       string  `json:"rule"`
	Observed   float64 `json:"observed"`
	Comparison string  `json:"comparison"`
	Threshold  float64 `json:"threshold"`
	Unit       string  `json:"unit"`
}

// Trace analysis rules the planner reads, from gpu-trace-analysis-v1.
const (
	TraceRuleIdleHostBound        = "gpu.idle_host_bound"
	TraceRuleLaunchBoundKernels   = "gpu.launch_bound_kernels"
	TraceRuleCommunicationExposed = "gpu.communication_exposed"
	TraceRuleMemoryBoundGEMM      = "gpu.memory_bound_gemm"
	TraceRuleComputeBoundGEMM     = "gpu.compute_bound_gemm"
	TraceRuleAttentionHeavy       = "gpu.attention_heavy"
	TraceRuleDecodeDominant       = "gpu.decode_dominant"
)

// Trace measurements a summary carries when the trace measured them. Shares
// are 0..1; durations are in the unit their name ends with.
const (
	TraceIdleShare            = "idle_share"
	TraceIdleHostWaitShare    = "idle_host_wait_share"
	TraceLaunchDelayP50US     = "launch_delay_p50_us"
	TraceShortKernelShare     = "short_kernel_share"
	TraceGraphLaunchShare     = "graph_launch_share"
	TraceOverlappedShare      = "communication_overlapped_share"
	TraceRooflineCoveredShare = "roofline_covered_share"
	TraceDecodeShare          = "decode_share"
	TraceStepP50MS            = "step_p50_ms"
	TraceStepP95MS            = "step_p95_ms"
)

// reviewedTraceAlgorithms are the trace analysis versions whose rules and
// thresholds the planner was written against. Another version is left out.
var reviewedTraceAlgorithms = map[string]bool{gpuprofile.AnalysisAlgorithm: true}

var (
	traceShareMeasurements = map[string]bool{
		TraceIdleShare: true, TraceIdleHostWaitShare: true, TraceShortKernelShare: true, TraceGraphLaunchShare: true,
		TraceOverlappedShare: true, TraceRooflineCoveredShare: true, TraceDecodeShare: true,
	}
	traceDurationMeasurements = map[string]bool{TraceLaunchDelayP50US: true, TraceStepP50MS: true, TraceStepP95MS: true}
	traceBottlenecks          = map[string]bool{
		gpuprofile.BottleneckHostLaunch: true, gpuprofile.BottleneckCommunication: true, gpuprofile.BottleneckMemoryBandwidth: true,
		gpuprofile.BottleneckCompute: true, gpuprofile.BottleneckAttention: true, gpuprofile.BottleneckBusy: true, gpuprofile.BottleneckUnknown: true,
	}
)

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
	summary.Trace = summarizeTrace(profile)
	return summary
}

// summarizeTrace keeps the fired rules and the measured values of a v2
// capture's trace analysis. It returns nil for a v1 capture, an analysis that
// fails validation, or an algorithm the planner has not reviewed.
func summarizeTrace(profile gpuprofile.Profile) *GPUTraceSummary {
	analysis := profile.Analysis
	if profile.SchemaVersion != gpuprofile.SchemaVersionV2 || analysis == nil || !reviewedTraceAlgorithms[analysis.AlgorithmVersion] || gpuprofile.ValidateAnalysis(*analysis) != nil {
		return nil
	}
	trace := &GPUTraceSummary{
		AlgorithmVersion: analysis.AlgorithmVersion, Bottleneck: analysis.Verdict.Bottleneck, Devices: analysis.Trace.Devices,
		WindowMS: round(analysis.Trace.WindowMS, 3), Findings: []GPUTraceFinding{}, Measurements: map[string]float64{},
	}
	seen := map[string]bool{}
	for _, finding := range analysis.Findings {
		if seen[finding.Rule] || !diagnosisName.MatchString(finding.Rule) {
			continue
		}
		seen[finding.Rule] = true
		trace.Findings = append(trace.Findings, GPUTraceFinding{Rule: finding.Rule, Observed: finding.Observed, Comparison: finding.Comparison, Threshold: finding.Threshold, Unit: finding.Unit})
	}
	measure := func(name string, value *float64, places int) {
		if value != nil && finite(*value) {
			trace.Measurements[name] = round(*value, places)
		}
	}
	idle := analysis.Utilization.IdleShare
	measure(TraceIdleShare, &idle, 4)
	measure(TraceIdleHostWaitShare, analysis.Idle.HostWaitShare, 4)
	measure(TraceLaunchDelayP50US, analysis.Launch.LaunchDelayP50US, 1)
	measure(TraceShortKernelShare, analysis.Launch.ShortKernelShare, 4)
	if analysis.Trace.LaunchesRecorded && analysis.Launch.Launches > 0 {
		graphs := float64(analysis.Launch.GraphLaunches) / float64(analysis.Launch.Launches)
		measure(TraceGraphLaunchShare, &graphs, 4)
	}
	measure(TraceOverlappedShare, analysis.Overlap.OverlappedShare, 4)
	if analysis.Roofline.Position != "unknown" {
		measure(TraceRooflineCoveredShare, analysis.Roofline.CoveredShare, 4)
	}
	if analysis.Phases.Source != "unknown" {
		decode := analysis.Phases.Decode.Share
		measure(TraceDecodeShare, &decode, 4)
	}
	measure(TraceStepP50MS, analysis.Steps.P50MS, 3)
	measure(TraceStepP95MS, analysis.Steps.P95MS, 3)
	for _, operator := range analysis.Operators {
		summary := TraceOperator{Operator: operator.Operator, Share: round(operator.Share, 4), CoveredShare: round(operator.CoveredShare, 4)}
		if operator.Efficiency != nil && finite(*operator.Efficiency) {
			summary.Efficiency = pointerTo(round(*operator.Efficiency, 4))
		}
		trace.Operators = append(trace.Operators, summary)
	}
	return trace
}

func pointerTo[T any](value T) *T { return &value }

// traceOperator returns an operator class from the trace summary.
func (d Diagnosis) traceOperator(class string) (TraceOperator, bool) {
	trace := d.trace()
	if trace == nil {
		return TraceOperator{}, false
	}
	for _, operator := range trace.Operators {
		if operator.Operator == class {
			return operator, true
		}
	}
	return TraceOperator{}, false
}

// KernelClass groups a GPU kernel by its name into attention, GEMM,
// communication, or other, using the same vocabulary as the trace analysis
// (gputrace.Category) folded into the four classes a plan reads.
func KernelClass(name string) string {
	return gputrace.DiagnosisClass(name)
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
		if profile.Trace != nil {
			trace := *profile.Trace
			if trace.Findings == nil {
				trace.Findings = []GPUTraceFinding{}
			}
			if trace.Measurements == nil {
				trace.Measurements = map[string]float64{}
			}
			profile.Trace = &trace
		}
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
		if profile.Trace != nil {
			return validateTraceSummary(*profile.Trace)
		}
	}
	return nil
}

func validateTraceSummary(trace GPUTraceSummary) error {
	if !reviewedTraceAlgorithms[trace.AlgorithmVersion] || !traceBottlenecks[trace.Bottleneck] || trace.Devices < 1 || trace.Devices > gpuprofile.MaxGPUs || !finite(trace.WindowMS) || trace.WindowMS <= 0 {
		return errors.New("diagnosis GPU trace summary is malformed")
	}
	if len(trace.Findings) > gpuprofile.MaxFindings || len(trace.Measurements) > len(traceShareMeasurements)+len(traceDurationMeasurements) {
		return fmt.Errorf("diagnosis GPU trace summary is limited to %d findings and the known measurements", gpuprofile.MaxFindings)
	}
	seen := map[string]bool{}
	for _, finding := range trace.Findings {
		if seen[finding.Rule] || !diagnosisName.MatchString(finding.Rule) || !diagnosisComparisons[finding.Comparison] || finding.Unit == "" || len(finding.Unit) > 32 || !finite(finding.Observed) || !finite(finding.Threshold) {
			return errors.New("diagnosis GPU trace finding is malformed")
		}
		seen[finding.Rule] = true
	}
	for name, value := range trace.Measurements {
		switch {
		case traceShareMeasurements[name] && finite(value) && value >= 0 && value <= 1:
		case traceDurationMeasurements[name] && finite(value) && value >= 0:
		default:
			return errors.New("diagnosis GPU trace measurement is unknown or out of range")
		}
	}
	operators := map[string]bool{}
	for _, operator := range trace.Operators {
		known := false
		for _, class := range gpuprofile.Operators {
			known = known || class == operator.Operator
		}
		share := func(value float64) bool { return finite(value) && value >= 0 && value <= 1 }
		if !known || operators[operator.Operator] || !share(operator.Share) || !share(operator.CoveredShare) || operator.Efficiency != nil && (!finite(*operator.Efficiency) || *operator.Efficiency <= 0 || *operator.Efficiency > 4) {
			return errors.New("diagnosis GPU trace operator is unknown, repeated, or out of range")
		}
		operators[operator.Operator] = true
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

// trace returns the capture's trace analysis summary, or nil for a v1
// capture or no capture.
func (d Diagnosis) trace() *GPUTraceSummary {
	if d.GPUProfile == nil {
		return nil
	}
	return d.GPUProfile.Trace
}

func (d Diagnosis) traceFinding(rule string) (GPUTraceFinding, bool) {
	if trace := d.trace(); trace != nil {
		for _, finding := range trace.Findings {
			if finding.Rule == rule {
				return finding, true
			}
		}
	}
	return GPUTraceFinding{}, false
}

func (d Diagnosis) traceMeasurement(name string) (float64, bool) {
	if trace := d.trace(); trace != nil {
		value, ok := trace.Measurements[name]
		return value, ok
	}
	return 0, false
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func round(value float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.Round(value*scale) / scale
}
