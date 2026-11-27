package provision

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/artifactcache"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/support"
)

func TestRunPodPodsLifecycleIsReplaySafeAndPreservesImmutableWorkload(t *testing.T) {
	var pods []runPodRecord
	createCalls, deleteCalls := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer provider-secret" {
			t.Fatal("provider authorization header missing")
		}
		if r.Header.Get("User-Agent") != runPodPodsUserAgent {
			t.Fatalf("unexpected RunPod user agent: %q", r.Header.Get("User-Agent"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/pods":
			writeRunPodV2List(w, "pods", pods)
		case r.Method == http.MethodPost && r.URL.Path == "/pods":
			createCalls++
			body, ok := decodeRunPodV2PodCreate(t, w, r)
			if !ok {
				return
			}
			if body.Image != testRunPodWorkload().Image || body.GPU.Count != 4 || body.GPU.ID != "NVIDIA H200" || body.Cloud != "SECURE" || body.Disk != 500 || body.Entrypoint[0] != "vllm" || body.Cmd[1] != "org/model" || body.Env["VLLM_API_KEY"] != "worker-secret" || strings.Join(body.Ports, ",") != "8000/http" || body.Mounts != nil || body.DataCenterIDs != nil {
				t.Fatalf("unexpected create body: %#v", body)
			}
			pods = []runPodRecord{body.pod("pod-1", "US-TX-3")}
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
	var details map[string]any
	if err = json.Unmarshal([]byte(observed.Details), &details); err != nil || details["status"] != "RUNNING" || details["data_center_id"] != "US-TX-3" || details["cost_per_hour"] != 0.79 || details["network_volume"] != nil {
		t.Fatalf("observation details=%s err=%v", observed.Details, err)
	}
	if err = provider.DeleteReplica(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if err = provider.DeleteReplica(context.Background(), second); err != nil || deleteCalls != 1 {
		t.Fatalf("idempotent delete: calls=%d err=%v", deleteCalls, err)
	}
}

func TestRunPodPodsObservationMapsV2Status(t *testing.T) {
	for status, want := range map[string]string{"PROVISIONING": "starting", "STARTING": "starting", "RUNNING": "ready", "EXITED": "failed", "ERROR": "failed", "TERMINATED": "failed"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeRunPodV2List(w, "pods", []runPodRecord{{ID: "pod-1", Name: clusterName("key"), Status: status}})
		}))
		observed, err := (RunPodPods{APIKey: "provider-secret", BaseURL: server.URL, Client: server.Client()}).ObserveReplica(context.Background(), ProviderHandle{ResourceID: clusterName("key")}, 8000)
		server.Close()
		if err != nil || observed.State != want {
			t.Fatalf("status %s: observed=%#v err=%v", status, observed, err)
		}
	}
}

func TestRunPodPodsDeleteTreatsVanishedPodAsDeleted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeRunPodV2List(w, "pods", []runPodRecord{{ID: "pod-1", Name: clusterName("key"), Status: "RUNNING"}})
			return
		}
		writeRunPodV2Problem(w, http.StatusNotFound, "pod not found")
	}))
	defer server.Close()
	if err := (RunPodPods{APIKey: "provider-secret", BaseURL: server.URL, Client: server.Client()}).DeleteReplica(context.Background(), ProviderHandle{ResourceID: clusterName("key")}); err != nil {
		t.Fatalf("a Pod deleted between list and delete failed cleanup: %v", err)
	}
}

