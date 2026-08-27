package provision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

func TestRunPodPodsLifecycleIsReplaySafeAndPreservesImmutableWorkload(t *testing.T) {
	var pods []runPodRecord
	createCalls, deleteCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Fatal("provider authorization header missing")
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/pods":
			_ = json.NewEncoder(w).Encode(pods)
		case r.Method == http.MethodPost && r.URL.Path == "/pods":
			createCalls++
			var body struct {
				Name             string            `json:"name"`
				ImageName        string            `json:"imageName"`
				GPUTypeIDs       []string          `json:"gpuTypeIds"`
				GPUCount         int               `json:"gpuCount"`
				DockerEntrypoint []string          `json:"dockerEntrypoint"`
				DockerStartCmd   []string          `json:"dockerStartCmd"`
				Environment      map[string]string `json:"env"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.ImageName != testRunPodWorkload().Image || body.GPUCount != 4 || body.GPUTypeIDs[0] != "NVIDIA H200" || body.DockerEntrypoint[0] != "vllm" || body.DockerStartCmd[1] != "org/model" || body.Environment["VLLM_API_KEY"] != "worker-secret" {
				t.Fatalf("unexpected create body: %#v", body)
			}
			pods = []runPodRecord{{ID: "pod-1", Name: body.Name, DesiredStatus: "RUNNING", ImageName: body.ImageName, GPUCount: body.GPUCount, DockerEntrypoint: body.DockerEntrypoint, DockerStartCmd: body.DockerStartCmd}}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(pods[0])
		case r.Method == http.MethodDelete && r.URL.Path == "/pods/pod-1":
			deleteCalls++
			pods = nil
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client(), ContainerDiskGiB: 500}
	spec := ReplicaSpec{ExternalKey: "deployment-revision-r0", Model: "org/model", ModelRevision: "commit", Cloud: "runpod", GPU: "H200", GPUCount: 4, Workload: testRunPodWorkload()}
	first, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || first.ResourceID != "pod-1" {
		t.Fatalf("first ensure: handle=%#v err=%v", first, err)
	}
	second, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || second.ResourceID != first.ResourceID || createCalls != 1 {
		t.Fatalf("replayed ensure: handle=%#v calls=%d err=%v", second, createCalls, err)
	}
	observed, err := provider.ObserveReplica(context.Background(), second, 8000)
	if err != nil || observed.State != "ready" || observed.Endpoint != "https://pod-1-8000.proxy.runpod.net" || strings.Contains(observed.Details, "secret") {
		t.Fatalf("observe: %#v err=%v", observed, err)
	}
	if err = provider.DeleteReplica(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err = provider.DeleteReplica(context.Background(), second); err != nil || deleteCalls != 1 {
		t.Fatalf("idempotent delete: calls=%d err=%v", deleteCalls, err)
	}
}

func TestRunPodPodsRejectsConflictingAdoptionAndRedactsErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]runPodRecord{{ID: "pod-1", Name: clusterName("key"), DesiredStatus: "RUNNING", ImageName: "registry.example/wrong@sha256:" + strings.Repeat("b", 64), GPUCount: 4}})
			return
		}
		http.Error(w, "provider-secret", http.StatusUnauthorized)
	}))
	defer server.Close()
	provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client()}
	_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "key", Model: "org/model", Cloud: "runpod", GPU: "H200", GPUCount: 4, Workload: testRunPodWorkload()})
	if err == nil || !strings.Contains(err.Error(), "immutable workload intent") || strings.Contains(err.Error(), "provider-secret") {
		t.Fatalf("unsafe conflict error: %v", err)
	}
}

func TestRunPodPodsAdoptsAfterLostCreateResponse(t *testing.T) {
	var pods []runPodRecord
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(pods)
		case http.MethodPost:
			var body struct {
				Name             string   `json:"name"`
				ImageName        string   `json:"imageName"`
				GPUCount         int      `json:"gpuCount"`
				DockerEntrypoint []string `json:"dockerEntrypoint"`
				DockerStartCmd   []string `json:"dockerStartCmd"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			pods = []runPodRecord{{ID: "adopted", Name: body.Name, DesiredStatus: "RUNNING", ImageName: body.ImageName, GPUCount: body.GPUCount, DockerEntrypoint: body.DockerEntrypoint, DockerStartCmd: body.DockerStartCmd}}
			http.Error(w, "response lost after commit", http.StatusBadGateway)
		}
	}))
	defer server.Close()
	provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client()}
	handle, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "lost", Model: "org/model", ModelRevision: "commit", Cloud: "runpod", GPU: "H200", GPUCount: 4, Workload: testRunPodWorkload()})
	if err != nil || handle.ResourceID != "adopted" {
		t.Fatalf("handle=%#v err=%v", handle, err)
	}
}

func testRunPodWorkload() runtimecontract.Workload {
	return runtimecontract.Workload{
		Image:    "registry.example/runtime@sha256:" + strings.Repeat("a", 64),
		Command:  []string{"vllm", "serve", "${MODEL}", "--revision", "${MODEL_REVISION}", "--port", "${PORT}"},
		Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics",
		Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30,
	}
}
