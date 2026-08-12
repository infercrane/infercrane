package provision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunPodServerlessEnsureIsReplaySafeAndScaleToZeroNative(t *testing.T) {
	creates := 0
	var endpoints []ServerlessEndpoint
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/templates/template-qwen"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "template-qwen", "isServerless": true, "env": map[string]string{"MODEL_NAME": "Qwen/Qwen3-8B", "MODEL_REVISION": "0123456789abcdef", "RAW_OPENAI_OUTPUT": "1", "ENABLE_AUTO_TOOL_CHOICE": "true", "TOOL_CALL_PARSER": "hermes"}})
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
			if request["workersMin"] != float64(0) || request["workersMax"] != float64(4) || request["templateId"] != "template-qwen" || request["minCudaVersion"] != "13.0" {
				t.Fatalf("request=%#v", request)
			}
			endpoint := ServerlessEndpoint{ID: "endpoint-1", Name: request["name"].(string), TemplateID: "template-qwen", GPUTypeIDs: []string{"NVIDIA L40S"}, WorkersMin: 0, WorkersMax: 4}
			endpoints = append(endpoints, endpoint)
			_ = json.NewEncoder(w).Encode(endpoint)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, TemplateID: "template-qwen", Client: server.Client()}
	spec := ServerlessEndpointSpec{ExternalKey: "deployment-1-rev-1", Model: "Qwen/Qwen3-8B", ModelRevision: "0123456789abcdef", GPU: "L40S", WorkersMax: 4}
	first, err := provider.EnsureEndpoint(context.Background(), spec)
	if err != nil || first.ID != "endpoint-1" || first.WorkersMin != 0 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := provider.EnsureEndpoint(context.Background(), spec)
	if err != nil || second.ID != first.ID || creates != 1 {
		t.Fatalf("second=%+v creates=%d err=%v", second, creates, err)
	}
}

func TestRunPodServerlessRejectsSemanticallyInvalidCreateSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/templates/template") {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "template", "isServerless": true, "env": map[string]string{"MODEL_NAME": "model", "MODEL_REVISION": "immutable", "RAW_OPENAI_OUTPUT": "1", "ENABLE_AUTO_TOOL_CHOICE": "true", "TOOL_CALL_PARSER": "hermes"}})
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"","name":"wrong","templateId":"other"}`))
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}
	_, err := provider.EnsureEndpoint(context.Background(), ServerlessEndpointSpec{ExternalKey: "key", Model: "model", ModelRevision: "immutable", TemplateID: "template", GPU: "L40S", WorkersMax: 1})
	if err == nil || !strings.Contains(err.Error(), "invalid endpoint identity") {
		t.Fatalf("invalid 2xx create response accepted: %v", err)
	}
}

func TestRunPodServerlessRejectsTemplateForMutableOrWrongArtifact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "template", "isServerless": true, "env": map[string]string{"MODEL_NAME": "Other/Model", "MODEL_REVISION": "main", "RAW_OPENAI_OUTPUT": "1"}})
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, TemplateID: "template", Client: server.Client()}
	_, err := provider.EnsureEndpoint(context.Background(), ServerlessEndpointSpec{ExternalKey: "key", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", GPU: "L40S", WorkersMax: 1})
	if err == nil || !strings.Contains(err.Error(), "MODEL_NAME") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunPodServerlessDeleteIsIdempotent(t *testing.T) {
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deletes++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}
	if err := provider.DeleteEndpoint(context.Background(), "gone"); err != nil || deletes != 1 {
		t.Fatalf("deletes=%d err=%v", deletes, err)
	}
}

func TestRunPodServerlessRedactsAndBoundsProviderErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("diagnostic secret " + strings.Repeat("x", 16<<10)))
	}))
	defer server.Close()
	err := (RunPodServerless{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}).DeleteEndpoint(context.Background(), "endpoint")
	if err == nil || strings.Contains(err.Error(), "secret") || len(err.Error()) > 5000 {
		t.Fatalf("unsafe provider error length=%d err=%v", len(err.Error()), err)
	}
}

func TestRunPodServerlessCheckRejectsMutableTemplate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "template", "isServerless": true, "env": map[string]string{"MODEL_NAME": "Qwen/Qwen3-8B", "MODEL_REVISION": "main", "RAW_OPENAI_OUTPUT": "1"}})
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, TemplateID: "template", Client: server.Client()}
	if err := provider.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunPodServerlessCheckRejectsTemplateWithoutToolCalling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "template", "isServerless": true, "env": map[string]string{"MODEL_NAME": "Qwen/Qwen3-8B", "MODEL_REVISION": "immutable", "RAW_OPENAI_OUTPUT": "1"}})
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, TemplateID: "template", Client: server.Client()}
	if err := provider.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "tool") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunPodServerlessHealthReportsWorkersWithoutInferenceRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/endpoint-1/health" || r.Method != http.MethodGet {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"workers": map[string]int{"idle": 0, "running": 0}, "jobs": map[string]int{"completed": 2, "failed": 1, "inProgress": 0, "inQueue": 1}})
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", InferenceBaseURL: server.URL, Client: server.Client()}
	health, err := provider.EndpointHealth(context.Background(), "endpoint-1")
	if err != nil || health.WorkersIdle != 0 || health.WorkersRunning != 0 || health.JobsCompleted != 2 || health.JobsQueue != 1 {
		t.Fatalf("health=%+v err=%v", health, err)
	}
}

func TestRunPodServerlessActiveWorkersExcludesExitedHistory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "endpoint-1", "workers": []map[string]string{{"desiredStatus": "RUNNING"}, {"desiredStatus": "EXITED"}, {"desiredStatus": "EXITED"}}}})
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}
	workers, err := provider.ActiveWorkers(context.Background(), "endpoint-1")
	if err != nil || workers != 1 {
		t.Fatalf("workers=%d err=%v", workers, err)
	}
}
