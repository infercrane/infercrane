package optimizer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/gputrace"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
	"github.com/infercrane/infercrane/internal/testtools/tracefixture"
)

// sampleTraceProfile analyzes a generated sample trace into a stored v2
// capture. The sample is a generator's output, never a measurement; the test
// only checks that the summary carries what the analysis found.
func sampleTraceProfile(t *testing.T, files map[string][]byte, options gputrace.Options) gpuprofile.Profile {
	t.Helper()
	directory := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	trace, err := gputrace.ParseFiles([]string{directory}, "")
	if err != nil {
		t.Fatal(err)
	}
	recording, err := gputrace.Profile(trace, options, gputrace.CaptureFromTrace(trace, "torch-profiler", "sample", tracefixture.SampleBase))
	if err != nil {
		t.Fatal(err)
	}
	profile := gpuprofile.ProfileFromRecording(recording)
	profile.Capture.ID, profile.Revision = "gpc-sample", 4
	return profile
}

func traceDiagnosis(bottleneck string, signals map[string]float64, trace *GPUTraceSummary, findings ...DiagnosisFinding) *Diagnosis {
	diagnosis := testDiagnosis(bottleneck, signals, findings...)
	diagnosis.GPUProfile = &GPUProfileSummary{
		CaptureID: "gpc-1", Tool: "torch-profiler", Revision: 4, CapturedAt: diagnosisWindow.Add(-2 * time.Hour), KernelSeconds: 8.4,
		Shares: map[string]float64{KernelClassAttention: .3, KernelClassGEMM: .45, KernelClassCommunication: 0, KernelClassOther: .25}, Trace: trace,
	}
	return diagnosis
}

func traceSummary(bottleneck string, measurements map[string]float64, findings ...GPUTraceFinding) *GPUTraceSummary {
	if measurements == nil {
		measurements = map[string]float64{}
	}
	return &GPUTraceSummary{AlgorithmVersion: gpuprofile.AnalysisAlgorithm, Bottleneck: bottleneck, Devices: 1, WindowMS: 9876.5, Findings: findings, Measurements: measurements}
}

var (
	idleHostBound   = GPUTraceFinding{Rule: TraceRuleIdleHostBound, Observed: .31, Comparison: ">=", Threshold: .1, Unit: "ratio"}
	launchBound     = GPUTraceFinding{Rule: TraceRuleLaunchBoundKernels, Observed: .64, Comparison: ">=", Threshold: .5, Unit: "ratio"}
	exposedComms    = GPUTraceFinding{Rule: TraceRuleCommunicationExposed, Observed: .18, Comparison: ">=", Threshold: .1, Unit: "ratio"}
	memoryBoundGEMM = GPUTraceFinding{Rule: TraceRuleMemoryBoundGEMM, Observed: .92, Comparison: ">=", Threshold: .6, Unit: "ratio"}
)

