package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
)

func TestDeployMaxModelLenBecomesTheRuntimesContextFlag(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending"},"context_length":{"tokens":4096,"model_tokens":40960,"set_by":"request","reason":"The deployment's engine arguments set the context length."}}`))
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	for runtime, want := range map[string]string{"vllm": "[--max-model-len 4096]", "sglang": "[--context-length 4096]"} {
		output, err := captureStdout(t, func() error {
			return deployAPICommand(context.Background(), cfg, "deploy", []string{"Qwen/Qwen3-8B", "--cloud", "kubernetes", "--gpu", "NVIDIA-A10G", "--runtime", runtime, "--max-model-len", "4096", "--idempotency-key", "context-1"})
		})
		args, _ := body["runtime_args"].([]any)
		if err != nil || len(args) != 2 || "["+args[0].(string)+" "+args[1].(string)+"]" != want {
			t.Fatalf("%s: runtime_args=%v err=%v", runtime, body["runtime_args"], err)
		}
		if !strings.Contains(output, "Context          4096 tokens\n") || strings.Contains(output, "engine arguments set") {
			t.Fatalf("%s: output %q", runtime, output)
		}
	}
	spec := filepath.Join(t.TempDir(), "qwen.yaml")
	if err := os.WriteFile(spec, []byte("apiVersion: infercrane.dev/v1\nkind: Deployment\nname: qwen\nmodel:\n  id: Qwen/Qwen3-8B\nresources:\n  gpu: L40S\nprovider:\n  cloud: runpod\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"Qwen/Qwen3-8B", "--cloud", "kubernetes", "--gpu", "NVIDIA-A10G", "--max-model-len", "100"},
		{"Qwen/Qwen3-8B", "--targets", "worker-a", "--max-model-len", "8192"},
		{"Qwen/Qwen3-8B", "--cloud", "kubernetes", "--gpu", "NVIDIA-A10G", "--runtime", "custom-oci", "--max-model-len", "8192"},
		{spec, "--max-model-len", "8192"},
	} {
		if err := deployAPICommand(context.Background(), cfg, "deploy", args); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
}

func TestDeployPrintsTheContextInferCraneChose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"operation":{"id":"op-1","status":"pending"},"context_length":{"tokens":8192,"model_tokens":40960,"set_by":"infercrane","reason":"By InferCrane's estimate, one request at the model's own maximum of 40960 tokens does not fit one NVIDIA-A10G next to the weights."}}`))
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	output, err := captureStdout(t, func() error {
		return deployAPICommand(context.Background(), cfg, "deploy", []string{"Qwen/Qwen3-8B", "--cloud", "kubernetes", "--gpu", "NVIDIA-A10G", "--idempotency-key", "context-2"})
	})
	if err != nil || !strings.Contains(output, "Context          8192 tokens, set by InferCrane\n") || !strings.Contains(output, "40960 tokens does not fit one NVIDIA-A10G") {
		t.Fatalf("output=%q err=%v", output, err)
	}
	output, err = captureStdout(t, func() error {
		return deployAPICommand(context.Background(), cfg, "deploy", []string{"Qwen/Qwen3-8B", "--cloud", "kubernetes", "--gpu", "NVIDIA-A10G", "--idempotency-key", "context-3", "--output", "json"})
	})
	var document struct {
		ContextLength deployContextLength `json:"context_length"`
	}
	if err != nil || json.Unmarshal([]byte(output), &document) != nil || document.ContextLength.Tokens == nil || *document.ContextLength.Tokens != 8192 || document.ContextLength.SetBy != "infercrane" {
		t.Fatalf("output=%q err=%v", output, err)
	}
}
