package gpuprofile

import (
	"errors"
	"fmt"
	"math"
)

// SchemaVersionV2 is a v1 recording plus the analysis of the trace it came
// from and an optional excerpt of that trace's timeline.
const SchemaVersionV2 = "infercrane.gpu-profile/v2"

// AnalysisAlgorithm names the rules that turn a trace into an Analysis. A
// change to any threshold or definition needs a new version.
const AnalysisAlgorithm = "gpu-trace-analysis-v1"

// Bounds on one analysis and one timeline excerpt.
const (
	MaxSeries         = 600
	MaxTopKernels     = 25
	MaxBubbles        = 10
	MaxStepItems      = 1000
	MaxRooflinePoints = 50
	MaxHardwareSeries = 8
	MaxFindings       = 32
	MaxExcerptEvents  = 20_000
	MaxExcerptNames   = 4_000
	MaxExcerptLanes   = 128
	MaxExcerptSteps   = 2_000
	// MaxExcerptMS bounds the excerpt to the busiest part of a capture.
	MaxExcerptMS = 2_000
)

// Kernel categories. The diagnosis planner folds memory, norm, activation,
// and sampling into other.
const (
	CategoryAttention     = "attention"
	CategoryGEMM          = "gemm"
	CategoryCommunication = "communication"
	CategoryMemory        = "memory"
	CategoryNorm          = "norm"
	CategoryActivation    = "activation"
	CategorySampling      = "sampling"
	CategoryOther         = "other"
	// CategoryLaunch names host launch calls in a timeline excerpt only.
	CategoryLaunch = "launch"
)

// Categories lists every kernel category.
var Categories = []string{CategoryAttention, CategoryGEMM, CategoryCommunication, CategoryMemory, CategoryNorm, CategoryActivation, CategorySampling, CategoryOther}

// Operators whose efficiency an analysis measures. Attention, mixture-of-
// experts expert matrix multiplies, expert routing, and normalization are
// the operator classes a custom kernel is planned for; plain matrix
// multiplies are on the roofline.
const (
	OperatorAttention  = "attention"
	OperatorMoEExperts = "moe-experts"
	OperatorMoERouting = "moe-routing"
	OperatorNorm       = "norm"
)

// Operators lists every measured operator class.
var Operators = []string{OperatorAttention, OperatorMoEExperts, OperatorMoERouting, OperatorNorm}

// Bottlenecks a verdict can name.
const (
	BottleneckHostLaunch      = "host-launch"
	BottleneckCommunication   = "communication"
	BottleneckMemoryBandwidth = "memory-bandwidth"
	BottleneckCompute         = "compute"
	BottleneckAttention       = "attention"
	BottleneckBusy            = "busy"
	BottleneckUnknown         = "unknown"
)

// Phases a step or kernel can belong to.
const (
	PhasePrefill = "prefill"
	PhaseDecode  = "decode"
	PhaseMixed   = "mixed"
	PhaseUnknown = "unknown"
)

var (
	traceFormats   = []string{"kineto", "rocprofv3", "nsys"}
	bottlenecks    = []string{BottleneckHostLaunch, BottleneckCommunication, BottleneckMemoryBandwidth, BottleneckCompute, BottleneckAttention, BottleneckBusy, BottleneckUnknown}
	phases         = []string{PhasePrefill, PhaseDecode, PhaseMixed, PhaseUnknown}
	phaseSources   = []string{"annotations", "kernel_names", "unknown"}
	stepSources    = []string{"annotations", "idle_gaps", "none"}
	positions      = []string{"memory_bound", "compute_bound", "unknown"}
	bubbleCauses   = []string{"host_wait", "launch_gap", "other"}
	hardwareFields = map[string]string{
		"gpu_sm_active": "percent", "gpu_sm_occupancy": "percent", "gpu_tensor_active": "percent", "gpu_dram_active": "percent",
		"gpu_pcie_throughput": "bytes_per_second", "gpu_nvlink_throughput": "bytes_per_second",
	}
	comparisons = []string{">=", "<=", ">", "<"}
)

