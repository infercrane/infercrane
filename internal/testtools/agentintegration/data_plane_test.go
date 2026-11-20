package agentintegration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agent"
	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/agentrelay"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/controlapi"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/testtools/fakeengine"
)

const candidateImage = "registry.example.test/models/llama-server@sha256:1111111111111111111111111111111111111111111111111111111111111111"

// fakeCandidates stands in for provision.Kubernetes: each candidate is a
// fake engine on this machine, so the executor's launch, readiness,
// measurement, answer check, and cleanup run for real.
type fakeCandidates struct {
	mu      sync.Mutex
	running map[string]*fakeengine.Engine
	specs   []provision.ReplicaSpec
	deleted []string
}

func (f *fakeCandidates) Check(context.Context) error { return nil }
func (f *fakeCandidates) Handle(key string) provision.ProviderHandle {
	return provision.ProviderHandle{ResourceID: "deployment/" + key, ExternalKey: key}
}
func (f *fakeCandidates) EnsureReplica(_ context.Context, spec provision.ReplicaSpec) (provision.ProviderHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == nil {
		f.running = map[string]*fakeengine.Engine{}
	}
	f.specs = append(f.specs, spec)
	if _, ok := f.running[spec.ExternalKey]; !ok {
		f.running[spec.ExternalKey] = fakeengine.Start(fakeengine.Options{Flavor: fakeengine.LlamaCPP, Models: []string{spec.Model}, Metrics: true, APIKey: "worker-key-in-cluster"})
	}
	return f.Handle(spec.ExternalKey), nil
}
func (f *fakeCandidates) ObserveReplica(_ context.Context, handle provision.ProviderHandle, _ int) (provision.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	engine, ok := f.running[handle.ExternalKey]
	if !ok {
		return provision.Observation{}, nil
	}
	return provision.Observation{Exists: true, State: "ready", Endpoint: engine.URL}, nil
}
func (f *fakeCandidates) DeleteReplica(_ context.Context, handle provision.ProviderHandle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if engine, ok := f.running[handle.ExternalKey]; ok {
		engine.Close()
		delete(f.running, handle.ExternalKey)
	}
	f.deleted = append(f.deleted, handle.ExternalKey)
	return nil
}

// fakeAIPerf records which servers were measured and returns fixed numbers.
type fakeAIPerf struct {
	mu        sync.Mutex
	endpoints []string
}

func (f *fakeAIPerf) run(_ context.Context, cfg benchmark.Config) (benchmark.Result, error) {
	f.mu.Lock()
	f.endpoints = append(f.endpoints, cfg.Endpoint)
	calls := len(f.endpoints)
	f.mu.Unlock()
	throughput, ttft := 100.0+float64(calls), 40.0
	return benchmark.Result{Tool: "aiperf", ToolVersion: benchmark.AIPerfVersion, Command: "aiperf profile --url " + cfg.Endpoint, Requests: cfg.Requests, Succeeded: cfg.Requests, DurationSeconds: 12, OutputTokenThroughput: &throughput, TTFTP50MS: &ttft, TTFTP95MS: &ttft}, nil
}

func chat(t *testing.T, base, key, model, prompt string, stream bool) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": model, "stream": stream, "messages": []map[string]string{{"role": "system", "content": "You are terse."}, {"role": "user", "content": prompt}}})
	request, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	request.Header.Set("X-Request-ID", "caller-chosen-request-id-5e1f")
	request.Header.Set("X-InferCrane-Session-ID", "customer-session-81aa")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(answer)
}

