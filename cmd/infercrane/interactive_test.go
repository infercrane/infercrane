package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/domain"
)

// usePrompt answers the next prompt with answer and captures what it showed.
func usePrompt(t *testing.T, allowed bool, answer string) *bytes.Buffer {
	t.Helper()
	shown := &bytes.Buffer{}
	previousInput, previousOutput, previousAllowed := promptInput, promptOutput, promptAllowed
	promptInput, promptOutput = strings.NewReader(answer), shown
	promptAllowed = func(output string) bool { return allowed && output != "json" }
	t.Cleanup(func() { promptInput, promptOutput, promptAllowed = previousInput, previousOutput, previousAllowed })
	return shown
}

type deleteServer struct {
	deleted bool
}

func (s *deleteServer) saw(method string) bool { return method == http.MethodDelete && s.deleted }

func (s *deleteServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v1/deployments/qwen":
		_, _ = w.Write([]byte(`{"deployment":{"name":"qwen","model":"Qwen/Qwen3-8B","observed_state":"serving","min_replicas":1,"max_replicas":2}}`))
	case "DELETE /api/v1/deployments/qwen":
		s.deleted = true
		_, _ = w.Write([]byte(`{"operation":{"id":"op-delete","kind":"deployment.delete","status":"queued"}}`))
	default:
		http.NotFound(w, r)
	}
}

func useDeleteServer(t *testing.T) *deleteServer {
	t.Helper()
	recorder := &deleteServer{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_CONFIG", t.TempDir()+"/missing.json")
	t.Setenv("INFERCRANE_CONTEXT", "")
	return recorder
}

func TestDeleteAsksForTheNameOnlyAtATerminal(t *testing.T) {
	t.Run("no terminal keeps requiring --yes", func(t *testing.T) {
		recorder := useDeleteServer(t)
		usePrompt(t, false, "qwen\n")
		_, _, err := runCLI(t, "delete", "qwen")
		if err == nil || !strings.Contains(err.Error(), "deletion requires --yes") {
			t.Fatalf("delete without --yes and without a terminal = %v", err)
		}
		if recorder.saw(http.MethodDelete) {
			t.Fatal("delete ran without confirmation")
		}
	})
	t.Run("wrong name cancels", func(t *testing.T) {
		recorder := useDeleteServer(t)
		shown := usePrompt(t, true, "qwen-other\n")
		_, _, err := runCLI(t, "delete", "qwen")
		if !errors.Is(err, errDeclined) {
			t.Fatalf("delete with a wrong name = %v", err)
		}
		if recorder.saw(http.MethodDelete) {
			t.Fatal("delete ran after a wrong confirmation")
		}
		for _, want := range []string{"DELETE", "qwen", "withdraw deployment from new routing", "Qwen/Qwen3-8B · SERVING · 1–2 replicas", "Type qwen to confirm"} {
			if !strings.Contains(ansi.Strip(shown.String()), want) {
				t.Fatalf("prompt is missing %q:\n%s", want, shown)
			}
		}
	})
	t.Run("typed name deletes", func(t *testing.T) {
		recorder := useDeleteServer(t)
		usePrompt(t, true, "qwen\n")
		if _, _, err := runCLI(t, "delete", "qwen"); err != nil {
			t.Fatalf("confirmed delete = %v", err)
		}
		if !recorder.saw(http.MethodDelete) {
			t.Fatal("confirmed delete did not call the control plane")
		}
	})
	t.Run("--yes never prompts", func(t *testing.T) {
		recorder := useDeleteServer(t)
		shown := usePrompt(t, true, "")
		if _, _, err := runCLI(t, "delete", "qwen", "--yes"); err != nil || shown.Len() != 0 || !recorder.saw(http.MethodDelete) {
			t.Fatalf("delete --yes err=%v prompt=%q", err, shown)
		}
	})
	t.Run("json never prompts", func(t *testing.T) {
		recorder := useDeleteServer(t)
		shown := usePrompt(t, true, "qwen\n")
		if _, _, err := runCLI(t, "delete", "qwen", "--output", "json"); err == nil || shown.Len() != 0 || recorder.saw(http.MethodDelete) {
			t.Fatalf("delete --output json err=%v prompt=%q", err, shown)
		}
	})
}

type approvalServer struct {
	approved bool
}

func (s *approvalServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	campaign := map[string]any{
		"id": "83ccd598540e2dd1f9c897040c1ea503", "model_identity": "Qwen/Qwen3-0.6B", "objective": "interactive", "state": "awaiting_approval",
		"warnings": []string{"prices are sourced observations"},
		"candidates": []map[string]any{
			{"id": "cand-1", "state": "proposed", "deployment_spec": map[string]any{"runtime": map[string]any{"engine": "vllm"}, "resources": map[string]any{"gpu": "RTX4090", "gpu_count": 1}, "provider": map[string]any{"cloud": "vast"}}},
			{"id": "cand-2", "state": "proposed", "deployment_spec": map[string]any{"runtime": map[string]any{"engine": "vllm", "args": []string{"--kv-cache-dtype", "fp8"}}, "resources": map[string]any{"gpu": "RTX4090", "gpu_count": 1}, "provider": map[string]any{"cloud": "vast", "region": "EU"}}},
		},
	}
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/approve") {
		s.approved = true
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"campaign": campaign})
}