// Analysis is what a trace says about where serving time goes. Shares are
// fractions in 0..1. A nil value is unknown and its section says why; it is
// never reported as zero.
type Analysis struct {
	AlgorithmVersion string         `json:"algorithm_version"`
	Trace            TraceSource    `json:"trace"`
	Verdict          Verdict        `json:"verdict"`
	Utilization      Utilization    `json:"utilization"`
	Idle             IdleBreakdown  `json:"idle"`
	Launch           LaunchStats    `json:"launch"`
	Categories       []CategoryTime `json:"categories"`
	TopKernels       []KernelStat   `json:"top_kernels"`
	// Operators is kernel time and efficiency per operator class. Absent from
	// an analysis written before it was measured.
	Operators []OperatorTime  `json:"operators,omitempty"`
	Overlap   Overlap         `json:"overlap"`
	Phases    PhaseSplit      `json:"phases"`
	Steps     StepStats       `json:"steps"`
	Roofline  Roofline        `json:"roofline"`
	Hardware  *HardwareSeries `json:"hardware"`
	Findings  []Finding       `json:"findings"`
}

// TraceSource describes the trace an analysis read and what it recorded.
type TraceSource struct {
	Format         string  `json:"format"`
	Engine         *string `json:"engine"`
	EngineVersion  *string `json:"engine_version"`
	Devices        int     `json:"devices"`
	Streams        int     `json:"streams"`
	Kernels        int64   `json:"kernels"`
	CPUOpsRecorded bool    `json:"cpu_ops_recorded"`
	ShapesRecorded bool    `json:"shapes_recorded"`
	// LaunchesRecorded is false when the trace has no host launch calls, so
	// host-bound idle time and launch delay are unknown.
	LaunchesRecorded bool    `json:"launches_recorded"`
	WindowMS         float64 `json:"window_ms"`
	// Truncated reports that the trace held more events than one analysis
	// reads; the analysis covers the events up to the limit.
	Truncated bool `json:"truncated"`
}

// Verdict is the one-sentence answer and the measured values it rests on.
type Verdict struct {
	Bottleneck string `json:"bottleneck"`
	Sentence   string `json:"sentence"`
	Evidence   string `json:"evidence"`
}

// Utilization is GPU busy and idle time over the analysis window, summed over
// devices. Busy time is the union of kernel and memory-operation intervals,
// so concurrent streams count once.
type Utilization struct {
	BusyShare       float64   `json:"busy_share"`
	ComputeShare    float64   `json:"compute_share"`
	NonComputeShare float64   `json:"non_compute_share"`
	IdleShare       float64   `json:"idle_share"`
	BucketMS        float64   `json:"bucket_ms"`
	BusySeries      []float64 `json:"busy_series"`
}

// IdleBreakdown splits idle GPU time by its cause.
type IdleBreakdown struct {
	IdleMS         float64     `json:"idle_ms"`
	HostWaitShare  *float64    `json:"host_wait_share"`
	LaunchGapShare *float64    `json:"launch_gap_share"`
	OtherShare     *float64    `json:"other_share"`
	Reason         string      `json:"reason"`
	Bubbles        BubbleStats `json:"bubbles"`
}

// BubbleStats describes the idle gaps between kernels on one device.
type BubbleStats struct {
	Count int      `json:"count"`
	P50US *float64 `json:"p50_us"`
	P95US *float64 `json:"p95_us"`
	MaxUS *float64 `json:"max_us"`
	Top   []Bubble `json:"top"`
}

// Bubble is one idle gap.
type Bubble struct {
	StartMS    float64 `json:"start_ms"`
	DurationUS float64 `json:"duration_us"`
	Device     int     `json:"device"`
	Cause      string  `json:"cause"`
}

// LaunchStats describes how the host enqueued GPU work.
type LaunchStats struct {
	Launches         int64    `json:"launches"`
	GraphLaunches    int64    `json:"graph_launches"`
	PerSecond        float64  `json:"per_second"`
	HostLaunchShare  *float64 `json:"host_launch_share"`
	LaunchDelayP50US *float64 `json:"launch_delay_p50_us"`
	LaunchDelayP95US *float64 `json:"launch_delay_p95_us"`
	ShortKernelShare *float64 `json:"short_kernel_share"`
	Reason           string   `json:"reason"`
}

// CategoryTime is kernel time in one category.
type CategoryTime struct {
	Category string  `json:"category"`
	KernelMS float64 `json:"kernel_ms"`
	Share    float64 `json:"share"`
	Count    int64   `json:"count"`
}