// TestAgentServesAgentManagedEndpointInsideCustomerCloud is the hermetic
// proof of the agent as data plane: an application calls the agent with an
// InferCrane API key, the agent routes to the server it owns with the
// server's own key, enforces admission and quota from leases, and the
// control plane receives only content-free records. A candidate run queued
// on the command channel runs where the agent runs and reports numbers only.
func TestAgentServesAgentManagedEndpointInsideCustomerCloud(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store := newMemoryStore()
	relay := &agentrelay.Service{Store: store, RecommendedAgentVersion: "1.0.0"}
	api := controlapi.API{Store: store, Authenticator: store, Agents: relay}
	sent := &recorder{}
	control := httptest.NewServer(sent.wrap(api.Handler()))
	defer control.Close()

	status, created := call(t, http.MethodPost, control.URL+"/api/v1/agents", operatorKey, `{"name":"cluster-a"}`)
	key, _ := created["key"].(string)
	if status != http.StatusCreated || key == "" {
		t.Fatalf("create agent: %d %v", status, created)
	}
	agentID := created["agent"].(map[string]any)["id"].(string)

	vllm := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.VLLM, Models: []string{"Qwen/Qwen3-8B"}, Metrics: true, APIKey: localAPIKey})
	defer vllm.Close()
	candidates := &fakeCandidates{}
	aiperf := &fakeAIPerf{}
	runner := agent.New(agent.Config{
		Client:  agent.Client{ControlURL: control.URL, Key: key, HTTP: control.Client()},
		Version: "1.0.0", Servers: []agent.ServerConfig{{URL: vllm.URL, AuthHeader: "Bearer " + localAPIKey}}, EndpointName: "support-bot",
		Serve: "127.0.0.1:0",
		Executor: agent.KubernetesExecutor{
			Provider: candidates, WorkerKey: "worker-key-in-cluster", AllowedImages: []string{"registry.example.test/models/"},
			Benchmark: aiperf.run, Completer: qualitycheck.HTTPCompleter{}, PollInterval: 10 * time.Millisecond,
		},
		Hardware: agent.HardwareProbe{Runner: noAccelerators{}, GOOS: "linux", GOARCH: "amd64"},
	})
	if _, err := runner.Start(ctx); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	serve := "http://" + runner.ServeAddress()

	// Observed only: the data plane serves nothing yet.
	if code, _ := chat(t, serve, appKey, "support-bot", secretPrompt, false); code != http.StatusNotFound && code != http.StatusUnauthorized {
		t.Fatalf("an observe-only endpoint was served: %d", code)
	}

	// A person with deploy permission makes it agent-managed.
	store.makeAgentManaged("support-bot")
	if err := runner.Step(ctx); err != nil {
		t.Fatalf("step: %v", err)
	}
	view := runner.Status()
	// Only keys that may call an endpoint this agent serves reach it: the
	// key restricted to another endpoint and every agent key stay behind.
	if view.DataPlane == nil || len(view.DataPlane.Endpoints) != 1 || view.DataPlane.Endpoints[0] != "support-bot" || view.DataPlane.Keys != 1 {
		t.Fatalf("data plane status: %+v", view.DataPlane)
	}

	// Authentication uses the digests only; agent keys, keys for other
	// endpoints, and no key at all are refused.
	if code, _ := chat(t, serve, "", "support-bot", secretPrompt, false); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
	if code, _ := chat(t, serve, key, "support-bot", secretPrompt, false); code != http.StatusUnauthorized {
		t.Fatalf("agent key on the data plane: %d", code)
	}
	if code, _ := chat(t, serve, otherScopeKey, "support-bot", secretPrompt, false); code != http.StatusUnauthorized {
		t.Fatalf("key restricted to another endpoint: %d", code)
	}
	for _, stream := range []bool{false, true} {
		code, answer := chat(t, serve, appKey, "support-bot", secretPrompt, stream)
		if code != http.StatusOK || !strings.Contains(answer, "local") || !strings.Contains(answer, "answer") {
			t.Fatalf("served request (stream=%v): %d %s", stream, code, answer)
		}
	}
	if vllm.Completions() != 2 || !strings.Contains(strings.Join(vllm.Prompts, "|"), secretPrompt) {
		t.Fatalf("the agent-owned server did not serve the requests: %d %v", vllm.Completions(), vllm.Prompts)
	}
	if store.quotaLeases() == 0 {
		t.Fatal("the data plane served a quota-limited workspace without a quota lease")
	}

	for wait := time.Now().Add(5 * time.Second); time.Now().Before(wait); time.Sleep(5 * time.Millisecond) {
		if pending, _ := runner.PendingShapes(); pending >= 2 {
			break
		}
	}
	if err := runner.Step(ctx); err != nil {
		t.Fatalf("step: %v", err)
	}
	served := store.serverByEndpoint("support-bot")
	records := store.requestRecords()
	if len(records) != 2 {
		t.Fatalf("request records: %+v", records)
	}
	for _, record := range records {
		if record.ComputeMode != agentprotocol.ComputeModeDataPlane || record.BindingID != served.BindingID || record.EndpointID != served.EndpointID || record.TargetID != served.TargetID || record.StatusCode != http.StatusOK {
			t.Fatalf("record identity: %+v", record)
		}
		if record.QueueMS == nil || record.InputTokens == nil || *record.InputTokens != 12 || record.SharedPrefixHash == "" || record.SessionIDHash == "" || !strings.HasPrefix(record.RequestID, "agent-") {
			t.Fatalf("record shape: %+v", record)
		}
	}

	// A candidate run goes through the command channel and runs here.
	spec := agentprotocol.CandidateRunSpec{
		Endpoint: "support-bot", Label: "llama.cpp parallel 8", Model: "Qwen/Qwen3-8B",
		Workload:    agentprotocolWorkload(),
		RuntimeArgs: []string{"--parallel", "8"}, Accelerator: "NVIDIA-L4", GPUCount: 1,
		Load:    agentprotocol.CandidateLoad{Requests: 20, Concurrency: 2, InputTokens: 64, OutputTokens: 16},
		Quality: agentprotocol.CandidateQuality{EvalSet: "default"}, TimeoutSecs: 600,
	}
	body, _ := json.Marshal(spec)
	status, queued := call(t, http.MethodPost, control.URL+"/api/v1/agents/"+agentID+"/candidate-runs", operatorKey, string(body))
	if status != http.StatusCreated {
		t.Fatalf("queue candidate run: %d %v", status, queued)
	}
	runID := queued["candidate_run"].(map[string]any)["id"].(string)
	commandCtx, stopCommands := context.WithCancel(ctx)
	commandsDone := make(chan error, 1)
	go func() { commandsDone <- runner.RunCommands(commandCtx) }()
	deadline := time.Now().Add(30 * time.Second)
	for store.command(runID).FinishedAt == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	stopCommands()
	if err := <-commandsDone; err != nil {
		t.Fatalf("commands: %v", err)
	}
	finished := store.command(runID)
	if finished.State != "succeeded" {
		t.Fatalf("candidate run: %+v", finished)
	}
	var result agentprotocol.CandidateRunResult
	if err := json.Unmarshal([]byte(finished.ResultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if result.Control == nil || result.Candidate == nil || result.ControlAfter == nil || result.Quality == nil || !result.CleanedUp || result.Noise == nil {
		t.Fatalf("result: %+v", result)
	}
	if result.Quality.Verdict != "PASS" || result.Quality.Score != 1 || result.Quality.Prompts != len(qualitycheck.Default().Prompts) {
		t.Fatalf("answer check: %+v", result.Quality)
	}
	if len(aiperf.endpoints) != 3 || aiperf.endpoints[0] != vllm.URL || aiperf.endpoints[2] != vllm.URL || aiperf.endpoints[1] == vllm.URL {
		t.Fatalf("AIPerf ran against %v; want current setup, candidate, current setup", aiperf.endpoints)
	}
	if len(candidates.specs) != 1 || candidates.specs[0].Workload.Image != candidateImage || len(candidates.deleted) != 1 {
		t.Fatalf("candidate lifecycle: specs=%+v deleted=%v", candidates.specs, candidates.deleted)
	}
	status, read := call(t, http.MethodGet, control.URL+"/api/v1/agents/"+agentID+"/candidate-runs/"+runID, operatorKey, "")
	if status != http.StatusOK || read["candidate_run"].(map[string]any)["state"] != "succeeded" {
		t.Fatalf("read candidate run: %d %v", status, read)
	}

	// An image the operator did not allow is refused before anything runs.
	spec.Workload.Image = "docker.io/library/other@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	body, _ = json.Marshal(spec)
	_, queued = call(t, http.MethodPost, control.URL+"/api/v1/agents/"+agentID+"/candidate-runs", operatorKey, string(body))
	refusedID := queued["candidate_run"].(map[string]any)["id"].(string)
	commandCtx, stopCommands = context.WithCancel(ctx)
	go func() { commandsDone <- runner.RunCommands(commandCtx) }()
	for store.command(refusedID).FinishedAt == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	stopCommands()
	<-commandsDone
	if refused := store.command(refusedID); refused.State != "failed" || refused.FailureCode != "image_not_allowed" || len(candidates.specs) != 1 {
		t.Fatalf("disallowed image: %+v (launched %d)", refused, len(candidates.specs))
	}

	// Nothing the agent sent carries a prompt, an answer, an eval prompt, a
	// key, the caller's request ID, or the session ID.
	everything := sent.all()
	forbidden := []string{secretPrompt, "You are terse", "local answer", appKey, otherScopeKey, localAPIKey, "worker-key-in-cluster", "caller-chosen-request-id-5e1f", "customer-session-81aa", "aiperf profile --url"}
	for _, prompt := range qualitycheck.Default().Prompts {
		forbidden = append(forbidden, prompt.Prompt)
	}
	for _, value := range forbidden {
		if strings.Contains(everything, value) {
			t.Fatalf("agent sent %q to the control plane", value)
		}
	}
	for _, expected := range []string{"/api/v1/agent/data-plane", "/api/v1/agent/quota-leases", "/api/v1/agent/commands", `"source":"data-plane"`} {
		if !strings.Contains(everything, expected) {
			t.Fatalf("expected %s in what the agent sent", expected)
		}
	}

	// Revocation stops the data plane too.
	if status, _ := call(t, http.MethodDelete, control.URL+"/api/v1/agents/"+agentID, operatorKey, ""); status != http.StatusOK {
		t.Fatalf("revoke: %d", status)
	}
	if err := runner.Step(ctx); !errors.Is(err, agent.ErrRevoked) && !errors.Is(err, agent.ErrUnauthenticated) {
		t.Fatalf("revoked agent kept serving: %v", err)
	}
}

func agentprotocolWorkload() runtimecontract.Workload {
	return runtimecontract.Workload{
		Image: candidateImage, Command: []string{"/app/llama-server", "--host", "0.0.0.0", "--port", "8000", "--model", "/models/model.gguf", "--metrics"},
		Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics",
		Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 20,
	}
}

func TestDataPlaneFailsClosedWhenTheSnapshotIsStale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := newMemoryStore()
	now := time.Now()
	var clockMu sync.Mutex
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	relay := &agentrelay.Service{Store: store, DataPlaneValidity: time.Minute}
	control := httptest.NewServer(controlapi.API{Store: store, Authenticator: store, Agents: relay}.Handler())
	defer control.Close()
	_, created := call(t, http.MethodPost, control.URL+"/api/v1/agents", operatorKey, `{"name":"cluster-b"}`)
	vllm := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.VLLM, Models: []string{"Qwen/Qwen3-8B"}})
	defer vllm.Close()
	runner := agent.New(agent.Config{
		Client: agent.Client{ControlURL: control.URL, Key: created["key"].(string), HTTP: control.Client()}, Version: "1.0.0",
		Servers: []agent.ServerConfig{{URL: vllm.URL}}, EndpointName: "chat", Serve: "127.0.0.1:0",
		Hardware: agent.HardwareProbe{Runner: noAccelerators{}, GOOS: "linux"}, Now: clock,
	})
	if _, err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	store.makeAgentManaged("chat")
	if err := runner.Step(ctx); err != nil {
		t.Fatal(err)
	}
	serve := "http://" + runner.ServeAddress()
	if code, _ := chat(t, serve, appKey, "chat", "hello", false); code != http.StatusOK {
		t.Fatalf("fresh snapshot: %d", code)
	}
	clockMu.Lock()
	now = now.Add(2 * time.Minute)
	clockMu.Unlock()
	code, body := chat(t, serve, appKey, "chat", "hello", false)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "data_plane_stale") {
		t.Fatalf("stale snapshot: %d %s", code, body)
	}
	if vllm.Completions() != 1 {
		t.Fatalf("a stale data plane forwarded a request: %d completions", vllm.Completions())
	}
}
