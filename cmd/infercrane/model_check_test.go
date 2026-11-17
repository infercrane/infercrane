package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/intentplan"
	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/spec"
)

func checkedEnvelope(t *testing.T) intentplan.Envelope {
	t.Helper()
	registries, err := modelcheck.Registries()
	if err != nil {
		t.Fatal(err)
	}
	model := modelcheck.Model{Repository: "microsoft/phi-4", RequestedRevision: "main", Commit: "2db69c1c3e91a05d2c64a3185acfbaf36f744e25", PipelineTag: "text-generation", License: "mit", Architectures: []string{"Phi3ForCausalLM"},
		Weights: &modelcheck.Weights{Parameters: 14659507200, ByDType: map[string]int64{"BF16": 14659507200}}, Config: &modelcheck.Config{Layers: 40, HiddenSize: 5120, AttentionHeads: 40, KVHeads: 10, ContextLength: 16384}, ConfigState: modelcheck.ConfigRead}
	report := modelcheck.Evaluate(model, modelcheck.EvaluateOptions{GPUs: []modelcheck.GPU{{Name: "L40S", MemoryGiB: 48}}, Registries: registries, Now: time.Now()})
	draft := intentplan.DeploymentDraft{Name: "phi-4", EndpointName: "phi-4", Model: "microsoft/phi-4", ModelRevision: report.Revision, Runtime: "vllm", RuntimeVersion: "0.22.1", Cloud: "runpod", ProviderAdapter: "runpod-pods", ComputeMode: "elastic", GPU: "L40S", GPUCount: 1, RuntimeArgs: []string{"--max-model-len", "8192"}, MinReplicas: 1, MaxReplicas: 1}
	plan := intentplan.Plan{Status: "ready", Model: &intentplan.Model{Review: intentplan.ReviewCheckedAutomatically, Name: "microsoft/phi-4", Repository: "microsoft/phi-4", Revision: report.Revision}, ModelCheck: &report,
		Configuration: &intentplan.Configuration{Runtime: "vllm", RuntimeVersion: "0.22.1", GPU: "L40S", GPUCount: 1, Provider: "runpod", RuntimeArgs: draft.RuntimeArgs}}
	return intentplan.Envelope{Plan: plan, DeploymentDraft: &draft}
}

func TestModelsCheckPrintsTheCheckAndWritesADeploymentSpec(t *testing.T) {
	var received intentplan.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/planning/intents" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(checkedEnvelope(t))
	}))
	defer server.Close()
	cfg := config.Config{ControlURL: server.URL, APIKey: "secret"}
	path := filepath.Join(t.TempDir(), "phi-4.yaml")
	output, err := captureStdout(t, func() error {
		return modelsCheckCommand(context.Background(), cfg, []string{"microsoft/phi-4", "--concurrency", "4", "--spec", path})
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.Model != "microsoft/phi-4" || received.Concurrency != 4 {
		t.Fatalf("unexpected planning request: %+v", received)
	}
	for _, expected := range []string{"Not reviewed: checked automatically", "Runs on vLLM 0.22.1", "Fits 1 × L40S", "infercrane deploy " + path} {
		if !strings.Contains(output, expected) {
			t.Fatalf("output is missing %q:\n%s", expected, output)
		}
	}
	document, err := spec.Load(path)
	if err != nil || document.Model.Revision != "2db69c1c3e91a05d2c64a3185acfbaf36f744e25" || strings.Join(document.Runtime.Args, " ") != "--max-model-len 8192" || document.Resources.GPU != "L40S" {
		t.Fatalf("unexpected spec: %+v err=%v", document, err)
	}
	if err = modelsCheckCommand(context.Background(), cfg, []string{"microsoft/phi-4", "--spec", path}); err == nil || !strings.Contains(err.Error(), "never overwritten") {
		t.Fatalf("an existing spec was overwritten: %v", err)
	}
	if err = modelsCheckCommand(context.Background(), cfg, []string{"phi-4"}); err == nil {
		t.Fatal("a name that is not a repository was accepted")
	}
}