// KernelStat is one of the kernels with the most GPU time.
type KernelStat struct {
	Name     string  `json:"name"`
	Category string  `json:"category"`
	Phase    *string `json:"phase"`
	Count    int64   `json:"count"`
	TotalMS  float64 `json:"total_ms"`
	MeanUS   float64 `json:"mean_us"`
	MaxUS    float64 `json:"max_us"`
	Share    float64 `json:"share"`
}

// OperatorTime is one operator class's kernel time and, when its host
// operators recorded shapes for at least half of that time, how close it
// came to what those shapes allow on this accelerator. Efficiency is the
// achieved throughput over the roofline's attainable throughput at the
// operator's intensity. A value the trace could not measure is nil and
// Reason says why; it is never zero.
type OperatorTime struct {
	Operator string  `json:"operator"`
	KernelMS float64 `json:"kernel_ms"`
	// Share is of all kernel time in the trace.
	Share float64 `json:"share"`
	// CoveredShare is the part of this operator's kernel time whose host
	// operator recorded usable shapes. Kernels replayed from a CUDA graph have
	// no host operator, so decode time is often uncovered.
	CoveredShare   float64  `json:"covered_share"`
	Intensity      *float64 `json:"intensity"`
	AchievedTFLOPS *float64 `json:"achieved_tflops"`
	AchievedTBps   *float64 `json:"achieved_tbps"`
	Bound          *string  `json:"bound"`
	Efficiency     *float64 `json:"efficiency"`
	Reason         string   `json:"reason"`
}

// Overlap is how much collective communication ran under compute.
type Overlap struct {
	CommunicationMS float64  `json:"communication_ms"`
	OverlappedShare *float64 `json:"overlapped_share"`
	Reason          string   `json:"reason"`
}

// PhaseSplit is GPU kernel time by serving phase.
type PhaseSplit struct {
	Source  string    `json:"source"`
	Prefill PhaseTime `json:"prefill"`
	Decode  PhaseTime `json:"decode"`
	Mixed   PhaseTime `json:"mixed"`
	Unknown PhaseTime `json:"unknown"`
}

// PhaseTime is the kernel time and step count of one phase.
type PhaseTime struct {
	GPUMS float64 `json:"gpu_ms"`
	Share float64 `json:"share"`
	Steps int     `json:"steps"`
}

// StepStats is the per-step timeline of the engine's scheduler loop.
type StepStats struct {
	Source string     `json:"source"`
	Marker *string    `json:"marker"`
	Count  int        `json:"count"`
	P50MS  *float64   `json:"p50_ms"`
	P95MS  *float64   `json:"p95_ms"`
	Items  []StepItem `json:"items"`
	Reason string     `json:"reason"`
}

// StepItem is one engine step.
type StepItem struct {
	StartMS    float64 `json:"start_ms"`
	DurationMS float64 `json:"duration_ms"`
	GPUBusyMS  float64 `json:"gpu_busy_ms"`
	Kernels    int     `json:"kernels"`
	Phase      string  `json:"phase"`
}

// Roofline places matrix multiplies against the accelerator's peak compute
// and memory bandwidth.
type Roofline struct {
	Accelerator      *string         `json:"accelerator"`
	Peak             *Peak           `json:"peak"`
	RidgeFLOPPerByte *float64        `json:"ridge_flop_per_byte"`
	Position         string          `json:"position"`
	MemoryBoundShare *float64        `json:"memory_bound_share"`
	CoveredShare     *float64        `json:"covered_share"`
	Points           []RooflinePoint `json:"points"`
	Reason           string          `json:"reason"`
}

// Peak is an accelerator's datasheet peak, not a measurement.
type Peak struct {
	ComputeTFLOPS float64 `json:"compute_tflops"`
	Precision     string  `json:"precision"`
	MemoryTBps    float64 `json:"memory_tbps"`
	Source        string  `json:"source"`
}

// RooflinePoint is one operator shape: FLOPs and bytes from its recorded
// shapes, time from its kernels.
type RooflinePoint struct {
	Label          string  `json:"label"`
	Category       string  `json:"category"`
	FLOPs          float64 `json:"flops"`
	Bytes          float64 `json:"bytes"`
	KernelMS       float64 `json:"kernel_ms"`
	Intensity      float64 `json:"intensity"`
	AchievedTFLOPS float64 `json:"achieved_tflops"`
	Bound          string  `json:"bound"`
}

// HardwareSeries holds DCGM readings sampled while the trace was captured.
type HardwareSeries struct {
	Source     string        `json:"source"`
	IntervalMS float64       `json:"interval_ms"`
	Series     []FieldSeries `json:"series"`
}

