package provision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// These contract tests are intentionally adapter-facing. Every provider added
// to InferCrane must prove replay-safe create/adopt, observable identity, and
// idempotent deletion without invoking a real cloud API.
func TestProviderContractSkyPilotLifecycle(t *testing.T) {
	runner := &fakeSkyRunner{}
	provider := SkyPilot{APIKey: "test-worker-key", Runner: runner}
	spec := ReplicaSpec{ExternalKey: "prod-r0", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", Cloud: "test-cloud", GPU: "test-gpu"}

	first, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.RequestID = first.RequestID
	second, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || second.ResourceID != first.ResourceID || runner.launches != 1 {
		t.Fatalf("replay created another resource: first=%+v second=%+v launches=%d err=%v", first, second, runner.launches, err)
	}
	observed, err := provider.ObserveReplica(context.Background(), second, 8000)
	if err != nil || !observed.Exists || observed.State != "ready" || observed.Endpoint == "" {
		t.Fatalf("observation=%+v err=%v", observed, err)
	}
	if err = provider.DeleteReplica(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err = provider.DeleteReplica(context.Background(), second); err != nil || runner.downs != 1 {
		t.Fatalf("delete is not idempotent: downs=%d err=%v", runner.downs, err)
	}
}

func TestProviderContractRunPodServerlessLifecycleAndLostResponseAdoption(t *testing.T) {
	var mu sync.Mutex
	var endpoints []ServerlessEndpoint
	creates, deletes := 0, 0
	dropFirstResponse := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/templates/template-qwen"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "template-qwen", "isServerless": true, "env": map[string]string{
				"MODEL_NAME": "Qwen/Qwen3-8B", "MODEL_REVISION": "immutable", "RAW_OPENAI_OUTPUT": "1",
				"ENABLE_AUTO_TOOL_CHOICE": "true", "TOOL_CALL_PARSER": "hermes",
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/endpoints":
			listed := make([]map[string]any, len(endpoints))
			for i, endpoint := range endpoints {
				listed[i] = map[string]any{"id": endpoint.ID, "name": endpoint.Name, "templateId": endpoint.TemplateID, "gpuTypeIds": endpoint.GPUTypeIDs, "workersMin": endpoint.WorkersMin, "workersMax": endpoint.WorkersMax, "workers": []any{}}
			}
			_ = json.NewEncoder(w).Encode(listed)
		case r.Method == http.MethodPost && r.URL.Path == "/endpoints":
			creates++
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			endpoint := ServerlessEndpoint{ID: "endpoint-1", Name: request["name"].(string), TemplateID: "template-qwen", GPUTypeIDs: []string{"NVIDIA L40S"}, WorkersMax: 2}
			endpoints = append(endpoints, endpoint)
			if dropFirstResponse {
				dropFirstResponse = false
				panic(http.ErrAbortHandler)
			}
			_ = json.NewEncoder(w).Encode(endpoint)
		case r.Method == http.MethodDelete && r.URL.Path == "/endpoints/endpoint-1":
			if len(endpoints) == 0 {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			deletes++
			endpoints = nil
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	provider := RunPodServerless{APIKey: "test-provider-key", BaseURL: server.URL, TemplateID: "template-qwen", Client: server.Client()}
	spec := ServerlessEndpointSpec{ExternalKey: "deployment-revision", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", GPU: "L40S", WorkersMax: 2}
	if _, err := provider.EnsureEndpoint(context.Background(), spec); err == nil {
		t.Fatal("lost provider response was not surfaced")
	}
	adopted, err := provider.EnsureEndpoint(context.Background(), spec)
	mu.Lock()
	createCount := creates
	mu.Unlock()
	if err != nil || adopted.ID != "endpoint-1" || createCount != 1 {
		t.Fatalf("provider response loss created a duplicate: endpoint=%+v creates=%d err=%v", adopted, createCount, err)
	}
	if err = provider.DeleteEndpoint(context.Background(), adopted.ID); err != nil {
		t.Fatal(err)
	}
	if err = provider.DeleteEndpoint(context.Background(), adopted.ID); err != nil {
		// The fake returns 404 on the replay, which the adapter must treat as success.
		t.Fatal(err)
	}
	mu.Lock()
	deleteCount, endpointCount := deletes, len(endpoints)
	mu.Unlock()
	if deleteCount != 1 || endpointCount != 0 {
		t.Fatalf("cleanup incomplete: deletes=%d endpoints=%d", deleteCount, endpointCount)
	}
}
