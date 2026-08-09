package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
)

func captureStdout(t *testing.T, run func() error) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	runErr := run()
	os.Stdout = original
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return string(output), runErr
}

func TestDeployCLIOnlySubmitsControlPlaneRequest(t *testing.T) {
	var path, key string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("Idempotency-Key")
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing API authentication")
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending"}}`))
	}))
	defer server.Close()

	err := deployAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{"Qwen/Qwen3-8B", "--cloud", "runpod", "--gpu", "L40S", "--min", "1", "--max", "4", "--idempotency-key", "release-1"})
	if err != nil || path != "/api/v1/deployments" || key != "release-1" || body["cloud"] != "runpod" || body["max_replicas"] != float64(4) {
		t.Fatalf("path=%s key=%s body=%#v err=%v", path, key, body, err)
	}
}

func TestDoctorCLIOnlyReadsAuthenticatedControlPlaneDiagnostics(t *testing.T) {
	var requested string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.RequestURI()
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("method=%s authorization=%q", r.Method, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ready":true,"checks":[{"name":"PostgreSQL","status":"pass","message":"Database connection succeeded"}]}`))
	}))
	defer server.Close()

	output, err := captureStdout(t, func() error {
		return doctorCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"--cloud", "--serverless", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(output), &report); err != nil || report["ready"] != true {
		t.Fatalf("output=%q report=%#v err=%v", output, report, err)
	}
	if requested != "/api/v1/doctor?cloud=true&serverless=true" {
		t.Fatalf("request=%q", requested)
	}
}

func TestPrimaryDeployPathDefaultsToRunPodL40S(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending"}}`))
	}))
	defer server.Close()

	err := deployAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{"Qwen/Qwen3-8B", "--idempotency-key", "release-1"})
	if err != nil || body["cloud"] != "runpod" || body["gpu"] != "L40S" {
		t.Fatalf("body=%#v err=%v", body, err)
	}
}

func TestServerlessDeployDefaultsToZeroMinimumWorkers(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending"}}`))
	}))
	defer server.Close()

	err := deployAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{"Qwen/Qwen3-8B", "--compute", "serverless", "--max", "4", "--idempotency-key", "release-serverless"})
	minimum, hasMinimum := body["min_replicas"]
	if err != nil || body["compute_mode"] != "serverless" || (hasMinimum && minimum != float64(0)) || body["max_replicas"] != float64(4) || body["cloud"] != "runpod" || body["gpu"] != "L40S" {
		t.Fatalf("body=%#v err=%v", body, err)
	}
}

func TestDeployYAMLPreservesArtifactRuntimeAndServerlessFields(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "deployment.yaml")
	if err := os.WriteFile(filename, []byte(`
name: qwen-serverless
model:
  id: Qwen/Qwen3-8B
  revision: 0123456789abcdef0123456789abcdef01234567
runtime:
  engine: vllm
  version: 0.10.2
  args: [--enable-prefix-caching]
compute: {mode: serverless}
resources: {gpu: L40S}
provider: {cloud: runpod, region: EU-RO-1}
scaling: {min_replicas: 0, max_replicas: 4}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending"}}`))
	}))
	defer server.Close()

	err := deployAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{filename, "--idempotency-key", "yaml-1"})
	if err != nil || body["compute_mode"] != "serverless" || body["min_replicas"] != nil || body["max_replicas"] != float64(4) || body["model_revision"] == nil || body["runtime_version"] != "0.10.2" || body["region"] != "EU-RO-1" {
		t.Fatalf("body=%#v err=%v", body, err)
	}
	args, _ := body["runtime_args"].([]any)
	if len(args) != 1 || args[0] != "--enable-prefix-caching" {
		t.Fatalf("runtime_args=%#v", body["runtime_args"])
	}
}

