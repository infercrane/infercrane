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
	fake := newRunPodV2ServerlessFake(t, "Qwen/Qwen3-8B", "0123456789abcdef")
	server := httptest.NewServer(fake)
	defer server.Close()

	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, TemplateID: fakeServerlessTemplateID, Client: server.Client()}
	spec := ServerlessEndpointSpec{ExternalKey: "deployment-1-rev-1", Model: "Qwen/Qwen3-8B", ModelRevision: "0123456789abcdef", GPU: "L40S", Region: "global", WorkersMax: 4}
	first, err := provider.EnsureEndpoint(context.Background(), spec)
	if err != nil || first.ID == "" || first.WorkersMin != 0 || first.WorkersMax != 4 || first.TemplateID != fakeServerlessTemplateID || first.Workers != nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := provider.EnsureEndpoint(context.Background(), spec)
	creates, _, _, unexpected := fake.snapshot()
	if err != nil || second.ID != first.ID || creates != 1 || len(unexpected) != 0 {
		t.Fatalf("second=%+v creates=%d unexpected=%v err=%v", second, creates, unexpected, err)
	}
	created := fake.creates[0]
	if created.TemplateID != fakeServerlessTemplateID || created.Type != "QUEUE" || created.Flashboot != "FLASHBOOT" || created.DataCenterIDs != nil {
		t.Fatalf("create identity=%#v", created)
	}
	if created.Workers.Min != 0 || created.Workers.Max != 4 || created.Workers.IdleTimeout != 5 || created.Scaling.Type != "QUEUE_DELAY" || created.Scaling.QueueDelay != 4 {
		t.Fatalf("create scaling=%#v", created)
	}
	// One exact GPU type: its pool minus every other type in that pool.
	if strings.Join(created.GPU.Pools, ",") != fakeServerlessPool || strings.Join(created.GPU.ExcludedTypes, ",") != "NVIDIA L40,NVIDIA RTX 6000 Ada Generation" || created.GPU.Count != 1 || created.GPU.MinCudaVersion != "13.0" {
		t.Fatalf("create GPU=%#v", created.GPU)
	}
}

func TestRunPodServerlessConcreteRegionPinsDataCenter(t *testing.T) {
	fake := newRunPodV2ServerlessFake(t, "Qwen/Qwen3-8B", "immutable")
	server := httptest.NewServer(fake)
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, TemplateID: fakeServerlessTemplateID, Client: server.Client()}
	if _, err := provider.EnsureEndpoint(context.Background(), ServerlessEndpointSpec{ExternalKey: "key", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", GPU: "L40S", Region: "EU-RO-1", WorkersMax: 1}); err != nil {
		t.Fatal(err)
	}
	if got := fake.creates[0].DataCenterIDs; len(got) != 1 || got[0] != "EU-RO-1" {
		t.Fatalf("dataCenterIds=%v", got)
	}
}