func TestSummarizeGPUProfileCarriesTraceFindings(t *testing.T) {
	profile := sampleTraceProfile(t, map[string][]byte{"rank0.pt.trace.json.gz": tracefixture.VLLMSingleGPU(tracefixture.SampleBase)}, gputrace.Options{Engine: "vllm", EngineVersion: "0.22.1"})
	now := profile.Capture.EndedAt.Add(time.Hour)
	summary := SummarizeGPUProfile(profile, now, GPUProfileMaxAge)
	if summary == nil || summary.Trace == nil {
		t.Fatalf("a v2 capture lost its trace analysis: %+v", summary)
	}
	analysis, trace := profile.Analysis, summary.Trace
	if trace.AlgorithmVersion != gpuprofile.AnalysisAlgorithm || trace.Bottleneck != analysis.Verdict.Bottleneck || trace.Devices != analysis.Trace.Devices || len(trace.Findings) != len(analysis.Findings) {
		t.Fatalf("trace=%+v analysis verdict=%+v findings=%d", trace, analysis.Verdict, len(analysis.Findings))
	}
	for index, finding := range analysis.Findings {
		got := trace.Findings[index]
		if got.Rule != finding.Rule || got.Observed != finding.Observed || got.Threshold != finding.Threshold || got.Comparison != finding.Comparison || got.Unit != finding.Unit {
			t.Fatalf("finding %d=%+v, analysis %+v", index, got, finding)
		}
	}
	if _, ok := trace.Measurements[TraceIdleHostWaitShare]; !ok {
		t.Fatalf("a measured idle cause was left out: %v", trace.Measurements)
	}
	if _, ok := trace.Measurements[TraceRooflineCoveredShare]; ok && analysis.Roofline.Position == "unknown" {
		t.Fatal("an unplaced roofline reported a coverage")
	}
	if _, ok := trace.Measurements[TraceOverlappedShare]; ok != (analysis.Overlap.OverlappedShare != nil) {
		t.Fatal("overlap must be carried exactly when the trace measured it")
	}
	diagnosis := testDiagnosis(optimizationreadiness.BottleneckHostScheduler, map[string]float64{optimizationreadiness.SignalRunningPerReplica: 6})
	diagnosis.GPUProfile = summary
	if err := validateDiagnosis(normalizeDiagnosis(*diagnosis)); err != nil {
		t.Fatalf("a summarized trace failed validation: %v", err)
	}

	tp2 := sampleTraceProfile(t, map[string][]byte{"rank0.trace.json.gz": tracefixture.SGLangTP2(tracefixture.SampleBase)[0], "rank1.trace.json.gz": tracefixture.SGLangTP2(tracefixture.SampleBase)[1]}, gputrace.Options{Engine: "sglang"})
	if summary := SummarizeGPUProfile(tp2, tp2.Capture.EndedAt, GPUProfileMaxAge); summary == nil || summary.Trace == nil || summary.Trace.Devices != 2 {
		t.Fatalf("two-GPU sample summary=%+v", summary)
	} else if _, ok := summary.Trace.Measurements[TraceOverlappedShare]; !ok {
		t.Fatal("a measured communication overlap was left out")
	}

	v1 := profile
	v1.SchemaVersion, v1.Analysis, v1.TimelineExcerpt = gpuprofile.SchemaVersion, nil, nil
	plain := SummarizeGPUProfile(v1, now, GPUProfileMaxAge)
	if plain == nil || plain.Trace != nil {
		t.Fatalf("v1 summary=%+v", plain)
	}
	encoded, _ := json.Marshal(plain)
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 6 || strings.Contains(string(encoded), `"trace"`) {
		t.Fatalf("a v1 summary must serialize exactly as before: %s", encoded)
	}
	unreviewed := profile
	changed := *profile.Analysis
	changed.AlgorithmVersion = "gpu-trace-analysis-v9"
	unreviewed.Analysis = &changed
	if summary := SummarizeGPUProfile(unreviewed, now, GPUProfileMaxAge); summary == nil || summary.Trace != nil {
		t.Fatal("an analysis algorithm the planner has not reviewed was summarized")
	}
	if SummarizeGPUProfile(profile, profile.Capture.EndedAt.Add(GPUProfileMaxAge+time.Minute), GPUProfileMaxAge) != nil {
		t.Fatal("a capture older than 7 days was summarized")
	}
}

