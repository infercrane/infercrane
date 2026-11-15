package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// cliRecorder is a fake control plane that answers the routes the JSON
// contract test exercises and records every request it receives.
type cliRecorder struct {
	mu       sync.Mutex
	requests []string
}

func (r *cliRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.requests...)
}

func (r *cliRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.requests = append(r.requests, req.Method+" "+req.URL.RequestURI())
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	route := req.Method + " " + req.URL.Path
	switch route {
	case "GET /api/v1/integrations":
		_, _ = io.WriteString(w, `{"data":{"provider_contract":"infercrane.provider/v1","runtime_contract":"infercrane.runtime/v1","providers":[],"runtimes":[]}}`)
	case "GET /api/v1/deployments/qwen":
		_, _ = io.WriteString(w, `{"deployment":{"name":"qwen","model":"Qwen/Qwen3-8B","runtime":"vllm","active_revision_id":"rev-1"},"lifecycle_status":{"serving_state":"serving","convergence_state":"converged"},"request_stats":{}}`)
	case "GET /api/v1/deployments/qwen/events":
		_, _ = io.WriteString(w, `{"data":[{"type":"provider.capacity","summary":"capacity accepted","created_at":"2026-08-09T12:00:00Z"},{"type":"runtime.ready","summary":"model ready","created_at":"2026-08-09T12:01:00Z"}]}`)
	case "GET /api/v1/deployments/qwen/passports":
		_, _ = io.WriteString(w, `{"data":[]}`)
	case "PUT /api/v1/deployments/qwen/route":
		_, _ = io.WriteString(w, `{"deployment":"qwen","routing_strategy":"cache-aware"}`)
	case "GET /api/v1/operations/op-1":
		_, _ = io.WriteString(w, `{"id":"op-1","kind":"deployment.create","status":"running","progress":40,"created_at":"2026-08-09T12:00:00Z"}`)
	case "GET /api/v1/operations":
		_, _ = io.WriteString(w, `{"data":[{"id":"op-1","kind":"deployment.create","resource_type":"deployment","resource_name":"qwen","status":"running","progress":40,"created_at":"2026-08-09T12:00:00Z"}]}`)
	case "GET /api/v1/endpoints/coder":
		_, _ = io.WriteString(w, `{"endpoint":{"name":"coder","observed_state":"ready"},"logical_model":{"name":"coder"},"environment":{"name":"production"},"active_plan":{"id":"plan-1","routing_policy":"manual"},"bindings":[]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","category":"client","message":"not found","retryable":false}}`)
	}
}

// runCLI runs the CLI as main does and returns stdout and stderr, with a
// returned error rendered to stderr by writeCLIError.
func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	outReader, outWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errReader, errWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	var copying sync.WaitGroup
	copying.Add(2)
	go func() { defer copying.Done(); _, _ = io.Copy(&stdout, outReader) }()
	go func() { defer copying.Done(); _, _ = io.Copy(&stderr, errReader) }()
	originalOut, originalErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outWriter, errWriter
	runErr := run(context.Background(), args)
	if runErr != nil {
		writeCLIError(os.Stderr, args, runErr)
	}
	os.Stdout, os.Stderr = originalOut, originalErr
	_ = outWriter.Close()
	_ = errWriter.Close()
	copying.Wait()
	return stdout.String(), stderr.String(), runErr
}

