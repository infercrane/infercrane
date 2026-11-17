package provision

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// Test fixtures for RunPod REST v2 (https://api.runpod.io/v2/openapi.json).
// Create bodies are decoded with DisallowUnknownFields because v2 rejects
// unknown body fields with 422: any retired v1 field (imageName, gpuTypeIds,
// interruptible, workersMin, ...) fails the test instead of passing silently.

type runPodV2PodCreate struct {
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	Cloud         string            `json:"cloud"`
	GPU           *runPodGPU        `json:"gpu"`
	Disk          int               `json:"disk"`
	Entrypoint    []string          `json:"entrypoint"`
	Cmd           []string          `json:"cmd"`
	Ports         []string          `json:"ports"`
	Env           map[string]string `json:"env"`
	DataCenterIDs []string          `json:"dataCenterIds"`
	Mounts        *struct {
		Network []runPodNetworkMount `json:"network"`
	} `json:"mounts"`
}

// decodeRunPodV2PodCreate decodes a CreatePodRequest. On a schema violation it
// reports the failure, answers 422 like RunPod, and returns false.
func decodeRunPodV2PodCreate(t *testing.T, w http.ResponseWriter, r *http.Request) (runPodV2PodCreate, bool) {
	t.Helper()
	var body runPodV2PodCreate
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.Name == "" || body.Image == "" || body.GPU == nil || body.GPU.ID == "" || body.GPU.Count < 1 {
		t.Errorf("create body is not a RunPod v2 CreatePodRequest: %v %#v", err, body)
		writeRunPodV2Problem(w, http.StatusUnprocessableEntity, "Request validation failed.")
		return runPodV2PodCreate{}, false
	}
	return body, true
}

// pod returns the Pod RunPod v2 would list for this create. A requested data
// center wins over the fallback the scheduler would otherwise choose.
func (body runPodV2PodCreate) pod(id, dataCenterID string) runPodRecord {
	if len(body.DataCenterIDs) > 0 {
		dataCenterID = body.DataCenterIDs[0]
	}
	pod := runPodRecord{ID: id, Name: body.Name, Status: "RUNNING", Image: body.Image, Entrypoint: body.Entrypoint, Cmd: body.Cmd, Ports: body.Ports, Environment: body.Env, DiskGiB: body.Disk, DataCenterID: dataCenterID, CostPerHour: json.RawMessage(`0.79`), GPU: body.GPU}
	if body.Mounts != nil {
		pod.Mounts.Network = body.Mounts.Network
	}
	return pod
}

func writeRunPodV2List(w http.ResponseWriter, key string, items any) {
	_ = json.NewEncoder(w).Encode(map[string]any{key: items, "pagination": map[string]any{"nextCursor": nil, "hasNextPage": false}})
}

func writeRunPodV2Problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"title": http.StatusText(status), "status": status, "detail": detail})
}

type runPodV2EndpointCreate struct {
	Name          string   `json:"name"`
	TemplateID    string   `json:"templateId"`
	Type          string   `json:"type"`
	Flashboot     string   `json:"flashboot"`
	DataCenterIDs []string `json:"dataCenterIds"`
	GPU           struct {
		Pools          []string `json:"pools"`
		ExcludedTypes  []string `json:"excludedTypes"`
		Count          int      `json:"count"`
		MinCudaVersion string   `json:"minCudaVersion"`
	} `json:"gpu"`
	Workers struct {
		Min         int `json:"min"`
		Max         int `json:"max"`
		IdleTimeout int `json:"idleTimeout"`
	} `json:"workers"`
	Scaling struct {
		Type       string  `json:"type"`
		QueueDelay float64 `json:"queueDelay"`
	} `json:"scaling"`
}

// runPodV2ServerlessFake is a stateful RunPod v2 Serverless API: one template,
// a GPU catalog with a three-type pool, and endpoints created from the
// template the way v2 resolves it (image and env copied, template not kept).
type runPodV2ServerlessFake struct {
	t        *testing.T
	mu       sync.Mutex
	template map[string]any
	// endpoints holds v2 Endpoint objects by ID.
	endpoints       map[string]map[string]any
	creates         []runPodV2EndpointCreate
	deletes         int
	dropNextCreate  bool
	workersByID     map[string]int
	unexpectedPaths []string
}

const (
	fakeServerlessTemplateID = "template-qwen"
	fakeServerlessImage      = "runpod/worker-v1-vllm@sha256:" + "c0ffee"
	fakeServerlessPool       = "ADA_48_PRO"
)

