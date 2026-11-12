package kerneltarget

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto/sha256"
	"encoding/hex"

	"github.com/infercrane/infercrane/internal/acceleratorlab"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/brezelexecutor"
	"github.com/infercrane/infercrane/internal/kernelgen"
	"github.com/infercrane/infercrane/internal/kernelplanner"
	"github.com/infercrane/infercrane/internal/kernelresearch"
)

const repositoryPacks = "../../tools/kernel-lab/packs"

var allStages = kernelresearch.Correctness{Smoke: true, ShapeSweep: true, NumericalStability: true, Determinism: true, EdgeCases: true, Sanitizer: true}

func digestOf(character string) string { return "sha256:" + strings.Repeat(character, 64) }

// fakeHost stands in for Docker, nvidia-smi, the serving runtime, and AIPerf.
type fakeHost struct {
	exited      bool
	mu          sync.Mutex
	t           *testing.T
	cache       string
	gpu         string
	calls       [][]string
	removed     []string
	failStart   string
	throughput  map[string]float64
	outputs     map[string]string
	current     string
	currentArgs []string
	skipMarker  bool
	noTrace     bool
	profiled    int
	memory      map[string]string
	server      *httptest.Server
}

func newFakeHost(t *testing.T, cache string) *fakeHost {
	t.Helper()
	host := &fakeHost{t: t, cache: cache, gpu: "NVIDIA H200 SXM\n", throughput: map[string]float64{}, outputs: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		host.mu.Lock()
		defer host.mu.Unlock()
		if host.exited {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /start_profile", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /stop_profile", func(w http.ResponseWriter, _ *http.Request) {
		host.mu.Lock()
		defer host.mu.Unlock()
		host.profiled++
		if !host.noTrace {
			host.writeTrace()
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		host.mu.Lock()
		defer host.mu.Unlock()
		content := "same output"
		if override, ok := forArm(host.outputs, host.current); ok {
			content = override
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": content}}}})
	})
	host.server = httptest.NewServer(mux)
	t.Cleanup(host.server.Close)
	return host
}

func forArm(values map[string]string, container string) (string, bool) {
	for arm, value := range values {
		if strings.Contains(container, arm) {
			return value, true
		}
	}
	return "", false
}

func (h *fakeHost) port() int {
	parsed, err := url.Parse(h.server.URL)
	if err != nil {
		h.t.Fatal(err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		h.t.Fatal(err)
	}
	return port
}

func flagValue(args []string, flag, prefix string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag && strings.HasPrefix(args[index+1], prefix) {
			return args[index+1]
		}
	}
	return ""
}

func (h *fakeHost) writeTrace() {
	volume := flagValue(h.currentArgs, "--volume", "")
	for index := 0; index+1 < len(h.currentArgs); index++ {
		if h.currentArgs[index] == "--volume" && strings.HasSuffix(h.currentArgs[index+1], ":/profiles") {
			volume = strings.TrimSuffix(h.currentArgs[index+1], ":/profiles")
		}
	}
	trace := map[string]any{"traceEvents": []map[string]any{
		{"ph": "X", "cat": "kernel", "name": "fused_add_rms_norm_kernel", "dur": 200.0},
		{"ph": "X", "cat": "kernel", "name": "cublas_gemm", "dur": 700.0},
		{"ph": "X", "cat": "kernel", "name": "mystery_kernel", "dur": 100.0},
		{"ph": "X", "cat": "cuda_runtime", "name": "cudaLaunchKernel_rmsnorm", "dur": 9000.0, "args": map[string]any{"stream": 7}},
		{"ph": "i", "cat": "kernel", "name": "instant_rmsnorm", "dur": 9000.0},
	}}
	body, err := json.Marshal(trace)
	if err != nil {
		h.t.Error(err)
		return
	}
	if err = os.WriteFile(filepath.Join(volume, "run.trace.json"), body, 0o600); err != nil {
		h.t.Error(err)
	}
}

func (h *fakeHost) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, append([]string{name}, args...))
	if name == "nvidia-smi" {
		if strings.Contains(strings.Join(args, " "), "memory.used") {
			if value, ok := forArm(h.memory, h.current); ok {
				return []byte(value), nil
			}
			return []byte("1000\n"), nil
		}
		return []byte(h.gpu), nil
	}
	switch args[0] {
	case "network":
		if args[1] == "inspect" {
			return nil, errors.New("no such network")
		}
		return nil, nil
	case "inspect":
		if strings.Contains(strings.Join(args, " "), "State.Running") {
			return []byte(strconv.FormatBool(!h.exited) + "\n"), nil
		}
		return []byte("127.0.0.1\n"), nil
	case "rm":
		h.removed = append(h.removed, args[2])
		return nil, nil
	case "run":
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "snapshot_download") {
			path := filepath.Join(h.cache, "hub", "models--acme--novel", "snapshots", strings.Repeat("a", 40))
			if err := os.MkdirAll(path, 0o755); err != nil {
				return nil, err
			}
			return nil, os.WriteFile(filepath.Join(path, "config.json"), []byte("{}"), 0o600)
		}
		name := flagValue(args, "--name", "")
		if h.failStart != "" && strings.Contains(name, h.failStart) {
			return nil, errors.New("container exited")
		}
		h.current, h.currentArgs = name, args
		return []byte("container-id\n"), nil
	}
	return nil, fmt.Errorf("unexpected docker command %v", args)
}

