package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/testtools/tracefixture"
)

var updateProfileGolden = flag.Bool("update-profile-golden", false, "rewrite the profile JSON golden")

// fakeEngine answers like vLLM or SGLang and writes a Kineto trace into dir
// when the profiler stops.
type fakeEngine struct {
	mu         sync.Mutex
	kind       string
	dir        string
	started    map[string]any
	stopped    int
	startCode  int
	traceBase  time.Time
	authHeader string
}

func (f *fakeEngine) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.authHeader = r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/get_server_info" && f.kind == "sglang":
			_, _ = io.WriteString(w, `{"version":"0.5.12","tp_size":1}`)
		case r.URL.Path == "/version" && f.kind == "vllm":
			_, _ = io.WriteString(w, `{"version":"0.22.1"}`)
		case r.URL.Path == "/start_profile" && r.Method == http.MethodPost:
			if f.startCode != 0 {
				w.WriteHeader(f.startCode)
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&f.started)
		case r.URL.Path == "/stop_profile" && r.Method == http.MethodPost:
			f.stopped++
			prefix, _ := f.started["profile_prefix"].(string)
			name := filepath.Join(f.dir, prefix+"-rank-0.pt.trace.json.gz")
			if err := os.WriteFile(name, tracefixture.VLLMSingleGPU(f.traceBase), 0o600); err != nil {
				t.Errorf("write trace: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	})
}

type fakeProfileControl struct {
	mu       sync.Mutex
	recorded []gpuprofile.Recording
	paths    []string
}

func (f *fakeProfileControl) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.paths = append(f.paths, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/deployments/coder/gpu-profiles":
			var recording gpuprofile.Recording
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&recording); err != nil {
				t.Errorf("decode recording: %v", err)
			}
			if err := gpuprofile.Validate(gpuprofile.Normalize(recording), time.Now().UTC()); err != nil {
				t.Errorf("the CLI posted an invalid recording: %v", err)
			}
			f.recorded = append(f.recorded, recording)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(gpuprofile.Receipt{ID: "gpc-" + strings.Repeat("a", 32), Deployment: "coder", Revision: 4, StartedAt: recording.Capture.StartedAt, EndedAt: recording.Capture.EndedAt, Frames: len(recording.Frames), Stacks: len(recording.Stacks), RecordedAt: time.Now().UTC()})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"deployment was not found"}}`)
		default:
			http.NotFound(w, r)
		}
	})
}

func fastProfileClock(t *testing.T) {
	t.Helper()
	previousSleep, previousPoll, previousTTY, previousProgress := profileSleep, profilePoll, profileStdinIsTTY, profileProgress
	profileSleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	profilePoll = time.Millisecond
	profileStdinIsTTY = func() bool { return false }
	profileProgress = io.Discard
	t.Cleanup(func() {
		profileSleep, profilePoll, profileStdinIsTTY, profileProgress = previousSleep, previousPoll, previousTTY, previousProgress
	})
}

func TestProfileCapturesThroughTheEngineAndRecordsTheAnalysis(t *testing.T) {
	fastProfileClock(t)
	for _, kind := range []string{"vllm", "sglang"} {
		t.Run(kind, func(t *testing.T) {
			directory := t.TempDir()
			// A trace left by an earlier session must not be read.
			if err := os.WriteFile(filepath.Join(directory, "old.pt.trace.json.gz"), []byte("stale"), 0o600); err != nil {
				t.Fatal(err)
			}
			engine := &fakeEngine{kind: kind, dir: directory, traceBase: time.Now().UTC().Add(-time.Minute)}
			engineServer := httptest.NewServer(engine.handler(t))
			defer engineServer.Close()
			exporter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "DCGM_FI_PROF_SM_ACTIVE{gpu=\"0\",UUID=\"GPU-1\"} 0.42\nDCGM_FI_PROF_DRAM_ACTIVE{gpu=\"0\",UUID=\"GPU-1\"} 0.71\n")
			}))
			defer exporter.Close()
			control := &fakeProfileControl{}
			controlServer := httptest.NewServer(control.handler(t))
			defer controlServer.Close()
			t.Setenv("ENGINE_KEY", "engine-secret")
			output, err := captureStdout(t, func() error {
				return profileCommand(context.Background(), config.Config{ControlURL: controlServer.URL, APIKey: "secret", ConsoleURL: "https://console.example.test"}, []string{
					"coder", "--engine-url", engineServer.URL, "--trace-dir", directory, "--duration", "2s", "--dcgm-url", exporter.URL + "/metrics",
					"--engine-key-env", "ENGINE_KEY", "--yes", "--output", "json",
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			var result ProfileResult
			if err = json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatalf("output=%s err=%v", output, err)
			}
			if result.SchemaVersion != profileResultSchema || !result.Uploaded || result.CaptureID == nil || result.Source != "capture" || result.Analysis == nil || len(result.TraceFiles) != 1 || strings.Contains(result.TraceFiles[0], "old.") {
				t.Fatalf("result %+v", result)
			}
			if result.DashboardURL == nil || *result.DashboardURL != "https://console.example.test/monitoring?capture=gpc-"+strings.Repeat("a", 32)+"&endpoint=coder&tab=profiling" {
				t.Fatalf("dashboard link %v", result.DashboardURL)
			}
			if engine.stopped != 1 || engine.authHeader != "Bearer engine-secret" {
				t.Fatalf("stopped=%d auth=%q", engine.stopped, engine.authHeader)
			}
			prefix, _ := engine.started["profile_prefix"].(string)
			if !strings.HasPrefix(prefix, "infercrane-") {
				t.Fatalf("start body %v", engine.started)
			}
			if kind == "sglang" && (engine.started["output_dir"] != directory || engine.started["record_shapes"] != true) {
				t.Fatalf("SGLang needs the output directory and shapes: %v", engine.started)
			}
			if len(control.recorded) != 1 {
				t.Fatalf("recorded %d", len(control.recorded))
			}
			recording := control.recorded[0]
			if recording.SchemaVersion != gpuprofile.SchemaVersionV2 || recording.Capture.ToolVersion != map[string]string{"vllm": "vllm-0.22.1", "sglang": "sglang-0.5.12"}[kind] {
				t.Fatalf("capture %+v", recording.Capture)
			}
			hardware := recording.Analysis.Hardware
			if hardware == nil || len(hardware.Series) != 2 || hardware.Series[0].Field != "gpu_sm_active" || hardware.Series[1].Field != "gpu_dram_active" || *hardware.Series[0].Values[0] != 42 {
				t.Fatalf("DCGM during the capture: %+v", hardware)
			}
		})
	}
}

func TestProfileCaptureNeedsConsentAndExplainsMissingProfiler(t *testing.T) {
	fastProfileClock(t)
	directory := t.TempDir()
	engine := &fakeEngine{kind: "vllm", dir: directory, traceBase: time.Now().UTC()}
	server := httptest.NewServer(engine.handler(t))
	defer server.Close()
	cfg := config.Config{ControlURL: "http://127.0.0.1:1", APIKey: "secret"}
	err := profileCommand(context.Background(), cfg, []string{"coder", "--engine-url", server.URL, "--trace-dir", directory})
	if err == nil || !strings.Contains(err.Error(), "--yes") || engine.started != nil {
		t.Fatalf("a non-interactive capture must ask for --yes before starting: %v", err)
	}
	profileStdinIsTTY = func() bool { return true }
	profileStdin = strings.NewReader("n\n")
	defer func() { profileStdin = os.Stdin }()
	if err = profileCommand(context.Background(), cfg, []string{"coder", "--engine-url", server.URL, "--trace-dir", directory}); err == nil || !strings.Contains(err.Error(), "cancelled") || engine.started != nil {
		t.Fatalf("answering no must not start: %v", err)
	}
	engine.startCode = http.StatusNotFound
	if err = profileCommand(context.Background(), cfg, []string{"coder", "--engine-url", server.URL, "--trace-dir", directory, "--yes"}); err == nil || !strings.Contains(err.Error(), "--profiler-config") {
		t.Fatalf("a vLLM without the profiler names the flag: %v", err)
	}
	engine.startCode = http.StatusConflict
	if err = profileCommand(context.Background(), cfg, []string{"coder", "--engine-url", server.URL, "--trace-dir", directory, "--yes"}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("a running session is named: %v", err)
	}
	for name, args := range map[string][]string{
		"duration":     {"coder", "--engine-url", server.URL, "--trace-dir", directory, "--duration", "2m", "--yes"},
		"credentials":  {"coder", "--engine-url", "http://user:pass@127.0.0.1:8000", "--trace-dir", directory, "--yes"},
		"half capture": {"coder", "--engine-url", server.URL},
		"two modes":    {"coder", "--list", "--trace", directory},
		"peak alone":   {"coder", "--trace", directory, "--peak-tflops", "100"},
		"no endpoint":  {"--list"},
	} {
		if err = profileCommand(context.Background(), cfg, args); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// TestProfileJSONIsStable pins the JSON field names of a local analysis so
// scripts can rely on them.
func TestProfileJSONIsStable(t *testing.T) {
	sample := filepath.Join("..", "..", "internal", "gputrace", "testdata", "vllm-h100")
	output, err := captureStdout(t, func() error {
		return profileCommand(context.Background(), config.Config{}, []string{"coder", "--trace", sample, "--no-upload", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err = json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatal(err)
	}
	keys := jsonKeyPaths(document, "")
	golden := filepath.Join("testdata", "profile-result-keys.golden")
	got := strings.Join(keys, "\n") + "\n"
	if *updateProfileGolden {
		if err = os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("profile JSON fields changed; a change to infercrane.profile-result/v2 needs a new schema version.\n%s", got)
	}
	if document["schema_version"] != profileResultSchema || document["uploaded"] != false || document["capture_id"] != nil {
		t.Fatalf("document %v", document)
	}
}

func jsonKeyPaths(value any, prefix string) []string {
	var paths []string
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			paths = append(paths, path)
			paths = append(paths, jsonKeyPaths(child, path)...)
		}
	case []any:
		if len(typed) > 0 {
			paths = append(paths, jsonKeyPaths(typed[0], prefix+"[]")...)
		}
	}
	sort.Strings(paths)
	unique := paths[:0]
	for index, path := range paths {
		if index == 0 || path != paths[index-1] {
			unique = append(unique, path)
		}
	}
	return unique
}

func TestProfileReadsHistoryAndComparesThroughTheAPI(t *testing.T) {
	before, after := "gpc-"+strings.Repeat("1", 32), "gpc-"+strings.Repeat("2", 32)
	var requested []string
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/v1/endpoints/coder/gpu-profiles":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []gpuprofile.Summary{{ID: after, Deployment: "coder", Revision: 4, Active: true, Tool: "torch-profiler"}, {ID: before, Deployment: "coder", Revision: 3, Tool: "torch-profiler"}}})
		case "/api/v1/endpoints/coder/gpu-profiles/compare":
			busy, idle := 0.64, 0.92
			improved := true
			delta := 0.28
			_ = json.NewEncoder(w).Encode(gpuprofile.Comparison{Base: gpuprofile.Summary{ID: before, Revision: 3}, Head: gpuprofile.Summary{ID: after, Revision: 4, Active: true}, Warnings: []string{},
				Metrics: []gpuprofile.MetricDelta{{Metric: "busy_share", Label: "GPU busy", Unit: "ratio", Base: &busy, Head: &idle, Delta: &delta, Better: "higher", Improved: &improved}}, Categories: []gpuprofile.CategoryComparison{}, Kernels: []gpuprofile.KernelComparison{}})
		case "/api/v1/endpoints/coder/gpu-profile":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"gpu_profile_not_found","message":"no GPU profile"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer control.Close()
	cfg := config.Config{ControlURL: control.URL, APIKey: "secret"}
	output, err := captureStdout(t, func() error {
		return profileCommand(context.Background(), cfg, []string{"coder", "--compare", before, "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var comparison struct {
		SchemaVersion string `json:"schema_version"`
		gpuprofile.Comparison
	}
	if err = json.Unmarshal([]byte(output), &comparison); err != nil || comparison.SchemaVersion != profileComparisonSchema || comparison.Head.ID != after {
		t.Fatalf("output=%s err=%v", output, err)
	}
	if last := requested[len(requested)-1]; !strings.Contains(last, "base="+before) || !strings.Contains(last, "head="+after) {
		t.Fatalf("compare defaults HEAD to the newest other capture: %s", last)
	}
	text, err := captureStdout(t, func() error {
		return profileCommand(context.Background(), cfg, []string{"coder", "--compare", before + "," + after})
	})
	if err != nil || !strings.Contains(text, "GPU busy") || !strings.Contains(text, "better") {
		t.Fatalf("text=%s err=%v", text, err)
	}
	listed, err := captureStdout(t, func() error {
		return profileCommand(context.Background(), cfg, []string{"coder", "--list", "--output", "json"})
	})
	if err != nil || !strings.Contains(listed, profileListSchema) {
		t.Fatalf("list=%s err=%v", listed, err)
	}
	if err = profileCommand(context.Background(), cfg, []string{"coder"}); err == nil || !strings.Contains(err.Error(), "--engine-url") {
		t.Fatalf("a missing capture names the capture command: %v", err)
	}
}

func TestProfileHumanOutputLeadsWithTheVerdict(t *testing.T) {
	sample := filepath.Join("..", "..", "internal", "gputrace", "testdata", "sglang-tp2")
	output, err := captureStdout(t, func() error {
		return profileCommand(context.Background(), config.Config{}, []string{"coder", "--trace", sample, "--no-upload"})
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(output, "\n")
	if !strings.HasPrefix(lines[0], "GPU profile · coder") || !strings.HasPrefix(lines[2], "Collective communication") || !bytes.Contains([]byte(output), []byte("Analyzed locally; not recorded")) {
		t.Fatalf("output:\n%s", output)
	}
}