func TestRunPodArtifactCacheAdoptsOnlyConfiguredExactVolume(t *testing.T) {
	identity := "org/model@0123456789abcdef0123456789abcdef01234567"
	if name := runPodArtifactVolumeName(identity); name != "infercrane-artifact-a5d33b0cefe277aeeb41" {
		t.Fatalf("provider-safe volume identity drifted from the external builder: %s", name)
	}
	volume := runPodNetworkVolume{ID: "volume_1234", Name: runPodArtifactVolumeName(identity), DataCenterID: "EU-RO-1", Size: 500}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/network-volumes" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"networkVolumes":[{"id":"volume_1234","name":"` + volume.Name + `","size":500,"dataCenter":"EU-RO-1","type":"STANDARD"}],"pagination":{"nextCursor":null,"hasNextPage":false}}`))
	}))
	defer server.Close()
	provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client(), ArtifactCachePolicy: "required", NetworkVolumes: map[string]string{identity: volume.ID}}
	request := artifactcache.Request{ArtifactID: "artifact", ModelIdentity: identity, Provider: "runpod", Region: volume.DataCenterID, Location: "runpod-volume://" + volume.ID, IdempotencyKey: "prefetch"}
	operation, err := provider.Prefetch(context.Background(), request)
	if err != nil || operation.Status != "succeeded" || operation.ProviderOperationID != volume.ID {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	observation, err := provider.Observe(context.Background(), request)
	if err != nil || observation.State != "present" || observation.Source != "runpod-network-volume" || !observation.ExpiresAt.After(time.Now()) || !strings.Contains(observation.EvidenceJSON, `"population_state":"operator_attested"`) || !strings.Contains(observation.EvidenceJSON, `"data_center_id":"EU-RO-1"`) {
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
			writeRunPodV2List(w, "pods", []runPodRecord{{ID: "pod-1", Name: clusterName("key"), Status: "RUNNING", Image: "registry.example/wrong@sha256:" + strings.Repeat("b", 64), GPU: &runPodGPU{ID: "NVIDIA H200", Count: 4}}})
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

// RunPod v2 create semantics: 422 is a contract violation (never retry), 400
// is a GPU and data center combination that could not be placed, 402 is an
// account without balance.
func TestRunPodPodsClassifiesV2CreateFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusUnprocessableEntity, ErrInvalidReplicaSpec},
		{http.StatusBadRequest, ErrProviderCapacity},
		{http.StatusPaymentRequired, ErrProviderQuota},
		{http.StatusForbidden, ErrProviderAuthorization},
		{http.StatusTooManyRequests, ErrProviderCapacity},
		{http.StatusServiceUnavailable, ErrRequestFailed},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				writeRunPodV2List(w, "pods", []runPodRecord{})
				return
			}
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"title":"x","status":0,"detail":"sensitive-provider-detail","errors":["$: additional properties 'bogus' not allowed"]}`))
		}))
		provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client()}
		_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "key", Model: "org/model", Cloud: "runpod", GPU: "L40S", GPUCount: 1, Workload: testRunPodWorkload()})
		server.Close()
		if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "sensitive-provider-detail") {
			t.Fatalf("HTTP %d: err=%v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestRunPodPodsGlobalRegionOmitsPlacementAndAdoptsConcreteDatacenter(t *testing.T) {
	var pods []runPodRecord
	createCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/pods":
			writeRunPodV2List(w, "pods", pods)
		case r.Method == http.MethodPost && r.URL.Path == "/pods":
			createCalls++
			body, ok := decodeRunPodV2PodCreate(t, w, r)
			if !ok {
				return
			}
			if len(body.DataCenterIDs) != 0 {
				t.Fatalf("global placement leaked as a RunPod datacenter constraint: %#v", body.DataCenterIDs)
			}
			pods = []runPodRecord{body.pod("pod-global", "EU-RO-1")}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(pods[0])
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client()}
	spec := ReplicaSpec{ExternalKey: "global", Model: "org/model", ModelRevision: "commit", Cloud: "runpod", Region: "global", GPU: "L40S", GPUCount: 1, Workload: testRunPodWorkload()}
	first, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || first.ResourceID == "" {
		t.Fatalf("create global placement: handle=%#v err=%v", first, err)
	}
	second, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || second != first || createCalls != 1 {
		t.Fatalf("adopt provider-selected datacenter: first=%#v second=%#v creates=%d err=%v", first, second, createCalls, err)
	}
}

func TestRunPodPodsLaunchesMI300XWithRunPodsAMDTypeAndTheROCmImage(t *testing.T) {
	workload, _ := support.RuntimeWorkload("vllm", "rocm-gfx942")
	var pods []runPodRecord
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeRunPodV2List(w, "pods", pods)
			return
		}
		body, ok := decodeRunPodV2PodCreate(t, w, r)
		if !ok {
			return
		}
		if body.GPU.ID != "AMD Instinct MI300X OAM" || body.Image != workload.Image || body.Cloud != "SECURE" || body.Entrypoint[0] != "vllm" {
			t.Fatalf("unexpected MI300X create body: %#v", body)
		}
		pods = []runPodRecord{body.pod("pod-amd", "EU-RO-1")}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(pods[0])
	}))
	defer server.Close()
	provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client()}
	spec := ReplicaSpec{ExternalKey: "amd", Model: "org/model", ModelRevision: "commit", Cloud: "runpod", GPU: "MI300X", GPUCount: 1, Workload: workload}
	first, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil {
		t.Fatalf("create MI300X pod: %v", err)
	}
	// Replay adopts the pod RunPod reports under its own AMD type name.
	if second, err := provider.EnsureReplica(context.Background(), spec); err != nil || second != first {
		t.Fatalf("adopt MI300X pod: handle=%#v err=%v", second, err)
	}
}

func TestRunPodPodsConcreteRegionRemainsExact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeRunPodV2List(w, "pods", []runPodRecord{})
			return
		}
		body, ok := decodeRunPodV2PodCreate(t, w, r)
		if !ok {
			return
		}
		if len(body.DataCenterIDs) != 1 || body.DataCenterIDs[0] != "EU-RO-1" {
			t.Fatalf("concrete RunPod placement changed: %#v", body.DataCenterIDs)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(body.pod("pod-concrete", "EU-RO-1"))
	}))
	defer server.Close()

	provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client()}
	_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "concrete", Model: "org/model", ModelRevision: "commit", Cloud: "runpod", Region: "EU-RO-1", GPU: "L40S", GPUCount: 1, Workload: testRunPodWorkload()})
	if err != nil {
		t.Fatalf("create concrete placement: %v", err)
	}
}

func TestRunPodPodsAdoptsV2ListShapeAndRefusesOtherDataCenter(t *testing.T) {
	workload := testRunPodWorkload()
	command := expandWorkloadCommand(workload.Command, "org/model", "commit", workload.Port, nil)
	// A literal RunPod v2 Pod list entry, as GET /v2/pods returns it.
	listed := `{"pods":[{"id":"pod-1","name":"` + clusterName("key") + `","status":"RUNNING","actions":["stop","restart","terminate"],"image":"` + workload.Image + `","args":"","entrypoint":["vllm"],"cmd":` + mustJSON(t, command[1:]) + `,"disk":100,"mounts":{},"ports":["8000/http"],"env":{},"registry":null,"cloud":"SECURE","dataCenterId":"EU-RO-1","cudaVersion":"12.8","ssh":{"proxy":null,"direct":null},"template":null,"cost":3.99,"locked":false,"runtime":null,"createdAt":"2026-11-17T00:00:00Z","startedAt":null,"globalNetworking":{"enabled":false},"gpu":{"id":"NVIDIA H200","count":4,"vcpuCount":24,"memory":251}}],"pagination":{"nextCursor":null,"hasNextPage":false}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/pods" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(listed))
	}))
	defer server.Close()
	provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client()}
	spec := ReplicaSpec{ExternalKey: "key", Model: "org/model", ModelRevision: "commit", Cloud: "runpod", Region: "EU-RO-1", GPU: "H200", GPUCount: 4, Workload: workload}
	handle, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || handle.ResourceID != clusterName("key") {
		t.Fatalf("handle=%#v err=%v", handle, err)
	}
	spec.Region = "US-TX-3"
	if _, err = provider.EnsureReplica(context.Background(), spec); !errors.Is(err, ErrInvalidReplicaSpec) {
		t.Fatalf("adopted a Pod in another data center: %v", err)
	}
}