func useFakeControlPlane(t *testing.T) *cliRecorder {
	t.Helper()
	recorder := &cliRecorder{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("INFERCRANE_CONTEXT", "")
	t.Setenv("NO_COLOR", "")
	return recorder
}

func withoutJSONOutput(args []string) []string {
	var human []string
	for index := 0; index < len(args); index++ {
		if args[index] == "--output" && index+1 < len(args) {
			index++
			continue
		}
		human = append(human, args[index])
	}
	return human
}

// Every command a client may script with --output json must print JSON on
// stdout when it succeeds and a JSON error object on stderr when it fails,
// never Go's flag usage dump, and never use a flag as a resource name.
func TestJSONOutputIsAlwaysMachineReadable(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantError string
		jsonLines bool
	}{
		{name: "version", args: []string{"version", "--output", "json"}},
		{name: "global output before the command", args: []string{"--output", "json", "version"}},
		{name: "integrations", args: []string{"integrations", "--output", "json"}},
		{name: "integrations list", args: []string{"integrations", "list", "--output", "json"}},
		{name: "integrations stray argument", args: []string{"integrations", "everything", "--output", "json"}, wantError: "usage: infercrane integrations"},
		{name: "logs", args: []string{"logs", "qwen", "--output", "json"}, jsonLines: true},
		{name: "logs without deployment", args: []string{"logs", "--output", "json"}, wantError: "usage: infercrane logs DEPLOYMENT"},
		{name: "passport list", args: []string{"passport", "list", "qwen", "--output", "json"}},
		{name: "passport list without deployment", args: []string{"passport", "list", "--output", "json"}, wantError: "usage: infercrane passport list DEPLOYMENT"},
		{name: "plan without model", args: []string{"plan", "--output", "json"}, wantError: "usage: infercrane plan MODEL"},
		{name: "route", args: []string{"route", "qwen", "--strategy", "cache-aware", "--output", "json"}},
		{name: "route list", args: []string{"route", "list", "--output", "json"}, wantError: "usage: infercrane route DEPLOYMENT --strategy STRATEGY"},
		{name: "status", args: []string{"status", "qwen", "--output", "json"}},
		{name: "status without deployment", args: []string{"status", "--output", "json"}, wantError: "usage: infercrane status DEPLOYMENT"},
		{name: "status flag before deployment", args: []string{"status", "--watch", "qwen", "--output", "json"}, wantError: "usage: infercrane status DEPLOYMENT"},
		{name: "status stray argument", args: []string{"status", "qwen", "extra", "--output", "json"}, wantError: "usage: infercrane status DEPLOYMENT"},
		{name: "status unknown flag", args: []string{"status", "qwen", "--bogus", "--output", "json"}, wantError: "flag provided but not defined: -bogus"},
		{name: "status no-color after command", args: []string{"status", "qwen", "--no-color", "--output", "json"}},
		{name: "events", args: []string{"events", "qwen", "--output", "json"}},
		{name: "events without deployment", args: []string{"events", "--output", "json"}, wantError: "usage: infercrane events DEPLOYMENT"},
		{name: "explain", args: []string{"explain", "qwen", "--output", "json"}},
		{name: "explain without deployment", args: []string{"explain", "--output", "json"}, wantError: "usage: infercrane explain"},
		{name: "explain cold-start without deployment", args: []string{"explain", "cold-start", "--output", "json"}, wantError: "usage: infercrane explain cold-start DEPLOYMENT"},
		{name: "inspect", args: []string{"inspect", "qwen", "--output", "json"}},
		{name: "inspect without deployment", args: []string{"inspect", "--output", "json"}, wantError: "usage: infercrane inspect DEPLOYMENT"},
		{name: "operation list", args: []string{"operation", "list", "--output", "json"}},
		{name: "operation without ID", args: []string{"operation", "--output", "json"}, wantError: "usage: infercrane operation [inspect] ID"},
		{name: "operation inspect", args: []string{"operation", "inspect", "op-1", "--output", "json"}},
		{name: "operation inspect without ID", args: []string{"operation", "inspect", "--output", "json"}, wantError: "usage: infercrane operation [inspect] ID"},
		{name: "observe endpoint", args: []string{"observe", "coder", "--output", "json"}},
		{name: "observe without subject", args: []string{"observe", "--output", "json"}, wantError: "usage: infercrane observe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := useFakeControlPlane(t)
			stdout, stderr, err := runCLI(t, test.args...)
			if strings.Contains(stderr, "Usage of") {
				t.Fatalf("stderr contains Go's flag usage dump:\n%s", stderr)
			}
			for _, request := range recorder.seen() {
				if strings.Contains(request, "-output") || strings.Contains(request, "/json") {
					t.Fatalf("a flag reached the control plane as a resource name: %s", request)
				}
			}
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("run(%v) error = %v; stderr=%s", test.args, err, stderr)
				}
				assertJSONStdout(t, stdout, test.jsonLines)
				return
			}
			if err == nil || stdout != "" {
				t.Fatalf("run(%v) err=%v stdout=%q, want a usage error and no stdout", test.args, err, stdout)
			}
			var envelope struct {
				Error struct {
					Code     string `json:"code"`
					Category string `json:"category"`
					Message  string `json:"message"`
				} `json:"error"`
			}
			if decodeErr := json.Unmarshal([]byte(stderr), &envelope); decodeErr != nil {
				t.Fatalf("stderr is not one JSON error object: %v\n%s", decodeErr, stderr)
			}
			if envelope.Error.Code != "client_error" || envelope.Error.Category != "client" || !strings.Contains(envelope.Error.Message, test.wantError) {
				t.Fatalf("error envelope = %+v, want client_error containing %q", envelope.Error, test.wantError)
			}
			if requests := recorder.seen(); len(requests) != 0 {
				t.Fatalf("a usage error contacted the control plane: %v", requests)
			}
			_, _, humanErr := runCLI(t, withoutJSONOutput(test.args)...)
			if humanErr == nil || humanErr.Error() != err.Error() {
				t.Fatalf("human error = %v, JSON error = %v; both modes must report the same usage error", humanErr, err)
			}
		})
	}
}

