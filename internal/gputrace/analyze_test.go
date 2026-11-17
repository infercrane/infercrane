package gputrace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/testtools/tracefixture"
)

// kinetoJSON builds a minimal Kineto trace from events with times in µs.
func kinetoJSON(t *testing.T, device string, events ...map[string]any) []byte {
	t.Helper()
	document := map[string]any{"schemaVersion": 1, "baseTimeNanoseconds": tracefixture.SampleBase.UnixNano(), "traceEvents": events}
	if device != "" {
		document["deviceProperties"] = []map[string]any{{"id": 0, "name": device}}
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func kernelEvent(name string, start, duration float64, stream, correlation int) map[string]any {
	return map[string]any{"ph": "X", "cat": "kernel", "name": name, "pid": 0, "tid": stream, "ts": start, "dur": duration, "args": map[string]any{"device": 0, "stream": stream, "correlation": correlation}}
}

func launchEvent(name string, start, duration float64, correlation int) map[string]any {
	return map[string]any{"ph": "X", "cat": "cuda_runtime", "name": name, "pid": 1, "tid": 1, "ts": start, "dur": duration, "args": map[string]any{"correlation": correlation}}
}

func opEvent(name string, start, duration float64, args map[string]any) map[string]any {
	if args == nil {
		args = map[string]any{}
	}
	return map[string]any{"ph": "X", "cat": "cpu_op", "name": name, "pid": 1, "tid": 1, "ts": start, "dur": duration, "args": args}
}

func analyze(t *testing.T, data []byte, options Options) gpuprofile.Analysis {
	t.Helper()
	trace, err := ParseKineto(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := Analyze(trace, options)
	if err != nil {
		t.Fatal(err)
	}
	return analysis
}

func near(t *testing.T, label string, got, want float64) {
	t.Helper()
	if diff := got - want; diff > 1e-3 || diff < -1e-3 {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

// Two kernels with a 100 µs gap: the second was launched only after the
// first ended, so the whole gap is host wait.
func TestIdleGapIsHostWaitWhenTheLaunchCameLate(t *testing.T) {
	analysis := analyze(t, kinetoJSON(t, "",
		launchEvent("cudaLaunchKernel", 0, 5, 1), kernelEvent("gemm_a", 10, 100, 7, 1),
		launchEvent("cudaLaunchKernel", 150, 5, 2), kernelEvent("gemm_b", 210, 100, 7, 2),
	), Options{})
	// Window 10..310 µs, busy 200 µs.
	near(t, "window", analysis.Trace.WindowMS, 0.3)
	near(t, "busy", analysis.Utilization.BusyShare, 0.6667)
	near(t, "idle", analysis.Utilization.IdleShare, 0.3333)
	near(t, "host wait", *analysis.Idle.HostWaitShare, 1)
	near(t, "bubble", *analysis.Idle.Bubbles.MaxUS, 100)
	if analysis.Idle.Bubbles.Top[0].Cause != "host_wait" || analysis.Verdict.Bottleneck != gpuprofile.BottleneckHostLaunch {
		t.Fatalf("idle cause %q, verdict %q", analysis.Idle.Bubbles.Top[0].Cause, analysis.Verdict.Bottleneck)
	}
	if !strings.Contains(analysis.Verdict.Sentence, "33%") || !strings.Contains(analysis.Verdict.Evidence, "100% of idle waiting on launches") {
		t.Fatalf("verdict does not cite the measured values: %+v", analysis.Verdict)
	}
	near(t, "launch delay p50", *analysis.Launch.LaunchDelayP50US, 10)
}

// Work queued ahead: a 20 µs gap is launch latency, a 50 µs gap is other.
func TestQueuedGapsAreLaunchLatencyOrOther(t *testing.T) {
	analysis := analyze(t, kinetoJSON(t, "",
		launchEvent("cudaLaunchKernel", 0, 5, 1), launchEvent("cudaLaunchKernel", 6, 5, 2), launchEvent("cudaLaunchKernel", 12, 5, 3),
		kernelEvent("k1", 20, 100, 7, 1), kernelEvent("k2", 140, 100, 7, 2), kernelEvent("k3", 290, 100, 7, 3),
	), Options{})
	// Gaps: 20 µs (launch_gap) and 50 µs (other); idle 70 µs.
	near(t, "launch gap", *analysis.Idle.LaunchGapShare, 20.0/70)
	near(t, "other", *analysis.Idle.OtherShare, 50.0/70)
	near(t, "host wait", *analysis.Idle.HostWaitShare, 0)
	// Every kernel ran longer than its launch call.
	near(t, "short", *analysis.Launch.ShortKernelShare, 0)
}

func TestNoLaunchCallsLeavesIdleCausesUnknown(t *testing.T) {
	analysis := analyze(t, kinetoJSON(t, "", kernelEvent("k1", 0, 50, 7, 0), kernelEvent("k2", 100, 50, 7, 0)), Options{})
	if analysis.Idle.HostWaitShare != nil || analysis.Idle.Reason == "" || analysis.Launch.LaunchDelayP50US != nil || analysis.Launch.Reason == "" {
		t.Fatalf("idle causes and launch delay must stay unknown with a reason: %+v %+v", analysis.Idle, analysis.Launch)
	}
	if analysis.Trace.LaunchesRecorded {
		t.Fatal("launches_recorded must be false")
	}
	if analysis.Verdict.Bottleneck != gpuprofile.BottleneckUnknown || !strings.Contains(analysis.Verdict.Sentence, "no host launch calls") {
		t.Fatalf("verdict %+v", analysis.Verdict)
	}
}

// Communication on its own stream overlapping compute for 30 of 80 µs.
func TestCommunicationOverlapIsTheShareUnderCompute(t *testing.T) {
	analysis := analyze(t, kinetoJSON(t, "",
		kernelEvent("sm90_xmma_gemm", 0, 100, 7, 0),
		kernelEvent("ncclDevKernel_AllReduce_Sum_bf16_RING_LL", 70, 80, 21, 0),
		kernelEvent("rms_norm_kernel", 150, 50, 7, 0),
	), Options{})
	near(t, "communication ms", analysis.Overlap.CommunicationMS, 0.08)
	near(t, "overlapped", *analysis.Overlap.OverlappedShare, 30.0/80)
	// Window 200 µs; compute union 150 µs; busy 200 µs.
	near(t, "compute", analysis.Utilization.ComputeShare, 0.75)
	near(t, "non-compute", analysis.Utilization.NonComputeShare, 0.25)
	// Exposed communication: 50 µs of 200 µs = 25% ≥ 10%.
	if analysis.Verdict.Bottleneck != gpuprofile.BottleneckCommunication {
		t.Fatalf("verdict %+v", analysis.Verdict)
	}
}

func TestKernelCategoriesAndTopKernels(t *testing.T) {
	analysis := analyze(t, kinetoJSON(t, "",
		kernelEvent("void flashinfer::BatchPrefillWithPagedKVCacheKernel<128u>", 0, 40, 7, 0),
		kernelEvent("ampere_bf16_s16816gemm_bf16_128x128", 40, 50, 7, 0),
		kernelEvent("ampere_bf16_s16816gemm_bf16_128x128", 90, 30, 7, 0),
		kernelEvent("vllm::act_and_mul_kernel", 120, 10, 7, 0),
		map[string]any{"ph": "X", "cat": "gpu_memcpy", "name": "Memcpy DtoD (Device -> Device)", "pid": 0, "tid": 7, "ts": 130, "dur": 20, "args": map[string]any{"device": 0, "stream": 7}},
	), Options{})
	want := map[string]float64{"gemm": 80.0 / 150, "attention": 40.0 / 150, "memory": 20.0 / 150, "activation": 10.0 / 150}
	if len(analysis.Categories) != len(want) {
		t.Fatalf("categories %+v", analysis.Categories)
	}
	for _, category := range analysis.Categories {
		near(t, category.Category, category.Share, want[category.Category])
	}
	top := analysis.TopKernels[0]
	if top.Category != "gemm" || top.Count != 2 || top.TotalMS != 0.08 || top.MeanUS != 40 || top.MaxUS != 50 {
		t.Fatalf("top kernel %+v", top)
	}
	if phase := analysis.TopKernels[1].Phase; phase == nil || *phase != gpuprofile.PhasePrefill {
		t.Fatalf("a prefill attention kernel names its phase: %+v", analysis.TopKernels[1])
	}
}

// Without step annotations, idle gaps of 100 µs or more separate steps.
func TestStepsFromIdleGaps(t *testing.T) {
	analysis := analyze(t, kinetoJSON(t, "",
		kernelEvent("BatchDecodeWithPagedKVCacheKernel", 0, 400, 7, 0),
		kernelEvent("gemm", 450, 100, 7, 0), // 50 µs gap stays inside the step
		kernelEvent("BatchDecodeWithPagedKVCacheKernel", 700, 300, 7, 0),
		kernelEvent("BatchPrefillWithPagedKVCacheKernel", 1200, 600, 7, 0),
	), Options{})
	steps := analysis.Steps
	if steps.Source != "idle_gaps" || steps.Count != 3 || len(steps.Items) != 3 {
		t.Fatalf("steps %+v", steps)
	}
	near(t, "first step", steps.Items[0].DurationMS, 0.55)
	if steps.Items[0].Phase != gpuprofile.PhaseDecode || steps.Items[2].Phase != gpuprofile.PhasePrefill {
		t.Fatalf("step phases %+v", steps.Items)
	}
	near(t, "decode share", analysis.Phases.Decode.Share, 800.0/1400)
	near(t, "prefill share", analysis.Phases.Prefill.Share, 600.0/1400)
	if analysis.Phases.Source != "kernel_names" {
		t.Fatalf("phase source %q", analysis.Phases.Source)
	}
}

func TestStepsFromProfilerStepAnnotations(t *testing.T) {
	annotation := func(index int, start, duration float64) map[string]any {
		return map[string]any{"ph": "X", "cat": "user_annotation", "name": fmt.Sprintf("ProfilerStep#%d", index), "pid": 1, "tid": 1, "ts": start, "dur": duration, "args": map[string]any{}}
	}
	analysis := analyze(t, kinetoJSON(t, "",
		annotation(0, 0, 100), launchEvent("cudaLaunchKernel", 10, 5, 1), kernelEvent("BatchDecodeWithPagedKVCacheKernel", 20, 150, 7, 1),
		annotation(1, 200, 100), launchEvent("cudaLaunchKernel", 210, 5, 2), kernelEvent("BatchDecodeWithPagedKVCacheKernel", 220, 50, 7, 2),
	), Options{})
	steps := analysis.Steps
	if steps.Source != "annotations" || steps.Marker == nil || *steps.Marker != "ProfilerStep#0" || steps.Count != 2 {
		t.Fatalf("steps %+v", steps)
	}
	// A step lasts until its last kernel ends: 0..170 µs and 200..300 µs.
	near(t, "first", steps.Items[0].DurationMS, 0.17)
	near(t, "second", steps.Items[1].DurationMS, 0.1)
}

// A recorded bf16 matrix multiply at 2×K×N FLOPs over its bytes; with the H100
// ridge near 295 FLOP/B, a 16-token GEMM is memory-bound.
func TestRooflinePlacesShapedMatrixMultiplies(t *testing.T) {
	mm := func(m int, start float64, correlation int) []map[string]any {
		args := map[string]any{"Input Dims": [][]int{{m, 4096}, {4096, 4096}}, "Input type": []string{"c10::BFloat16", "c10::BFloat16"}}
		return []map[string]any{opEvent("aten::mm", start, 20, args), launchEvent("cudaLaunchKernel", start+2, 5, correlation), kernelEvent("sm90_xmma_gemm_bf16", start+10, 50, 7, correlation)}
	}
	var events []map[string]any
	events = append(events, mm(16, 0, 1)...)
	events = append(events, mm(16, 100, 2)...)
	events = append(events, mm(4096, 200, 3)...)
	analysis := analyze(t, kinetoJSON(t, "NVIDIA H100 80GB HBM3", events...), Options{})
	roof := analysis.Roofline
	if roof.Peak == nil || roof.Peak.ComputeTFLOPS != 989.5 || roof.Position != "memory_bound" {
		t.Fatalf("roofline %+v", roof)
	}
	if len(roof.Points) != 2 || roof.Points[0].Label != "mm 16×4096×4096 bf16" || roof.Points[0].Bound != "memory" || roof.Points[1].Bound != "compute" {
		t.Fatalf("points %+v", roof.Points)
	}
	// 16×4096×4096: 2·16·4096·4096 FLOPs over (16·4096 + 4096·4096 + 16·4096)·2 bytes.
	flops := 2.0 * 16 * 4096 * 4096
	bytes := (16*4096 + 4096*4096 + 16*4096) * 2.0
	near(t, "intensity", roof.Points[0].Intensity, float64(int64(flops/bytes*100+0.5))/100)
	near(t, "memory-bound share", *roof.MemoryBoundShare, 100.0/150)
	near(t, "covered", *roof.CoveredShare, 1)
}

func TestRooflineUnknownWithoutAPeakOrShapes(t *testing.T) {
	plain := analyze(t, kinetoJSON(t, "NVIDIA B300 Experimental", kernelEvent("gemm", 0, 10, 7, 0)), Options{})
	if plain.Roofline.Position != "unknown" || !strings.Contains(plain.Roofline.Reason, "--peak-tflops") {
		t.Fatalf("an unlisted accelerator names the override: %+v", plain.Roofline)
	}
	override := analyze(t, kinetoJSON(t, "", kernelEvent("gemm", 0, 10, 7, 0)), Options{Peak: &gpuprofile.Peak{ComputeTFLOPS: 100, Precision: "BF16 dense tensor", MemoryTBps: 1, Source: "operator"}})
	if override.Roofline.RidgeFLOPPerByte == nil || *override.Roofline.RidgeFLOPPerByte != 100 || !strings.Contains(override.Roofline.Reason, "recorded shapes") {
		t.Fatalf("an override sets the ridge and still needs shapes: %+v", override.Roofline)
	}
}

func TestLookupPeakMatchesDeviceNames(t *testing.T) {
	cases := map[string]float64{
		"NVIDIA H100 80GB HBM3": 3.35, "NVIDIA H100 PCIe": 2.0, "NVIDIA H200": 4.8, "NVIDIA A100-SXM4-80GB": 2.039,
		"NVIDIA A100-SXM4-40GB": 1.555, "NVIDIA L40S": 0.864, "NVIDIA L4": 0.3, "NVIDIA GeForce RTX 4090": 1.008, "AMD Instinct MI300X": 5.3,
	}
	for name, bandwidth := range cases {
		peak, ok := LookupPeak(name)
		if !ok || peak.MemoryTBps != bandwidth {
			t.Fatalf("%s: %+v %v", name, peak, ok)
		}
	}
	for _, name := range []string{"NVIDIA A10G", "NVIDIA B200", ""} {
		if _, ok := LookupPeak(name); ok {
			t.Fatalf("%q must stay unknown", name)
		}
	}
}

func TestParserIgnoresContentAndFreeTextAnnotations(t *testing.T) {
	trace, err := ParseKineto(bytes.NewReader(tracefixture.VLLMSingleGPU(tracefixture.SampleBase)))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range trace.Names {
		if strings.Contains(name, "SECRET") || strings.Contains(name, ".py(") {
			t.Fatalf("parser kept %q", name)
		}
	}
	recording, err := Profile(trace, Options{}, CaptureFromTrace(trace, "torch-profiler", "vllm-0.22.1", time.Time{}))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(recording)
	if bytes.Contains(encoded, []byte("SECRET")) || recording.Capture.ContentRecorded {
		t.Fatal("a recording must never carry request content")
	}
}

func TestKinetoParserReadsDevicesShapesAndOrigin(t *testing.T) {
	trace, err := ParseKineto(bytes.NewReader(tracefixture.VLLMSingleGPU(tracefixture.SampleBase)))
	if err != nil {
		t.Fatal(err)
	}
	if trace.Accelerator() != "NVIDIA H100 80GB HBM3" || !trace.ShapesRecorded || !trace.CPUOpsRecorded || !trace.OriginKnown {
		t.Fatalf("trace %s shapes=%v ops=%v origin=%v", trace.Accelerator(), trace.ShapesRecorded, trace.CPUOpsRecorded, trace.OriginKnown)
	}
	// The first host call happens at 100 µs after the base time.
	if got := time.Unix(0, trace.OriginNS).UTC(); !got.Equal(tracefixture.SampleBase.Add(101 * time.Microsecond)) {
		t.Fatalf("origin %s", got)
	}
	if _, err = ParseKineto(strings.NewReader(`{"traceEvents": []}`)); err != ErrNoKernels {
		t.Fatalf("an empty trace: %v", err)
	}
	if _, err = ParseKineto(strings.NewReader(`[1,2]`)); err == nil {
		t.Fatal("a list is not a trace")
	}
}

func TestParserMarksTruncatedTraces(t *testing.T) {
	previous := kernelLimit
	kernelLimit = 10
	defer func() { kernelLimit = previous }()
	trace, err := ParseKineto(bytes.NewReader(tracefixture.VLLMSingleGPU(tracefixture.SampleBase)))
	if err != nil {
		t.Fatal(err)
	}
	if !trace.Truncated || len(trace.Kernels) != 10 {
		t.Fatalf("truncated=%v kernels=%d", trace.Truncated, len(trace.Kernels))
	}
	analysis, err := Analyze(trace, Options{})
	if err != nil || !analysis.Trace.Truncated {
		t.Fatalf("analysis must say it is truncated: %v", err)
	}
}

func TestMergedRanksKeepDevicesAndCorrelationsApart(t *testing.T) {
	trace, err := ParseFiles([]string{filepath.Join("testdata", "sglang-tp2")}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Devices) != 2 || trace.Threads != 2 {
		t.Fatalf("devices %+v threads %d", trace.Devices, trace.Threads)
	}
	p := prepare(trace)
	for launch, kernels := range p.launchKernels {
		device := trace.Kernels[kernels[0]].Device
		for _, kernel := range kernels {
			if trace.Kernels[kernel].Device != device {
				t.Fatalf("launch %d links kernels on two ranks", launch)
			}
		}
	}
}

// Every sample becomes a valid v2 recording whose stacks fit the capture and
// whose excerpt fits the analyzed window.
func TestSampleRecordingsAreValidAndBounded(t *testing.T) {
	for _, current := range samples() {
		t.Run(current.name, func(t *testing.T) {
			trace, err := ParseFiles([]string{filepath.Join("testdata", current.name)}, "")
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			capture := Capture{Tool: "torch-profiler", ToolVersion: "unknown", StartedAt: now.Add(-time.Minute), EndedAt: now.Add(-time.Second)}
			if trace.OriginKnown {
				capture = CaptureFromTrace(trace, "torch-profiler", "unknown", now)
				// The samples are dated in the past; move the clock to just after them.
				now = capture.EndedAt.Add(time.Minute)
			}
			recording, err := Profile(trace, current.options, capture)
			if err != nil {
				t.Fatal(err)
			}
			if err = gpuprofile.Validate(recording, now); err != nil {
				t.Fatal(err)
			}
			var stacked int64
			for _, stack := range recording.Stacks {
				stacked += stack.GPUNS
			}
			busy := recording.Analysis.Utilization.BusyShare * recording.Analysis.Trace.WindowMS * 1e6 * float64(recording.Analysis.Trace.Devices)
			if diff := float64(stacked) - busy; diff > 1e4 || diff < -1e4*float64(len(trace.Kernels)) {
				t.Fatalf("stacks hold %d ns, busy time is %.0f ns", stacked, busy)
			}
			if recording.TimelineExcerpt == nil || len(recording.TimelineExcerpt.Events) == 0 {
				t.Fatal("a sample has an excerpt")
			}
			if !contains(recording.UnavailableLayers, gpuprofile.KindSASS) || !contains(recording.UnavailableLayers, gpuprofile.KindStall) {
				t.Fatalf("a trace never records SASS or stall reasons: %v", recording.UnavailableLayers)
			}
			if current.format != FormatKineto && !contains(recording.UnavailableLayers, gpuprofile.KindTorch) {
				t.Fatalf("a CSV trace has no operators: %v", recording.UnavailableLayers)
			}
		})
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestExcerptShrinksToFitTheEventLimit(t *testing.T) {
	trace, err := ParseFiles([]string{filepath.Join("testdata", "nsys-l40s")}, "")
	if err != nil {
		t.Fatal(err)
	}
	p := prepare(trace)
	p.steps()
	full := p.excerpt(p.window.end - p.window.start)
	// The host launched the first kernel before the GPU window opened.
	if full.Truncated || len(full.Events) != len(trace.Kernels)+len(trace.Launches)-1 {
		t.Fatalf("a small trace fits whole: %d events, truncated %v", len(full.Events), full.Truncated)
	}
	if err = gpuprofile.ValidateExcerpt(*full); err != nil {
		t.Fatal(err)
	}
}

func TestDetectRejectsUnknownFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.csv")
	if err := os.WriteFile(path, []byte("a,b,c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFiles([]string{path}, ""); err == nil || !strings.Contains(err.Error(), "is not a Kineto JSON") {
		t.Fatalf("got %v", err)
	}
	if _, err := ParseFiles([]string{path}, "perfetto"); err == nil {
		t.Fatal("an unknown format is refused")
	}
}

func TestCategoryAndPhaseVocabulary(t *testing.T) {
	cases := map[string]string{
		"ncclDevKernel_AllReduce_Sum_bf16_RING_LL":                 "communication",
		"void vllm::cross_device_reduce_1stage<__nv_bfloat16, 2>":  "communication",
		"void flashinfer::BatchDecodeWithPagedKVCacheKernel<128u>": "attention",
		"Cijk_Alik_Bljk_BBS_BH_MT64x64x64_MI16x16x16x1":            "gemm",
		"fused_moe_kernel":                                            "gemm",
		"Memcpy HtoD (Pageable -> Device)":                            "memory",
		"void vllm::rms_norm_kernel<c10::BFloat16, 8, 1>":             "norm",
		"void vllm::act_and_mul_kernel<c10::BFloat16>":                "activation",
		"flashinfer::sampling::TopKTopPSamplingFromProbKernel<1024u>": "sampling",
		"void at::native::vectorized_elementwise_kernel<4>":           "other",
	}
	for name, want := range cases {
		if got := Category(name); got != want {
			t.Fatalf("Category(%q) = %s, want %s", name, got, want)
		}
	}
	if DiagnosisClass("void vllm::rms_norm_kernel") != "other" || DiagnosisClass("nvjet_tst_64x8") != "gemm" {
		t.Fatal("diagnosis folds the finer categories into other")
	}
	phases := map[string]string{
		"BatchPrefillWithPagedKVCacheKernel": "prefill", "BatchDecodeWithPagedKVCacheKernel": "decode",
		"paged_attention_v2_kernel": "decode", "flash_fwd_splitkv_kernel": "", "sm90_xmma_gemm": "",
	}
	for name, want := range phases {
		if got := KernelPhase(name); got != want {
			t.Fatalf("KernelPhase(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestExcerptHalvesItsLengthWhenTooBusy(t *testing.T) {
	trace, err := ParseFiles([]string{filepath.Join("testdata", "vllm-h100")}, "")
	if err != nil {
		t.Fatal(err)
	}
	p := prepare(trace)
	p.steps()
	window := p.window.end - p.window.start
	full := p.excerpt(window)
	busiest := p.busiestStart(window / 8)
	if count := p.eventsIn(busiest, busiest+window/8); count == 0 || count >= len(full.Events) {
		t.Fatalf("an eighth of the window holds %d of %d events", count, len(full.Events))
	}
	if full.DurationMS != ms(window) || full.StartMS != 0 {
		t.Fatalf("a window shorter than a second is excerpted whole: %v + %v", full.StartMS, full.DurationMS)
	}
}