func newRunPodV2ServerlessFake(t *testing.T, model, revision string) *runPodV2ServerlessFake {
	return &runPodV2ServerlessFake{t: t, endpoints: map[string]map[string]any{}, workersByID: map[string]int{}, template: map[string]any{
		"id": fakeServerlessTemplateID, "name": "vLLM", "image": fakeServerlessImage, "serverless": true,
		"env": map[string]string{"MODEL_NAME": model, "MODEL_REVISION": revision, "RAW_OPENAI_OUTPUT": "1", "ENABLE_AUTO_TOOL_CHOICE": "true", "TOOL_CALL_PARSER": "hermes"},
	}}
}

func (f *runPodV2ServerlessFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") == "" {
		writeRunPodV2Problem(w, http.StatusUnauthorized, "missing bearer token")
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/templates/"+fakeServerlessTemplateID:
		_ = json.NewEncoder(w).Encode(f.template)
	case r.Method == http.MethodGet && r.URL.Path == "/catalog/gpus":
		pool := fakeServerlessPool
		_ = json.NewEncoder(w).Encode(map[string]any{"gpus": []map[string]any{
			{"id": "NVIDIA L40S", "pool": pool}, {"id": "NVIDIA L40", "pool": pool}, {"id": "NVIDIA RTX 6000 Ada Generation", "pool": pool},
			{"id": "NVIDIA H200", "pool": "HOPPER_141"}, {"id": "AMD Instinct MI300X OAM", "pool": nil},
		}})
	case r.Method == http.MethodGet && r.URL.Path == "/serverless":
		listed := make([]map[string]any, 0, len(f.endpoints))
		for _, endpoint := range f.endpoints {
			listed = append(listed, endpoint)
		}
		writeRunPodV2List(w, "endpoints", listed)
	case r.Method == http.MethodPost && r.URL.Path == "/serverless":
		var body runPodV2EndpointCreate
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || body.Name == "" || body.Type == "" || body.Scaling.Type == "" {
			f.t.Errorf("create body is not a RunPod v2 CreateEndpointRequest: %v %#v", err, body)
			writeRunPodV2Problem(w, http.StatusUnprocessableEntity, "Request validation failed.")
			return
		}
		f.creates = append(f.creates, body)
		id := "endpoint-" + string(rune('0'+len(f.creates)))
		env := f.template["env"].(map[string]string)
		endpoint := map[string]any{
			"id": id, "name": body.Name, "type": body.Type, "image": f.template["image"], "env": env,
			"gpu":     map[string]any{"pools": body.GPU.Pools, "excludedTypes": body.GPU.ExcludedTypes, "count": body.GPU.Count, "allowedCudaVersions": []string{}, "minCudaVersion": body.GPU.MinCudaVersion},
			"workers": map[string]any{"min": body.Workers.Min, "max": body.Workers.Max, "idleTimeout": body.Workers.IdleTimeout},
			"scaling": body.Scaling, "dataCenterIds": body.DataCenterIDs, "networkVolumes": []string{}, "mounts": map[string]any{"network": []any{}, "global": []any{}},
			"timeout": 300000, "flashboot": body.Flashboot, "createdAt": "2026-11-17T00:00:00Z",
		}
		f.endpoints[id] = endpoint
		if f.dropNextCreate {
			f.dropNextCreate = false
			panic(http.ErrAbortHandler)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(endpoint)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/serverless/") && strings.HasSuffix(r.URL.Path, "/workers"):
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/serverless/"), "/workers")
		if _, ok := f.endpoints[id]; !ok {
			writeRunPodV2Problem(w, http.StatusNotFound, "endpoint not found")
			return
		}
		total := f.workersByID[id]
		_ = json.NewEncoder(w).Encode(map[string]any{"workers": []any{}, "summary": map[string]int{"running": total, "idle": 0, "initializing": 0, "throttled": 0, "unhealthy": 0, "total": total}})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/serverless/"):
		id := strings.TrimPrefix(r.URL.Path, "/serverless/")
		if _, ok := f.endpoints[id]; !ok {
			writeRunPodV2Problem(w, http.StatusNotFound, "endpoint not found")
			return
		}
		f.deletes++
		delete(f.endpoints, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.unexpectedPaths = append(f.unexpectedPaths, r.Method+" "+r.URL.Path)
		writeRunPodV2Problem(w, http.StatusNotFound, "resource not found")
	}
}

func (f *runPodV2ServerlessFake) snapshot() (creates, deletes, endpoints int, unexpected []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates), f.deletes, len(f.endpoints), append([]string(nil), f.unexpectedPaths...)
}