// FieldSeries is one DCGM field over the capture. A nil value was not sent.
type FieldSeries struct {
	Field  string     `json:"field"`
	Unit   string     `json:"unit"`
	Values []*float64 `json:"values"`
}

// Finding is one rule that fired, with the measured value it crossed.
type Finding struct {
	Rule       string  `json:"rule"`
	Observed   float64 `json:"observed"`
	Comparison string  `json:"comparison"`
	Threshold  float64 `json:"threshold"`
	Unit       string  `json:"unit"`
	Sentence   string  `json:"sentence"`
}

// TimelineExcerpt is the busiest part of the capture, for a timeline viewer.
// Events are [lane, name, start_us, duration_us] with start relative to the
// excerpt.
type TimelineExcerpt struct {
	StartMS    float64       `json:"start_ms"`
	DurationMS float64       `json:"duration_ms"`
	Names      []string      `json:"names"`
	Categories []string      `json:"categories"`
	Lanes      []Lane        `json:"lanes"`
	Events     [][4]float64  `json:"events"`
	Steps      []ExcerptStep `json:"steps"`
	Truncated  bool          `json:"truncated"`
}

// Lane is one GPU stream or host thread row.
type Lane struct {
	Kind   string `json:"kind"`
	Device *int   `json:"device"`
	Stream *int64 `json:"stream"`
	Label  string `json:"label"`
}

// ExcerptStep is one engine step inside the excerpt.
type ExcerptStep struct {
	StartUS    float64 `json:"start_us"`
	DurationUS float64 `json:"duration_us"`
	Phase      string  `json:"phase"`
}

// ValidateAnalysis rejects an analysis that is unbounded, out of range, or
// presents an unknown value as a number.
func ValidateAnalysis(analysis Analysis) error {
	if analysis.AlgorithmVersion != AnalysisAlgorithm {
		return fmt.Errorf("analysis algorithm_version must be %s", AnalysisAlgorithm)
	}
	if err := validateTraceSource(analysis.Trace); err != nil {
		return err
	}
	verdict := analysis.Verdict
	if !oneOf(verdict.Bottleneck, bottlenecks) || !boundedText(verdict.Sentence, 280) || len(verdict.Evidence) > 400 || verdict.Evidence != "" && !boundedText(verdict.Evidence, 400) {
		return errors.New("analysis verdict needs a known bottleneck, a sentence, and bounded evidence")
	}
	if err := validateUtilization(analysis.Utilization); err != nil {
		return err
	}
	if err := validateIdle(analysis.Idle); err != nil {
		return err
	}
	if err := validateLaunch(analysis.Launch); err != nil {
		return err
	}
	if len(analysis.Categories) > len(Categories) {
		return errors.New("analysis categories repeat")
	}
	seen := map[string]bool{}
	for _, category := range analysis.Categories {
		if !oneOf(category.Category, Categories) || seen[category.Category] || !nonNegative(category.KernelMS) || !share(category.Share) || category.Count < 0 {
			return errors.New("analysis category is unknown, repeated, or out of range")
		}
		seen[category.Category] = true
	}
	if err := validateOperators(analysis.Operators); err != nil {
		return err
	}
	if len(analysis.TopKernels) > MaxTopKernels {
		return fmt.Errorf("analysis lists at most %d top kernels", MaxTopKernels)
	}
	for _, kernel := range analysis.TopKernels {
		if !boundedText(kernel.Name, MaxFrameName) || !oneOf(kernel.Category, Categories) || kernel.Phase != nil && !oneOf(*kernel.Phase, phases) || kernel.Count < 1 || !nonNegative(kernel.TotalMS) || !nonNegative(kernel.MeanUS) || !nonNegative(kernel.MaxUS) || !share(kernel.Share) {
			return errors.New("analysis top kernel is malformed")
		}
	}
	if err := validateOptionalShare(analysis.Overlap.OverlappedShare, analysis.Overlap.Reason, "overlap"); err != nil || !nonNegative(analysis.Overlap.CommunicationMS) {
		return errors.New("analysis overlap is out of range or unknown without a reason")
	}
	if err := validatePhases(analysis.Phases); err != nil {
		return err
	}
	if err := validateSteps(analysis.Steps); err != nil {
		return err
	}
	if err := validateRoofline(analysis.Roofline); err != nil {
		return err
	}
	if err := validateHardware(analysis.Hardware); err != nil {
		return err
	}
	if len(analysis.Findings) > MaxFindings {
		return fmt.Errorf("analysis lists at most %d findings", MaxFindings)
	}
	for _, finding := range analysis.Findings {
		if !toolPattern.MatchString(finding.Rule) || !oneOf(finding.Comparison, comparisons) || !finite(finding.Observed) || !finite(finding.Threshold) || len(finding.Unit) > 32 || !boundedText(finding.Sentence, 280) {
			return errors.New("analysis finding is malformed")
		}
	}
	return nil
}

