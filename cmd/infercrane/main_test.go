package main

import (
	"bytes"
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
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/support"
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

func TestCommandHelpDoesNotRequireAuthentication(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	output, err := captureStdout(t, func() error {
		return run(context.Background(), []string{"deploy", "--help"})
	})
	if err != nil || !strings.Contains(output, "infercrane deploy MODEL") {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestNestedCommandHelpIsSafeAndSuccessful(t *testing.T) {
	t.Setenv("INFERCRANE_API_KEY", "")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	for _, args := range [][]string{
		{"workload", "init", "--help"},
		{"optimize", "propose", "--help"},
		{"endpoint", "create", "--help"},
		{"rollout", "guard", "--help"},
	} {
		t.Run(strings.Join(args[:2], "-"), func(t *testing.T) {
			output, err := captureStdout(t, func() error {
				return run(context.Background(), args)
			})
			if err != nil {
				t.Fatalf("run(%v) error = %v", args, err)
			}
			if !strings.Contains(output, "Usage:") || !strings.Contains(output, "infercrane "+args[0]) {
				t.Fatalf("run(%v) returned incomplete help: %q", args, output)
			}
		})
	}
}

func TestOptimizeHelpMatchesCampaignWorkflow(t *testing.T) {
	output, err := captureStdout(t, func() error {
		return run(context.Background(), []string{"optimize", "propose", "--help"})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"approve|activate|cancel",
		"--max-error-rate",
		"--min-goodput",
		"--target-deployment",
		"--candidate",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("optimize help is missing %q:\n%s", expected, output)
		}
	}
}

func TestJSONCapableCommandsAdvertiseOutputFlag(t *testing.T) {
	for _, name := range []string{"auth", "system", "secret", "external", "rollout", "target"} {
		t.Run(name, func(t *testing.T) {
			output, err := captureStdout(t, func() error {
				return run(context.Background(), []string{name, "--help"})
			})
			if err != nil || !strings.Contains(output, "--output string") {
				t.Fatalf("output=%q err=%v", output, err)
			}
		})
	}
}

func TestIntelligenceCommandsRejectUnsupportedOutputBeforeNetworkAccess(t *testing.T) {
	cfg := config.Config{ControlURL: "http://127.0.0.1:1", APIKey: "secret"}
	tests := []struct {
		name string
		run  func() error
	}{
		{"replay", func() error { return replayCommand(context.Background(), cfg, []string{"prod", "--output", "yaml"}) }},
		{"capacity", func() error { return capacityCommand(context.Background(), cfg, []string{"--output", "yaml"}) }},
		{"finops", func() error { return finOpsCommand(context.Background(), cfg, []string{"prod", "--output", "yaml"}) }},
		{"autopilot", func() error {
			return autopilotCommand(context.Background(), cfg, []string{"plan", "prod", "--output", "yaml"})
		}},
		{"session", func() error {
			return sessionCommand(context.Background(), cfg, []string{"inspect", "session-1", "--output", "yaml"})
		}},
		{"burst", func() error { return burstCommand(context.Background(), cfg, []string{"prod", "--output", "yaml"}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil || err.Error() != "--output must be human or json" {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestGPUCountIsNeverSilentlyIgnoredForExistingCapacity(t *testing.T) {
	cfg := config.Config{ControlURL: "http://127.0.0.1:1", APIKey: "secret"}
	tests := []struct {
		name string
		run  func() error
	}{
		{"plan", func() error {
			return planCommand(context.Background(), cfg, []string{"model", "--targets", "worker-a", "--gpu-count", "4"})
		}},
		{"deploy", func() error {
			return deployAPICommand(context.Background(), cfg, "deploy", []string{"model", "--targets", "worker-a", "--gpu-count", "4"})
		}},
		{"rollout", func() error {
			return rolloutCommand(context.Background(), cfg, []string{"create", "production", "--model", "model", "--gpu-count", "4"})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil || !strings.Contains(err.Error(), "applies only to provisioned") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestCapacityRejectsUnexpectedPositionalArgument(t *testing.T) {
	err := capacityCommand(context.Background(), config.Config{ControlURL: "http://127.0.0.1:1", APIKey: "secret"}, []string{"deployment-name"})
	if err == nil || !strings.Contains(err.Error(), "usage: infercrane capacity") {
		t.Fatalf("err=%v", err)
	}
}

func TestTargetListSupportsJSONOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/targets" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return targetAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"list", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Data []targetView `json:"data"`
	}
	if err = json.Unmarshal([]byte(output), &response); err != nil || response.Data == nil {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestTargetAddSupportsJSONOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/targets" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"target":{"name":"existing"}}`)
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return targetAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"add", "existing", "--url", "http://runtime.test:8000", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err = json.Unmarshal([]byte(output), &response); err != nil || response["target"] == nil {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestIntelligenceCommandsUseDocumentedControlPlaneRoutes(t *testing.T) {
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/deployments/prod/replays":
			_, _ = io.WriteString(w, `{"replay":{"id":"replay-1","shape_digest":"digest","request_count":1,"summary":{"requests":1,"inputtokensmean":8,"outputtokensmean":4,"peakconcurrency":1},"window_start":"2026-08-11T00:00:00Z","window_end":"2026-08-11T01:00:00Z"}}`)
		case "/api/v1/capacity/intelligence":
			_, _ = io.WriteString(w, `{"capacity":[],"evidence":"observed","window_seconds":3600}`)
		case "/api/v1/deployments/prod/finops/reports":
			_, _ = io.WriteString(w, `{"report":{"id":"cost-1","status":"unavailable","currency":"","input_digest":"digest"}}`)
		case "/api/v1/deployments/prod/autopilot/plans":
			_, _ = io.WriteString(w, `{"plan":{"id":"plan-1","status":"advisory","objective":"minimize_cost","recommendation_id":"rec-1"},"mutation":"none"}`)
		case "/api/v1/context-passports":
			_, _ = io.WriteString(w, `{"context_passport":{"id":"session-1","status":"active","expires_at":"2026-08-12T00:00:00Z"},"durable_kv":false}`)
		case "/api/v1/deployments/prod/burst-guard/evaluate":
			_, _ = io.WriteString(w, `{"decision":{"action":"hold","reason":"below threshold","incremental_cost_microusd_hour":0}}`)
		default:
			t.Fatalf("unexpected route %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	runs := []func() error{
		func() error {
			return replayCommand(context.Background(), cfg, []string{"prod", "--window", "1h", "--output", "json"})
		},
		func() error {
			return capacityCommand(context.Background(), cfg, []string{"--window", "1h", "--output", "json"})
		},
		func() error {
			return finOpsCommand(context.Background(), cfg, []string{"prod", "--window", "1h", "--output", "json"})
		},
		func() error {
			return autopilotCommand(context.Background(), cfg, []string{"plan", "prod", "--output", "json"})
		},
		func() error {
			return sessionCommand(context.Background(), cfg, []string{"create", "prod", "--ttl", "1h", "--output", "json"})
		},
		func() error { return burstCommand(context.Background(), cfg, []string{"prod", "--output", "json"}) },
	}
	for _, run := range runs {
		if _, err := captureStdout(t, run); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{
		"POST /api/v1/deployments/prod/replays",
		"GET /api/v1/capacity/intelligence?window_seconds=3600",
		"POST /api/v1/deployments/prod/finops/reports",
		"POST /api/v1/deployments/prod/autopilot/plans",
		"POST /api/v1/context-passports",
		"POST /api/v1/deployments/prod/burst-guard/evaluate",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("seen=%q want=%q", seen, want)
	}
}

func TestRequestCommandSendsOpenAICompatibleRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("path=%s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "qwen-prod" || body["stream"] != false {
			t.Fatalf("body=%#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Hello from InferCrane"}}],"usage":{"total_tokens":8}}`))
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return requestCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"qwen-prod", "--message", "Hello"})
	})
	if err != nil || strings.TrimSpace(output) != "Hello from InferCrane" {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestRequestCommandSelectsProtocolSurface(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("path=%s auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["model"] != "search-production" || body["input"] != "document" {
			t.Fatalf("body=%#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"embedding":[0.1,0.2]}]}`))
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return requestCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"search-production", "--protocol", "embeddings", "--message", "document"})
	})
	if err != nil || !strings.Contains(output, `"embedding"`) {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestEndpointCommandsAcceptNaturalResourceThenFlagsOrder(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/logical-models":
			_, _ = io.WriteString(w, `{"logical_model":{"name":"coder"}}`)
		case "/api/v1/endpoints":
			_, _ = io.WriteString(w, `{"endpoint":{"name":"coder-production"}}`)
		default:
			t.Fatalf("path=%s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	if _, err := captureStdout(t, func() error {
		return logicalModelCommand(context.Background(), cfg, []string{"create", "coder", "--description", "Stable coding model", "--output", "json"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error {
		return endpointCommand(context.Background(), cfg, []string{"create", "coder-production", "--model", "coder", "--environment", "production", "--output", "json"})
	}); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0]["description"] != "Stable coding model" || requests[1]["logical_model"] != "coder" || requests[1]["environment"] != "production" {
		t.Fatalf("requests=%#v", requests)
	}
}

func TestEndpointBindBuildsReferenceOnlyManagedExternalPolicy(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/endpoints/coder-production/bindings" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"binding":{"id":"binding"}}`)
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	_, err := captureStdout(t, func() error {
		return endpointCommand(context.Background(), cfg, []string{
			"bind", "coder-production", "--name", "managed-api", "--target", "openrouter-coder",
			"--ownership", "traffic-managed", "--external-adapter", "openrouter",
			"--secret-reference", "secret-ref", "--request-limit", "1000",
			"--cost-limit-usd", "25", "--max-request-cost-usd", "0.05",
			"--acknowledge-external-data", "--enable-external",
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	configValue, _ := request["config"].(map[string]any)
	if request["ownership_mode"] != "traffic-managed" || configValue["secret_reference_id"] != "secret-ref" || configValue["adapter"] != "openrouter" || configValue["privacy_acknowledged"] != true || configValue["enabled"] != true {
		t.Fatalf("request=%#v", request)
	}
	if _, ok := configValue["api_key"]; ok {
		t.Fatalf("raw credential entered request: %#v", request)
	}
	if configValue["cost_limit_microusd"] != float64(25_000_000) || configValue["max_request_cost_microusd"] != float64(50_000) {
		t.Fatalf("budget=%#v", configValue)
	}
}

func TestProviderConnectRegistersReusableReferenceOnlyConfiguration(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/targets":
			_, _ = io.WriteString(w, `{"target":{"id":"target","name":"provider-openrouter-main"}}`)
		case "/api/v1/provider-connections":
			_, _ = io.WriteString(w, `{"connection":{"id":"connection","name":"openrouter-main"}}`)
		default:
			t.Fatalf("path=%s", r.URL.Path)
		}
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return providerCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"connect", "openrouter-main", "--model", "openai/gpt-4.1-mini", "--secret-reference", "secret-ref"})
	})
	if err != nil || len(requests) != 2 {
		t.Fatalf("output=%q requests=%#v err=%v", output, requests, err)
	}
	if requests[0]["url"] != "https://openrouter.ai/api/v1" || requests[0]["upstream_model"] != "openai/gpt-4.1-mini" || requests[1]["secret_reference_id"] != "secret-ref" {
		t.Fatalf("requests=%#v", requests)
	}
	for _, request := range requests {
		if _, leaked := request["api_key"]; leaked {
			t.Fatalf("raw credential entered request: %#v", request)
		}
	}
}

func TestProviderConnectCanCreateIdempotentEnvironmentReference(t *testing.T) {
	var paths []string
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, r.URL.Path)
		requests = append(requests, body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/secrets":
			_, _ = io.WriteString(w, `{"secret":{"id":"secret-ref","name":"provider-openrouter-main","resolver":"env","reference":"OPENROUTER_API_KEY"}}`)
		case "/api/v1/targets":
			_, _ = io.WriteString(w, `{"target":{"id":"target","name":"provider-openrouter-main"}}`)
		case "/api/v1/provider-connections":
			_, _ = io.WriteString(w, `{"connection":{"id":"connection","name":"openrouter-main","secret_reference_id":"secret-ref"}}`)
		default:
			t.Fatalf("path=%s", r.URL.Path)
		}
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return providerCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"connect", "openrouter-main", "--model", "openai/gpt-4.1-mini", "--from-env", "OPENROUTER_API_KEY", "--output", "json"})
	})
	if err != nil || len(requests) != 3 {
		t.Fatalf("output=%q paths=%#v requests=%#v err=%v", output, paths, requests, err)
	}
	if paths[0] != "/api/v1/secrets" || requests[0]["reference"] != "OPENROUTER_API_KEY" || requests[0]["resolver"] != "env" || requests[2]["secret_reference_id"] != "secret-ref" {
		t.Fatalf("paths=%#v requests=%#v", paths, requests)
	}
	for _, request := range requests {
		if _, leaked := request["api_key"]; leaked {
			t.Fatalf("raw credential entered request: %#v", request)
		}
	}
	if !strings.Contains(output, `"secret_reference_id": "secret-ref"`) {
		t.Fatalf("output=%q", output)
	}
}

func TestProviderConnectRequiresExactlyOneCredentialReferenceSource(t *testing.T) {
	for name, args := range map[string][]string{
		"missing": {"connect", "openrouter-main", "--model", "openai/gpt-4.1-mini"},
		"both":    {"connect", "openrouter-main", "--model", "openai/gpt-4.1-mini", "--from-env", "OPENROUTER_API_KEY", "--secret-reference", "secret-ref"},
	} {
		t.Run(name, func(t *testing.T) {
			err := providerCommand(context.Background(), config.Config{}, args)
			if err == nil || !strings.Contains(err.Error(), "--from-env VARIABLE | --secret-reference ID") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestEndpointBindUsesProviderConnectionAndStillRequiresConsent(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"binding":{"id":"binding"}}`)
	}))
	defer server.Close()
	_, err := captureStdout(t, func() error {
		return endpointCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{
			"bind", "coder-production", "--name", "fallback", "--connection", "openrouter-main",
			"--request-limit", "100", "--cost-limit-usd", "10", "--max-request-cost-usd", "0.10",
			"--acknowledge-external-data", "--enable-external",
		})
	})
	if err != nil || request["provider_connection"] != "openrouter-main" || request["ownership_mode"] != "traffic-managed" || request["target"] != "" {
		t.Fatalf("request=%#v err=%v", request, err)
	}
	configValue, _ := request["config"].(map[string]any)
	if configValue["privacy_acknowledged"] != true || configValue["request_limit"] != float64(100) {
		t.Fatalf("config=%#v", configValue)
	}
	err = endpointCommand(context.Background(), config.Config{}, []string{
		"bind", "coder-production", "--connection", "openrouter-main", "--request-limit", "1",
		"--cost-limit-usd", "1", "--max-request-cost-usd", "1", "--enable-external",
	})
	if err == nil || !strings.Contains(err.Error(), "--acknowledge-external-data") {
		t.Fatalf("missing consent err=%v", err)
	}
}

func TestEndpointBindRejectsEnabledExternalPolicyWithoutConsent(t *testing.T) {
	err := endpointCommand(context.Background(), config.Config{}, []string{
		"bind", "coder-production", "--target", "openrouter-coder",
		"--external-adapter", "openrouter", "--secret-reference", "secret-ref",
		"--request-limit", "10", "--cost-limit-usd", "1", "--max-request-cost-usd", "0.10",
		"--enable-external",
	})
	if err == nil || !strings.Contains(err.Error(), "--acknowledge-external-data") {
		t.Fatalf("err=%v", err)
	}
}

func TestRequestCommandPrintsStreamingDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello \"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return requestCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"qwen-prod", "--stream"})
	})
	if err != nil || output != "Hello world\n" {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestLogsCommandFiltersDurableTimeline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/deployments/qwen-prod/events" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"data":[{"type":"provider.capacity","summary":"capacity accepted","created_at":"2026-08-09T12:00:00Z"},{"type":"runtime.ready","summary":"model ready","created_at":"2026-08-09T12:01:00Z"}]}`)
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return logsCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"qwen-prod", "--type", "runtime"})
	})
	if err != nil || !strings.Contains(output, "runtime.ready") || strings.Contains(output, "provider.capacity") {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestCobraRootProvidesSuggestionsAndCompletion(t *testing.T) {
	root := newRootCommand(context.Background())
	root.SetArgs([]string{"completion", "bash"})
	var output bytes.Buffer
	root.SetOut(&output)
	if err := root.Execute(); err != nil || !strings.Contains(output.String(), "__start_infercrane") {
		t.Fatalf("completion output missing: err=%v output=%q", err, output.String())
	}
	root = newRootCommand(context.Background())
	root.SetArgs([]string{"deply"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("err=%v", err)
	}
}

func TestAdvertisedIntelligenceCommandsReachPublicDispatcher(t *testing.T) {
	t.Setenv("INFERCRANE_URL", "http://127.0.0.1:1")
	t.Setenv("INFERCRANE_API_KEY", "test")
	commands := [][]string{
		{"replay", "prod", "--output", "yaml"},
		{"capacity", "--output", "yaml"},
		{"finops", "prod", "--output", "yaml"},
		{"autopilot", "plan", "prod", "--output", "yaml"},
		{"session", "inspect", "session-1", "--output", "yaml"},
		{"burst", "prod", "--output", "yaml"},
	}
	for _, args := range commands {
		err := runLegacy(context.Background(), args)
		if err == nil || strings.Contains(err.Error(), "unknown command") {
			t.Fatalf("runLegacy(%v) error = %v, want handler validation error", args, err)
		}
	}
}

func TestRecipesAcceptsDocumentedQueryBeforeFlags(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()
	if err := recipesCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"qwen-local", "--output", "json"}); err != nil {
		t.Fatal(err)
	}
	if query != "qwen-local" {
		t.Fatalf("query = %q", query)
	}
}

func TestGlobalContextIsHandledBeforeCommandDispatch(t *testing.T) {
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("INFERCRANE_CONTEXT", "")
	if _, err := config.InitializeClientContext("staging", "https://staging.example", "secret", true); err != nil {
		t.Fatal(err)
	}
	output, err := captureStdout(t, func() error {
		return run(context.Background(), []string{"--context", "staging", "context", "show"})
	})
	if err != nil || !strings.Contains(output, "https://staging.example") {
		t.Fatalf("output=%q err=%v", output, err)
	}
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
	if err != nil || path != "/api/v1/deployments" || key != "release-1" || body["cloud"] != "runpod" || body["endpoint_name"] != "qwen3-8b" || body["max_replicas"] != float64(4) {
		t.Fatalf("path=%s key=%s body=%#v err=%v", path, key, body, err)
	}
}

func TestDeployCLISeparatesDeploymentFromStableEndpointIdentity(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending"}}`))
	}))
	defer server.Close()

	err := deployAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{"Qwen/Qwen3-8B", "--name", "qwen-rev-1", "--endpoint", "support-production", "--idempotency-key", "release-1"})
	if err != nil || body["name"] != "qwen-rev-1" || body["endpoint_name"] != "support-production" {
		t.Fatalf("body=%#v err=%v", body, err)
	}
}

