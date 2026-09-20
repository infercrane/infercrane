package benchmark

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAIPerfLiveEndpoint(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("INFERCRANE_AIPERF_LIVE_ENDPOINT"))
	if endpoint == "" {
		t.Skip("set INFERCRANE_AIPERF_LIVE_ENDPOINT to run the live AIPerf adapter test")
	}
	binary := strings.TrimSpace(os.Getenv("INFERCRANE_AIPERF_LIVE_BINARY"))
	if binary == "" {
		binary = "aiperf"
	}
	model := strings.TrimSpace(os.Getenv("INFERCRANE_AIPERF_LIVE_MODEL"))
	if model == "" {
		model = "Qwen/Qwen3-0.6B"
	}
	tokenizer := strings.TrimSpace(os.Getenv("INFERCRANE_AIPERF_LIVE_TOKENIZER"))
	if tokenizer == "" {
		tokenizer = model
	}
	requests := liveBenchmarkInt(t, "INFERCRANE_AIPERF_LIVE_REQUESTS", 2)
	concurrency := liveBenchmarkInt(t, "INFERCRANE_AIPERF_LIVE_CONCURRENCY", 1)
	profileRuns := liveBenchmarkInt(t, "INFERCRANE_AIPERF_LIVE_RUNS", 3)
	warmupRequests := liveBenchmarkInt(t, "INFERCRANE_AIPERF_LIVE_WARMUP_REQUESTS", 1)

	result, err := Run(context.Background(), Config{
		Binary:             binary,
		Endpoint:           endpoint,
		Model:              model,
		Tokenizer:          tokenizer,
		Requests:           requests,
		Concurrency:        concurrency,
		InputTokens:        32,
		OutputTokens:       16,
		ProfileRuns:        profileRuns,
		WarmupRequests:     warmupRequests,
		ProfileRunCooldown: time.Second,
		RandomSeed:         42,
		Timeout:            10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live AIPerf result:\n%s", encoded)
	if result.Tool != "aiperf" || result.ToolVersion != AIPerfVersion || result.ProfileRuns != profileRuns || result.Requests != requests*profileRuns || result.Failed != 0 {
		t.Fatalf("unexpected live result: %+v", result)
	}
	if result.OutputTokenThroughput == nil || result.TTFTP95MS == nil || result.TPOTP95MS == nil || result.Confidence["output_token_throughput"].Samples != profileRuns {
		t.Fatalf("live result is missing required performance evidence: %+v", result)
	}
}

func liveBenchmarkInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		t.Fatalf("%s must be a positive integer; got %q", name, value)
	}
	return parsed
}

func TestParseRecordsMeasuresAIPerfMetrics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	data := `{"metadata":{"benchmark_phase":"warmup"},"metrics":{"time_to_first_token":{"value":999}}}
{"metadata":{"benchmark_phase":"profiling","request_start_ns":1000000000,"request_end_ns":2000000000},"metrics":{"time_to_first_token":{"value":10},"request_latency":{"value":30},"inter_token_latency":{"value":2},"output_token_count":{"value":20}}}
{"metadata":{"benchmark_phase":"profiling","request_start_ns":1500000000,"request_end_ns":3000000000},"metrics":{"time_to_first_token":{"value":20},"request_latency":{"value":40},"inter_token_latency":{"value":4},"output_token_count":{"value":30}}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := parseRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Requests != 2 || result.Succeeded != 2 || result.TTFTP50MS == nil || *result.TTFTP50MS != 10 || result.TPOTP95MS == nil || *result.TPOTP95MS != 4 {
		t.Fatalf("result=%#v", result)
	}
	if result.DurationSeconds != 2 || result.OutputTokenThroughput == nil || *result.OutputTokenThroughput != 25 {
		t.Fatalf("throughput result=%#v", result)
	}
}

func TestParseRecordsComputesSLOGoodputWithoutTreatingMissingMetricsAsPassing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	data := `{"metadata":{"benchmark_phase":"profiling","request_start_ns":1000000000,"request_end_ns":2000000000},"metrics":{"time_to_first_token":{"value":100},"request_latency":{"value":500},"inter_token_latency":{"value":10}}}
{"metadata":{"benchmark_phase":"profiling","request_start_ns":1000000000,"request_end_ns":3000000000},"metrics":{"time_to_first_token":{"value":400},"request_latency":{"value":900},"inter_token_latency":{"value":20}}}
{"metadata":{"benchmark_phase":"profiling","request_start_ns":1000000000,"request_end_ns":3000000000},"metrics":{"time_to_first_token":{"value":100},"request_latency":{"value":500}}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := parseRecordsWithSLO(path, Config{TTFTSLOMS: 250, TPOTSLOMS: 15, LatencySLOMS: 700})
	if err != nil || result.Goodput == nil || *result.Goodput != .5 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestPercentileUsesNearestRankWithoutHidingTail(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 100}
	if got := percentile(values, .95); got != 100 {
		t.Fatalf("p95=%v, want 100", got)
	}
}