func validateTraceSource(source TraceSource) error {
	if !oneOf(source.Format, traceFormats) {
		return errors.New("analysis trace format must be kineto, rocprofv3, or nsys")
	}
	if source.Engine != nil && *source.Engine != "vllm" && *source.Engine != "sglang" {
		return errors.New("analysis trace engine must be vllm, sglang, or null")
	}
	if source.EngineVersion != nil && !toolVersionPattern.MatchString(*source.EngineVersion) {
		return errors.New("analysis trace engine_version must be a bounded identifier")
	}
	if source.Devices < 1 || source.Devices > MaxGPUs || source.Streams < 1 || source.Streams > 100_000 || source.Kernels < 1 || !finite(source.WindowMS) || source.WindowMS <= 0 || source.WindowMS > float64(MaxCapture/1e6) {
		return errors.New("analysis trace needs devices, streams, kernels, and a positive window")
	}
	return nil
}

func validateUtilization(utilization Utilization) error {
	if !share(utilization.BusyShare) || !share(utilization.ComputeShare) || !share(utilization.NonComputeShare) || !share(utilization.IdleShare) {
		return errors.New("analysis utilization shares must be 0..1")
	}
	if math.Abs(utilization.BusyShare+utilization.IdleShare-1) > 0.01 || utilization.ComputeShare+utilization.NonComputeShare > utilization.BusyShare+0.01 {
		return errors.New("analysis utilization busy and idle must add up to the window")
	}
	if len(utilization.BusySeries) > MaxSeries || len(utilization.BusySeries) > 0 && (!finite(utilization.BucketMS) || utilization.BucketMS <= 0) {
		return fmt.Errorf("analysis busy_series holds at most %d buckets of a positive width", MaxSeries)
	}
	for _, value := range utilization.BusySeries {
		if !share(value) {
			return errors.New("analysis busy_series values must be 0..1")
		}
	}
	return nil
}

func validateIdle(idle IdleBreakdown) error {
	if !nonNegative(idle.IdleMS) || len(idle.Reason) > 280 {
		return errors.New("analysis idle time is out of range")
	}
	shares := []*float64{idle.HostWaitShare, idle.LaunchGapShare, idle.OtherShare}
	known := 0
	for _, value := range shares {
		if value != nil {
			known++
			if !share(*value) {
				return errors.New("analysis idle shares must be 0..1")
			}
		}
	}
	if known != 0 && known != len(shares) || known == 0 && idle.Reason == "" {
		return errors.New("analysis idle causes are all known or all unknown with a reason")
	}
	bubbles := idle.Bubbles
	if bubbles.Count < 0 || len(bubbles.Top) > MaxBubbles || !optionalNonNegative(bubbles.P50US) || !optionalNonNegative(bubbles.P95US) || !optionalNonNegative(bubbles.MaxUS) {
		return errors.New("analysis bubbles are out of range")
	}
	for _, bubble := range bubbles.Top {
		if !nonNegative(bubble.StartMS) || !nonNegative(bubble.DurationUS) || bubble.Device < 0 || bubble.Device >= MaxGPUs || !oneOf(bubble.Cause, bubbleCauses) {
			return errors.New("analysis bubble is malformed")
		}
	}
	return nil
}

func validateLaunch(launch LaunchStats) error {
	if launch.Launches < 0 || launch.GraphLaunches < 0 || launch.GraphLaunches > launch.Launches || !nonNegative(launch.PerSecond) || len(launch.Reason) > 280 {
		return errors.New("analysis launch counts are out of range")
	}
	for _, value := range []*float64{launch.HostLaunchShare, launch.ShortKernelShare} {
		if value != nil && !share(*value) {
			return errors.New("analysis launch shares must be 0..1")
		}
	}
	if !optionalNonNegative(launch.LaunchDelayP50US) || !optionalNonNegative(launch.LaunchDelayP95US) {
		return errors.New("analysis launch delay must not be negative")
	}
	if (launch.HostLaunchShare == nil || launch.LaunchDelayP50US == nil || launch.ShortKernelShare == nil) && launch.Reason == "" {
		return errors.New("analysis launch statistics that are unknown need a reason")
	}
	return nil
}

