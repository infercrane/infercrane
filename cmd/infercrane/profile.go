package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/gputrace"
	"github.com/infercrane/infercrane/internal/metrics"
)

// Stable JSON schemas of `infercrane profile --output json`.
const (
	profileResultSchema     = "infercrane.profile-result/v2"
	profileListSchema       = "infercrane.profile-list/v1"
	profileComparisonSchema = "infercrane.profile-comparison/v1"
)

const (
	minProfileDuration = 2 * time.Second
	maxProfileDuration = 30 * time.Second
	profileUsage       = `usage: infercrane profile ENDPOINT [--engine-url URL --trace-dir DIR [--duration 10s] [--dcgm-url URL] [--yes]] [--trace PATH ...] [--list] [--compare BASE[,HEAD]] [--capture ID] [--open] [--output json]`
)

// Test seams.
var (
	profileNow   = func() time.Time { return time.Now().UTC() }
	profileSleep = func(ctx context.Context, wait time.Duration) error {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	profilePoll                  = time.Second
	profileStdin       io.Reader = os.Stdin
	profileStdinIsTTY            = func() bool { return isTerminal(os.Stdin) }
	profileProgress    io.Writer = os.Stderr
	profileEngineCheck           = 5 * time.Second
)

type profileOptions struct {
	endpoint, deployment                 string
	engineURL, traceDir, engineTraceDir  string
	engine, engineKeyEnv                 string
	duration, flushTimeout               time.Duration
	dcgmURL, dcgmSelector                string
	yes, noUpload, open, list            bool
	traces                               []string
	format, accelerator, compare, output string
	captureID                            string
	peakTFLOPS, peakTBps                 float64
}

// ProfileResult is the stable JSON output of a capture, a trace analysis, or
// a stored capture read.
type ProfileResult struct {
	SchemaVersion string               `json:"schema_version"`
	Endpoint      string               `json:"endpoint"`
	Deployment    *string              `json:"deployment"`
	Source        string               `json:"source"`
	CaptureID     *string              `json:"capture_id"`
	Revision      *int                 `json:"revision"`
	Uploaded      bool                 `json:"uploaded"`
	DashboardURL  *string              `json:"dashboard_url"`
	TraceFiles    []string             `json:"trace_files"`
	Capture       ProfileCapture       `json:"capture"`
	Analysis      *gpuprofile.Analysis `json:"analysis"`
	Reason        string               `json:"reason"`
}

// ProfileCapture is how and when a capture was taken.
type ProfileCapture struct {
	Tool        string    `json:"tool"`
	ToolVersion string    `json:"tool_version"`
	Accelerator *string   `json:"accelerator"`
	GPUs        *int      `json:"gpus"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func profileCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(profileUsage)
	}
	options := profileOptions{endpoint: args[0]}
	fs := newFlagSet("profile")
	fs.StringVar(&options.deployment, "deployment", "", "deployment the capture belongs to (default: ENDPOINT)")
	fs.StringVar(&options.engineURL, "engine-url", "", "serving engine base URL that has /start_profile and /stop_profile")
	fs.StringVar(&options.traceDir, "trace-dir", "", "directory, readable here, where the engine writes its traces")
	fs.StringVar(&options.engineTraceDir, "engine-trace-dir", "", "the same directory as the engine sees it (default: --trace-dir)")
	fs.StringVar(&options.engine, "engine", "auto", "auto, vllm, or sglang")
	fs.StringVar(&options.engineKeyEnv, "engine-key-env", "", "environment variable that holds a bearer key for the engine")
	fs.DurationVar(&options.duration, "duration", 10*time.Second, "how long to record, 2s to 30s")
	fs.DurationVar(&options.flushTimeout, "flush-timeout", 5*time.Minute, "how long to wait for the engine to write its trace")
	fs.StringVar(&options.dcgmURL, "dcgm-url", "", "dcgm-exporter metrics URL to sample every second during the capture")
	fs.StringVar(&options.dcgmSelector, "dcgm-selector", "", "comma-separated exact label selectors for --dcgm-url")
	fs.BoolVar(&options.yes, "yes", false, "capture without asking, accepting that the replica slows while it runs")
	fs.Var((*stringList)(&options.traces), "trace", "analyze an existing trace file or directory instead of capturing (repeatable)")
	fs.StringVar(&options.format, "format", "", "trace format for --trace: kineto, rocprofv3, or nsys (default: detect)")
	fs.StringVar(&options.accelerator, "accelerator", "", "accelerator name when the trace does not say, for the roofline peak")
	fs.Float64Var(&options.peakTFLOPS, "peak-tflops", 0, "peak dense 16-bit tensor TFLOPS, when no datasheet peak is listed")
	fs.Float64Var(&options.peakTBps, "peak-tbps", 0, "peak memory bandwidth in TB/s, with --peak-tflops")
	fs.BoolVar(&options.noUpload, "no-upload", false, "analyze locally without recording the capture")
	fs.BoolVar(&options.open, "open", false, "open the capture's timeline in the dashboard")
	fs.BoolVar(&options.list, "list", false, "list the endpoint's retained captures")
	fs.StringVar(&options.compare, "compare", "", "compare two captures: BASE[,HEAD]; HEAD defaults to the newest")
	fs.StringVar(&options.captureID, "capture", "", "read this capture instead of the newest")
	fs.StringVar(&options.output, "output", "human", "human or json")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(profileUsage)
	}
	if err := validateOutput(options.output); err != nil {
		return err
	}
	if options.deployment == "" {
		options.deployment = options.endpoint
	}
	capturing := options.engineURL != "" || options.traceDir != ""
	modes := 0
	for _, active := range []bool{capturing, len(options.traces) > 0, options.list, options.compare != "", options.captureID != ""} {
		if active {
			modes++
		}
	}
	if modes > 1 {
		return errors.New("choose one of: a capture (--engine-url and --trace-dir), --trace, --list, --compare, or --capture")
	}
	if (options.peakTFLOPS > 0) != (options.peakTBps > 0) || options.peakTFLOPS < 0 || options.peakTBps < 0 {
		return errors.New("--peak-tflops and --peak-tbps go together and must be positive")
	}
	switch {
	case options.list:
		return profileList(ctx, cfg, options)
	case options.compare != "":
		return profileCompare(ctx, cfg, options)
	case capturing:
		if options.engineURL == "" || options.traceDir == "" {
			return errors.New("a capture needs both --engine-url and --trace-dir")
		}
		return profileCapture(ctx, cfg, options)
	case len(options.traces) > 0:
		trace, err := gputrace.ParseFiles(options.traces, options.format)
		if err != nil {
			return err
		}
		capture := gputrace.CaptureFromTrace(trace, traceTool(trace.Format), "unknown", profileNow())
		return profileFinish(ctx, cfg, options, trace, capture, gputrace.Options{}, "trace_files", options.traces)
	default:
		return profileStored(ctx, cfg, options)
	}
}

// profileIsLocal reports whether a profile command only analyzes locally.
func profileIsLocal(args []string) bool {
	for _, arg := range args {
		if arg == "--no-upload" || arg == "--no-upload=true" {
			return true
		}
	}
	return false
}

func traceTool(format string) string {
	switch format {
	case gputrace.FormatRocprofV3:
		return "rocprofv3"
	case gputrace.FormatNsys:
		return "nsight-systems"
	default:
		return "torch-profiler"
	}
}

// profileCapture records a bounded trace through the engine's own profiler
// endpoints, waits for the engine to write it, and analyzes it.
func profileCapture(ctx context.Context, cfg config.Config, options profileOptions) error {
	if options.duration < minProfileDuration || options.duration > maxProfileDuration {
		return fmt.Errorf("--duration must be %s to %s", minProfileDuration, maxProfileDuration)
	}
	engineURL, err := url.Parse(strings.TrimRight(options.engineURL, "/"))
	if err != nil || (engineURL.Scheme != "http" && engineURL.Scheme != "https") || engineURL.Host == "" || engineURL.User != nil {
		return errors.New("--engine-url must be an HTTP(S) URL without embedded credentials")
	}
	info, err := os.Stat(options.traceDir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("--trace-dir %s is not a readable directory", options.traceDir)
	}
	if options.engineTraceDir == "" {
		options.engineTraceDir = options.traceDir
	}
	engine := &engineClient{base: engineURL.String(), client: &http.Client{Timeout: options.flushTimeout}}
	if options.engineKeyEnv != "" {
		if engine.key = os.Getenv(options.engineKeyEnv); engine.key == "" {
			return fmt.Errorf("%s is empty", options.engineKeyEnv)
		}
	}
	kind, version, err := engine.detect(ctx, options.engine)
	if err != nil {
		return err
	}
	fmt.Fprintf(profileProgress, "Profiling %s for %s with %s's own profiler (PyTorch profiler, CUPTI).\n", options.endpoint, options.duration, kind)
	fmt.Fprintln(profileProgress, "This slows the replica while it records: the engine traces every kernel launch. InferCrane does not measure the slowdown, and no request content is recorded.")
	if !options.yes {
		if !profileStdinIsTTY() {
			return errors.New("pass --yes to confirm that the capture slows this replica while it runs")
		}
		fmt.Fprint(profileProgress, "Start the capture? [y/N] ")
		answer, _ := bufio.NewReader(profileStdin).ReadString('\n')
		if answer = strings.ToLower(strings.TrimSpace(answer)); answer != "y" && answer != "yes" {
			return errors.New("capture cancelled")
		}
	}
	before, err := traceSnapshot(options.traceDir)
	if err != nil {
		return err
	}
	prefix := "infercrane-" + randomHex(6)
	var sampler *dcgmSampler
	if options.dcgmURL != "" {
		selector, selectorErr := parseMetricSelector(options.dcgmSelector)
		if selectorErr != nil {
			return selectorErr
		}
		sampler = &dcgmSampler{url: options.dcgmURL, selector: selector}
	}
	started := profileNow()
	if err = engine.start(ctx, kind, options.engineTraceDir, prefix); err != nil {
		return err
	}
	samplerCtx, stopSampler := context.WithCancel(ctx)
	var samplerDone sync.WaitGroup
	if sampler != nil {
		sampler.interval = time.Second
		sampler.sample(ctx)
		samplerDone.Add(1)
		go func() { defer samplerDone.Done(); sampler.run(samplerCtx) }()
	}
	waitErr := profileSleep(ctx, options.duration)
	stopSampler()
	samplerDone.Wait()
	// Always stop the profiler, even when interrupted, so the replica does not
	// keep tracing.
	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), options.flushTimeout)
	defer cancelStop()
	stopErr := engine.stop(stopCtx)
	ended := profileNow()
	if waitErr != nil {
		return fmt.Errorf("capture interrupted; the profiler was stopped: %w", waitErr)
	}
	if stopErr != nil {
		return stopErr
	}
	fmt.Fprintln(profileProgress, "Waiting for the engine to write its trace…")
	files, err := waitForTraces(stopCtx, options.traceDir, before, prefix, started)
	if err != nil {
		return err
	}
	trace, err := gputrace.ParseFiles(files, gputrace.FormatKineto)
	if err != nil {
		return err
	}
	analysisOptions := gputrace.Options{Engine: kind, EngineVersion: version}
	if sampler != nil {
		analysisOptions.Hardware = sampler.series()
	}
	toolVersion := kind
	if version != "" {
		toolVersion = kind + "-" + version
	}
	capture := gputrace.Capture{Tool: "torch-profiler", ToolVersion: toolVersion, StartedAt: started, EndedAt: ended}
	return profileFinish(ctx, cfg, options, trace, capture, analysisOptions, "capture", files)
}

// profileFinish analyzes a parsed trace, records it unless --no-upload, and
// prints the verdict.
func profileFinish(ctx context.Context, cfg config.Config, options profileOptions, trace *gputrace.Trace, capture gputrace.Capture, analysisOptions gputrace.Options, source string, files []string) error {
	analysisOptions.Accelerator = options.accelerator
	if options.peakTFLOPS > 0 {
		analysisOptions.Peak = &gpuprofile.Peak{ComputeTFLOPS: options.peakTFLOPS, Precision: "16-bit dense tensor (operator-declared)", MemoryTBps: options.peakTBps, Source: "operator"}
	}
	if !versionLike(capture.ToolVersion) {
		capture.ToolVersion = "unknown"
	}
	recording, err := gputrace.Profile(trace, analysisOptions, capture)
	if err != nil {
		return err
	}
	result := ProfileResult{
		SchemaVersion: profileResultSchema, Endpoint: options.endpoint, Source: source, TraceFiles: files, Analysis: recording.Analysis,
		Capture: ProfileCapture{Tool: recording.Capture.Tool, ToolVersion: recording.Capture.ToolVersion, Accelerator: recording.Capture.Accelerator, GPUs: recording.Capture.GPUs, StartedAt: recording.Capture.StartedAt, EndedAt: recording.Capture.EndedAt},
	}
	if !options.noUpload {
		var receipt gpuprofile.Receipt
		path := "/api/v1/deployments/" + url.PathEscape(options.deployment) + "/gpu-profiles"
		if err = controlJSONWithTimeout(ctx, cfg, http.MethodPost, path, "", recording, &receipt, 2*time.Minute); err != nil {
			var control *ControlError
			if errors.As(err, &control) && control.Code == "not_found" {
				return fmt.Errorf("no deployment named %s; pass --deployment with the deployment behind endpoint %s, or --no-upload to analyze only", options.deployment, options.endpoint)
			}
			return fmt.Errorf("record the capture (rerun with --no-upload to analyze only): %w", err)
		}
		result.Uploaded = true
		result.Deployment, result.CaptureID, result.Revision = &receipt.Deployment, &receipt.ID, &receipt.Revision
		if link := profileDashboardURL(cfg, options.endpoint, receipt.ID); link != "" {
			result.DashboardURL = &link
		}
	}
	return profilePrint(result, options)
}

// profileStored reads the newest capture of the endpoint's active revision,
// or one named by --capture.
func profileStored(ctx context.Context, cfg config.Config, options profileOptions) error {
	path := "/api/v1/endpoints/" + url.PathEscape(options.endpoint) + "/gpu-profile"
	if options.captureID != "" {
		path = "/api/v1/endpoints/" + url.PathEscape(options.endpoint) + "/gpu-profiles/" + url.PathEscape(options.captureID)
	}
	var profile gpuprofile.Profile
	if err := controlJSONWithTimeout(ctx, cfg, http.MethodGet, path, "", nil, &profile, time.Minute); err != nil {
		var control *ControlError
		if errors.As(err, &control) && control.Code == "gpu_profile_not_found" {
			return fmt.Errorf("no GPU profile is recorded for %s; capture one on the GPU host with: infercrane profile %s --engine-url http://127.0.0.1:8000 --trace-dir /profiles --duration 10s", options.endpoint, options.endpoint)
		}
		return err
	}
	result := ProfileResult{
		SchemaVersion: profileResultSchema, Endpoint: options.endpoint, Deployment: &profile.Deployment, Source: "stored", CaptureID: &profile.Capture.ID,
		Revision: &profile.Revision, Uploaded: true, TraceFiles: []string{}, Analysis: profile.Analysis,
		Capture: ProfileCapture{Tool: profile.Capture.Tool, ToolVersion: profile.Capture.ToolVersion, Accelerator: profile.Capture.Accelerator, GPUs: profile.Capture.GPUs, StartedAt: profile.Capture.StartedAt, EndedAt: profile.Capture.EndedAt},
	}
	if profile.Analysis == nil {
		result.Reason = "This capture has call stacks only, without a trace analysis. Capture again with infercrane profile for the analysis."
	}
	if link := profileDashboardURL(cfg, options.endpoint, profile.Capture.ID); link != "" {
		result.DashboardURL = &link
	}
	return profilePrint(result, options)
}

func profileDashboardURL(cfg config.Config, endpoint, capture string) string {
	query := url.Values{"tab": {"profiling"}, "endpoint": {endpoint}, "capture": {capture}}
	return dashboardURL(cfg, "/monitoring?"+query.Encode())
}

func profilePrint(result ProfileResult, options profileOptions) error {
	if result.TraceFiles == nil {
		result.TraceFiles = []string{}
	}
	if options.open && result.DashboardURL != nil {
		if err := openBrowser(*result.DashboardURL); err != nil {
			fmt.Fprintf(profileProgress, "Open %s\n", *result.DashboardURL)
		}
	}
	if options.output == "json" {
		return printJSON(result)
	}
	fmt.Print(renderProfile(result))
	return nil
}

func profileList(ctx context.Context, cfg config.Config, options profileOptions) error {
	summaries, err := listProfiles(ctx, cfg, options.endpoint)
	if err != nil {
		return err
	}
	if options.output == "json" {
		return printJSON(map[string]any{"schema_version": profileListSchema, "endpoint": options.endpoint, "data": summaries})
	}
	if len(summaries) == 0 {
		fmt.Printf("No GPU profiles are retained for %s.\n", options.endpoint)
		return nil
	}
	fmt.Printf("GPU profiles · %s\n\n", options.endpoint)
	for _, summary := range summaries {
		marker := " "
		if summary.Active {
			marker = "*"
		}
		verdict := "call stacks only"
		if summary.Verdict != nil {
			verdict = summary.Verdict.Sentence
		}
		fmt.Printf("%s %s  rev %-3d %s  %-15s %s\n", marker, summary.ID, summary.Revision, summary.EndedAt.Format("2006-01-02 15:04"), summary.Tool, verdict)
	}
	fmt.Println("\n* serving revision · compare two with --compare BASE,HEAD")
	return nil
}

func listProfiles(ctx context.Context, cfg config.Config, endpoint string) ([]gpuprofile.Summary, error) {
	var response struct {
		Data []gpuprofile.Summary `json:"data"`
	}
	if err := controlJSONWithTimeout(ctx, cfg, http.MethodGet, "/api/v1/endpoints/"+url.PathEscape(endpoint)+"/gpu-profiles", "", nil, &response, time.Minute); err != nil {
		return nil, err
	}
	if response.Data == nil {
		response.Data = []gpuprofile.Summary{}
	}
	return response.Data, nil
}

func profileCompare(ctx context.Context, cfg config.Config, options profileOptions) error {
	base, head, _ := strings.Cut(options.compare, ",")
	if head == "" {
		summaries, err := listProfiles(ctx, cfg, options.endpoint)
		if err != nil {
			return err
		}
		for _, summary := range summaries {
			if summary.ID != base {
				head = summary.ID
				break
			}
		}
		if head == "" {
			return fmt.Errorf("%s has no other capture to compare with %s", options.endpoint, base)
		}
	}
	query := url.Values{"base": {base}, "head": {head}}
	var comparison gpuprofile.Comparison
	if err := controlJSONWithTimeout(ctx, cfg, http.MethodGet, "/api/v1/endpoints/"+url.PathEscape(options.endpoint)+"/gpu-profiles/compare?"+query.Encode(), "", nil, &comparison, time.Minute); err != nil {
		return err
	}
	if options.output == "json" {
		return printJSON(struct {
			SchemaVersion string `json:"schema_version"`
			Endpoint      string `json:"endpoint"`
			gpuprofile.Comparison
		}{profileComparisonSchema, options.endpoint, comparison})
	}
	fmt.Print(renderComparison(options.endpoint, comparison))
	return nil
}

func versionLike(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for index, character := range value {
		alphanumeric := character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
		if index == 0 && !alphanumeric || !alphanumeric && !strings.ContainsRune("._+-", character) {
			return false
		}
	}
	return true
}

func randomHex(bytes int) string {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buffer)
}

// engineClient calls a serving engine's profiler endpoints.
type engineClient struct {
	base, key string
	client    *http.Client
}

func (e *engineClient) call(ctx context.Context, method, path string, body any, timeout time.Duration) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, e.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if e.key != "" {
		request.Header.Set("Authorization", "Bearer "+e.key)
	}
	response, err := e.client.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("engine %s is unreachable: %w", e.base, err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	return response.StatusCode, data, nil
}

// detect names the engine: SGLang answers /get_server_info, vLLM /version.
func (e *engineClient) detect(ctx context.Context, requested string) (string, string, error) {
	var payload struct {
		Version string `json:"version"`
	}
	probe := func(path string) (bool, string) {
		status, data, err := e.call(ctx, http.MethodGet, path, nil, profileEngineCheck)
		if err != nil || status != http.StatusOK || json.Unmarshal(data, &payload) != nil {
			return false, ""
		}
		return true, payload.Version
	}
	switch requested {
	case "sglang":
		_, version := probe("/get_server_info")
		return "sglang", cleanVersion(version), nil
	case "vllm":
		_, version := probe("/version")
		return "vllm", cleanVersion(version), nil
	case "auto", "":
		if ok, version := probe("/get_server_info"); ok {
			return "sglang", cleanVersion(version), nil
		}
		if ok, version := probe("/version"); ok {
			return "vllm", cleanVersion(version), nil
		}
		return "", "", fmt.Errorf("%s answered neither SGLang's /get_server_info nor vLLM's /version; pass --engine vllm or --engine sglang", e.base)
	default:
		return "", "", errors.New("--engine must be auto, vllm, or sglang")
	}
}

func cleanVersion(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if !versionLike(version) {
		return ""
	}
	return version
}

func (e *engineClient) start(ctx context.Context, engine, directory, prefix string) error {
	body := map[string]any{"profile_prefix": prefix}
	if engine == "sglang" {
		body = map[string]any{"output_dir": directory, "activities": []string{"CPU", "GPU"}, "record_shapes": true, "with_stack": false, "profile_prefix": prefix}
	}
	status, data, err := e.call(ctx, http.MethodPost, "/start_profile", body, 30*time.Second)
	switch {
	case err != nil:
		return err
	case status == http.StatusNotFound && engine == "vllm":
		return errors.New(`vLLM serves /start_profile only when started with --profiler-config, for example --profiler-config '{"profiler":"torch","torch_profiler_dir":"/profiles"}'`)
	case status == http.StatusConflict:
		return errors.New("a profiling session is already running on this engine; stop it with POST /stop_profile first")
	case status < 200 || status >= 300:
		return fmt.Errorf("the engine refused /start_profile: HTTP %d %s", status, strings.TrimSpace(printableSnippet(data)))
	}
	return nil
}

func (e *engineClient) stop(ctx context.Context) error {
	status, data, err := e.call(ctx, http.MethodPost, "/stop_profile", nil, e.client.Timeout)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("the engine refused /stop_profile: HTTP %d %s", status, strings.TrimSpace(printableSnippet(data)))
	}
	return nil
}

func printableSnippet(data []byte) string {
	text := strings.Map(func(r rune) rune {
		if r < 0x20 && r != ' ' {
			return ' '
		}
		return r
	}, string(data))
	if len(text) > 200 {
		text = text[:200]
	}
	return text
}

type traceFile struct {
	size    int64
	modTime time.Time
}

func traceSnapshot(directory string) (map[string]traceFile, error) {
	snapshot := map[string]traceFile{}
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".json") && !strings.HasSuffix(path, ".json.gz") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		snapshot[path] = traceFile{info.Size(), info.ModTime()}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read --trace-dir: %w", err)
	}
	return snapshot, nil
}

// waitForTraces returns the trace files the engine wrote for this capture
// once their sizes stop changing: files named with the capture's prefix when
// there are any, otherwise files that are new or rewritten since the start.
func waitForTraces(ctx context.Context, directory string, before map[string]traceFile, prefix string, started time.Time) ([]string, error) {
	var previous map[string]int64
	for {
		current, err := traceSnapshot(directory)
		if err != nil {
			return nil, err
		}
		fresh := map[string]int64{}
		named := map[string]int64{}
		for path, file := range current {
			old, existed := before[path]
			if existed && old == file || file.size == 0 {
				continue
			}
			if !existed || file.modTime.After(started.Add(-time.Second)) {
				fresh[path] = file.size
				if strings.Contains(filepath.Base(path), prefix) {
					named[path] = file.size
				}
			}
		}
		if len(named) > 0 {
			fresh = named
		}
		if len(fresh) > 0 && equalSizes(fresh, previous) {
			files := make([]string, 0, len(fresh))
			for path := range fresh {
				files = append(files, path)
			}
			sort.Strings(files)
			if len(files) > gputrace.MaxTraceFiles {
				return nil, fmt.Errorf("the engine wrote %d trace files; one capture merges at most %d", len(files), gputrace.MaxTraceFiles)
			}
			return files, nil
		}
		previous = fresh
		if err = profileSleep(ctx, profilePoll); err != nil {
			return nil, fmt.Errorf("the engine wrote no complete trace to %s before --flush-timeout: %w", directory, err)
		}
	}
}

func equalSizes(left, right map[string]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for path, size := range left {
		if right[path] != size {
			return false
		}
	}
	return true
}

// dcgmSampler reads dcgm-exporter once a second during a capture.
type dcgmSampler struct {
	url      string
	selector map[string]string
	samples  []map[string]float64
	interval time.Duration
}

var dcgmCaptureFields = []struct{ name, unit string }{
	{metrics.MeasurementGPUSMActive, "percent"}, {metrics.MeasurementGPUSMOccupancy, "percent"}, {metrics.MeasurementGPUTensorActive, "percent"},
	{metrics.MeasurementGPUDRAMActive, "percent"}, {metrics.MeasurementGPUPCIeThroughput, "bytes_per_second"}, {metrics.MeasurementGPUNVLinkThroughput, "bytes_per_second"},
}

// sample reads the exporter once; a failed read is a sample with no fields.
func (d *dcgmSampler) sample(ctx context.Context) {
	sample := map[string]float64{}
	if payload, err := readCollectorPayload(ctx, d.url, ""); err == nil {
		if rows, parseErr := metrics.ParseDCGM(string(payload), metrics.DCGMOptions{Selector: d.selector, ObservedAt: profileNow(), TTL: time.Minute}); parseErr == nil {
			for _, row := range rows {
				sample[row.Name] = row.Value
			}
		}
	}
	d.samples = append(d.samples, sample)
}

// run samples once per interval after the first sample, until cancelled.
func (d *dcgmSampler) run(ctx context.Context) {
	for len(d.samples) < gpuprofile.MaxSeries {
		if profileSleep(ctx, d.interval) != nil {
			return
		}
		d.sample(ctx)
	}
}

// series keeps every field that was sent at least once; a sample that missed
// a field is null there, never zero.
func (d *dcgmSampler) series() *gpuprofile.HardwareSeries {
	result := &gpuprofile.HardwareSeries{Source: "dcgm-exporter", IntervalMS: float64(d.interval / time.Millisecond)}
	for _, field := range dcgmCaptureFields {
		values := make([]*float64, len(d.samples))
		seen := false
		for index, sample := range d.samples {
			if value, ok := sample[field.name]; ok {
				if field.unit == "percent" {
					value = min(value, 100)
				}
				values[index], seen = &value, true
			}
		}
		if seen {
			result.Series = append(result.Series, gpuprofile.FieldSeries{Field: field.name, Unit: field.unit, Values: values})
		}
	}
	if len(result.Series) == 0 {
		fmt.Fprintln(profileProgress, "dcgm-exporter sent none of the profiling fields during the capture; hardware counters stay unknown.")
		return nil
	}
	return result
}