func TestTraceHostLaunchChoosesOnlyChangesTheDefaultsLeaveRoomFor(t *testing.T) {
	launch := traceSummary(gpuprofile.BottleneckHostLaunch, map[string]float64{TraceIdleHostWaitShare: .71, TraceLaunchDelayP50US: 8.1, TraceGraphLaunchShare: .02}, idleHostBound, launchBound)
	moderate := traceDiagnosis(optimizationreadiness.BottleneckHostScheduler, map[string]float64{optimizationreadiness.SignalRunningPerReplica: 48}, launch)
	defaults, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", moderate))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Diagnosis.State != DiagnosisNoActionableFinding || len(defaults.Candidates) != 1 {
		t.Fatalf("defaults already cover launch overhead, got %v %+v", reasonOrder(defaults), defaults.Diagnosis)
	}
	graphs := plannedTechnique(t, defaults, TechniqueCUDAGraphs)
	if !strings.Contains(graphs.Skipped, "batches of up to 512") || graphs.Priority != 93 || strings.Join(graphs.Rules, ",") != TraceRuleIdleHostBound+","+TraceRuleLaunchBoundKernels {
		t.Fatalf("cuda graphs=%+v", graphs)
	}
	if graphs.Evidence[0].Signal != "gpu_trace_host_wait_share" || graphs.Evidence[0].Threshold == nil || *graphs.Evidence[0].Threshold != .1 {
		t.Fatalf("evidence must cite the trace's own value and threshold: %+v", graphs.Evidence)
	}
	if got := plannedTechnique(t, defaults, TechniqueAsyncScheduling).Skipped; !strings.Contains(got, "on by default") {
		t.Fatalf("async scheduling skipped=%q", got)
	}

	busy := traceDiagnosis(optimizationreadiness.BottleneckHostScheduler, map[string]float64{optimizationreadiness.SignalRunningPerReplica: 700}, launch)
	larger, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", busy))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(reasonOrder(larger), ","); got != "control,cuda-graphs" {
		t.Fatalf("order=%s", got)
	}
	if args := strings.Join(larger.Candidates[1].Deployment.Runtime.Args, " "); !strings.HasSuffix(args, "--max-cudagraph-capture-size 880") {
		t.Fatalf("capture args=%q", args)
	}
	if err = ValidateProposal(larger); err != nil {
		t.Fatal(err)
	}

	small, err := catalogSource(t).Propose(context.Background(), genericRequest("RTX4090", traceDiagnosis(optimizationreadiness.BottleneckHostScheduler, map[string]float64{optimizationreadiness.SignalRunningPerReplica: 200}, launch)))
	if err != nil {
		t.Fatal(err)
	}
	if got := plannedTechnique(t, small, TechniqueCUDAGraphs).Skipped; !strings.Contains(got, "batches of up to 512") || len(small.Candidates) != 1 {
		t.Fatalf("RTX 4090 skipped=%q order=%v", got, reasonOrder(small))
	}

	control := Candidate{ID: strings.Repeat("c", 64)}
	control.Deployment.Runtime.Engine, control.Deployment.Runtime.Version = "vllm", "0.22.1"
	for name, test := range map[string]struct {
		engine, version string
		args            []string
		technique       string
		want, why       string
	}{
		"eager turned back on":        {"vllm", "0.22.1", []string{"--enforce-eager", "--max-num-seqs", "64"}, TechniqueCUDAGraphs, "--max-num-seqs 64", ""},
		"sglang graphs back on":       {"sglang", "0.5.12", []string{"--disable-cuda-graph"}, TechniqueCUDAGraphs, "", ""},
		"sglang sized default":        {"sglang", "0.5.12", nil, TechniqueCUDAGraphs, "", "sizes --cuda-graph-max-bs from GPU memory"},
		"unreviewed vllm":             {"vllm", "0.99.0", nil, TechniqueCUDAGraphs, "", "has not recorded vLLM 0.99.0's defaults"},
		"compilation options":         {"vllm", "0.22.1", []string{"-O3"}, TechniqueCUDAGraphs, "", "cannot tell"},
		"capture size already larger": {"vllm", "0.22.1", []string{"--max-cudagraph-capture-size", "1024"}, TechniqueCUDAGraphs, "", "batches of up to 1024"},
		"async back on":               {"vllm", "0.22.1", []string{"--no-async-scheduling"}, TechniqueAsyncScheduling, "", ""},
		"sglang overlap back on":      {"sglang", "0.5.12", []string{"--disable-overlap-schedule", "--schedule-policy", "lpm"}, TechniqueAsyncScheduling, "--schedule-policy lpm", ""},
		"speculation decides async":   {"vllm", "0.22.1", []string{"--no-async-scheduling", "--speculative-config", `{"method":"ngram"}`}, TechniqueAsyncScheduling, "", "speculates"},
	} {
		setup := control
		setup.Deployment.Runtime.Engine, setup.Deployment.Runtime.Version, setup.Deployment.Runtime.Args = test.engine, test.version, test.args
		args, why := deriveArgs(setup, techniques[test.technique], *busy, Request{GPU: "H100"})
		if strings.Join(args, " ") != test.want || test.why == "" && why != "" || test.why != "" && !strings.Contains(why, test.why) {
			t.Errorf("%s: args=%q why=%q", name, args, why)
		}
	}
}

