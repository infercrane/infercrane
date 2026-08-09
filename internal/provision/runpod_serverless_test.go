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
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "template-qwen", "isServerless": true, "env": map[string]string{"MODEL_NAME": "Qwen/Qwen3-8B", "MODEL_REVISION": "0123456789abcdef", "RAW_OPENAI_OUTPUT": "1"}})
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
			if request["workersMin"] != float64(0) || request["workersMax"] != float64(4) || request["templateId"] != "template-qwen" {
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