func TestDeleteCLIOnlySubmitsControlPlaneRequest(t *testing.T) {
	var method, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{"operation":{"id":"op-delete","status":"pending"}}`))
	}))
	defer server.Close()

	err := deleteAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"qwen prod", "--yes", "--idempotency-key", "delete-1"})
	if err != nil || method != http.MethodDelete || path != "/api/v1/deployments/qwen prod" {
		t.Fatalf("method=%s path=%s err=%v", method, path, err)
	}
}

func TestDeletePlanHonorsJSONWithoutControlPlaneMutation(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()

	output, err := captureStdout(t, func() error {
		return deleteAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"qwen-prod", "--plan", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Deployment string   `json:"deployment"`
		Actions    []string `json:"actions"`
	}
	if err := json.Unmarshal([]byte(output), &plan); err != nil {
		t.Fatalf("output is not one JSON document: %q: %v", output, err)
	}
	if requests != 0 || plan.Deployment != "qwen-prod" || len(plan.Actions) != 2 {
		t.Fatalf("requests=%d plan=%#v", requests, plan)
	}
}

func TestDeployWaitJSONReturnsOneFinalDocument(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending","progress":0}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"op-1","status":"succeeded","progress":100,"message":"ready"}`))
	}))
	defer server.Close()

	output, err := captureStdout(t, func() error {
		return deployAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{"Qwen/Qwen3-8B", "--wait", "--output", "json", "--idempotency-key", "release-1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Operation struct {
			Status   string `json:"status"`
			Progress int    `json:"progress"`
		} `json:"operation"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not one JSON document: %q: %v", output, err)
	}
	if result.Operation.Status != "succeeded" || result.Operation.Progress != 100 || strings.Count(output, "\n{") != 0 {
		t.Fatalf("unexpected waited result: %s", output)
	}
}

func TestDeployDisconnectLeavesDurableOperationRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"operation":{"id":"op-durable","status":"pending"}}`))
			return
		}
		cancel()
		_, _ = w.Write([]byte(`{"id":"op-durable","status":"running","progress":25}`))
	}))
	defer server.Close()

	_, err := captureStdout(t, func() error {
		return deployAPICommand(ctx, config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{"Qwen/Qwen3-8B", "--wait", "--idempotency-key", "disconnect-1"})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected disconnect cancellation, got %v", err)
	}
	if strings.Join(methods, ",") != "POST,GET" {
		t.Fatalf("disconnect must not submit a cancellation mutation: methods=%v", methods)
	}
}

func TestInvalidOutputIsRejectedBeforeAnyControlPlaneRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	tests := map[string]func() error{
		"plan": func() error {
			return planCommand(context.Background(), cfg, []string{"Qwen/Qwen3-8B", "--output", "xml"})
		},
		"doctor":      func() error { return doctorCommand(context.Background(), cfg, []string{"--output", "xml"}) },
		"benchmark":   func() error { return benchmarkCommand(context.Background(), cfg, []string{"qwen", "--output", "xml"}) },
		"deployments": func() error { return listDeployments(context.Background(), cfg, []string{"--output", "xml"}) },
		"status":      func() error { return statusCommand(context.Background(), cfg, []string{"qwen", "--output", "xml"}) },
		"orphans":     func() error { return orphanAPICommand(context.Background(), cfg, []string{"--output", "xml"}) },
		"inspect":     func() error { return inspectCommand(context.Background(), cfg, []string{"qwen", "--output", "xml"}) },
		"events":      func() error { return eventsCommand(context.Background(), cfg, []string{"qwen", "--output", "xml"}) },
		"explain":     func() error { return explainCommand(context.Background(), cfg, []string{"qwen", "--output", "xml"}) },
		"explain scaling": func() error {
			return explainCommand(context.Background(), cfg, []string{"scaling", "qwen", "--output", "xml"})
		},
		"explain rollout": func() error {
			return explainCommand(context.Background(), cfg, []string{"rollout", "qwen", "--output", "xml"})
		},
		"explain cold": func() error {
			return explainCommand(context.Background(), cfg, []string{"cold-start", "qwen", "--output", "xml"})
		},
		"rollout inspect": func() error {
			return rolloutCommand(context.Background(), cfg, []string{"inspect", "qwen", "--output", "xml"})
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			before := requests
			if err := run(); err == nil || !strings.Contains(err.Error(), "--output") {
				t.Fatalf("err=%v", err)
			}
			if requests != before {
				t.Fatalf("control-plane requests=%d, want %d", requests, before)
			}
		})
	}
}