func TestDynamoDeployConvenienceExpandsToSafeExplicitContract(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"operation":{"id":"op-dynamo","status":"pending"}}`))
	}))
	defer server.Close()

	err := deployAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{
		"meta-llama/Llama-3.1-8B-Instruct", "--backend", "dynamo", "--gpu", "NVIDIA-L40S",
		"--runtime", "sglang", "--idempotency-key", "dynamo-1",
	})
	serving, _ := body["serving"].(map[string]any)
	worker, _ := serving["worker"].(map[string]any)
	if err != nil || body["cloud"] != "kubernetes" || body["provider_adapter"] != "kubernetes-dynamo" || body["runtime"] != "sglang" || body["min_replicas"] != float64(1) || body["max_replicas"] != float64(1) || serving["backend"] != "dynamo" || serving["profile"] != "baseline" || worker["replicas"] != float64(1) {
		t.Fatalf("body=%#v err=%v", body, err)
	}
	if err = deployAPICommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "deploy", []string{"model", "--backend", "dynamo"}); err == nil || !strings.Contains(err.Error(), "requires --gpu") {
		t.Fatalf("missing cluster GPU was accepted: %v", err)
	}
}

func TestPlanMakesUnknownStartupEvidenceExplicit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/capacity/intelligence" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"capacity":[]}`)
			return
		}
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/api/v1/deployments/") {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"deployment was not found"}}`)
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}

	human, err := captureStdout(t, func() error {
		return planCommand(context.Background(), cfg, []string{"Qwen/Qwen3-8B", "--cloud", "runpod", "--gpu", "L40S"})
	})
	if err != nil || !strings.Contains(human, "Readiness: unavailable") || !strings.Contains(human, "Artifact cache: unknown") || !strings.Contains(human, "capacity -> container -> artifact -> runtime -> readiness") {
		t.Fatalf("human=%q err=%v", human, err)
	}

	encoded, err := captureStdout(t, func() error {
		return planCommand(context.Background(), cfg, []string{"Qwen/Qwen3-8B", "--cloud", "runpod", "--gpu", "L40S", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Readiness struct {
			EstimateStatus     string   `json:"estimate_status"`
			ArtifactCacheState string   `json:"artifact_cache_state"`
			CapacityState      string   `json:"capacity_state"`
			Stages             []string `json:"stages"`
		} `json:"readiness"`
	}
	if err = json.Unmarshal([]byte(encoded), &output); err != nil || output.Readiness.EstimateStatus != "unavailable" || output.Readiness.ArtifactCacheState != "unknown" || output.Readiness.CapacityState != "unknown" || len(output.Readiness.Stages) != 5 {
		t.Fatalf("output=%s parsed=%#v err=%v", encoded, output, err)
	}
}

func TestPlanShowsOnlyQualifiedObservedReadinessHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/capacity/intelligence" {
			_, _ = io.WriteString(w, `{"capacity":[{"provider":"aws-ec2","runtime":"vllm","compute_mode":"elastic","region":"eu-central-1","gpu":"L40S","attempts":22,"succeeded":20,"pending":1,"success_rate":0.9523809524,"duration_p50_seconds":410,"duration_p95_seconds":720}]}`)
			return
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/deployments/") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"deployment was not found"}}`)
			return
		}
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return planCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"Qwen/Qwen3-8B", "--cloud", "aws", "--gpu", "L40S", "--region", "eu-central-1"})
	})
	if err != nil || !strings.Contains(output, "Readiness: observed") || !strings.Contains(output, "Observed p50: 410s · p95 720s · 20 successful samples") || !strings.Contains(output, "durable replica intent through runtime readiness") {
		t.Fatalf("output=%q err=%v", output, err)
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
		return doctorCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"--cloud", "--serverless", "--aws", "--gcp", "--kubernetes", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(output), &report); err != nil || report["ready"] != true {
		t.Fatalf("output=%q report=%#v err=%v", output, report, err)
	}
	if requested != "/api/v1/doctor?aws=true&cloud=true&gcp=true&kubernetes=true&serverless=true" {
		t.Fatalf("request=%q", requested)
	}
}