func TestVLLMCaptureSizeFollowsTheResolvedDefaults(t *testing.T) {
	for name, test := range map[string]struct {
		gpu         string
		args        []string
		size, seqs  int
		unknownWhen bool
	}{
		"80 GB defaults":       {"H100", nil, 512, 1024, false},
		"24 GB defaults":       {"RTX4090", nil, 512, 256, false},
		"fewer sequences":      {"H100", []string{"--max-num-seqs", "60"}, 120, 60, false},
		"small token budget":   {"RTX4090", []string{"--max-num-batched-tokens=300"}, 300, 256, false},
		"explicit off grid":    {"H100", []string{"--max-cudagraph-capture-size", "1000"}, 992, 1024, false},
		"explicit small":       {"H100", []string{"--max-cudagraph-capture-size", "3"}, 2, 1024, false},
		"compilation config":   {"H100", []string{"--compilation-config", `{"cudagraph_mode":"NONE"}`}, 0, 0, true},
		"speculation":          {"H100", []string{"--speculative-config", `{"method":"mtp"}`}, 0, 0, true},
		"unreadable seqs":      {"H100", []string{"--max-num-seqs", "many"}, 0, 0, true},
		"unknown accelerator":  {"Mystery-GPU", nil, 0, 0, true},
		"explicit unreadable":  {"H100", []string{"--max-cudagraph-capture-size", "0"}, 0, 0, true},
		"performance mode set": {"H100", []string{"--performance-mode", "throughput"}, 0, 0, true},
	} {
		size, seqs, _, why := vllmCaptureSize(test.args, test.gpu)
		if test.unknownWhen != (why != "") || !test.unknownWhen && (size != test.size || seqs != test.seqs) {
			t.Errorf("%s: size=%d seqs=%d why=%q", name, size, seqs, why)
		}
	}
	for size, want := range map[int]int{1: 1, 3: 4, 5: 8, 255: 256, 256: 256, 257: 272, 875: 880} {
		if got := captureGridUp(size); got != want {
			t.Errorf("grid up %d=%d, want %d", size, got, want)
		}
	}
	for limit, want := range map[int]int{1: 1, 3: 2, 7: 4, 9: 8, 255: 248, 300: 288} {
		if got := captureGridDown(limit); got != want {
			t.Errorf("grid down %d=%d, want %d", limit, got, want)
		}
	}
}