func TestBenchmarkParsesFlagsAfterDeploymentName(t *testing.T) {
	var path string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"benchmark":{"id":"benchmark-1"}}`))
	}))
	defer server.Close()

	_, err := captureStdout(t, func() error {
		return benchmarkCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"qwen prod", "--requests", "40", "--concurrency", "4", "--revision", "candidate", "--output", "json"})
	})
	if err != nil || path != "/api/v1/deployments/qwen prod/benchmarks" || body["requests"] != float64(40) || body["concurrency"] != float64(4) || body["revision"] != "candidate" {
		t.Fatalf("path=%q body=%#v err=%v", path, body, err)
	}
}

func TestExplainReportsPersistedBlockingOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "deployment":{"name":"qwen","observed_state":"degraded","active_revision_id":"rev-1"},
  "replicas":[],"targets":[],"revisions":[],"model_artifacts":[],"release_guard_evaluations":[],
  "active_operation":{"id":"op-7","kind":"deployment.converge","status":"waiting","progress":35,"message":"provider capacity pending","error_code":"provider_pending"}
}`))
	}))
	defer server.Close()

	output, err := captureStdout(t, func() error {
		return explainCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"qwen", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var explanation struct {
		BlockingOperation *struct {
			ID string `json:"id"`
		} `json:"blocking_operation"`
		Reasons []string `json:"reasons"`
	}
	if err := json.Unmarshal([]byte(output), &explanation); err != nil || explanation.BlockingOperation == nil || explanation.BlockingOperation.ID != "op-7" || len(explanation.Reasons) != 1 || !strings.Contains(explanation.Reasons[0], "provider_pending") {
		t.Fatalf("output=%s explanation=%#v err=%v", output, explanation, err)
	}
}

func TestRolloutInspectFormatsPersistedGuardComparison(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "deployment":{"name":"qwen","active_revision_id":"rev-active","candidate_revision_id":"rev-candidate"},
  "targets":[],"replicas":[],"revisions":[],"model_artifacts":[],
  "release_guard_evaluations":[{
    "id":"guard-1","active_revision_id":"rev-active","candidate_revision_id":"rev-candidate","decision":"REJECT",
    "metrics":{"active":{"requests":40,"ready_replicas":1,"error_rate":0.001,"p95_ttft_ms":221},"candidate":{"requests":40,"ready_replicas":1,"error_rate":0.002,"p95_ttft_ms":317}},
    "reasons":[{"code":"ttft_regression","message":"Candidate TTFT regression 43.4% exceeds policy"}],
    "policy":{},"created_at":"2026-08-09T12:00:00Z"
  }]
}`))
	}))
	defer server.Close()

	output, err := captureStdout(t, func() error {
		return rolloutCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"inspect", "qwen"})
	})
	if err != nil || !strings.Contains(output, "TTFT p95") || !strings.Contains(output, "221.0ms") || !strings.Contains(output, "317.0ms") || !strings.Contains(output, "Guard: REJECT") || !strings.Contains(output, "ttft_regression") || strings.Contains(output, `\"active\"`) {
		t.Fatalf("output=%s err=%v", output, err)
	}
}

func TestControlErrorPreservesTaxonomyAndRemediation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"benchmark_unavailable","category":"dependency","message":"AIPerf is unavailable","retryable":true,"remediation":"Install the configured AIPerf version."}}`))
	}))
	defer server.Close()
	err := controlJSON(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, http.MethodGet, "/api/v1/test", "", nil, nil)
	var controlErr *ControlError
	if !errors.As(err, &controlErr) || controlErr.Code != "benchmark_unavailable" || controlErr.Category != "dependency" || !controlErr.Retryable || controlErr.Remediation == "" || !strings.Contains(err.Error(), "next:") {
		t.Fatalf("error=%#v rendered=%v", controlErr, err)
	}
}