func TestConnectUsesControlPlaneDiscoveryAndPrintsUsefulNextSteps(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/adoptions/endpoints" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("request = %s %s authorization=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"discovery":{"runtime":"vllm","connector":"vllm","model":"Qwen/Qwen3-8B","models":["Qwen/Qwen3-8B"],"health":"reachable"}}`))
	}))
	defer server.Close()

	output, err := captureStdout(t, func() error {
		return connectCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"https://worker.example/v1", "--as", "coder"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["discover"] != true || body["connector"] != "auto" || body["ownership_mode"] != "observe-only" || body["logical_model"] != "coder" {
		t.Fatalf("body = %#v", body)
	}
	for _, wanted := range []string{"Connected coder", "Qwen/Qwen3-8B", server.URL + "/v1", "infercrane doctor coder", "infercrane adopt promote coder"} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("output %q missing %q", output, wanted)
		}
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
	if err != nil || body["cloud"] != "runpod" || body["gpu"] != "L40S" || body["runtime_version"] != support.DefaultRuntimeVersion {
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
endpoint: support-production
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
	if err != nil || body["endpoint_name"] != "support-production" || body["compute_mode"] != "serverless" || body["min_replicas"] != nil || body["max_replicas"] != float64(4) || body["model_revision"] == nil || body["runtime_version"] != "0.10.2" || body["region"] != "EU-RO-1" {
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

func TestAuthStatusJSONUsesStableLowercaseFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"principal":{"id":"principal-1","tenant_id":"tenant-1","name":"developer","role":"operator","kind":"service","scopes":["read"]}}`))
	}))
	defer server.Close()

	output, err := captureStdout(t, func() error {
		return authCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"status", "--output", "json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Principal struct {
			ID       string `json:"id"`
			TenantID string `json:"tenant_id"`
			Role     string `json:"role"`
		} `json:"principal"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not JSON: %q: %v", output, err)
	}
	if result.Principal.ID != "principal-1" || result.Principal.TenantID != "tenant-1" || result.Principal.Role != "operator" {
		t.Fatalf("unstable auth JSON: %s", output)
	}
	if strings.Contains(output, `"TenantID"`) || strings.Contains(output, `"Role"`) {
		t.Fatalf("Go field names leaked into public JSON: %s", output)
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

func TestWaitFailureIncludesDurableErrorAndInspectionCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"op-failed","status":"failed","progress":55,"message":"secure worker bootstrap failed","error_code":"provider_ensure_failed","attempt":4,"max_attempts":120}`))
	}))
	defer server.Close()

	_, err := waitForOperation(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "op-failed", false)
	if err == nil || !strings.Contains(err.Error(), "[provider_ensure_failed]") || !strings.Contains(err.Error(), "4/120 attempts") || !strings.Contains(err.Error(), "infercrane operation op-failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitTimeoutDisconnectsWithoutCancellingDurableOperation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"op-running","status":"running","progress":55,"message":"provider capacity: INIT"}`))
	}))
	defer server.Close()

	_, err := waitForOperationWithin(context.Background(), 20*time.Millisecond, config.Config{ControlURL: server.URL, APIKey: "secret"}, "op-running", false)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "operation op-running continues") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWaitTimeoutDuringControlPlanePollStillExplainsDurability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	_, err := waitForOperationWithin(context.Background(), 20*time.Millisecond, config.Config{ControlURL: server.URL, APIKey: "secret"}, "op-slow", false)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "operation op-slow continues safely") || strings.Contains(err.Error(), "is unreachable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOperationWatchResumesPersistedOperation(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/operations/op-resume" {
			t.Fatalf("method=%s path=%s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"op-resume","status":"succeeded","progress":100,"message":"completed"}`))
	}))
	defer server.Close()

	output, err := captureStdout(t, func() error {
		return operationCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"watch", "op-resume", "--output", "json"})
	})
	if err != nil || requests != 1 || !strings.Contains(output, `"id": "op-resume"`) || !strings.Contains(output, `"status": "succeeded"`) {
		t.Fatalf("requests=%d output=%q err=%v", requests, output, err)
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
		"doctor":    func() error { return doctorCommand(context.Background(), cfg, []string{"--output", "xml"}) },
		"benchmark": func() error { return benchmarkCommand(context.Background(), cfg, []string{"qwen", "--output", "xml"}) },
		"recipe": func() error {
			return recipeCommand(context.Background(), cfg, []string{"create", "qwen", "--name", "balanced", "--version", "1", "--output", "xml"})
		},
		"recipes": func() error { return recipesCommand(context.Background(), cfg, []string{"--output", "xml"}) },
		"models":  func() error { return modelsCommand([]string{"--output", "xml"}) },
		"lab": func() error {
			return labCommand(context.Background(), cfg, []string{"model@commit", "--output", "xml"})
		},
		"recommend":    func() error { return recommendCommand(context.Background(), cfg, []string{"qwen", "--output", "xml"}) },
		"slo":          func() error { return sloCommand(context.Background(), cfg, []string{"get", "qwen", "--output", "xml"}) },
		"deployments":  func() error { return listDeployments(context.Background(), cfg, []string{"--output", "xml"}) },
		"status":       func() error { return statusCommand(context.Background(), cfg, []string{"qwen", "--output", "xml"}) },
		"orphans":      func() error { return orphanAPICommand(context.Background(), cfg, []string{"--output", "xml"}) },
		"integrations": func() error { return integrationsCommand(context.Background(), cfg, []string{"--output", "xml"}) },
		"inspect":      func() error { return inspectCommand(context.Background(), cfg, []string{"qwen", "--output", "xml"}) },
		"events":       func() error { return eventsCommand(context.Background(), cfg, []string{"qwen", "--output", "xml"}) },
		"explain":      func() error { return explainCommand(context.Background(), cfg, []string{"qwen", "--output", "xml"}) },
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

func TestModelsCommandExploresReviewedCatalogWithoutControlPlane(t *testing.T) {
	output, err := captureStdout(t, func() error { return modelsCommand([]string{"embeddings"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "bge-m3-embeddings") || strings.Contains(output, "qwen3-8b") || !strings.Contains(output, "configuration-verified") {
		t.Fatalf("unexpected catalog output: %s", output)
	}
	detail, err := captureStdout(t, func() error { return modelsCommand([]string{"inspect", "mistral-7b-instruct"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Mistral 7B Instruct", "c170c708c41dac9275d15a8fff4eca08d52bab71", "performance", "workload init --recipe mistral-7b-instruct"} {
		if !strings.Contains(strings.ToLower(detail), strings.ToLower(expected)) {
			t.Fatalf("detail missing %q: %s", expected, detail)
		}
	}
	frontier, err := captureStdout(t, func() error { return modelsCommand([]string{"inspect", "qwen3.8-flash-next"}) })
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"8×H200", "vllm/vllm-openai@sha256:fc120ece", "Command: python3 -c", "--enable-expert-parallel", "qwen3_xml", "qwen-community-1.0", "separate Qwen license"} {
		if !strings.Contains(frontier, expected) {
			t.Fatalf("frontier detail missing %q: %s", expected, frontier)
		}
	}
}

func TestIntegrationsCommandShowsContractsAndDeferredEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/integrations" || r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"provider_contract":"infercrane.provider/v1","runtime_contract":"infercrane.runtime/v1","providers":[{"adapter":"fixture","cloud":"test","contract_version":"infercrane.provider/v1","adapter_version":"1","modes":["elastic"],"capabilities":[],"qualification":[{"state":"deferred","reason":"manual"}]}],"runtimes":[]}}`))
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return integrationsCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, nil)
	})
	if err != nil || !strings.Contains(output, "infercrane.provider/v1") || !strings.Contains(output, "fixture") || !strings.Contains(output, "deferred") {
		t.Fatalf("output=%q err=%v", output, err)
	}
}

func TestJSONOutputRendersStructuredControlPlaneFailure(t *testing.T) {
	var output bytes.Buffer
	writeCLIError(&output, []string{"status", "qwen", "--output", "json"}, &ControlError{StatusCode: http.StatusServiceUnavailable, Code: "provider_unavailable", Category: "dependency", Message: "RunPod is unavailable", Retryable: true, Remediation: "Retry with the same idempotency key."})
	var envelope struct {
		Error struct {
			Code, Category, Message, Remediation string
			Retryable                            bool
			Status                               int
		} `json:"error"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil || envelope.Error.Code != "provider_unavailable" || envelope.Error.Category != "dependency" || !envelope.Error.Retryable || envelope.Error.Status != http.StatusServiceUnavailable || strings.Contains(output.String(), "Error:") {
		t.Fatalf("output=%s envelope=%#v err=%v", output.String(), envelope, err)
	}
}

func TestJSONOutputClassifiesDurableOperationWatchTimeout(t *testing.T) {
	var output bytes.Buffer
	writeCLIError(&output, []string{"operation", "watch", "op-123", "--output", "json"}, watcherStoppedError(context.DeadlineExceeded, "op-123"))
	var envelope struct {
		Error struct {
			Code, Category, Message, Remediation string
			Retryable                            bool
		} `json:"error"`
	}
	if err := json.Unmarshal(output.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "operation_watch_timeout" || envelope.Error.Category != "operation" || !envelope.Error.Retryable || !strings.Contains(envelope.Error.Remediation, "operation watch op-123") || strings.Contains(envelope.Error.Remediation, "Correct the command") {
		t.Fatalf("unexpected timeout taxonomy: %#v", envelope.Error)
	}
}

func TestWatcherStoppedErrorPreservesCancellationCause(t *testing.T) {
	err := watcherStoppedError(context.Canceled, "op-456")
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "operation op-456 continues safely") {
		t.Fatalf("error=%v", err)
	}
}

func TestStatusShowsServingAndConvergenceSeparately(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deployment":{"name":"qwen","model":"Qwen/Qwen3-8B","runtime":"vllm","observed_state":"degraded","min_replicas":1,"active_revision_id":"rev-1"},"targets":[{"health":"healthy"},{"health":"starting"}],"replicas":[{"health":"healthy"},{"health":"starting"}],"lifecycle_status":{"serving_state":"serving","convergence_state":"converging","candidate_state":"none","ready_replicas":1,"desired_replicas":2,"provisioning_replicas":1,"draining_replicas":0,"blocking_operation_id":"op-scale","blocking_operation_kind":"deployment.scale"},"request_stats":{}}`))
	}))
	defer server.Close()

	output, err := captureStdout(t, func() error {
		return statusCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"qwen"})
	})
	if err != nil || !strings.Contains(output, "Serving      serving") || !strings.Contains(output, "Convergence  converging") || !strings.Contains(output, "Ready        1/2") || !strings.Contains(output, "Operation    op-scale (deployment.scale)") {
		t.Fatalf("output=%q err=%v", output, err)
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

func TestBenchmarkProfileAppliesDefaultsAndPreservesExplicitOverrides(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"benchmark":{"id":"benchmark-1"}}`))
	}))
	defer server.Close()

	_, err := captureStdout(t, func() error {
		return benchmarkCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"coder", "--profile", "long-context", "--concurrency", "2", "--output", "json"})
	})
	if err != nil || body["requests"] != float64(64) || body["concurrency"] != float64(2) || body["input_tokens"] != float64(8192) || body["output_tokens"] != float64(256) || body["profile"] != "long-context" || body["profile_version"] != "benchmark-profile-v1" {
		t.Fatalf("body=%#v err=%v", body, err)
	}
}

func TestStartupEvidenceLinesExposeWaterfallWithoutRawConsole(t *testing.T) {
	lines := startupEvidenceLines(json.RawMessage(`{"runtime_ready_at":"2026-08-23T10:01:10Z","startup_evidence":{"image_cache":"miss","current_stage":"runtime_container_started","stages":[{"name":"identity_start","at":"2026-08-23T10:00:00Z"},{"name":"identity_ready","at":"2026-08-23T10:00:02Z"},{"name":"image_pull_start","at":"2026-08-23T10:00:03Z"},{"name":"image_pull_complete","at":"2026-08-23T10:00:33Z"},{"name":"runtime_start","at":"2026-08-23T10:00:34Z"},{"name":"runtime_container_started","at":"2026-08-23T10:00:40Z"}]}}`))
	joined := strings.Join(lines, "\n")
	for _, expected := range []string{"Image      cache miss", "measured provider waterfall", "+3s", "image pull", "+40s", "container started", "+1m10s", "runtime ready", "workload identity  2s", "container transfer 30s", "container launch   6s", "model + runtime ready 30s", "provider allocation unavailable", "model download      unavailable"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("lines=%q missing=%q", joined, expected)
		}
	}
}

func TestStartupEvidenceLinesExposeVerifiedArtifactCacheBoundary(t *testing.T) {
	lines := startupEvidenceLines(json.RawMessage(`{"runtime_ready_at":"2026-08-23T10:00:50Z","startup_evidence":{"image_cache":"hit","artifact_cache":"hit","current_stage":"runtime_container_started","stages":[{"name":"identity_start","at":"2026-08-23T10:00:00Z"},{"name":"image_check","at":"2026-08-23T10:00:02Z"},{"name":"image_cache_hit","at":"2026-08-23T10:00:03Z"},{"name":"artifact_check","at":"2026-08-23T10:00:04Z"},{"name":"artifact_cache_hit","at":"2026-08-23T10:00:09Z"},{"name":"runtime_start","at":"2026-08-23T10:00:10Z"},{"name":"runtime_container_started","at":"2026-08-23T10:00:20Z"}]}}`))
	joined := strings.Join(lines, "\n")
	for _, expected := range []string{"Image      cache hit", "Artifact   cache hit", "artifact attach    5s", "model materialize   included in model + runtime ready"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("lines=%q missing=%q", joined, expected)
		}
	}
	if strings.Contains(joined, "model download      unavailable") {
		t.Fatalf("verified artifact hit was still rendered unavailable: %q", joined)
	}
}

func TestRecipeAndLabCommandsUseControlAPI(t *testing.T) {
	var paths []string
	var labBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/recipes"):
			_ = json.NewEncoder(w).Encode(map[string]any{"recipe": map[string]any{"name": "balanced", "version": "1.0.0", "digest": strings.Repeat("a", 64), "payload": map[string]any{"benchmark_id": "bench-1", "model_identity": "model@commit", "runtime": "vllm", "runtime_version": "1", "provider": "aws", "gpu": "H100"}}})
		case r.URL.Path == "/api/v1/recipes":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		default:
			if err := json.NewDecoder(r.Body).Decode(&labBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"evaluation": map[string]any{"id": "lab-1", "input_digest": strings.Repeat("b", 64), "results": []any{map[string]any{"evidence_class": "measured", "provider": "aws", "runtime": "vllm", "gpu": "H100", "ttft_p95_ms": 200, "error_rate": 0, "cost_metadata": map[string]any{"available": false}}}}})
		}
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	if _, err := captureStdout(t, func() error {
		return recipeCommand(context.Background(), cfg, []string{"create", "qwen prod", "--name", "balanced", "--version", "1.0.0"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error { return recipesCommand(context.Background(), cfg, []string{"bal"}) }); err != nil {
		t.Fatal(err)
	}
	if output, err := captureStdout(t, func() error {
		return labCommand(context.Background(), cfg, []string{"model@commit", "--objective", "interactive", "--profile", "interactive", "--max-ttft-p95-ms", "250ms", "--max-tpot-p95-ms", "25", "--max-error-rate", "0.01", "--min-goodput", "4", "--min-output-tokens-second", "80", "--max-hourly-cost", "3", "--region", "eu-central-1", "--max-gpu-count", "1"})
	}); err != nil || !strings.Contains(output, "MEASURED") {
		t.Fatalf("output=%s err=%v", output, err)
	}
	if strings.Join(paths, ",") != "/api/v1/deployments/qwen prod/recipes,/api/v1/recipes,/api/v1/lab/evaluations" {
		t.Fatalf("paths=%v", paths)
	}
	if labBody["objective"] != "interactive" || labBody["workload_profile"] != "interactive" || labBody["max_ttft_p95_ms"] != float64(250) || labBody["max_tpot_p95_ms"] != float64(25) || labBody["max_error_rate"] != 0.01 || labBody["min_goodput"] != float64(4) || labBody["min_output_tokens_second"] != float64(80) || labBody["max_hourly_cost"] != float64(3) || labBody["region"] != "eu-central-1" || labBody["max_gpu_count"] != float64(1) {
		t.Fatalf("lab body=%#v", labBody)
	}
}

func TestLabRejectsUnknownObjectiveBeforeNetworkAccess(t *testing.T) {
	if err := labCommand(context.Background(), config.Config{}, []string{"model@commit", "--objective", "fastest"}); err == nil || !strings.Contains(err.Error(), "objective") {
		t.Fatalf("err=%v", err)
	}
}

func TestLabRejectsInvalidConstraintsBeforeNetworkAccess(t *testing.T) {
	for _, args := range [][]string{
		{"model@commit", "--max-ttft-p95-ms", "later"},
		{"model@commit", "--max-error-rate", "1.1"},
		{"model@commit", "--min-goodput", "-1"},
		{"model@commit", "--max-gpu-count", "-1"},
	} {
		if err := labCommand(context.Background(), config.Config{}, args); err == nil {
			t.Fatalf("args=%v accepted", args)
		}
	}
}

func TestSLOAndRecommendationCommandsUseControlAPI(t *testing.T) {
	var methods, paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"policy": body})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"recommendation": map[string]any{"status": "unknown", "reason": "missing benchmark", "algorithm_version": "recommendation-v1", "input_digest": strings.Repeat("a", 64), "missing": []string{"benchmark_evidence"}}})
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	if _, err := captureStdout(t, func() error {
		return sloCommand(context.Background(), cfg, []string{"set", "qwen prod", "--ttft-p95", "250", "--output", "json"})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error {
		return recommendCommand(context.Background(), cfg, []string{"qwen prod", "--output", "json"})
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "PUT,POST" || paths[0] != "/api/v1/deployments/qwen prod/slo-policy" || paths[1] != "/api/v1/deployments/qwen prod/recommendations" {
		t.Fatalf("methods=%v paths=%v", methods, paths)
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

func TestRolloutCreateFromFileFailsClosedForDeferredNIXLTopology(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "candidate.yaml")
	body := `
apiVersion: infercrane.dev/v1
kind: Deployment
name: llama-production
model: {id: meta-llama/Llama-3.1-8B-Instruct, revision: immutable}
runtime: {engine: vllm, version: qualified}
compute: {mode: elastic}
resources: {gpu: NVIDIA-L40S}
provider: {cloud: kubernetes, adapter: kubernetes-dynamo}
scaling: {min_replicas: 1, max_replicas: 1}
routing: {strategy: round-robin}
serving:
  backend: dynamo
  profile: custom
  mode: disaggregated
  routing: kv-aware
  prefill: {replicas: 1, tensor_parallelism: 1}
  decode: {replicas: 2, tensor_parallelism: 1}
`
	if err := os.WriteFile(filename, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"operation":{"id":"op-rollout","status":"pending"}}`))
	}))
	defer server.Close()
	err := rolloutCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"create", "llama-production", "--file", filename, "--idempotency-key", "rollout-file-1"})
	if err == nil || !strings.Contains(err.Error(), "registered for argument translation") || request != nil {
		t.Fatalf("request=%#v err=%v", request, err)
	}
	if err = rolloutCommand(context.Background(), config.Config{}, []string{"create", "llama-production", "--file", filename, "--model", "other"}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("mixed file/flags accepted: %v", err)
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

func TestExplainColdStartMakesUnavailableBoundariesExplicit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "deployment":{"name":"qwen","active_revision_id":"rev-1"},
  "targets":[],"replicas":[],"revisions":[],"model_artifacts":[],
  "cold_start_stats":{"classified_requests":1,"cold_starts":1,"warm_requests":0,"cold_ttft_p50_ms":72100,"cold_ttft_p95_ms":null,"warm_ttft_p50_ms":null,"warm_ttft_p95_ms":null,"time_to_ready_p50_ms":null,"time_to_ready_p95_ms":null,"available_boundaries":["request_arrival","gateway_first_response_byte"],"unavailable_boundaries":["capacity_allocation","time_to_ready","first_token"],"bottleneck_code":"provider_capacity_or_worker_initialization","evidence":"grounded provider evidence"}
}`))
	}))
	defer server.Close()
	output, err := captureStdout(t, func() error {
		return explainCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"cold-start", "qwen"})
	})
	if err != nil || !strings.Contains(output, "Time-to-ready        unavailable") || !strings.Contains(output, "capacity_allocation, time_to_ready, first_token") || !strings.Contains(output, "provider_capacity_or_worker_initialization") {
		t.Fatalf("output=%s err=%v", output, err)
	}
}

func TestPassportSigningKeyRequiresRestrictedFile(t *testing.T) {
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "signing-key")
	if err = os.WriteFile(path, []byte(passport.EncodePrivateKey(key)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadPassportSigningKey(path)
	if err != nil || string(loaded) != string(key) {
		t.Fatalf("loaded=%d err=%v", len(loaded), err)
	}
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = loadPassportSigningKey(path); err == nil || !strings.Contains(err.Error(), "group or others") {
		t.Fatalf("err=%v", err)
	}
}

func TestRolloutValidateUsesBoundedExplicitActiveAndCandidateBenchmarks(t *testing.T) {
	var revisions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/deployments/prod"):
			_, _ = w.Write([]byte(`{"deployment":{"name":"prod","candidate_revision_id":"rev-2"},"release_guard_policy":{"validation_max_requests":20,"validation_max_concurrency":2}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/benchmarks"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			revisions = append(revisions, body["revision"].(string))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"benchmark":{"id":"bench"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rollouts/guard/evaluate"):
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"operation":{"id":"op","status":"pending"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	_, err := captureStdout(t, func() error {
		return rolloutCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"validate", "prod", "--requests", "20", "--concurrency", "2", "--acknowledge-validation-cost", "--output", "json"})
	})
	if err != nil || len(revisions) != 2 || revisions[0] != "active" || revisions[1] != "candidate" {
		t.Fatalf("revisions=%#v err=%v", revisions, err)
	}
	if _, err = captureStdout(t, func() error {
		return rolloutCommand(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, []string{"validate", "prod", "--requests", "21", "--acknowledge-validation-cost"})
	}); err == nil || !strings.Contains(err.Error(), "persisted bounds") {
		t.Fatalf("err=%v", err)
	}
}