func (h *fakeHost) servingRuns() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var names []string
	for _, call := range h.calls {
		if call[0] == "docker" && call[1] == "run" && !strings.Contains(strings.Join(call, " "), "snapshot_download") {
			names = append(names, flagValue(call[1:], "--name", ""))
		}
	}
	return names
}

func (h *fakeHost) Run2(_ context.Context, cfg benchmark.Config) (benchmark.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if strings.Contains(h.current, "candidate") && !h.skipMarker {
		for index := 0; index+1 < len(h.currentArgs); index++ {
			if h.currentArgs[index] == "--volume" && strings.HasSuffix(h.currentArgs[index+1], ":/kernel-state") {
				if err := os.WriteFile(filepath.Join(strings.TrimSuffix(h.currentArgs[index+1], ":/kernel-state"), invokedMarkerName), nil, 0o600); err != nil {
					return benchmark.Result{}, err
				}
			}
		}
	}
	rate := 100.0
	for key, value := range h.throughput {
		if strings.Contains(h.current, key) {
			rate = value
		}
	}
	return benchmark.Result{Tool: "aiperf", Requests: cfg.Requests, Succeeded: cfg.Requests, OutputTokenThroughput: &rate}, nil
}

type loadAdapter struct{ host *fakeHost }

func (l loadAdapter) Run(ctx context.Context, cfg benchmark.Config) (benchmark.Result, error) {
	return l.host.Run2(ctx, cfg)
}

type stubEvaluator struct {
	calls   int
	respond func(kernelgen.EvaluationRequest) kernelgen.EvaluationResult
}

func (s *stubEvaluator) Evaluate(_ context.Context, request kernelgen.EvaluationRequest) (kernelgen.EvaluationResult, error) {
	s.calls++
	return s.respond(request), nil
}

func passingEvaluator() *stubEvaluator {
	return &stubEvaluator{respond: func(kernelgen.EvaluationRequest) kernelgen.EvaluationResult {
		return kernelgen.EvaluationResult{Correctness: allStages, BaselineMedianUS: 100, CandidateMedianUS: 80}
	}}
}

type memoryArtifacts struct{ body string }

func (m memoryArtifacts) Open(context.Context, brezelexecutor.ArtifactRef) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(m.body)), nil
}

const builtKernel = "@triton.jit\ndef fused_residual_rmsnorm(x, residual, weight, epsilon):\n    pass\n"

func sourceDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type fixture struct {
	target    Target
	host      *fakeHost
	evaluator *stubEvaluator
	replay    string
	suite     string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cache, work, replayDir := t.TempDir(), t.TempDir(), t.TempDir()
	host := newFakeHost(t, cache)
	writeAddressed := func(body string) string {
		sum := sha256.Sum256([]byte(body))
		digest := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(replayDir, digest), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	evaluator := passingEvaluator()
	target := Target{
		Runtimes:   []RuntimeSpec{{Name: "vllm", Version: "0.22.1", Repository: "vllm/vllm-openai", Python: "python3", Launcher: []string{"vllm", "serve", "${MODEL}", "--revision", "${MODEL_REVISION}", "--port", "${PORT}"}, Port: host.port(), ProfilerEnv: "VLLM_TORCH_PROFILER_DIR"}},
		ModelCache: cache, WorkDir: work, PackDir: repositoryPacks, ReplayDir: replayDir, GPUHourlyUSD: 4, HasHFToken: true,
		Commands: host, Load: loadAdapter{host}, Evaluator: evaluator, Artifacts: memoryArtifacts{body: builtKernel},
		ReadyTimeout: 5 * time.Second, PollInterval: 5 * time.Millisecond,
	}
	return &fixture{
		target: target, host: host, evaluator: evaluator,
		replay: "aiperf-replay@sha256:" + writeAddressed(`{"text":"x"}`),
		suite:  "quality-suite@sha256:" + writeAddressed(`{"prompts":["one","two"],"max_tokens":8}`),
	}
}

var (
	testHardware = kernelplanner.HardwareIdentity{Vendor: "nvidia", Accelerator: "H200", ComputeCapability: "sm90"}
	testTopology = acceleratorlab.Topology{Mode: "aggregated", Nodes: 1, Accelerators: 1, TensorParallel: 1, PipelineParallel: 1, ExpertParallel: 1}
	testModel    = kernelplanner.ModelIdentity{Repository: "acme/novel", Revision: strings.Repeat("a", 40)}
	testRuntime  = kernelplanner.RuntimeIdentity{Name: "vllm", Version: "0.22.1", ImageDigest: digestOf("1")}
)

func testWorkload(replay, suite string) acceleratorlab.Workload {
	return acceleratorlab.Workload{Digest: digestOf("2"), Modality: acceleratorlab.ModalityText, Phase: kernelplanner.PhaseDecode, BatchSize: 1, Concurrency: 4, InputTokens: 2000, OutputTokens: 300, QualitySuite: suite, Replay: replay}
}

func profileIntent(f *fixture) acceleratorlab.ProfileIntent {
	return acceleratorlab.ProfileIntent{InputDigest: digestOf("9"), Model: testModel, Runtime: testRuntime, Hardware: testHardware, Topology: testTopology, Workload: testWorkload(f.replay, f.suite)}
}

func qualificationRequest(f *fixture) acceleratorlab.QualificationRequest {
	return acceleratorlab.QualificationRequest{
		InputDigest: digestOf("9"), Experiment: kernelplanner.Candidate{ID: "candidate-1", OperatorFamily: kernelplanner.ResidualRMSNorm},
		Build: acceleratorlab.BuildEvidence{Artifacts: []acceleratorlab.Artifact{{Kind: kernelBundleKind, URI: "https://worker.example/k", SHA256: sourceDigest(builtKernel), Size: int64(len(builtKernel))}}},
		Model: testModel, Runtime: testRuntime, Hardware: testHardware, Topology: testTopology, Workload: testWorkload(f.replay, f.suite),
		Policy: acceleratorlab.Policy{MaxCandidates: 1},
	}
}

func TestProfileMeasuresOnlyGPUKernelsAndMapsReviewedOperators(t *testing.T) {
	f := newFixture(t)
	evidence, err := f.target.Capture(context.Background(), profileIntent(f))
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Hotspots) != 2 || evidence.Hotspots[0].Family != kernelplanner.QuantizedLinear || evidence.Hotspots[1].Family != kernelplanner.ResidualRMSNorm {
		t.Fatalf("hotspots=%+v", evidence.Hotspots)
	}
	// Runtime API events are excluded; the unclassified kernel stays in the denominator.
	if evidence.Hotspots[0].DeviceTimeFraction != .7 || evidence.Hotspots[1].DeviceTimeFraction != .2 {
		t.Fatalf("fractions=%+v", evidence.Hotspots)
	}
	if evidence.Hardware != testHardware || evidence.RuntimeImageDigest != digestOf("1") || evidence.WorkloadDigest != digestOf("2") || evidence.Tool == "" || evidence.ToolVersion == "" || !digestPattern.MatchString(evidence.ArtifactDigest) {
		t.Fatalf("identity=%+v", evidence)
	}
	if len(f.host.removed) != 2 {
		t.Fatalf("container must be cleared before start and removed after: %v", f.host.removed)
	}
}

