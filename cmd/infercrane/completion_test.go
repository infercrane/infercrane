package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type completionServer struct {
	mu    sync.Mutex
	paths []string
}

func (s *completionServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.RequestURI())
	s.mu.Unlock()
	if r.Method != http.MethodGet {
		http.Error(w, "completion must only read", http.StatusMethodNotAllowed)
		return
	}
	var body any
	switch r.URL.Path {
	case "/api/v1/deployments":
		body = map[string]any{"data": []map[string]any{{"name": "qwen-prod", "model": "Qwen/Qwen3-8B", "observed_state": "serving"}, {"name": "llama-canary", "observed_state": "failed"}}}
	case "/api/v1/endpoints":
		body = map[string]any{"data": []map[string]any{{"name": "support-bot", "observed_state": "serving"}}}
	case "/api/v1/optimization/campaigns":
		body = map[string]any{"data": []map[string]any{{"id": "83ccd598540e2dd1f9c897040c1ea503", "model_identity": "Qwen/Qwen3-0.6B", "state": "ranked"}, {"id": "4f1c2d9e0b7a4e63a1d2c3b4a5f60718", "model_identity": "Qwen/Qwen3-8B", "state": "awaiting_approval"}}}
	case "/api/v1/optimization/campaigns/83ccd598540e2dd1f9c897040c1ea503":
		body = map[string]any{"campaign": map[string]any{"id": "83ccd598540e2dd1f9c897040c1ea503", "candidates": []map[string]any{{"id": "cand-winner", "state": "qualified"}, {"id": "cand-lost", "state": "rejected"}}}}
	case "/api/v1/catalog/models":
		body = map[string]any{"data": []map[string]any{{"model": "Qwen/Qwen3-8B", "display_name": "Qwen3 8B"}, {"model": "meta-llama/Llama-3.1-8B-Instruct", "display_name": "Llama 3.1 8B Instruct"}}}
	case "/api/v1/catalog/gpu-prices":
		rows := []map[string]any{
			{"provider": "runpod", "region": "EU-RO-1", "gpu": "NVIDIA H100 80GB HBM3"},
			{"provider": "runpod", "region": "US-TX-3", "gpu": "NVIDIA L40S"},
			{"provider": "aws", "region": "eu-central-1", "gpu": "L40S"},
		}
		provider, gpu := r.URL.Query().Get("provider"), r.URL.Query().Get("gpu")
		var filtered []map[string]any
		for _, row := range rows {
			if (provider == "" || row["provider"] == provider) && (gpu == "" || row["gpu"] == gpu) {
				filtered = append(filtered, row)
			}
		}
		body = map[string]any{"data": filtered}
	default:
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func useCompletionServer(t *testing.T) *completionServer {
	t.Helper()
	recorder := &completionServer{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("INFERCRANE_CONTEXT", "")
	return recorder
}

// completions runs the shell's hidden completion command and returns the
// suggestions without the trailing directive line.
func completions(t *testing.T, args ...string) []string {
	t.Helper()
	stdout, _, err := runCLI(t, append([]string{"__complete"}, args...)...)
	if err != nil {
		t.Fatalf("completion %v failed: %v", args, err)
	}
	var values []string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line != "" && !strings.HasPrefix(line, ":") {
			values = append(values, line)
		}
	}
	return values
}

func TestCompletionSuggestsLiveNamesAndIDs(t *testing.T) {
	server := useCompletionServer(t)
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"campaign IDs describe model and state", []string{"optimize", "report", ""}, []string{"4f1c2d9e0b7a4e63a1d2c3b4a5f60718\tQwen/Qwen3-8B · awaiting approval", "83ccd598540e2dd1f9c897040c1ea503\tQwen/Qwen3-0.6B · ranked"}},
		{"campaign ID prefix", []string{"optimize", "approve", "83"}, []string{"83ccd598540e2dd1f9c897040c1ea503\tQwen/Qwen3-0.6B · ranked"}},
		{"optimize actions", []string{"optimize", "re"}, []string{"report", "results"}},
		{"deployment names", []string{"status", ""}, []string{"llama-canary\tfailed", "qwen-prod\tserving · Qwen/Qwen3-8B"}},
		{"deployment for delete", []string{"delete", "qw"}, []string{"qwen-prod\tserving · Qwen/Qwen3-8B"}},
		{"endpoint names", []string{"endpoint", "inspect", ""}, []string{"support-bot\tserving"}},
		{"endpoint to profile", []string{"profile", "sup"}, []string{"support-bot\tserving"}},
		{"models for propose", []string{"optimize", "propose", "Qwen"}, []string{"Qwen/Qwen3-8B\tQwen3 8B"}},
		{"models for deploy", []string{"deploy", "meta"}, []string{"meta-llama/Llama-3.1-8B-Instruct\tLlama 3.1 8B Instruct"}},
		{"GPUs for a provider", []string{"optimize", "propose", "Qwen/Qwen3-8B", "--provider", "runpod", "--gpu", ""}, []string{"NVIDIA H100 80GB HBM3\trunpod", "NVIDIA L40S\trunpod"}},
		{"GPU with equals form", []string{"deploy", "Qwen/Qwen3-8B", "--gpu=L4"}, []string{"--gpu=L40S\taws"}},
		{"regions for provider and GPU", []string{"optimize", "propose", "Qwen/Qwen3-8B", "--provider", "runpod", "--gpu", "NVIDIA L40S", "--region", ""}, []string{"US-TX-3\trunpod"}},
		{"providers", []string{"deploy", "Qwen/Qwen3-8B", "--cloud", ""}, []string{"aws", "runpod"}},
		{"candidate of the typed campaign", []string{"optimize", "activate", "83ccd598540e2dd1f9c897040c1ea503", "--candidate", ""}, []string{"cand-lost\trejected", "cand-winner\tqualified"}},
		{"target deployment", []string{"optimize", "propose", "Qwen/Qwen3-8B", "--target-deployment", "q"}, []string{"qwen-prod\tserving · Qwen/Qwen3-8B"}},
		{"objective values", []string{"optimize", "propose", "Qwen/Qwen3-8B", "--objective", ""}, []string{"cost-efficiency", "interactive", "latency", "throughput"}},
		{"open a run", []string{"open", "run", "4f"}, []string{"4f1c2d9e0b7a4e63a1d2c3b4a5f60718\tQwen/Qwen3-8B · awaiting approval"}},
		{"flags complete once, with help", []string{"delete", "qwen-prod", "--y"}, []string{"--yes\tconfirm destructive deletion"}},
		{"unregistered flags still complete", []string{"optimize", "report", "83ccd598540e2dd1f9c897040c1ea503", "--ht"}, []string{"--html"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := completions(t, test.args...); strings.Join(got, "\n") != strings.Join(test.want, "\n") {
				t.Fatalf("completions = %q, want %q", got, test.want)
			}
		})
	}
	for _, path := range server.paths {
		if strings.Contains(path, "/approve") || strings.Contains(path, "/activate") {
			t.Fatalf("completion called a mutation path: %s", path)
		}
	}
}