func validatePhases(split PhaseSplit) error {
	if !oneOf(split.Source, phaseSources) {
		return errors.New("analysis phases source is unknown")
	}
	total := 0.0
	for _, phase := range []PhaseTime{split.Prefill, split.Decode, split.Mixed, split.Unknown} {
		if !nonNegative(phase.GPUMS) || !share(phase.Share) || phase.Steps < 0 {
			return errors.New("analysis phase time is out of range")
		}
		total += phase.Share
	}
	if total > 1.01 {
		return errors.New("analysis phase shares add up to more than the kernel time")
	}
	return nil
}

func validateSteps(steps StepStats) error {
	if !oneOf(steps.Source, stepSources) || steps.Marker != nil && !boundedText(*steps.Marker, 128) || steps.Count < 0 || len(steps.Items) > MaxStepItems || len(steps.Items) > steps.Count || len(steps.Reason) > 280 {
		return errors.New("analysis steps are malformed or unbounded")
	}
	if !optionalNonNegative(steps.P50MS) || !optionalNonNegative(steps.P95MS) || (steps.P50MS == nil || steps.P95MS == nil) && steps.Reason == "" {
		return errors.New("analysis step percentiles must not be negative, and unknown ones need a reason")
	}
	for _, step := range steps.Items {
		if !nonNegative(step.StartMS) || !nonNegative(step.DurationMS) || !nonNegative(step.GPUBusyMS) || step.Kernels < 0 || !oneOf(step.Phase, phases) {
			return errors.New("analysis step is malformed")
		}
	}
	return nil
}

func validateRoofline(roofline Roofline) error {
	if roofline.Accelerator != nil && !boundedText(*roofline.Accelerator, 128) || !oneOf(roofline.Position, positions) || len(roofline.Points) > MaxRooflinePoints || len(roofline.Reason) > 280 {
		return errors.New("analysis roofline is malformed")
	}
	if peak := roofline.Peak; peak != nil && (!positive(peak.ComputeTFLOPS) || !positive(peak.MemoryTBps) || !boundedText(peak.Precision, 64) || !boundedText(peak.Source, 200)) {
		return errors.New("analysis roofline peak must be positive and cite its source")
	}
	if roofline.RidgeFLOPPerByte != nil && !positive(*roofline.RidgeFLOPPerByte) || roofline.Position != "unknown" && (roofline.Peak == nil || roofline.MemoryBoundShare == nil) || roofline.Position == "unknown" && roofline.Reason == "" {
		return errors.New("analysis roofline position needs a peak and a measured share, or a reason it is unknown")
	}
	for _, value := range []*float64{roofline.MemoryBoundShare, roofline.CoveredShare} {
		if value != nil && !share(*value) {
			return errors.New("analysis roofline shares must be 0..1")
		}
	}
	for _, point := range roofline.Points {
		if !boundedText(point.Label, 200) || !oneOf(point.Category, Categories) || !positive(point.FLOPs) || !positive(point.Bytes) || !positive(point.KernelMS) || !positive(point.Intensity) || !positive(point.AchievedTFLOPS) || point.Bound != "memory" && point.Bound != "compute" {
			return errors.New("analysis roofline point is malformed")
		}
	}
	return nil
}

func validateOperators(operators []OperatorTime) error {
	seen := map[string]bool{}
	for _, operator := range operators {
		if !oneOf(operator.Operator, Operators) || seen[operator.Operator] || !positive(operator.KernelMS) || !share(operator.Share) || !share(operator.CoveredShare) || len(operator.Reason) > 280 {
			return errors.New("analysis operator is unknown, repeated, or out of range")
		}
		seen[operator.Operator] = true
		measured := operator.Efficiency != nil
		for _, value := range []*float64{operator.Intensity, operator.AchievedTFLOPS, operator.AchievedTBps} {
			if (value != nil) != measured || value != nil && !positive(*value) {
				return errors.New("analysis operator throughput is measured together with its efficiency, and positive")
			}
		}
		if measured && (!positive(*operator.Efficiency) || *operator.Efficiency > 4 || operator.Bound == nil || *operator.Bound != "memory" && *operator.Bound != "compute") || !measured && (operator.Bound != nil || operator.Reason == "") {
			return errors.New("analysis operator efficiency needs a bound, or a reason it is unknown")
		}
	}
	return nil
}