func TestServingContainersAreIsolatedAndOnlyDownloadSeesTheToken(t *testing.T) {
	f := newFixture(t)
	if _, err := f.target.Capture(context.Background(), profileIntent(f)); err != nil {
		t.Fatal(err)
	}
	var download, serving []string
	for _, call := range f.host.calls {
		if call[0] != "docker" || call[1] != "run" {
			continue
		}
		if strings.Contains(strings.Join(call, " "), "snapshot_download") {
			download = call
		} else {
			serving = call
		}
	}
	joined := strings.Join(serving, " ")
	for _, want := range []string{"--network " + internalNetwork, "--read-only", "--cap-drop ALL", "no-new-privileges", "HF_HUB_OFFLINE=1", ":/model-cache:ro", "vllm/vllm-openai@" + digestOf("1"), "--entrypoint vllm", "--revision " + strings.Repeat("a", 40)} {
		if !strings.Contains(joined, want) {
			t.Fatalf("serving container missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "HF_TOKEN") || !strings.Contains(strings.Join(download, " "), "--env HF_TOKEN") {
		t.Fatalf("token boundary wrong:\nserving %s\ndownload %v", joined, download)
	}
	if strings.Contains(strings.Join(download, " "), "--env HF_TOKEN=") {
		t.Fatal("token value placed on the command line")
	}
}

func TestRuntimeCanEnableProfilingByArgument(t *testing.T) {
	f := newFixture(t)
	f.target.Runtimes[0].ProfilerEnv = ""
	f.target.Runtimes[0].ProfilerArgs = []string{"--profiler-config.torch_profiler_dir=${PROFILE_DIR}"}
	if _, err := f.target.Capture(context.Background(), profileIntent(f)); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(f.host.currentArgs, " ")
	if !strings.Contains(joined, "--profiler-config.torch_profiler_dir=/profiles") || strings.Contains(joined, "VLLM_TORCH_PROFILER_DIR") {
		t.Fatalf("profiling arguments wrong: %s", joined)
	}
}

func TestProfileRefusesTheWrongHostBeforeStartingAnything(t *testing.T) {
	cases := map[string]func(*fixture, *acceleratorlab.ProfileIntent){
		"wrong gpu":       func(f *fixture, _ *acceleratorlab.ProfileIntent) { f.host.gpu = "NVIDIA A100\n" },
		"two gpus":        func(f *fixture, _ *acceleratorlab.ProfileIntent) { f.host.gpu = "NVIDIA H200\nNVIDIA H200\n" },
		"unknown runtime": func(_ *fixture, intent *acceleratorlab.ProfileIntent) { intent.Runtime.Version = "9.9.9" },
		"mutable model":   func(_ *fixture, intent *acceleratorlab.ProfileIntent) { intent.Model.Revision = "main" },
		"multi node":      func(_ *fixture, intent *acceleratorlab.ProfileIntent) { intent.Topology.Nodes = 2 },
		"bad digest":      func(_ *fixture, intent *acceleratorlab.ProfileIntent) { intent.Runtime.ImageDigest = "latest" },
	}
	for name, mutate := range cases {
		f := newFixture(t)
		intent := profileIntent(f)
		mutate(f, &intent)
		if _, err := f.target.Capture(context.Background(), intent); err == nil {
			t.Fatalf("%s accepted", name)
		}
		if runs := f.host.servingRuns(); len(runs) != 0 {
			t.Fatalf("%s started a server: %v", name, runs)
		}
	}
}

func TestProfileWithoutTraceFailsAndStillCleansUp(t *testing.T) {
	f := newFixture(t)
	f.host.noTrace = true
	if _, err := f.target.Capture(context.Background(), profileIntent(f)); err == nil {
		t.Fatal("missing trace accepted")
	}
	if len(f.host.removed) != 2 {
		t.Fatalf("removed=%v", f.host.removed)
	}
}

func TestExitedServerFailsWithoutWaitingForTheTimeout(t *testing.T) {
	f := newFixture(t)
	f.host.exited = true
	f.target.ReadyTimeout = time.Minute
	started := time.Now()
	_, err := f.target.Capture(context.Background(), profileIntent(f))
	if err == nil || !strings.Contains(err.Error(), "exited") || time.Since(started) > 10*time.Second {
		t.Fatalf("err=%v elapsed=%v", err, time.Since(started))
	}
}

func TestClassifyAttentionNeedsAKnownPhase(t *testing.T) {
	if family, ok := classify("flash_attn_kernel", kernelplanner.PhasePrefill); !ok || family != kernelplanner.AttentionPrefill {
		t.Fatalf("prefill family=%v ok=%v", family, ok)
	}
	if family, ok := classify("flash_attn_kernel", kernelplanner.PhaseDecode); !ok || family != kernelplanner.AttentionDecode {
		t.Fatalf("decode family=%v ok=%v", family, ok)
	}
	if _, ok := classify("flash_attn_kernel", kernelplanner.PhaseMixed); ok {
		t.Fatal("mixed trace attributed attention to one path")
	}
}

func TestShapeForPhaseSeparatesPrefillAndDecode(t *testing.T) {
	workload := testWorkload("", "")
	workload.Phase = kernelplanner.PhasePrefill
	if input, output := shapeForPhase(workload); input != 2000 || output != 1 {
		t.Fatalf("prefill %d/%d", input, output)
	}
	workload.Phase = kernelplanner.PhaseDecode
	if input, output := shapeForPhase(workload); input != 256 || output != 300 {
		t.Fatalf("decode %d/%d", input, output)
	}
}

func TestSummarizeReadsCompressedTraces(t *testing.T) {
	directory := t.TempDir()
	file, err := os.Create(filepath.Join(directory, "a.trace.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	writer := gzip.NewWriter(file)
	if _, err = writer.Write([]byte(`{"traceEvents":[{"ph":"X","cat":"kernel","name":"rmsnorm","dur":5}]}`)); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	hotspots, digest, err := summarizeTraces(directory, kernelplanner.PhaseDecode)
	if err != nil || len(hotspots) != 1 || hotspots[0].DeviceTimeFraction != 1 || !digestPattern.MatchString(digest) {
		t.Fatalf("hotspots=%+v digest=%s err=%v", hotspots, digest, err)
	}
}

func TestQualifyPairsCandidateBetweenControlsOnTheSameGPU(t *testing.T) {
	f := newFixture(t)
	f.host.throughput = map[string]float64{"control-a": 100, "candidate": 120, "control-b": 101}
	f.host.memory = map[string]string{"control-a": "1000\n", "candidate": "1100\n", "control-b": "1000\n"}
	request := qualificationRequest(f)
	evidence, err := f.target.Qualify(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	runs := f.host.servingRuns()
	if len(runs) != 3 || !strings.Contains(runs[0], "control-a") || !strings.Contains(runs[1], "candidate") || !strings.Contains(runs[2], "control-b") {
		t.Fatalf("arms=%v", runs)
	}
	if want := 120.0 / 100.5; evidence.EndToEndSpeedup != want {
		t.Fatalf("speedup=%v want %v", evidence.EndToEndSpeedup, want)
	}
	if !evidence.ServingReplayMeasured || !evidence.QualityPassed || !evidence.CorrectnessPassed || !evidence.HiddenShapesPassed || !evidence.SanitizerPassed || evidence.KernelSpeedup != 1.25 || evidence.CostRegressionPct >= 0 {
		t.Fatalf("evidence=%+v", evidence)
	}
	if evidence.MemoryRegressionPct != 10 {
		t.Fatalf("memory=%v", evidence.MemoryRegressionPct)
	}
	if evidence.Replay != request.Workload.Replay || evidence.QualitySuite != request.Workload.QualitySuite || evidence.BuildArtifactDigest != sourceDigest(builtKernel) || !digestPattern.MatchString(evidence.ProfilerArtifactDigest) || evidence.BenchmarkEvidenceID == "" || evidence.QualityEvidenceID == "" || evidence.MeasuredAt.IsZero() {
		t.Fatalf("identity=%+v", evidence)
	}
	if f.evaluator.calls != 2 {
		t.Fatalf("hidden suite runs=%d", f.evaluator.calls)
	}
	if f.host.profiled != 1 {
		t.Fatalf("only the candidate arm should be profiled: %d", f.host.profiled)
	}
	var kernelArms int
	const priority = "--kernel-config.ir_op_priority.fused_add_rms_norm=infercrane,vllm_c,native"
	for _, call := range f.host.calls {
		if call[0] != "docker" || call[1] != "run" {
			continue
		}
		joined := strings.Join(call, " ")
		isCandidate := strings.Contains(flagValue(call[1:], "--name", ""), "candidate")
		if strings.Contains(joined, priority) != isCandidate && !strings.Contains(joined, "snapshot_download") {
			t.Fatalf("operator priority must be set on the candidate arm only: %v", call)
		}
		if strings.Contains(joined, "INFERCRANE_KERNEL_SOURCE") {
			kernelArms++
			if !isCandidate {
				t.Fatalf("generated kernel loaded into a control: %v", call)
			}
		}
	}
	if kernelArms != 1 {
		t.Fatalf("kernel arms=%d", kernelArms)
	}
}

func TestQualifyTreatsBrokenComparisonsAsInvalid(t *testing.T) {
	cases := map[string]func(*fixture){
		"control drift":            func(f *fixture) { f.host.throughput = map[string]float64{"control-a": 100, "control-b": 130} },
		"nondeterministic control": func(f *fixture) { f.host.outputs = map[string]string{"control-b": "different"} },
		"kernel never called":      func(f *fixture) { f.host.skipMarker = true },
		"control fails":            func(f *fixture) { f.host.failStart = "control-a" },
	}
	for name, mutate := range cases {
		f := newFixture(t)
		mutate(f)
		if evidence, err := f.target.Qualify(context.Background(), qualificationRequest(f)); err == nil {
			t.Fatalf("%s produced evidence: %+v", name, evidence)
		}
	}
}

func TestQualifyKeepsCandidateFailuresAsEvidence(t *testing.T) {
	f := newFixture(t)
	f.host.outputs = map[string]string{"candidate": "changed"}
	evidence, err := f.target.Qualify(context.Background(), qualificationRequest(f))
	if err != nil || evidence.QualityPassed || !evidence.ServingReplayMeasured {
		t.Fatalf("output drift: %+v err=%v", evidence, err)
	}
	f = newFixture(t)
	f.host.failStart = "candidate"
	evidence, err = f.target.Qualify(context.Background(), qualificationRequest(f))
	if err != nil || evidence.ServingReplayMeasured || evidence.EndToEndSpeedup != 0 || !digestPattern.MatchString(evidence.ProfilerArtifactDigest) {
		t.Fatalf("candidate crash: %+v err=%v", evidence, err)
	}
	if runs := f.host.servingRuns(); len(runs) != 3 || !strings.Contains(runs[2], "control-b") {
		t.Fatalf("second control must still run: %v", runs)
	}
}

func TestQualifySkipsGPUReplayWhenHiddenSuiteFails(t *testing.T) {
	f := newFixture(t)
	f.evaluator.respond = func(request kernelgen.EvaluationRequest) kernelgen.EvaluationResult {
		result := kernelgen.EvaluationResult{Correctness: allStages, BaselineMedianUS: 100, CandidateMedianUS: 80}
		if request.Verification {
			result.Correctness.ShapeSweep = false
		}
		return result
	}
	evidence, err := f.target.Qualify(context.Background(), qualificationRequest(f))
	if err != nil || evidence.HiddenShapesPassed || evidence.ServingReplayMeasured || len(f.host.servingRuns()) != 0 {
		t.Fatalf("evidence=%+v err=%v runs=%v", evidence, err, f.host.servingRuns())
	}
}

func TestQualifyRefusesUnverifiableInputsBeforeUsingTheGPU(t *testing.T) {
	cases := map[string]func(*fixture, *acceleratorlab.QualificationRequest){
		"missing replay": func(_ *fixture, r *acceleratorlab.QualificationRequest) {
			r.Workload.Replay = "aiperf-replay@" + digestOf("c")
		},
		"unaddressed": func(_ *fixture, r *acceleratorlab.QualificationRequest) { r.Workload.Replay = "latest-traffic" },
		"tampered replay": func(f *fixture, r *acceleratorlab.QualificationRequest) {
			digest := r.Workload.Replay[strings.LastIndex(r.Workload.Replay, ":")+1:]
			if err := os.WriteFile(filepath.Join(f.target.ReplayDir, digest), []byte("edited"), 0o600); err != nil {
				panic(err)
			}
		},
		"no bundle":    func(_ *fixture, r *acceleratorlab.QualificationRequest) { r.Build.Artifacts[0].Kind = "other" },
		"wrong digest": func(_ *fixture, r *acceleratorlab.QualificationRequest) { r.Build.Artifacts[0].SHA256 = digestOf("d") },
		"no pack": func(_ *fixture, r *acceleratorlab.QualificationRequest) {
			r.Experiment.OperatorFamily = kernelplanner.Sampling
		},
		"pack runtime": func(f *fixture, r *acceleratorlab.QualificationRequest) {
			f.target.Runtimes[0].Name = "sglang"
			r.Runtime.Name = "sglang"
		},
		// vLLM 0.34.0 removes the IR op-priority setting the pack relies on.
		"pack runtime version": func(f *fixture, r *acceleratorlab.QualificationRequest) {
			f.target.Runtimes[0].Version = "0.34.0"
			r.Runtime.Version = "0.34.0"
		},
	}
	for name, mutate := range cases {
		f := newFixture(t)
		request := qualificationRequest(f)
		mutate(f, &request)
		if evidence, err := f.target.Qualify(context.Background(), request); err == nil {
			t.Fatalf("%s accepted: %+v", name, evidence)
		}
		if runs := f.host.servingRuns(); len(runs) != 0 || f.evaluator.calls != 0 {
			t.Fatalf("%s used the GPU: runs=%v evaluator=%d", name, runs, f.evaluator.calls)
		}
	}
}

func TestLoadRuntimesAcceptsTheReviewedCatalogAndRejectsIncompleteOnes(t *testing.T) {
	runtimes, err := LoadRuntimes("../../tools/kernel-lab/target-runtimes.json")
	if err != nil || len(runtimes) != 2 {
		t.Fatalf("runtimes=%+v err=%v", runtimes, err)
	}
	path := filepath.Join(t.TempDir(), "runtimes.json")
	for _, body := range []string{`{"schema_version":"wrong","runtimes":[]}`, `{"schema_version":"` + RuntimeCatalogSchemaV1 + `","runtimes":[{"name":"x"}]}`, `{"schema_version":"` + RuntimeCatalogSchemaV1 + `","extra":1}`} {
		if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err = LoadRuntimes(path); err == nil {
			t.Fatalf("catalog accepted: %s", body)
		}
	}
}

type scriptedAuthor struct{ drafts []string }

func (a *scriptedAuthor) Identity() string { return "scripted" }

func (a *scriptedAuthor) Draft(context.Context, kernelgen.DraftRequest) (kernelgen.Draft, error) {
	if len(a.drafts) == 0 {
		return kernelgen.Draft{}, kernelgen.ErrInvalidDraft
	}
	draft := a.drafts[0]
	a.drafts = a.drafts[1:]
	return kernelgen.Draft{Source: draft}, nil
}

type publisherStub struct{}

func (publisherStub) Publish(_ context.Context, _, _ string, reader io.Reader, _ int64) (string, error) {
	_, err := io.Copy(io.Discard, reader)
	return "https://worker.example/source", err
}

type builderFunc func(context.Context, acceleratorlab.BuildRequest) (acceleratorlab.BuildEvidence, error)

func (f builderFunc) Build(ctx context.Context, input acceleratorlab.BuildRequest) (acceleratorlab.BuildEvidence, error) {
	return f(ctx, input)
}

func TestProfileGenerateAndQualifyDriveTheEngineToAQualifiedKernel(t *testing.T) {
	f := newFixture(t)
	f.host.gpu = "NVIDIA H200 SXM\n"
	f.host.throughput = map[string]float64{"control-a": 100, "candidate": 110, "control-b": 100}
	request := acceleratorlab.Request{
		SchemaVersion: acceleratorlab.RequestSchemaV1, TenantID: "tenant", CampaignID: "campaign", CandidateID: "candidate", Model: testModel, Runtime: testRuntime, Hardware: testHardware, Topology: testTopology,
		Workload: testWorkload(f.replay, f.suite),
		Policy: acceleratorlab.Policy{
			MaxCandidates: 2, MaxCostUSD: 20, MinHotspotFraction: .15, MinAmdahlCeiling: 1.05, TargetEndToEndSpeedup: 1.02, MinKernelSpeedup: 1.01, MaxMemoryRegressionPct: 2,
			RequireSanitizer: true, RequireHiddenShapes: true, RequireServingReplay: true, RequireQuality: true, AllowGeneratedKernels: true, AuthorizedUntil: time.Now().UTC().Add(time.Hour),
			KernelResearch: kernelresearch.Policy{Enabled: true, MaxIterations: 3, MaxConsecutiveRejections: 2, MaxDurationSeconds: 3600, MaxCostUSD: 5, MinRelativeImprovement: 1.01, TargetKernelSpeedup: 1.2, MaxRooflineUtilization: .95},
		},
	}
	builder := builderFunc(func(_ context.Context, input acceleratorlab.BuildRequest) (acceleratorlab.BuildEvidence, error) {
		return acceleratorlab.BuildEvidence{InputDigest: input.InputDigest, Builder: "brezel", BuilderVersion: "1", Environment: "envr_964f832968870a80ca50570b", SourceDigest: digestOf("6"), Status: "passed", Checks: []string{"syntax"}, Artifacts: []acceleratorlab.Artifact{{Kind: kernelBundleKind, URI: "https://worker.example/k", SHA256: sourceDigest(builtKernel), Size: int64(len(builtKernel))}}, ReceiptDigest: digestOf("8")}, nil
	})
	generator := kernelgen.Generator{PackDirectory: repositoryPacks, Author: &scriptedAuthor{drafts: []string{"import triton\n" + builtKernel}}, Evaluator: f.evaluator, Publisher: publisherStub{}}
	engine := acceleratorlab.Engine{Profiler: f.target, Generator: generator, Builder: builder, Qualifier: f.target}
	result, err := engine.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	// Real traces are usually led by GEMM, which has no reviewed pack. That
	// experiment must fail visibly without blocking the operator that has one.
	if result.State != "qualified" || len(result.Experiments) != 2 {
		t.Fatalf("state=%s plan=%+v experiments=%+v", result.State, result.Plan.Rejected, result.Experiments)
	}
	if first := result.Experiments[0]; first.Candidate.OperatorFamily != kernelplanner.QuantizedLinear || first.FailureCode != "kernel_generation_failed" {
		t.Fatalf("first=%+v", first)
	}
	if second := result.Experiments[1]; second.Candidate.OperatorFamily != kernelplanner.ResidualRMSNorm || second.Qualification == nil || second.Generation == nil {
		t.Fatalf("second=%+v", second)
	}
}