func TestRunPodPodsAdoptsAfterLostCreateResponse(t *testing.T) {
	var pods []runPodRecord
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeRunPodV2List(w, "pods", pods)
		case http.MethodPost:
			body, ok := decodeRunPodV2PodCreate(t, w, r)
			if !ok {
				return
			}
			pods = []runPodRecord{body.pod("adopted", "")}
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
				writeRunPodV2List(w, "pods", []runPodRecord{})
			} else {
				writeRunPodV2List(w, "pods", []runPodRecord{created})
			}
		case r.Method == http.MethodGet && r.URL.Path == "/network-volumes":
			writeRunPodV2List(w, "networkVolumes", []runPodNetworkVolume{volume})
		case r.Method == http.MethodPost && r.URL.Path == "/pods":
			body, ok := decodeRunPodV2PodCreate(t, w, r)
			if !ok {
				return
			}
			if body.Mounts == nil || len(body.Mounts.Network) != 1 || body.Mounts.Network[0] != (runPodNetworkMount{VolumeID: volume.ID, Path: "/workspace"}) || len(body.DataCenterIDs) != 0 || body.Env["INFERCRANE_MODEL_DIR"] != "/workspace/infercrane/model" || body.Env["HF_XET_HIGH_PERFORMANCE"] != "1" || body.Env["HF_TOKEN"] != "{{ RUNPOD_SECRET_infercrane-hf }}" {
				t.Fatalf("unexpected persistent cache request: %#v", body)
			}
			created = body.pod("pod-volume", volume.DataCenterID)
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
	if err != nil || !strings.Contains(observed.Details, `"network_volume":{"id":"volume_1234","mount":"/workspace","state":"attached"}`) || strings.Contains(observed.Details, "worker-secret") {
		t.Fatalf("observation=%#v err=%v", observed, err)
	}
}

