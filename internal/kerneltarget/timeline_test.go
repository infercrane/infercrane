package kerneltarget

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/benchmark"
)

const fakeNsys = "/opt/nvidia/nsight-systems/bin/nsys"

// runNsys fakes the two one-shot nsys containers: the version preflight and
// the offline stats export, which copies the checked-in CSV fixtures.
func (h *fakeHost) runNsys(args []string) ([]byte, error) {
	joined := strings.Join(args, " ")
	if h.nsysMissing {
		return nil, errors.New("exec: \"" + fakeNsys + "\": stat: no such file or directory")
	}
	if strings.Contains(joined, "--version") {
		return []byte("NVIDIA Nsight Systems version 2025.3.1.90-253135822126v0\n"), nil
	}
	if !strings.Contains(joined, " stats ") || !strings.Contains(joined, "--network none") || !strings.Contains(joined, "--cap-drop ALL") {
		return nil, errors.New("unexpected nsys invocation: " + joined)
	}
	volume := ""
	for index := 0; index+1 < len(args); index++ {
		if args[index] == "--volume" && strings.HasSuffix(args[index+1], ":/profiles") {
			volume = strings.TrimSuffix(args[index+1], ":/profiles")
		}
	}
	if _, err := os.Stat(filepath.Join(volume, timelineReportFilename)); err != nil {
		return nil, err
	}
	for _, report := range []string{timelineKernelReport, timelineAPIReport, timelineMemoryReport} {
		body, err := os.ReadFile(filepath.Join("testdata", "nsys", "stats_"+report+".csv"))
		if err != nil {
			return nil, err
		}
		if err = os.WriteFile(filepath.Join(volume, "stats_"+report+".csv"), body, 0o600); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// blockingLoad keeps traffic flowing until the capture window closes.
type blockingLoad struct{}

func (blockingLoad) Run(ctx context.Context, _ benchmark.Config) (benchmark.Result, error) {
	<-ctx.Done()
	return benchmark.Result{}, ctx.Err()
}

func timelineFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.target.Runtimes[0].Timeline = &TimelineSpec{
		NSys: fakeNsys, ProfilerArgs: []string{"--profiler-config.profiler=cuda"},
		Env: []string{"VLLM_WORKER_MULTIPROC_METHOD=spawn"},
	}
	f.target.Load = blockingLoad{}
	f.target.TimelineWindow, f.target.TimelineWarmup, f.target.ReportTimeout = 20*time.Millisecond, time.Millisecond, time.Second
	return f
}

func TestSummarizeNsysStatsFromCheckedInExports(t *testing.T) {
	summary, err := summarizeNsysStats(filepath.Join("testdata", "nsys"), 20*time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CaptureWindowMS != 20000 || summary.Accelerators != 2 || summary.KernelTimeMS != 21000 {
		t.Fatalf("summary=%+v", summary)
	}
	// 21 s of kernels plus 1 s of memory work over 2 GPUs * 20 s.
	if summary.MemoryOperationTimeMS == nil || *summary.MemoryOperationTimeMS != 1000 || summary.GPUIdleFraction == nil || *summary.GPUIdleFraction != .45 {
		t.Fatalf("idle=%v memory=%v", summary.GPUIdleFraction, summary.MemoryOperationTimeMS)
	}
	// cudaLaunchKernel + cudaGraphLaunch + cuLaunchKernel; synchronize and memcpy calls are not launches.
	if summary.KernelLaunchTimeMS == nil || *summary.KernelLaunchTimeMS != 4600 || *summary.KernelLaunchOverheadShare != .115 {
		t.Fatalf("launch=%v share=%v", summary.KernelLaunchTimeMS, summary.KernelLaunchOverheadShare)
	}
	// NCCL all-reduce plus vLLM's custom cross-device reduce.
	if math.Abs(summary.CommunicationKernelShare-5.0/21) > 1e-6 {
		t.Fatalf("communication share=%v", summary.CommunicationKernelShare)
	}
	if len(summary.TopKernels) != 7 || !strings.HasPrefix(summary.TopKernels[0].Name, "void flash_fwd_splitkv_kernel") || summary.TopKernels[0].Instances != 120000 || summary.TopKernels[0].KernelTimeShare != .333333 {
		t.Fatalf("top kernels=%+v", summary.TopKernels)
	}
	if !summary.TopKernels[2].CommunicationKind || summary.TopKernels[1].CommunicationKind {
		t.Fatalf("communication flags=%+v", summary.TopKernels[:3])
	}
	for _, kernel := range summary.TopKernels {
		if len(kernel.Name) > maxTimelineKernelName {
			t.Fatalf("kernel name not bounded: %d", len(kernel.Name))
		}
	}
	encoded, err := json.Marshal(summary)
	if err != nil || len(encoded) > 8<<10 {
		t.Fatalf("summary is not bounded: %d bytes err=%v", len(encoded), err)
	}
}

func TestSummarizeNsysStatsKeepsMissingReportsUnknown(t *testing.T) {
	directory := t.TempDir()
	write := func(report, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(directory, "stats_"+report+".csv"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := summarizeNsysStats(directory, time.Second, 1); err == nil {
		t.Fatal("missing kernel export accepted")
	}
	write(timelineKernelReport, "Time (%),Total Time (ns),Instances,Name\n")
	if _, err := summarizeNsysStats(directory, time.Second, 1); err == nil || !strings.Contains(err.Error(), "no GPU kernels") {
		t.Fatalf("empty kernel export err=%v", err)
	}
	// Older and newer header spellings, with the unit read from the header.
	write(timelineKernelReport, "Time(%),Total Time (us),Instances,Name\n100,500000,10,gemm\n")
	summary, err := summarizeNsysStats(directory, time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if summary.KernelTimeMS != 500 || summary.GPUIdleFraction != nil || summary.KernelLaunchOverheadShare != nil || summary.MemoryOperationTimeMS != nil {
		t.Fatalf("absent reports must stay unknown: %+v", summary)
	}
	for name, body := range map[string]string{
		"unknown unit":  "Total Time (fortnights),Instances,Name\n1,1,gemm\n",
		"no name":       "Total Time (ns),Instances\n1,1\n",
		"negative time": "Total Time (ns),Instances,Name\n-1,1,gemm\n",
		"not a number":  "Total Time (ns),Instances,Name\nfast,1,gemm\n",
	} {
		write(timelineKernelReport, body)
		if _, err = summarizeNsysStats(directory, time.Second, 1); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestCaptureTimelineWrapsTheServerAndSummarizesOffline(t *testing.T) {
	f := timelineFixture(t)
	evidence, err := f.target.CaptureTimeline(context.Background(), profileIntent(f))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.SchemaVersion != TimelineSchemaV1 || evidence.Tool != timelineTool || !strings.Contains(evidence.ToolVersion, "2025.3.1") || !digestPattern.MatchString(evidence.ArtifactDigest) || evidence.ContentRecorded {
		t.Fatalf("evidence=%+v", evidence)
	}
	if evidence.Summary.KernelTimeMS != 21000 || evidence.Summary.CaptureWindowMS <= 0 || evidence.RuntimeImageDigest != digestOf("1") || evidence.WorkloadDigest != digestOf("2") {
		t.Fatalf("summary=%+v", evidence.Summary)
	}
	var serving []string
	for _, call := range f.host.calls {
		if call[0] == "docker" && call[1] == "run" && flagValue(call[1:], "--name", "") != "" && !strings.Contains(strings.Join(call, " "), "snapshot_download") {
			serving = call
		}
	}
	joined := strings.Join(serving, " ")
	for _, want := range []string{"--entrypoint " + fakeNsys, "--cap-drop ALL", "--read-only", "profile --trace=cuda,nvtx --sample=none --cpuctxsw=none", "--capture-range=cudaProfilerApi", "--capture-range-end=stop-shutdown", "-- vllm serve acme/novel", "--profiler-config.profiler=cuda", "VLLM_WORKER_MULTIPROC_METHOD=spawn"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("serving command lacks %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "VLLM_TORCH_PROFILER_DIR") {
		t.Fatalf("timeline run kept the torch profiler: %s", joined)
	}
	if len(f.host.profileBodies) != 1 || f.host.profileBodies[0] != "{}" || f.host.profiled != 1 {
		t.Fatalf("profile range bodies=%v stops=%d", f.host.profileBodies, f.host.profiled)
	}

	// SGLang opens the range with a request body instead of launch flags.
	f = timelineFixture(t)
	f.target.Runtimes[0].Timeline.ProfilerArgs, f.target.Runtimes[0].Timeline.ProfileRequest = nil, json.RawMessage(`{"activities":["CUDA_PROFILER"]}`)
	if _, err = f.target.CaptureTimeline(context.Background(), profileIntent(f)); err != nil {
		t.Fatal(err)
	}
	if f.host.profileBodies[0] != `{"activities":["CUDA_PROFILER"]}` {
		t.Fatalf("start_profile body=%s", f.host.profileBodies[0])
	}
}

func TestCaptureTimelineToleratesShutdownDuringStop(t *testing.T) {
	// nsys terminates the server when the range closes, so /stop_profile may
	// fail after the report is already being written.
	f := timelineFixture(t)
	f.host.stopFails = true
	evidence, err := f.target.CaptureTimeline(context.Background(), profileIntent(f))
	if err != nil || evidence.Summary.KernelTimeMS != 21000 {
		t.Fatalf("evidence=%+v err=%v", evidence.Summary, err)
	}
}

func TestCaptureTimelineFailsClosed(t *testing.T) {
	servingStarted := func(f *fixture) bool {
		for _, call := range f.host.calls {
			if call[0] == "docker" && call[1] == "run" && flagValue(call[1:], "--name", "") != "" && !strings.Contains(strings.Join(call, " "), "snapshot_download") {
				return true
			}
		}
		return false
	}
	f := newFixture(t)
	if _, err := f.target.CaptureTimeline(context.Background(), profileIntent(f)); err == nil || !strings.Contains(err.Error(), "opt-in") || len(f.host.calls) != 0 {
		t.Fatalf("runtime without a timeline config err=%v calls=%d", err, len(f.host.calls))
	}
	f = timelineFixture(t)
	f.host.nsysMissing = true
	if _, err := f.target.CaptureTimeline(context.Background(), profileIntent(f)); err == nil || !strings.Contains(err.Error(), "use an image that includes nsys") || servingStarted(f) {
		t.Fatalf("missing nsys err=%v started=%v", err, servingStarted(f))
	}
	f = timelineFixture(t)
	f.target.Load = loadAdapter{f.host}
	if _, err := f.target.CaptureTimeline(context.Background(), profileIntent(f)); err == nil || !strings.Contains(err.Error(), "load ended before") {
		t.Fatalf("finished load err=%v", err)
	}
	f = timelineFixture(t)
	f.host.noTrace = true
	f.target.ReportTimeout = 30 * time.Millisecond
	if _, err := f.target.CaptureTimeline(context.Background(), profileIntent(f)); err == nil || !strings.Contains(err.Error(), "did not write a timeline report") {
		t.Fatalf("missing report err=%v", err)
	}
	f = timelineFixture(t)
	f.host.noTrace = true
	f.host.stopFails = true
	f.target.ReportTimeout = 30 * time.Millisecond
	if _, err := f.target.CaptureTimeline(context.Background(), profileIntent(f)); err == nil || !strings.Contains(err.Error(), "stop_profile also failed") {
		t.Fatalf("failed stop without a report err=%v", err)
	}
	f = timelineFixture(t)
	f.target.TimelineWindow = 3 * time.Minute
	if _, err := f.target.CaptureTimeline(context.Background(), profileIntent(f)); err == nil || servingStarted(f) {
		t.Fatalf("unbounded window err=%v", err)
	}
}

func TestRuntimeCatalogValidatesTimelineConfiguration(t *testing.T) {
	for name, spec := range map[string]TimelineSpec{
		"relative nsys":   {NSys: "nsys"},
		"unclean nsys":    {NSys: "/opt/../nsys"},
		"shell in env":    {NSys: fakeNsys, Env: []string{"X=$(id)"}},
		"lowercase key":   {NSys: fakeNsys, Env: []string{"path=/tmp"}},
		"invalid request": {NSys: fakeNsys, ProfileRequest: json.RawMessage(`{`)},
	} {
		if err := validateTimeline(&spec); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if err := validateTimeline(&TimelineSpec{NSys: fakeNsys, Env: []string{"VLLM_WORKER_MULTIPROC_METHOD=spawn"}}); err != nil {
		t.Fatal(err)
	}
}