func assertJSONStdout(t *testing.T, stdout string, jsonLines bool) {
	t.Helper()
	if !jsonLines {
		var document any
		if err := json.Unmarshal([]byte(stdout), &document); err != nil {
			t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
		}
		return
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if stdout == "" || len(lines) == 0 {
		t.Fatal("JSON Lines output is empty")
	}
	for _, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil || record["type"] == nil {
			t.Fatalf("JSON Lines record %q is not an event object: %v", line, err)
		}
	}
}

func TestCommandFlagSetsNeverPrintGoUsage(t *testing.T) {
	if newFlagSet("probe").Output() != io.Discard {
		t.Fatal("newFlagSet must discard Go's usage output")
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "cliargs.go" {
			continue
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte("flag.NewFlagSet(")) {
			t.Errorf("%s builds a flag set without newFlagSet; Go would print its usage dump", file)
		}
	}
}

func TestGlobalFlagsWorkBeforeAndAfterTheCommand(t *testing.T) {
	tests := []struct {
		args, want []string
		noColor    bool
	}{
		{args: []string{"--no-color", "deployments"}, want: []string{"deployments"}, noColor: true},
		{args: []string{"deployments", "--no-color"}, want: []string{"deployments"}, noColor: true},
		{args: []string{"--output", "json", "status", "qwen"}, want: []string{"status", "qwen", "--output", "json"}},
		{args: []string{"--output=json", "--no-color", "status", "qwen", "--watch"}, want: []string{"status", "qwen", "--watch", "--output=json"}, noColor: true},
		{args: []string{"--output", "json", "workload", "dev", "--", "--no-color"}, want: []string{"workload", "dev", "--output", "json", "--", "--no-color"}},
	}
	for _, test := range tests {
		t.Setenv("NO_COLOR", "")
		got, err := applyGlobalFlags(test.args)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("applyGlobalFlags(%v) = %v, %v; want %v", test.args, got, err, test.want)
		}
		if test.noColor != (os.Getenv("NO_COLOR") != "") {
			t.Fatalf("applyGlobalFlags(%v) NO_COLOR=%q, want set=%t", test.args, os.Getenv("NO_COLOR"), test.noColor)
		}
	}
	if _, err := applyGlobalFlags([]string{"--output"}); err == nil {
		t.Fatal("a leading --output without a value was accepted")
	}

	useFakeControlPlane(t)
	if _, stderr, err := runCLI(t, "status", "qwen", "--no-color"); err != nil || os.Getenv("NO_COLOR") != "1" {
		t.Fatalf("--no-color after the command: err=%v NO_COLOR=%q stderr=%s", err, os.Getenv("NO_COLOR"), stderr)
	}
}