func TestParseRecordsNormalizesAIPerfLatencyUnits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	data := `{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":1000000001},"metrics":{"time_to_first_token":{"value":0.25,"unit":"s"},"request_latency":{"value":900000000,"unit":"ns"},"inter_token_latency":{"value":12000,"unit":"us"}}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := parseRecords(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.TTFTP50MS == nil || *result.TTFTP50MS != 250 || result.LatencyP50MS == nil || *result.LatencyP50MS != 900 || result.TPOTP50MS == nil || *result.TPOTP50MS != 12 {
		t.Fatalf("result=%#v", result)
	}
}

func TestParseRecordsRejectsUnknownLatencyUnit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	data := `{"metadata":{"benchmark_phase":"profiling"},"metrics":{"time_to_first_token":{"value":10,"unit":"ticks"}}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := parseRecords(path)
	if err == nil || !strings.Contains(err.Error(), `unsupported latency unit "ticks"`) {
		t.Fatalf("err=%v", err)
	}
}

func TestParseRecordsIgnoresStructuredMetricsItDoesNotConsume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	data := `{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":1000000001},"metrics":{"time_to_first_token":{"value":10},"output_sequence":{"value":[1,2,3]}}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := parseRecords(path)
	if err != nil || result.Requests != 1 || result.TTFTP50MS == nil || *result.TTFTP50MS != 10 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestReproductionCommandRedactsCredential(t *testing.T) {
	command := shellCommand("aiperf", []string{"profile", "--model", "Qwen/Qwen3-8B", "--api-key", "top-secret"}, "top-secret", "INFERCRANE_API_KEY")
	if strings.Contains(command, "top-secret") || !strings.Contains(command, "${INFERCRANE_API_KEY}") {
		t.Fatalf("command=%s", command)
	}
}

func TestRunUsesIndependentServingModelAndTokenizer(t *testing.T) {
	runner := &capturingRunner{}
	_, _ = run(context.Background(), Config{Endpoint: "http://example", Model: "logical-deployment", Tokenizer: "Qwen/Qwen3-8B", Requests: 1, Concurrency: 1}, runner)
	command := strings.Join(runner.profileArgs, " ")
	if !strings.Contains(command, "--model logical-deployment") || !strings.Contains(command, "--tokenizer Qwen/Qwen3-8B") || !strings.Contains(command, "--synthetic-input-tokens-mean 128") || !strings.Contains(command, "--output-tokens-mean 32") {
		t.Fatalf("args=%v", runner.profileArgs)
	}
}

func TestRunSelectsStreamingAndBufferedAIPerfModes(t *testing.T) {
	for _, test := range []struct {
		name          string
		mode          *bool
		wantStreaming bool
	}{
		{name: "default-streaming", wantStreaming: true},
		{name: "streaming", mode: benchmarkBool(true), wantStreaming: true},
		{name: "buffered", mode: benchmarkBool(false), wantStreaming: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &capturingRunner{}
			_, _ = run(context.Background(), Config{Endpoint: "http://example", Model: "m", Requests: 1, Concurrency: 1, Streaming: test.mode}, runner)
			hasStreaming := false
			for _, arg := range runner.profileArgs {
				if arg == "--streaming" {
					hasStreaming = true
				}
				if arg == "--no-streaming" {
					t.Fatalf("AIPerf does not define --no-streaming: args=%v", runner.profileArgs)
				}
			}
			if hasStreaming != test.wantStreaming {
				t.Fatalf("args=%v hasStreaming=%t want=%t", runner.profileArgs, hasStreaming, test.wantStreaming)
			}
		})
	}
}

func benchmarkBool(value bool) *bool { return &value }

type capturingRunner struct{ profileArgs []string }

func (r *capturingRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) == 1 && args[0] == "--version" {
		return []byte("aiperf " + AIPerfVersion), nil
	}
	r.profileArgs = append([]string(nil), args...)
	return nil, os.ErrNotExist
}

type exportingRunner struct{}

func (exportingRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) == 1 && args[0] == "--version" {
		return []byte("aiperf " + AIPerfVersion), nil
	}
	value := func(flag string) string {
		for index := range args {
			if args[index] == flag && index+1 < len(args) {
				return args[index+1]
			}
		}
		return ""
	}
	directory, prefix := value("--artifact-dir"), value("--profile-export-prefix")
	runs := 1
	if encoded := value("--num-profile-runs"); encoded != "" {
		if _, err := fmt.Sscan(encoded, &runs); err != nil {
			return nil, err
		}
	}
	for index := 1; index <= runs; index++ {
		target := directory
		if runs > 1 {
			target = filepath.Join(directory, "profile_runs", fmt.Sprintf("run_%04d", index))
		}
		if err := os.MkdirAll(target, 0o700); err != nil {
			return nil, err
		}
		data := fmt.Sprintf(`{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":1000000001},"metrics":{"time_to_first_token":{"value":%d,"unit":"ms"},"request_latency":{"value":30,"unit":"ms"},"inter_token_latency":{"value":2,"unit":"ms"},"output_token_count":{"value":20,"unit":"tokens"}},"error":null}`+"\n", 10+index)
		if err := os.WriteFile(filepath.Join(target, prefix+".jsonl"), []byte(data), 0o600); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func TestRunConsumesAIPerfMultiRunRecordLayout(t *testing.T) {
	result, err := run(context.Background(), Config{Endpoint: "http://example", Model: "m", Requests: 1, Concurrency: 1, ProfileRuns: 3}, exportingRunner{})
	if err != nil || result.ProfileRuns != 3 || result.Requests != 3 || result.OutputTokenThroughput == nil || *result.OutputTokenThroughput != 20 || result.Confidence["ttft_p95_ms"].Samples != 3 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestReproductionCommandDoesNotPersistDeletedTemporaryPath(t *testing.T) {
	temporary := "/tmp/infercrane-aiperf-123"
	command := portableReproductionCommand(shellCommand("aiperf", []string{"profile", "--artifact-dir", temporary, "--profile-export-prefix", "infercrane"}, "", "INFERCRANE_API_KEY"), temporary)
	if strings.Contains(command, temporary) || !strings.Contains(command, "--artifact-dir ./infercrane-benchmark-artifacts") || !strings.Contains(command, "--profile-export-prefix infercrane") {
		t.Fatalf("command=%s", command)
	}
}

func TestRunBuildsRepeatableArrivalPatternCampaign(t *testing.T) {
	runner := &capturingRunner{}
	_, _ = run(context.Background(), Config{Endpoint: "http://example", Model: "m", Requests: 50, Concurrency: 4, ProfileRuns: 5, WarmupRequests: 8, ProfileRunCooldown: 3 * time.Second, ArrivalPattern: "poisson", RequestRate: 7.5}, runner)
	command := strings.Join(runner.profileArgs, " ")
	for _, expected := range []string{"--num-profile-runs 5", "--warmup-request-count 8", "--profile-run-cooldown-seconds 3", "--arrival-pattern poisson", "--request-rate 7.5"} {
		if !strings.Contains(command, expected) {
			t.Fatalf("missing %q in args=%v", expected, runner.profileArgs)
		}
	}
}

func TestRunUsesAIPerfCustomTraceWithoutSyntheticPromptFlags(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "workload.jsonl")
	if err := os.WriteFile(trace, []byte(`{"text":"redacted fixture"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &capturingRunner{}
	_, _ = run(context.Background(), Config{Endpoint: "http://example", Model: "m", Requests: 1, Concurrency: 1, InputFile: trace, DatasetType: "single_turn"}, runner)
	command := strings.Join(runner.profileArgs, " ")
	if !strings.Contains(command, "--input-file "+trace+" --custom-dataset-type single_turn") || strings.Contains(command, "--synthetic-input-tokens-mean") {
		t.Fatalf("trace boundary was not preserved: args=%v", runner.profileArgs)
	}
}

func TestParseMultipleProfileRunsPoolsMetricsAndComputesConfidence(t *testing.T) {
	directory := t.TempDir()
	paths := make([]string, 0, 3)
	for index, duration := range []int64{1, 2, 4} {
		path := filepath.Join(directory, fmt.Sprintf("run-%d.jsonl", index))
		data := fmt.Sprintf(`{"metadata":{"benchmark_phase":"profiling","request_start_ns":1,"request_end_ns":%d},"metrics":{"time_to_first_token":{"value":%d},"request_latency":{"value":%d},"inter_token_latency":{"value":2},"output_token_count":{"value":20}}}`+"\n", duration*int64(time.Second)+1, 10+index, 30+index)
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	result, err := parseRecordFilesWithSLO(paths, Config{})
	if err != nil || result.Requests != 3 || result.DurationSeconds != 7 || result.OutputTokenThroughput == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	confidence := profileConfidence(paths, Config{})
	interval, ok := confidence["request_throughput"]
	if !ok || interval.Samples != 3 || interval.Upper95 <= interval.Lower95 {
		t.Fatalf("confidence=%+v", confidence)
	}
}

func TestAIPerfVersionRequiresPinnedTool(t *testing.T) {
	if version, err := aiperfVersion("AIPerf version 0.12.0"); err != nil || version != AIPerfVersion {
		t.Fatalf("version=%q err=%v", version, err)
	}
}

type missingRunner struct{}

func (missingRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, os.ErrNotExist
}
func TestRunReportsMissingAIPerf(t *testing.T) {
	_, err := run(context.Background(), Config{Endpoint: "http://example", Model: "m", Requests: 1, Concurrency: 1}, missingRunner{})
	if err == nil || !strings.Contains(err.Error(), "pipx install aiperf") {
		t.Fatalf("err=%v", err)
	}
}
