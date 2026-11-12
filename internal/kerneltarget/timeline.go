package kerneltarget

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/kernelplanner"
)

// MinTimelineWindow and MaxTimelineWindow bound an operator-configured
// capture window.
const (
	MinTimelineWindow = 5 * time.Second
	MaxTimelineWindow = 2 * time.Minute
)

const (
	TimelineSchemaV1 = "infercrane.serving-timeline/v1"
	timelineTool     = "nsight-systems"

	defaultTimelineWindow  = 20 * time.Second
	defaultTimelineWarmup  = 5 * time.Second
	defaultReportTimeout   = 10 * time.Minute
	timelineReportBase     = "timeline"
	timelineStatsBase      = "stats"
	maxStatsFileBytes      = 64 << 20
	maxStatsRows           = 100000
	maxTimelineTopKernels  = 10
	maxTimelineKernelName  = 200
	timelineLoadRequests   = 100000
	timelineVersionLimit   = 200
	timelineKernelReport   = "cuda_gpu_kern_sum"
	timelineAPIReport      = "cuda_api_sum"
	timelineMemoryReport   = "cuda_gpu_mem_time_sum"
	timelineReportFilename = timelineReportBase + ".nsys-rep"
)

// TimelineSpec is reviewed configuration for a short Nsight Systems capture of
// one exact runtime version. A runtime without it cannot be traced: the
// capture is opt-in and needs an image that ships the nsys CLI.
type TimelineSpec struct {
	// NSys is the absolute path of the nsys CLI inside the runtime image.
	NSys string `json:"nsys"`
	// ProfilerArgs replace the runtime's torch-profiler arguments so that
	// /start_profile and /stop_profile call cudaProfilerStart and Stop (for
	// example vLLM's --profiler-config.profiler=cuda).
	ProfilerArgs []string `json:"profiler_args,omitempty"`
	// ProfileRequest is the /start_profile body (for example SGLang's
	// {"activities":["CUDA_PROFILER"]}).
	ProfileRequest json.RawMessage `json:"profile_request,omitempty"`
	// Env lists extra KEY=VALUE settings, such as
	// VLLM_WORKER_MULTIPROC_METHOD=spawn so nsys follows worker processes.
	Env []string `json:"env,omitempty"`
}

var timelineEnvPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}=[A-Za-z0-9._:/=,+-]{0,256}$`)

func validateTimeline(spec *TimelineSpec) error {
	if spec == nil {
		return nil
	}
	if !filepath.IsAbs(spec.NSys) || filepath.Clean(spec.NSys) != spec.NSys {
		return errors.New("timeline nsys must be a clean absolute path inside the runtime image")
	}
	for _, entry := range spec.Env {
		if !timelineEnvPattern.MatchString(entry) {
			return fmt.Errorf("timeline env entry %q is not a bounded KEY=VALUE pair", entry)
		}
	}
	if len(spec.ProfileRequest) > 0 && !json.Valid(spec.ProfileRequest) {
		return errors.New("timeline profile_request must be valid JSON")
	}
	return nil
}

// nsysProfileArgs records CUDA API and kernel activity only. CPU sampling and
// context-switch tracing stay off so the capture needs no extra Linux
// capability in the hardened serving container. stop-shutdown ends the
// session (and writes the report) when the runtime calls cudaProfilerStop.
func nsysProfileArgs() []string {
	return []string{
		"profile", "--trace=cuda,nvtx", "--sample=none", "--cpuctxsw=none", "--cuda-graph-trace=node",
		"--trace-fork-before-exec=true", "--capture-range=cudaProfilerApi", "--capture-range-end=stop-shutdown",
		"--kill=sigterm", "--force-overwrite=true", "--output=" + containerProfileDir + "/" + timelineReportBase,
	}
}

// TimelineKernel is one of the busiest GPU kernels in the capture.
type TimelineKernel struct {
	Name              string  `json:"name"`
	TotalTimeMS       float64 `json:"total_time_ms"`
	Instances         int     `json:"instances"`
	KernelTimeShare   float64 `json:"kernel_time_share"`
	CommunicationKind bool    `json:"communication"`
}

// TimelineSummary is the bounded result of one capture. Times are summed over
// every GPU and stream. A nil field was not exported by nsys and stays
// unknown.
type TimelineSummary struct {
	CaptureWindowMS float64 `json:"capture_window_ms"`
	Accelerators    int     `json:"accelerators"`
	KernelTimeMS    float64 `json:"kernel_time_ms"`
	// MemoryOperationTimeMS covers memcpy and memset work on the GPU.
	MemoryOperationTimeMS *float64 `json:"memory_operation_time_ms"`
	// GPUIdleFraction is 1 - (kernel + memory time) / (window * GPUs),
	// clamped to 0..1. Overlapping streams count twice, so it is a lower
	// bound on idle time.
	GPUIdleFraction *float64 `json:"gpu_idle_fraction"`
	// KernelLaunchTimeMS is host time inside kernel and graph launch calls.
	KernelLaunchTimeMS *float64 `json:"kernel_launch_time_ms"`
	// KernelLaunchOverheadShare divides launch time by window * GPUs, which
	// assumes one launching worker thread per GPU.
	KernelLaunchOverheadShare *float64 `json:"kernel_launch_overhead_share"`
	// CommunicationKernelShare is the share of kernel time in collective
	// communication kernels (NCCL and runtime all-reduce kernels), which
	// includes time spent waiting for peers.
	CommunicationKernelShare float64          `json:"communication_kernel_share"`
	TopKernels               []TimelineKernel `json:"top_kernels"`
}

// TimelineEvidence binds a summary to the exact capture inputs.
type TimelineEvidence struct {
	SchemaVersion      string                         `json:"schema_version"`
	InputDigest        string                         `json:"input_digest"`
	Tool               string                         `json:"tool"`
	ToolVersion        string                         `json:"tool_version"`
	Hardware           kernelplanner.HardwareIdentity `json:"hardware"`
	RuntimeImageDigest string                         `json:"runtime_image_digest"`
	Topology           acceleratorlab.Topology        `json:"topology"`
	WorkloadDigest     string                         `json:"workload_digest"`
	CapturedAt         time.Time                      `json:"captured_at"`
	Summary            TimelineSummary                `json:"summary"`
	CostUSD            float64                        `json:"cost_usd"`
	ArtifactDigest     string                         `json:"artifact_digest"`
	ContentRecorded    bool                           `json:"content_recorded"`
}

// launchAPIs are the CUDA runtime and driver calls that enqueue GPU work.
var launchAPIs = map[string]struct{}{
	"cudalaunchkernel": {}, "cudalaunchkernelexc": {}, "cudalaunchcooperativekernel": {}, "cugraphlaunch": {},
	"culaunchkernel": {}, "culaunchkernelex": {}, "culaunchcooperativekernel": {}, "cudagraphlaunch": {},
}

// communicationMarkers is reviewed vocabulary for collective-communication
// kernel names: NCCL plus the custom all-reduce kernels vLLM and SGLang ship.
var communicationMarkers = []string{"nccl", "cross_device_reduce", "allreduce", "all_reduce", "allgather", "all_gather", "reduce_scatter", "reducescatter", "alltoall", "all_to_all"}

// CaptureTimeline records a short Nsight Systems timeline of the serving
// process under a representative load. It runs only for runtimes with a
// reviewed timeline configuration and an image that contains nsys; otherwise
// it fails closed before any GPU work starts.
func (t Target) CaptureTimeline(ctx context.Context, intent acceleratorlab.ProfileIntent) (TimelineEvidence, error) {
	started := t.now()
	if err := validateBoundary(intent.Model, intent.Topology); err != nil {
		return TimelineEvidence{}, err
	}
	if intent.Workload.Modality != acceleratorlab.ModalityText {
		return TimelineEvidence{}, errors.New("this target adapter traces text workloads only")
	}
	spec, err := t.runtime(intent.Runtime)
	if err != nil {
		return TimelineEvidence{}, err
	}
	if spec.Timeline == nil {
		return TimelineEvidence{}, fmt.Errorf("runtime %s %s has no reviewed Nsight Systems configuration; timeline capture is opt-in", spec.Name, spec.Version)
	}
	window, warmup := t.timelineWindow(), t.timelineWarmup()
	if window > MaxTimelineWindow {
		return TimelineEvidence{}, fmt.Errorf("timeline window must not exceed %s", MaxTimelineWindow)
	}
	if err = t.verifyAccelerator(ctx, intent.Hardware); err != nil {
		return TimelineEvidence{}, err
	}
	image := spec.Repository + "@" + intent.Runtime.ImageDigest
	if err = t.materialize(ctx, spec, image, intent.Model); err != nil {
		return TimelineEvidence{}, err
	}
	version, err := t.nsysVersion(ctx, image, spec.Timeline.NSys)
	if err != nil {
		return TimelineEvidence{}, err
	}
	if err = t.ensureNetwork(ctx); err != nil {
		return TimelineEvidence{}, err
	}
	profileDir, err := t.privateDirectory("timeline-")
	if err != nil {
		return TimelineEvidence{}, err
	}
	instance, err := t.start(ctx, serverRequest{Spec: spec, Image: image, Model: intent.Model, Name: containerName("timeline", intent.InputDigest), ProfileDir: profileDir, Timeline: spec.Timeline})
	if err != nil {
		return TimelineEvidence{}, err
	}
	defer instance.stop()

	measured, stopErr, err := t.traceWindow(ctx, instance, spec, intent, window, warmup)
	if err != nil {
		return TimelineEvidence{}, err
	}
	report := filepath.Join(profileDir, timelineReportFilename)
	if err = t.waitForReport(ctx, report); err != nil {
		if stopErr != nil {
			return TimelineEvidence{}, fmt.Errorf("%w (stop_profile also failed: %v)", err, stopErr)
		}
		return TimelineEvidence{}, err
	}
	digest, err := hashFile(report)
	if err != nil {
		return TimelineEvidence{}, fmt.Errorf("hash timeline report: %w", err)
	}
	if err = t.exportStats(ctx, image, spec.Timeline.NSys, profileDir); err != nil {
		return TimelineEvidence{}, err
	}
	summary, err := summarizeNsysStats(profileDir, measured, intent.Topology.Accelerators)
	if err != nil {
		return TimelineEvidence{}, err
	}
	return TimelineEvidence{
		SchemaVersion: TimelineSchemaV1, InputDigest: intent.InputDigest, Tool: timelineTool, ToolVersion: version,
		Hardware: intent.Hardware, RuntimeImageDigest: intent.Runtime.ImageDigest, Topology: intent.Topology,
		WorkloadDigest: intent.Workload.Digest, CapturedAt: t.now(), Summary: summary,
		CostUSD: t.elapsedCost(started), ArtifactDigest: "sha256:" + digest, ContentRecorded: false,
	}, nil
}

// traceWindow keeps load running, opens the CUDA profiler range for the
// window, and closes it. It returns the measured window length. nsys ends the
// serving process as soon as the range closes, so a failed /stop_profile
// response is returned separately: the written report decides the outcome.
func (t Target) traceWindow(ctx context.Context, instance *server, spec RuntimeSpec, intent acceleratorlab.ProfileIntent, window, warmup time.Duration) (time.Duration, error, error) {
	inputTokens, outputTokens := shapeForPhase(intent.Workload)
	concurrency := min(maxProfileConcurrent, max(1, intent.Workload.Concurrency))
	load := benchmark.Config{
		Binary: t.AIPerf, Endpoint: instance.baseURL, Model: intent.Model.Repository, Tokenizer: t.snapshotPath(intent.Model),
		Requests: timelineLoadRequests, Concurrency: concurrency, InputTokens: inputTokens, OutputTokens: outputTokens, WarmupRequests: max(2, concurrency),
	}
	loadContext, stopLoad := context.WithCancel(ctx)
	defer stopLoad()
	loadDone := make(chan error, 1)
	go func() {
		_, err := t.Load.Run(loadContext, load)
		loadDone <- err
	}()
	if err := sleepContext(ctx, warmup); err != nil {
		return 0, nil, err
	}
	select {
	case err := <-loadDone:
		return 0, nil, fmt.Errorf("timeline load ended before the capture window opened: %v", err)
	default:
	}
	body := spec.Timeline.ProfileRequest
	if len(body) == 0 {
		body = json.RawMessage(`{}`)
	}
	if _, err := instance.post(ctx, "/start_profile", body); err != nil {
		return 0, nil, err
	}
	opened := t.now()
	if err := sleepContext(ctx, window); err != nil {
		return 0, nil, err
	}
	_, stopErr := instance.post(ctx, "/stop_profile", nil)
	measured := t.now().Sub(opened)
	stopLoad()
	<-loadDone
	if measured <= 0 {
		measured = window
	}
	return measured, stopErr, nil
}

// nsysVersion proves the runtime image ships nsys before any serving
// container starts, so a missing tool fails closed with its reason.
func (t Target) nsysVersion(ctx context.Context, image, nsys string) (string, error) {
	output, err := t.docker(ctx, "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--entrypoint", nsys, image, "--version")
	if err != nil {
		return "", fmt.Errorf("Nsight Systems CLI %s is not usable in runtime image %s; use an image that includes nsys: %w", nsys, image, err)
	}
	version := strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
	if version == "" {
		return "", fmt.Errorf("Nsight Systems CLI %s in runtime image %s reported no version", nsys, image)
	}
	if len(version) > timelineVersionLimit {
		version = version[:timelineVersionLimit]
	}
	return version, nil
}

func (t Target) waitForReport(ctx context.Context, path string) error {
	timeout := t.ReportTimeout
	if timeout <= 0 {
		timeout = defaultReportTimeout
	}
	deadline := time.Now().Add(timeout)
	previous := int64(-1)
	for {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
			// The report is complete once its size stops changing.
			if info.Size() == previous {
				return nil
			}
			previous = info.Size()
		}
		if time.Now().After(deadline) {
			return errors.New("Nsight Systems did not write a timeline report; check that the runtime calls cudaProfilerStart and cudaProfilerStop from /start_profile and /stop_profile")
		}
		if err := sleepContext(ctx, t.poll()); err != nil {
			return err
		}
	}
}

// exportStats runs nsys stats in the same pinned image, offline and without
// capabilities, writing one CSV per report next to the timeline.
func (t Target) exportStats(ctx context.Context, image, nsys, profileDir string) error {
	args := []string{
		"run", "--rm", "--network", "none", "--read-only", "--tmpfs", "/tmp:rw,nosuid,size=2g", "--cap-drop", "ALL",
		"--security-opt", "no-new-privileges", "--env", "HOME=/tmp", "--env", "TMPDIR=/tmp", "--entrypoint", nsys,
		"--volume", profileDir + ":" + containerProfileDir, image,
		"stats", "--report", strings.Join([]string{timelineKernelReport, timelineAPIReport, timelineMemoryReport}, ","),
		"--format", "csv", "--force-export=true", "--force-overwrite=true",
		"--output", containerProfileDir + "/" + timelineStatsBase, containerProfileDir + "/" + timelineReportFilename,
	}
	if _, err := t.docker(ctx, args...); err != nil {
		return fmt.Errorf("export timeline statistics: %w", err)
	}
	return nil
}

// summarizeNsysStats turns the CSV exports of one capture into a bounded
// summary. The kernel report is required; the API and memory reports are
// optional because nsys skips a report with no data, and their absence leaves
// the dependent fields unknown.
func summarizeNsysStats(directory string, window time.Duration, accelerators int) (TimelineSummary, error) {
	if window <= 0 {
		return TimelineSummary{}, errors.New("timeline window must be positive")
	}
	if accelerators < 1 {
		accelerators = 1
	}
	kernels, err := readStatsReport(directory, timelineKernelReport, true)
	if err != nil {
		return TimelineSummary{}, err
	}
	if len(kernels) == 0 {
		return TimelineSummary{}, errors.New("timeline capture contains no GPU kernels; the CUDA profiler range may not have opened")
	}
	summary := TimelineSummary{CaptureWindowMS: round3(float64(window) / float64(time.Millisecond)), Accelerators: accelerators, TopKernels: []TimelineKernel{}}
	var communication float64
	for _, row := range kernels {
		summary.KernelTimeMS += row.totalMS
		if isCommunicationKernel(row.name) {
			communication += row.totalMS
		}
	}
	if summary.KernelTimeMS <= 0 {
		return TimelineSummary{}, errors.New("timeline capture contains no measurable GPU kernel time")
	}
	summary.CommunicationKernelShare = round6(communication / summary.KernelTimeMS)
	sort.SliceStable(kernels, func(i, j int) bool {
		if kernels[i].totalMS != kernels[j].totalMS {
			return kernels[i].totalMS > kernels[j].totalMS
		}
		return kernels[i].name < kernels[j].name
	})
	for _, row := range kernels[:min(maxTimelineTopKernels, len(kernels))] {
		name := row.name
		if len(name) > maxTimelineKernelName {
			name = name[:maxTimelineKernelName]
		}
		summary.TopKernels = append(summary.TopKernels, TimelineKernel{
			Name: name, TotalTimeMS: round3(row.totalMS), Instances: row.count,
			KernelTimeShare: round6(row.totalMS / summary.KernelTimeMS), CommunicationKind: isCommunicationKernel(row.name),
		})
	}
	capacity := summary.CaptureWindowMS * float64(accelerators)
	if memory, err := readStatsReport(directory, timelineMemoryReport, false); err != nil {
		return TimelineSummary{}, err
	} else if memory != nil {
		var total float64
		for _, row := range memory {
			total += row.totalMS
		}
		idle := math.Max(0, math.Min(1, 1-(summary.KernelTimeMS+total)/capacity))
		summary.MemoryOperationTimeMS, summary.GPUIdleFraction = floatPointer(round3(total)), floatPointer(round6(idle))
	}
	if api, err := readStatsReport(directory, timelineAPIReport, false); err != nil {
		return TimelineSummary{}, err
	} else if api != nil {
		var launch float64
		for _, row := range api {
			if _, ok := launchAPIs[strings.ToLower(strings.TrimSpace(strings.SplitN(row.name, "_v", 2)[0]))]; ok {
				launch += row.totalMS
			}
		}
		summary.KernelLaunchTimeMS = floatPointer(round3(launch))
		summary.KernelLaunchOverheadShare = floatPointer(round6(math.Min(1, launch/capacity)))
	}
	summary.KernelTimeMS = round3(summary.KernelTimeMS)
	return summary, nil
}

type statsRow struct {
	name    string
	totalMS float64
	count   int
}

// readStatsReport parses one nsys stats CSV. Column headers are matched by
// their normalized name, so unit suffixes and spacing changes between nsys
// releases do not break parsing; the time unit is read from the header.
func readStatsReport(directory, report string, required bool) ([]statsRow, error) {
	path := filepath.Join(directory, timelineStatsBase+"_"+report+".csv")
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && !required {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s export: %w", report, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxStatsFileBytes {
		return nil, fmt.Errorf("%s export exceeds the size limit", report)
	}
	reader := csv.NewReader(io.LimitReader(file, maxStatsFileBytes))
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read %s header: %w", report, err)
	}
	totalColumn, countColumn, nameColumn, scale := -1, -1, -1, 0.0
	for index, raw := range header {
		key, unit := normalizeHeader(raw)
		switch key {
		case "totaltime":
			totalColumn = index
			scale = unitToMilliseconds(unit)
		case "instances", "numcalls", "count":
			countColumn = index
		case "name", "operation":
			nameColumn = index
		}
	}
	if totalColumn < 0 || nameColumn < 0 || scale == 0 {
		return nil, fmt.Errorf("%s export has no recognized Total Time and Name columns", report)
	}
	rows := []statsRow{}
	for {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read %s row: %w", report, readErr)
		}
		if len(rows) >= maxStatsRows {
			return nil, fmt.Errorf("%s export has more than %d rows", report, maxStatsRows)
		}
		if totalColumn >= len(record) || nameColumn >= len(record) {
			return nil, fmt.Errorf("%s export has a short row", report)
		}
		total, parseErr := strconv.ParseFloat(strings.TrimSpace(record[totalColumn]), 64)
		if parseErr != nil || total < 0 || math.IsNaN(total) || math.IsInf(total, 0) {
			return nil, fmt.Errorf("%s export has an invalid total time %q", report, record[totalColumn])
		}
		row := statsRow{name: strings.TrimSpace(record[nameColumn]), totalMS: total * scale}
		if countColumn >= 0 && countColumn < len(record) {
			if count, countErr := strconv.Atoi(strings.TrimSpace(record[countColumn])); countErr == nil && count >= 0 {
				row.count = count
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func normalizeHeader(raw string) (string, string) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	unit := ""
	if open := strings.IndexByte(raw, '('); open >= 0 {
		if end := strings.IndexByte(raw[open:], ')'); end > 0 {
			unit = strings.TrimSpace(raw[open+1 : open+end])
		}
		raw = raw[:open]
	}
	return strings.NewReplacer(" ", "", "_", "").Replace(raw), unit
}

func unitToMilliseconds(unit string) float64 {
	switch unit {
	case "", "ns":
		return 1e-6
	case "us", "μs":
		return 1e-3
	case "ms":
		return 1
	case "s", "sec":
		return 1000
	default:
		return 0
	}
}

func isCommunicationKernel(name string) bool {
	lowered := strings.ToLower(name)
	for _, marker := range communicationMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (t Target) timelineWindow() time.Duration {
	if t.TimelineWindow <= 0 {
		return defaultTimelineWindow
	}
	return t.TimelineWindow
}

func (t Target) timelineWarmup() time.Duration {
	if t.TimelineWarmup < 0 {
		return 0
	}
	if t.TimelineWarmup == 0 {
		return defaultTimelineWarmup
	}
	return t.TimelineWarmup
}

func floatPointer(value float64) *float64 { return &value }

func round3(value float64) float64 { return math.Round(value*1e3) / 1e3 }

func round6(value float64) float64 { return math.Round(value*1e6) / 1e6 }