// A Pod is reached with the credential it was started with. One that was
// started with another credential is neither adopted as this replica nor
// recorded as holding this adapter's scoped credential.
func TestRunPodPodsTieAReplicaToTheWorkerCredentialItWasStartedWith(t *testing.T) {
	workload := testRunPodWorkload()
	command := expandWorkloadCommand(workload.Command, "org/model", "commit", workload.Port, nil)
	listing := func(environment string) string {
		return `{"pods":[{"id":"pod-1","name":"` + clusterName("key") + `","status":"RUNNING","image":"` + workload.Image + `","entrypoint":["vllm"],"cmd":` + mustJSON(t, command[1:]) + `,"disk":100,"mounts":{},"ports":["8000/http"],"env":` + environment + `,"dataCenterId":"EU-RO-1","cost":3.99,"createdAt":"2026-11-17T00:00:00Z","gpu":{"id":"NVIDIA H200","count":4}}],"pagination":{"nextCursor":null,"hasNextPage":false}}`
	}
	version := func(t *testing.T, environment string) (string, error) {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(listing(environment))) }))
		defer server.Close()
		provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "scoped-key", WorkerCredentialVersion: "tenant-hmac-v1", BaseURL: server.URL, Client: server.Client()}
		observation, err := provider.ObserveReplica(context.Background(), ProviderHandle{ResourceID: "pod-1"}, 8000)
		if err != nil || !observation.Exists {
			t.Fatalf("observe: %#v %v", observation, err)
		}
		var details struct {
			Version *string `json:"worker_credential_version"`
		}
		if err = json.Unmarshal([]byte(observation.Details), &details); err != nil || details.Version == nil {
			t.Fatalf("details do not state the worker credential version: %s", observation.Details)
		}
		spec := ReplicaSpec{ExternalKey: "key", Model: "org/model", ModelRevision: "commit", Cloud: "runpod", Region: "EU-RO-1", GPU: "H200", GPUCount: 4, Workload: workload}
		_, ensureErr := provider.EnsureReplica(context.Background(), spec)
		return *details.Version, ensureErr
	}

	if got, err := version(t, `{"INFERCRANE_WORKER_API_KEY":"scoped-key","VLLM_API_KEY":"scoped-key"}`); got != "tenant-hmac-v1" || err != nil {
		t.Fatalf("Pod started with this credential: version=%q adopt=%v", got, err)
	}
	// RunPod may leave the environment out of a listing. The Pod is then taken
	// at this adapter's word, as before.
	if got, err := version(t, `{}`); got != "tenant-hmac-v1" || err != nil {
		t.Fatalf("Pod with no listed environment: version=%q adopt=%v", got, err)
	}
	// A Pod started before scoped credentials still holds the older shared one.
	got, err := version(t, `{"INFERCRANE_WORKER_API_KEY":"older-shared-key","VLLM_API_KEY":"older-shared-key"}`)
	if got != "" {
		t.Fatalf("a Pod holding another credential was recorded as scoped (%q); it would be sent a credential it does not accept", got)
	}
	if !errors.Is(err, ErrInvalidReplicaSpec) {
		t.Fatalf("a Pod holding another credential was adopted as this replica: %v", err)
	}
	if _, err = version(t, `{"INFERCRANE_WORKER_API_KEY":"scoped-key","VLLM_API_KEY":"older-shared-key"}`); !errors.Is(err, ErrInvalidReplicaSpec) {
		t.Fatalf("a Pod whose engine holds another credential was adopted: %v", err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func testRunPodWorkload() runtimecontract.Workload {
	return runtimecontract.Workload{
		Image:    "registry.example/runtime@sha256:" + strings.Repeat("a", 64),
		Command:  []string{"vllm", "serve", "${MODEL}", "--revision", "${MODEL_REVISION}", "--port", "${PORT}"},
		Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics",
		Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30,
	}
}