func TestCompletionIsSilentOfflineAndSignedOut(t *testing.T) {
	t.Run("offline", func(t *testing.T) {
		t.Setenv("INFERCRANE_URL", "http://127.0.0.1:1")
		t.Setenv("INFERCRANE_API_KEY", "secret")
		t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
		t.Setenv("INFERCRANE_CONTEXT", "")
		started := time.Now()
		stdout, stderr, err := runCLI(t, "__complete", "optimize", "report", "")
		if err != nil || strings.TrimSpace(stderr) != "" && !strings.HasPrefix(strings.TrimSpace(stderr), "Completion ended with directive") {
			t.Fatalf("offline completion err=%v stderr=%q", err, stderr)
		}
		if lines := strings.Split(strings.TrimSpace(stdout), "\n"); len(lines) != 1 || !strings.HasPrefix(lines[0], ":") {
			t.Fatalf("offline completion printed suggestions: %q", stdout)
		}
		if elapsed := time.Since(started); elapsed > completionTimeout+time.Second {
			t.Fatalf("offline completion took %s", elapsed)
		}
	})
	t.Run("signed out", func(t *testing.T) {
		server := useCompletionServer(t)
		t.Setenv("INFERCRANE_API_KEY", "")
		t.Setenv("INFERCRANE_CREDENTIAL_STORE", "file")
		if got := completions(t, "status", ""); len(got) != 0 {
			t.Fatalf("signed-out completion = %q", got)
		}
		if len(server.paths) != 0 {
			t.Fatalf("signed-out completion contacted the control plane: %v", server.paths)
		}
	})
	t.Run("slow control plane", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(func() { close(release); server.Close() })
		t.Setenv("INFERCRANE_URL", server.URL)
		t.Setenv("INFERCRANE_API_KEY", "secret")
		t.Setenv("INFERCRANE_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
		started := time.Now()
		if got := completions(t, "status", ""); len(got) != 0 {
			t.Fatalf("slow completion = %q", got)
		}
		if elapsed := time.Since(started); elapsed > completionTimeout+time.Second {
			t.Fatalf("completion waited %s for a slow control plane", elapsed)
		}
	})
}
