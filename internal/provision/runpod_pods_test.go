package provision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/artifactcache"
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
	if err != nil || first.ResourceID != clusterName(spec.ExternalKey) {
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

func TestRunPodArtifactCacheAdoptsOnlyConfiguredExactVolume(t *testing.T) {
	identity := "org/model@0123456789abcdef0123456789abcdef01234567"
	if name := runPodArtifactVolumeName(identity); name != "infercrane-artifact-a5d33b0cefe277aeeb41" {
		t.Fatalf("provider-safe volume identity drifted from the external builder: %s", name)
	}
	volume := runPodNetworkVolume{ID: "volume_1234", Name: runPodArtifactVolumeName(identity), DataCenterID: "EU-RO-1", Size: 500}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/networkvolumes" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]runPodNetworkVolume{volume})
	}))
	defer server.Close()
	provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client(), ArtifactCachePolicy: "required", NetworkVolumes: map[string]string{identity: volume.ID}}
	request := artifactcache.Request{ArtifactID: "artifact", ModelIdentity: identity, Provider: "runpod", Region: volume.DataCenterID, Location: "runpod-volume://" + volume.ID, IdempotencyKey: "prefetch"}
	operation, err := provider.Prefetch(context.Background(), request)
	if err != nil || operation.Status != "succeeded" || operation.ProviderOperationID != volume.ID {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	observation, err := provider.Observe(context.Background(), request)
	if err != nil || observation.State != "present" || observation.Source != "runpod-network-volume" || !observation.ExpiresAt.After(time.Now()) || !strings.Contains(observation.EvidenceJSON, `"population_state":"operator_attested"`) {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
	request.Location = "runpod-volume://other"
	if _, err = provider.Prefetch(context.Background(), request); err == nil {
		t.Fatal("unconfigured volume was adopted")
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

func TestRunPodPodsAdoptsRunPodListShapeWithMachineGPUType(t *testing.T) {
	workload := testRunPodWorkload()
	command := expandWorkloadCommand(workload.Command, "org/model", "commit", workload.Port, nil)
	pod := runPodRecord{ID: "pod-1", Name: clusterName("key"), DesiredStatus: "RUNNING", Image: workload.Image, GPUCount: 4, DockerEntrypoint: command[:1], DockerStartCmd: command[1:]}
	pod.Machine.GPUTypeID = "NVIDIA H200"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/pods" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]runPodRecord{pod})
	}))
	defer server.Close()
	provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client()}
	handle, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "key", Model: "org/model", ModelRevision: "commit", Cloud: "runpod", GPU: "H200", GPUCount: 4, Workload: workload})
	if err != nil || handle.ResourceID != clusterName("key") {
		t.Fatalf("handle=%#v err=%v", handle, err)
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
	if err != nil || handle.ResourceID != clusterName("lost") {
		t.Fatalf("handle=%#v err=%v", handle, err)
	}
}

func TestRunPodPodsUsesExactPersistentModelVolumeAndRegion(t *testing.T) {
	identity := "org/model@0123456789abcdef0123456789abcdef01234567"
	volume := runPodNetworkVolume{ID: "volume_1234", Name: runPodArtifactVolumeName(identity), DataCenterID: "EU-RO-1", Size: 500}
	var created runPodRecord
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/pods":
			if created.ID == "" {
				_ = json.NewEncoder(w).Encode([]runPodRecord{})
			} else {
				_ = json.NewEncoder(w).Encode([]runPodRecord{created})
			}
		case r.Method == http.MethodGet && r.URL.Path == "/networkvolumes":
			_ = json.NewEncoder(w).Encode([]runPodNetworkVolume{volume})
		case r.Method == http.MethodPost && r.URL.Path == "/pods":
			var body struct {
				Name, ImageName, NetworkVolumeID, VolumeMountPath string
				GPUCount                                          int
				DockerEntrypoint, DockerStartCmd, DataCenterIDs   []string
				Environment                                       map[string]string `json:"env"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.NetworkVolumeID != volume.ID || body.VolumeMountPath != "/workspace" || len(body.DataCenterIDs) != 0 || body.Environment["INFERCRANE_MODEL_DIR"] != "/workspace/infercrane/model" || body.Environment["HF_XET_HIGH_PERFORMANCE"] != "1" || body.Environment["HF_TOKEN"] != "{{ RUNPOD_SECRET_infercrane-hf }}" {
				t.Fatalf("unexpected persistent cache request: %#v", body)
			}
			created = runPodRecord{ID: "pod-volume", Name: body.Name, DesiredStatus: "RUNNING", ImageName: body.ImageName, GPUCount: body.GPUCount, DockerEntrypoint: body.DockerEntrypoint, DockerStartCmd: body.DockerStartCmd, NetworkVolume: &volume}
			created.Machine.GPUTypeID = "NVIDIA H200"
			created.Machine.DataCenterID = volume.DataCenterID
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(created)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client(), ArtifactCachePolicy: "required", NetworkVolumes: map[string]string{identity: volume.ID}, HFTokenSecret: "infercrane-hf"}
	spec := ReplicaSpec{ExternalKey: "volume", Model: "org/model", ModelRevision: "0123456789abcdef0123456789abcdef01234567", Cloud: "runpod", Region: volume.DataCenterID, GPU: "H200", GPUCount: 1, Workload: testRunPodWorkload()}
	handle, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || handle.ResourceID == "" {
		t.Fatalf("handle=%#v err=%v", handle, err)
	}
	if _, err = provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatalf("adopt persistent volume pod: %v", err)
	}
	observed, err := provider.ObserveReplica(context.Background(), handle, 8000)
	if err != nil || !strings.Contains(observed.Details, `"state":"attached"`) || strings.Contains(observed.Details, "worker-secret") {
		t.Fatalf("observation=%#v err=%v", observed, err)
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