func validateHardware(hardware *HardwareSeries) error {
	if hardware == nil {
		return nil
	}
	if hardware.Source != "dcgm-exporter" || !positive(hardware.IntervalMS) || len(hardware.Series) < 1 || len(hardware.Series) > MaxHardwareSeries {
		return errors.New("analysis hardware series must come from dcgm-exporter at a positive interval")
	}
	seen := map[string]bool{}
	for _, series := range hardware.Series {
		unit, known := hardwareFields[series.Field]
		if !known || unit != series.Unit || seen[series.Field] || len(series.Values) < 1 || len(series.Values) > MaxSeries {
			return errors.New("analysis hardware series names an unknown or repeated field")
		}
		seen[series.Field] = true
		for _, value := range series.Values {
			if value != nil && (!nonNegative(*value) || unit == "percent" && *value > 100) {
				return errors.New("analysis hardware value is out of range")
			}
		}
	}
	return nil
}

// ValidateExcerpt rejects an excerpt that is unbounded or names a lane or
// name it does not list.
func ValidateExcerpt(excerpt TimelineExcerpt) error {
	if !nonNegative(excerpt.StartMS) || !positive(excerpt.DurationMS) || excerpt.DurationMS > MaxExcerptMS {
		return fmt.Errorf("timeline excerpt must cover 0..%d ms", MaxExcerptMS)
	}
	if len(excerpt.Names) < 1 || len(excerpt.Names) > MaxExcerptNames || len(excerpt.Categories) != len(excerpt.Names) {
		return errors.New("timeline excerpt names and categories must match and stay bounded")
	}
	for index, name := range excerpt.Names {
		if !boundedText(name, MaxFrameName) || !oneOf(excerpt.Categories[index], Categories) && excerpt.Categories[index] != CategoryLaunch {
			return errors.New("timeline excerpt name or category is malformed")
		}
	}
	if len(excerpt.Lanes) < 1 || len(excerpt.Lanes) > MaxExcerptLanes {
		return fmt.Errorf("timeline excerpt has 1..%d lanes", MaxExcerptLanes)
	}
	for _, lane := range excerpt.Lanes {
		if lane.Kind != "gpu" && lane.Kind != "host" || !boundedText(lane.Label, 128) || lane.Kind == "gpu" && (lane.Device == nil || lane.Stream == nil) {
			return errors.New("timeline excerpt lane is malformed")
		}
	}
	if len(excerpt.Events) > MaxExcerptEvents || len(excerpt.Steps) > MaxExcerptSteps {
		return errors.New("timeline excerpt holds too many events or steps")
	}
	limit := excerpt.DurationMS*1000 + 1
	for _, event := range excerpt.Events {
		lane, name := event[0], event[1]
		if lane != math.Trunc(lane) || lane < 0 || int(lane) >= len(excerpt.Lanes) || name != math.Trunc(name) || name < 0 || int(name) >= len(excerpt.Names) {
			return errors.New("timeline excerpt event names a lane or name that does not exist")
		}
		if !nonNegative(event[2]) || !nonNegative(event[3]) || event[2]+event[3] > limit {
			return errors.New("timeline excerpt event must fit inside the excerpt")
		}
	}
	for _, step := range excerpt.Steps {
		if !nonNegative(step.StartUS) || !nonNegative(step.DurationUS) || step.StartUS > limit || !oneOf(step.Phase, phases) {
			return errors.New("timeline excerpt step is malformed")
		}
	}
	return nil
}

func validateOptionalShare(value *float64, reason, section string) error {
	if value == nil && reason == "" {
		return fmt.Errorf("analysis %s is unknown without a reason", section)
	}
	if value != nil && !share(*value) || len(reason) > 280 {
		return fmt.Errorf("analysis %s is out of range", section)
	}
	return nil
}

func oneOf(value string, allowed []string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func nonNegative(value float64) bool { return finite(value) && value >= 0 }

func positive(value float64) bool { return finite(value) && value > 0 }

func share(value float64) bool { return finite(value) && value >= 0 && value <= 1 }

func optionalNonNegative(value *float64) bool { return value == nil || nonNegative(*value) }