func TestVersionJSONIncludesBuildInformation(t *testing.T) {
	stdout, _, err := runCLI(t, "version", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var info versionInfo
	if err = json.Unmarshal([]byte(stdout), &info); err != nil || info.Version != version || info.Build["go_version"] == "" || info.Build["platform"] == "" {
		t.Fatalf("version JSON = %s (err=%v)", stdout, err)
	}
	human, _, err := runCLI(t, "version")
	if err != nil || human != version+"\n" {
		t.Fatalf("human version = %q err=%v", human, err)
	}
}

func TestOperationListFindsRecentOperations(t *testing.T) {
	recorder := useFakeControlPlane(t)
	stdout, stderr, err := runCLI(t, "operation", "list", "--limit", "1", "--before", "2026-08-10T00:00:00Z")
	if err != nil {
		t.Fatalf("err=%v stderr=%s", err, stderr)
	}
	for _, expected := range []string{"op-1", "deployment.create", "deployment/qwen", "running", "40%", "infercrane operation list --before 2026-08-09T12:00:00Z"} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("operation list output missing %q:\n%s", expected, stdout)
		}
	}
	if requests := recorder.seen(); len(requests) != 1 || requests[0] != "GET /api/v1/operations?before=2026-08-10T00%3A00%3A00Z&limit=1" {
		t.Fatalf("requests = %v", requests)
	}
	for _, args := range [][]string{
		{"operation", "list", "--limit", "0"},
		{"operation", "list", "--limit", "501"},
		{"operation", "list", "--before", "yesterday"},
		{"operation", "list", "op-1"},
	} {
		recorder := useFakeControlPlane(t)
		if _, _, err := runCLI(t, args...); err == nil {
			t.Fatalf("run(%v) accepted invalid input", args)
		}
		if requests := recorder.seen(); len(requests) != 0 {
			t.Fatalf("run(%v) contacted the control plane: %v", args, requests)
		}
	}
}

func TestObserveNeverPrintsGoNilForAbsentFields(t *testing.T) {
	useFakeControlPlane(t)
	human, _, err := runCLI(t, "observe", "coder")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(human, "<nil>") || !strings.Contains(human, "Candidate      none") || !strings.Contains(human, "Policy         manual") {
		t.Fatalf("observe human output:\n%s", human)
	}
	stdout, _, err := runCLI(t, "observe", "coder", "--output", "json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Endpoint map[string]json.RawMessage `json:"endpoint"`
	}
	if err = json.Unmarshal([]byte(stdout), &snapshot); err != nil {
		t.Fatal(err)
	}
	if candidate, present := snapshot.Endpoint["candidate_plan"]; !present || string(candidate) != "null" {
		t.Fatalf("candidate_plan = %q (present=%t), want explicit null", candidate, present)
	}

	text, _ := captureStdout(t, func() error {
		printObserveSnapshot(observeSnapshot{Kind: "endpoint", Name: "bare", Endpoint: map[string]any{}, Guard: map[string]any{"evaluations": []map[string]any{{}}}, Findings: []map[string]any{{}}})
		printObserveSnapshot(observeSnapshot{Kind: "deployment", Name: "qwen", Deployment: &deploymentView{}})
		return nil
	})
	if strings.Contains(text, "<nil>") || !strings.Contains(text, "Candidate     none") || !strings.Contains(text, "Logical model  unknown") || !strings.Contains(text, "Decision       unknown") {
		t.Fatalf("observe printed absent fields as Go values:\n%s", text)
	}
}