func TestTraceCommunicationExposedNeverAddsGPUs(t *testing.T) {
	request := genericRequest("H100", nil)
	request.GPUCount = 2
	base, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	control := base.Candidates[0]
	wider := control
	wider.ID = strings.Repeat("d", 64)
	wider.Deployment.Resources.GPUCount = 4
	wider.Features = append(append([]Feature{}, control.Features...), Feature{Name: "diagnosis_technique", State: TechniqueFP8KVCache, Source: "review"})
	plan := func(trace *GPUTraceSummary) Proposal {
		t.Helper()
		diagnosis := traceDiagnosis(optimizationreadiness.BottleneckKVCachePressure, map[string]float64{optimizationreadiness.SignalRunningPerReplica: 48}, trace, preemptionDiagnosis().Findings...)
		proposal := base
		proposal.Input = request
		proposal.Input.Diagnosis = diagnosis
		proposal.Candidates = []Candidate{control, wider}
		planned, err := applyDiagnosis(proposal)
		if err != nil {
			t.Fatal(err)
		}
		return planned
	}
	overlapped := traceSummary(gpuprofile.BottleneckBusy, map[string]float64{TraceOverlappedShare: .9})
	if planned := plan(overlapped); planned.Candidates[1].ID != wider.ID {
		t.Fatalf("without exposed communication the reviewed wider setup ranks first: %v", reasonOrder(planned))
	}
	exposed := traceSummary(gpuprofile.BottleneckCommunication, map[string]float64{TraceOverlappedShare: .22}, exposedComms)
	planned := plan(exposed)
	for _, candidate := range planned.Candidates {
		if gpusPerServer(candidate) > gpusPerServer(control) {
			t.Fatalf("exposed communication kept a setup with more GPUs per server: %v", reasonOrder(planned))
		}
	}
	if parallelism := plannedTechnique(t, planned, TechniqueParallelism); parallelism.Skipped == "" || parallelism.Priority != 75 || parallelism.Rules[0] != TraceRuleCommunicationExposed {
		t.Fatalf("parallelism=%+v", parallelism)
	}

	heavy := traceDiagnosis(optimizationreadiness.BottleneckDecode, map[string]float64{optimizationreadiness.SignalRunningPerReplica: 64}, nil)
	heavy.GPUProfile.Shares[KernelClassCommunication], heavy.GPUProfile.Shares[KernelClassGEMM] = .3, .15
	withShares, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", heavy))
	if err != nil {
		t.Fatal(err)
	}
	plannedTechnique(t, withShares, TechniqueParallelism)
	heavy.GPUProfile.Trace = overlapped
	hidden, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", heavy))
	if err != nil {
		t.Fatal(err)
	}
	for _, technique := range hidden.Diagnosis.Techniques {
		if technique.Technique == TechniqueParallelism {
			t.Fatalf("communication the trace measured as hidden under compute still chose fewer GPUs: %+v", technique)
		}
	}
}

func TestTraceMatrixMultiplyVerdictNeedsRooflineCoverage(t *testing.T) {
	signals := map[string]float64{optimizationreadiness.SignalRunningPerReplica: 12}
	for name, test := range map[string]struct {
		coverage *float64
		finding  GPUTraceFinding
		order    string
		priority int
	}{
		"memory bound, covered":     {floatPointer(.71), memoryBoundGEMM, "control,fp8-weights", 80},
		"compute bound, covered":    {floatPointer(.8), GPUTraceFinding{Rule: TraceRuleComputeBoundGEMM, Observed: .75, Comparison: ">=", Threshold: .6, Unit: "ratio"}, "control,fp8-weights", 75},
		"memory bound, few shapes":  {floatPointer(.18), memoryBoundGEMM, "control", 0},
		"memory bound, no coverage": {nil, memoryBoundGEMM, "control", 0},
	} {
		measurements := map[string]float64{}
		if test.coverage != nil {
			measurements[TraceRooflineCoveredShare] = *test.coverage
		}
		diagnosis := traceDiagnosis(optimizationreadiness.BottleneckUnattributed, signals, traceSummary(gpuprofile.BottleneckMemoryBandwidth, measurements, test.finding))
		proposal, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", diagnosis))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(reasonOrder(proposal), ","); got != test.order {
			t.Errorf("%s: order=%s", name, got)
			continue
		}
		if test.priority > 0 && plannedTechnique(t, proposal, TechniqueFP8Weights).Priority != test.priority {
			t.Errorf("%s: plan=%+v", name, proposal.Diagnosis.Techniques)
		}
		if test.priority == 0 && (proposal.Diagnosis.State != DiagnosisNoActionableFinding || len(proposal.Diagnosis.Techniques) != 0) {
			t.Errorf("%s: an uncovered roofline chose a change: %+v", name, proposal.Diagnosis)
		}
	}
}