func TestRunPodServerlessRefusesGPUOutsideServerlessPools(t *testing.T) {
	fake := newRunPodV2ServerlessFake(t, "Qwen/Qwen3-8B", "immutable")
	server := httptest.NewServer(fake)
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, TemplateID: fakeServerlessTemplateID, Client: server.Client()}
	_, err := provider.EnsureEndpoint(context.Background(), ServerlessEndpointSpec{ExternalKey: "key", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", GPU: "MI300X", WorkersMax: 1})
	if creates, _, _, _ := fake.snapshot(); err == nil || !strings.Contains(err.Error(), "not offered in a Serverless pool") || creates != 0 {
		t.Fatalf("creates=%d err=%v", creates, err)
	}
}

func TestRunPodServerlessRejectsAdoptionWhenPlacementWidened(t *testing.T) {
	fake := newRunPodV2ServerlessFake(t, "Qwen/Qwen3-8B", "immutable")
	server := httptest.NewServer(fake)
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, TemplateID: fakeServerlessTemplateID, Client: server.Client()}
	spec := ServerlessEndpointSpec{ExternalKey: "key", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", GPU: "L40S", WorkersMax: 1}
	first, err := provider.EnsureEndpoint(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an endpoint whose exclusions no longer cover the pool.
	fake.mu.Lock()
	fake.endpoints[first.ID]["gpu"].(map[string]any)["excludedTypes"] = []string{"NVIDIA L40"}
	fake.mu.Unlock()
	if _, err = provider.EnsureEndpoint(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "GPU type exclusion") {
		t.Fatalf("adopted an endpoint that can place workers on another GPU type: %v", err)
	}
	fake.mu.Lock()
	fake.endpoints[first.ID]["gpu"].(map[string]any)["excludedTypes"] = []string{"NVIDIA L40", "NVIDIA RTX 6000 Ada Generation"}
	fake.endpoints[first.ID]["env"] = map[string]string{"MODEL_NAME": "Qwen/Qwen3-8B", "MODEL_REVISION": "other"}
	fake.mu.Unlock()
	if _, err = provider.EnsureEndpoint(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "(model)") {
		t.Fatalf("adopted an endpoint serving another model revision: %v", err)
	}
}

func TestRunPodServerlessRejectsSemanticallyInvalidCreateSuccess(t *testing.T) {
	fake := newRunPodV2ServerlessFake(t, "model", "immutable")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"","name":"wrong"}`))
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}
	_, err := provider.EnsureEndpoint(context.Background(), ServerlessEndpointSpec{ExternalKey: "key", Model: "model", ModelRevision: "immutable", TemplateID: fakeServerlessTemplateID, GPU: "L40S", WorkersMax: 1})
	if err == nil || !strings.Contains(err.Error(), "invalid endpoint identity") {
		t.Fatalf("invalid 2xx create response accepted: %v", err)
	}
}

func TestRunPodServerlessRejectsTemplateForMutableOrWrongArtifact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "template", "serverless": true, "env": map[string]string{"MODEL_NAME": "Other/Model", "MODEL_REVISION": "main", "RAW_OPENAI_OUTPUT": "1"}})
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, TemplateID: "template", Client: server.Client()}
	_, err := provider.EnsureEndpoint(context.Background(), ServerlessEndpointSpec{ExternalKey: "key", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", GPU: "L40S", WorkersMax: 1})
	if err == nil || !strings.Contains(err.Error(), "MODEL_NAME") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunPodServerlessRejectsPodTemplate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/templates/template" || r.URL.RawQuery != "" {
			t.Errorf("template read=%s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "template", "serverless": false, "env": map[string]string{"MODEL_NAME": "Qwen/Qwen3-8B", "MODEL_REVISION": "immutable", "RAW_OPENAI_OUTPUT": "1", "ENABLE_AUTO_TOOL_CHOICE": "true", "TOOL_CALL_PARSER": "hermes"}})
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, TemplateID: "template", Client: server.Client()}
	if err := provider.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not a Serverless template") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunPodServerlessDeleteIsIdempotent(t *testing.T) {
	deletes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/serverless/gone" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		deletes++
		writeRunPodV2Problem(w, http.StatusNotFound, "endpoint not found")
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
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "diagnostic") || err.Error() != "RunPod API request failed (HTTP 502)" {
		t.Fatalf("unsafe provider error length=%d err=%v", len(err.Error()), err)
	}
}

func TestRunPodServerlessCheckRejectsMutableTemplate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "template", "serverless": true, "env": map[string]string{"MODEL_NAME": "Qwen/Qwen3-8B", "MODEL_REVISION": "main", "RAW_OPENAI_OUTPUT": "1"}})
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, TemplateID: "template", Client: server.Client()}
	if err := provider.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunPodServerlessCheckRejectsTemplateWithoutToolCalling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "template", "serverless": true, "env": map[string]string{"MODEL_NAME": "Qwen/Qwen3-8B", "MODEL_REVISION": "immutable", "RAW_OPENAI_OUTPUT": "1"}})
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

func TestRunPodServerlessActiveWorkersReadsEndpointWorkerSummary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/serverless/endpoint-1/workers":
			_ = json.NewEncoder(w).Encode(map[string]any{"workers": []map[string]any{{"id": "w1", "status": "RUNNING"}, {"id": "w2", "status": "INITIALIZING"}}, "summary": map[string]int{"running": 1, "idle": 0, "initializing": 1, "throttled": 0, "unhealthy": 0, "total": 2}})
		case "/serverless/endpoint-2/workers":
			_ = json.NewEncoder(w).Encode(map[string]any{"workers": []any{}})
		default:
			writeRunPodV2Problem(w, http.StatusNotFound, "endpoint not found")
		}
	}))
	defer server.Close()
	provider := RunPodServerless{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}
	if workers, err := provider.ActiveWorkers(context.Background(), "endpoint-1"); err != nil || workers != 2 {
		t.Fatalf("workers=%d err=%v", workers, err)
	}
	// A response without the summary is unknown, never zero workers.
	if workers, err := provider.ActiveWorkers(context.Background(), "endpoint-2"); err == nil {
		t.Fatalf("missing summary reported %d workers", workers)
	}
	if _, err := provider.ActiveWorkers(context.Background(), "missing"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunPodServerlessListWalksEveryPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != "1000" {
			t.Errorf("limit=%q", r.URL.Query().Get("limit"))
		}
		if r.URL.Query().Get("cursor") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"endpoints": []map[string]any{{"id": "a", "name": "first"}}, "pagination": map[string]any{"nextCursor": "next-page", "hasNextPage": true}})
			return
		}
		if r.URL.Query().Get("cursor") != "next-page" {
			t.Errorf("cursor=%q", r.URL.Query().Get("cursor"))
		}
		writeRunPodV2List(w, "endpoints", []map[string]any{{"id": "b", "name": "second"}})
	}))
	defer server.Close()
	endpoints, err := (RunPodServerless{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}).ListEndpoints(context.Background())
	if err != nil || len(endpoints) != 2 || endpoints[0].ID != "a" || endpoints[1].ID != "b" {
		t.Fatalf("endpoints=%+v err=%v", endpoints, err)
	}
}

func TestRunPodListRefusesPartialInventory(t *testing.T) {
	for name, body := range map[string]string{
		"missing pagination":     `{"endpoints":[]}`,
		"missing resource key":   `{"pagination":{"nextCursor":null,"hasNextPage":false}}`,
		"more pages, no cursor":  `{"endpoints":[],"pagination":{"nextCursor":null,"hasNextPage":true}}`,
		"v1 bare array response": `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			if endpoints, err := (RunPodServerless{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}).ListEndpoints(context.Background()); err == nil {
				t.Fatalf("partial inventory accepted: %+v", endpoints)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"endpoints":[],"pagination":{"nextCursor":"same","hasNextPage":true}}`))
	}))
	defer server.Close()
	if _, err := (RunPodServerless{APIKey: "secret", BaseURL: server.URL, Client: server.Client()}).ListEndpoints(context.Background()); err == nil {
		t.Fatal("a cursor that never advances was followed without bound")
	}
}

func TestRunPodGPUTypeMapsBlackwellServerAlias(t *testing.T) {
	const providerID = "NVIDIA RTX PRO 6000 Blackwell Server Edition"
	for _, input := range []string{"RTXPRO6000", "RTX PRO 6000", providerID} {
		if got := RunPodGPUTypeID(input); got != providerID {
			t.Fatalf("RunPodGPUTypeID(%q)=%q, want %q", input, got, providerID)
		}
	}
}

func TestRunPodAdaptersDefaultToRESTv2(t *testing.T) {
	var requested []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requested = append(requested, r.URL.Scheme+"://"+r.URL.Host+r.URL.Path)
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: http.Header{}, Request: r}, nil
	})}
	if err := (RunPodServerless{APIKey: "secret", Client: client}).DeleteEndpoint(context.Background(), "endpoint-1"); err != nil {
		t.Fatal(err)
	}
	if err := (RunPodPods{APIKey: "secret", Client: client}).do(context.Background(), http.MethodDelete, "/pods/pod-1", nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(requested, " ") != "https://api.runpod.io/v2/serverless/endpoint-1 https://api.runpod.io/v2/pods/pod-1" {
		t.Fatalf("requested=%v", requested)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