func useApprovalServer(t *testing.T) *approvalServer {
	t.Helper()
	recorder := &approvalServer{}
	server := httptest.NewServer(recorder)
	t.Cleanup(server.Close)
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "secret")
	t.Setenv("INFERCRANE_CONFIG", t.TempDir()+"/missing.json")
	t.Setenv("INFERCRANE_CONTEXT", "")
	return recorder
}

func TestApproveShowsCapExpiryAndWhatRuns(t *testing.T) {
	args := []string{"optimize", "approve", "83ccd598540e2dd1f9c897040c1ea503", "--max-cost-usd", "25", "--expires-in", "2h"}
	t.Run("no answers no", func(t *testing.T) {
		server := useApprovalServer(t)
		shown := usePrompt(t, true, "\n")
		if _, _, err := runCLI(t, args...); !errors.Is(err, errDeclined) {
			t.Fatalf("declined approval = %v", err)
		}
		if server.approved {
			t.Fatal("approval was sent after the person said no")
		}
		text := ansi.Strip(shown.String())
		for _, want := range []string{"APPROVE PAID RUN", "$25.00, a hard maximum", "(in 2h)", "2 setups", "vllm, default settings · 1 × RTX4090 · vast", "vllm, --kv-cache-dtype fp8 · 1 × RTX4090 · vast EU", "▲ prices are sourced observations", "Approve spending up to $25.00? [y/N]"} {
			if !strings.Contains(text, want) {
				t.Fatalf("approval prompt is missing %q:\n%s", want, text)
			}
		}
	})
	t.Run("yes approves", func(t *testing.T) {
		server := useApprovalServer(t)
		usePrompt(t, true, "y\n")
		if _, _, err := runCLI(t, args...); err != nil || !server.approved {
			t.Fatalf("approved err=%v sent=%t", err, server.approved)
		}
	})
	t.Run("no terminal approves without a prompt, as scripts expect", func(t *testing.T) {
		server := useApprovalServer(t)
		shown := usePrompt(t, false, "")
		if _, _, err := runCLI(t, args...); err != nil || !server.approved || shown.Len() != 0 {
			t.Fatalf("scripted approve err=%v sent=%t prompt=%q", err, server.approved, shown)
		}
	})
	t.Run("--yes skips the prompt", func(t *testing.T) {
		server := useApprovalServer(t)
		shown := usePrompt(t, true, "")
		if _, _, err := runCLI(t, append(args, "--yes")...); err != nil || !server.approved || shown.Len() != 0 {
			t.Fatalf("approve --yes err=%v sent=%t prompt=%q", err, server.approved, shown)
		}
	})
}

func TestLiveProgressCommitsFinishedPhasesAndClearsItsLine(t *testing.T) {
	var out bytes.Buffer
	tracker := newOperationTracker(&out, styledTheme(80, "dark"))
	start := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	tracker.update(domain.Operation{ID: "op-1", Status: "waiting", Progress: 30, CurrentStep: "replica.0.ensure", Message: "provider is allocating capacity", CreatedAt: start}, start.Add(5*time.Second))
	tracker.update(domain.Operation{ID: "op-1", Status: "running", Progress: 70, CurrentStep: "replica.0.runtime", Message: "starting vllm", CreatedAt: start}, start.Add(65*time.Second))
	final := domain.Operation{ID: "op-1", Status: "succeeded", Progress: 100, CreatedAt: start}
	tracker.update(final, start.Add(90*time.Second))
	tracker.done(final, start.Add(90*time.Second))
	text := ansi.Strip(out.String())
	for _, want := range []string{"✓ WAITING FOR CAPACITY", "1m0s", "✓ STARTING RUNTIME", "25s", "starting vllm", "✓ READY                 1m30s in all"} {
		if !strings.Contains(text, want) {
			t.Fatalf("live progress is missing %q:\n%q", want, text)
		}
	}
	if !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("live progress left the cursor on a redrawn line: %q", out.String())
	}
}

func TestWaitProgressStaysPlainForScripts(t *testing.T) {
	if mode := progressFor("json"); mode != progressLines {
		t.Fatalf("json progress = %v, want plain lines", mode)
	}
	if mode := progressForHuman("json"); mode != progressNone {
		t.Fatalf("rollout json progress = %v, want none", mode)
	}
	// Tests run without a terminal, so human output also stays plain.
	if mode := progressFor("human"); mode != progressLines {
		t.Fatalf("redirected human progress = %v, want plain lines", mode)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(domain.Operation{ID: "op-1", Status: "succeeded", Progress: 100})
	}))
	defer server.Close()
	if _, err := waitForOperation(context.Background(), config.Config{ControlURL: server.URL, APIKey: "secret"}, "op-1", progressNone); err != nil {
		t.Fatal(err)
	}
}