func TestTraceFindingsWithoutAChangeChooseNothing(t *testing.T) {
	idle := traceSummary(gpuprofile.BottleneckUnknown, map[string]float64{TraceIdleShare: .36, TraceDecodeShare: .86},
		GPUTraceFinding{Rule: "gpu.idle", Observed: .36, Comparison: ">=", Threshold: .2, Unit: "ratio"},
		GPUTraceFinding{Rule: "gpu.long_bubbles", Observed: 5400, Comparison: ">=", Threshold: 1000, Unit: "us"},
		GPUTraceFinding{Rule: TraceRuleDecodeDominant, Observed: .86, Comparison: ">=", Threshold: .7, Unit: "ratio"},
		GPUTraceFinding{Rule: TraceRuleAttentionHeavy, Observed: .38, Comparison: ">=", Threshold: .35, Unit: "ratio"})
	diagnosis := traceDiagnosis(optimizationreadiness.BottleneckUnattributed, map[string]float64{optimizationreadiness.SignalRunningPerReplica: 12, "mean_input_tokens": 600}, idle)
	proposal, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", diagnosis))
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Diagnosis.State != DiagnosisNoActionableFinding || len(proposal.Diagnosis.Techniques) != 0 || len(proposal.Candidates) != 1 {
		t.Fatalf("descriptive findings or short-context attention chose a change: %v %+v", reasonOrder(proposal), proposal.Diagnosis)
	}
}

func TestValidateDiagnosisRejectsMalformedTraceSummaries(t *testing.T) {
	mutations := map[string]func(*GPUTraceSummary){
		"algorithm":           func(trace *GPUTraceSummary) { trace.AlgorithmVersion = "gpu-trace-analysis-v9" },
		"bottleneck":          func(trace *GPUTraceSummary) { trace.Bottleneck = "slow" },
		"devices":             func(trace *GPUTraceSummary) { trace.Devices = 0 },
		"window":              func(trace *GPUTraceSummary) { trace.WindowMS = -1 },
		"duplicate":           func(trace *GPUTraceSummary) { trace.Findings = append(trace.Findings, trace.Findings[0]) },
		"comparison":          func(trace *GPUTraceSummary) { trace.Findings[0].Comparison = "~" },
		"rule":                func(trace *GPUTraceSummary) { trace.Findings[0].Rule = "GPU Idle!" },
		"unit":                func(trace *GPUTraceSummary) { trace.Findings[0].Unit = "" },
		"unknown measurement": func(trace *GPUTraceSummary) { trace.Measurements["made_up"] = 1 },
		"share above one":     func(trace *GPUTraceSummary) { trace.Measurements[TraceIdleShare] = 1.5 },
		"negative duration":   func(trace *GPUTraceSummary) { trace.Measurements[TraceStepP50MS] = -2 },
		"too many findings": func(trace *GPUTraceSummary) {
			for index := range gpuprofile.MaxFindings {
				trace.Findings = append(trace.Findings, GPUTraceFinding{Rule: "gpu.extra" + strings.Repeat("x", index+1), Comparison: ">=", Unit: "ratio"})
			}
		},
	}
	for name, mutate := range mutations {
		trace := traceSummary(gpuprofile.BottleneckHostLaunch, map[string]float64{TraceIdleShare: .3}, idleHostBound)
		mutate(trace)
		if _, err := catalogSource(t).Propose(context.Background(), genericRequest("H100", traceDiagnosis(optimizationreadiness.BottleneckHostScheduler, nil, trace))); err == nil {
			t.Errorf("%s: malformed trace summary accepted", name)
		}
	}
}

func floatPointer(value float64) *float64 { return &value }
