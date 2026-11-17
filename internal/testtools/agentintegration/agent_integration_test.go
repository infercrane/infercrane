package agentintegration

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agent"
	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/agentrelay"
	"github.com/infercrane/infercrane/internal/controlapi"
	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/testtools/fakeengine"
)

const (
	operatorKey  = "operator-test-key"
	secretPrompt = "TOP-SECRET-PROMPT-7f3a"
	secretLabel  = "TOP-SECRET-LABEL-91c2"
	localAPIKey  = "local-vllm-key-5d1e"
)

// noAccelerators answers like a host without nvidia-smi or rocm-smi.
type noAccelerators struct{}

func (noAccelerators) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, exec.ErrNotFound
}

// recorder keeps every decompressed body the agent sent to the control plane.
type recorder struct {
	mu     sync.Mutex
	bodies []string
}

func (r *recorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		body := raw
		if request.Header.Get("Content-Encoding") == "gzip" {
			if reader, err := gzip.NewReader(bytes.NewReader(raw)); err == nil {
				body, _ = io.ReadAll(reader)
			}
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, request.URL.Path+" "+string(body))
		r.mu.Unlock()
		request.Body = io.NopCloser(bytes.NewReader(raw))
		next.ServeHTTP(w, request)
	})
}

func (r *recorder) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.bodies, "\n")
}

func call(t *testing.T, method, url, key, body string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	_ = json.NewDecoder(response.Body).Decode(&decoded)
	return response.StatusCode, decoded
}

func TestAgentObservesLocalServersThroughOutboundSyncOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newMemoryStore()
	api := controlapi.API{Store: store, Authenticator: store, Agents: agentrelay.Service{Store: store, RecommendedAgentVersion: "1.0.0"}, GatewayURL: "https://control.example.test"}
	sent := &recorder{}
	control := httptest.NewServer(sent.wrap(api.Handler()))
	defer control.Close()

	status, created := call(t, http.MethodPost, control.URL+"/api/v1/agents", operatorKey, `{"name":"laptop"}`)
	key, _ := created["key"].(string)
	if status != http.StatusCreated || key == "" {
		t.Fatalf("create agent: %d %v", status, created)
	}
	agentID := created["agent"].(map[string]any)["id"].(string)

	vllm := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.VLLM, Models: []string{"Qwen/Qwen3-8B"}, Metrics: true, APIKey: localAPIKey,
		ExtraMetrics: "# HELP app_private custom metric\napp_private_queue{customer_prompt=\"" + secretLabel + "\"} 7\n"})
	defer vllm.Close()
	sglang := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.SGLang, Models: []string{"meta-llama/Llama-3.1-8B-Instruct"}, Metrics: true})
	defer sglang.Close()
	llama := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.LlamaCPP, Models: []string{"gemma-3-4b-it-Q4_K_M.gguf"}, Metrics: true})
	defer llama.Close()
	ollama := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.Ollama, Models: []string{"llama3.2:3b", "qwen3:8b"}, Loaded: "qwen3:8b"})
	defer ollama.Close()
	lmstudio := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.LMStudio, Models: []string{"qwen2.5-7b-instruct", "text-embedding-nomic"}, Loaded: "qwen2.5-7b-instruct"})
	defer lmstudio.Close()

	sys := t.TempDir()
	card := filepath.Join(sys, "sys/class/drm/card0/device")
	if err := os.MkdirAll(card, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"vendor": "0x1002", "class": "0x030000", "mem_info_vram_total": "25753026560"} {
		if err := os.WriteFile(filepath.Join(card, name), []byte(value+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var gatewayURL string
	var eventsMu sync.Mutex
	runner := agent.New(agent.Config{
		Client:  agent.Client{ControlURL: control.URL, Key: key, HTTP: control.Client()},
		Version: "1.0.0",
		Servers: []agent.ServerConfig{{URL: vllm.URL + "/v1", AuthHeader: "Bearer " + localAPIKey}, {URL: sglang.URL}, {URL: llama.URL}, {URL: ollama.URL}, {URL: lmstudio.URL}},
		Gateway: "127.0.0.1:0", GatewayServer: vllm.URL,
		Hardware: agent.HardwareProbe{Runner: noAccelerators{}, GOOS: "linux", GOARCH: "amd64", SysRoot: sys},
		Events: func(event agent.Event) {
			if event.Kind == "gateway_listening" {
				eventsMu.Lock()
				gatewayURL = strings.Fields(event.Message)[0]
				eventsMu.Unlock()
			}
		},
	})
	if _, err := runner.Start(ctx); err != nil {
		t.Fatalf("start agent: %v", err)
	}

	view := runner.Status()
	if len(view.Servers) != 5 {
		t.Fatalf("servers: %+v", view.Servers)
	}
	want := map[string]struct{ engine, version, model, endpoint string }{
		vllm.URL:     {agentprotocol.EngineVLLM, "0.11.2", "Qwen/Qwen3-8B", "qwen3-8b"},
		sglang.URL:   {agentprotocol.EngineSGLang, "0.5.4", "meta-llama/Llama-3.1-8B-Instruct", "llama-3.1-8b-instruct"},
		llama.URL:    {agentprotocol.EngineLlamaCPP, "b6900-abcdef1", "gemma-3-4b-it-Q4_K_M.gguf", "gemma-3-4b-it-q4_k_m.gguf"},
		ollama.URL:   {agentprotocol.EngineOllama, "0.12.10", "qwen3:8b", "qwen3-8b"},
		lmstudio.URL: {agentprotocol.EngineLMStudio, "", "qwen2.5-7b-instruct", "qwen2.5-7b-instruct"},
	}
	for _, server := range view.Servers {
		expected, ok := want[server.Address]
		if !ok {
			t.Fatalf("unexpected server %+v", server)
		}
		if server.Engine != expected.engine || server.EngineVersion != expected.version || server.Model != expected.model || !server.Registered || server.Health != "healthy" {
			t.Fatalf("server %s = %+v, want %+v", server.Address, server, expected)
		}
		if server.Address == ollama.URL {
			// Ollama's model name collides with vLLM's; the agent prefixes
			// its own name instead of guessing.
			expected.endpoint = "laptop-qwen3-8b"
		}
		if server.Endpoint != expected.endpoint {
			t.Fatalf("endpoint for %s = %s, want %s", server.Address, server.Endpoint, expected.endpoint)
		}
		switch server.Engine {
		case agentprotocol.EngineOllama, agentprotocol.EngineLMStudio:
			if server.MetricsAvailable || !strings.Contains(server.MetricsDetail, "does not expose engine metrics") {
				t.Fatalf("%s metrics must be explicit unknowns: %+v", server.Engine, server)
			}
		default:
			if !server.MetricsAvailable {
				t.Fatalf("%s metrics unavailable: %+v", server.Engine, server)
			}
		}
	}
	if view.Hardware.State != "partial" || len(view.Hardware.Accelerators) != 1 || view.Hardware.Accelerators[0].Vendor != "amd" || view.Hardware.Accelerators[0].MemoryMiB == nil || *view.Hardware.Accelerators[0].MemoryMiB != 24560 {
		t.Fatalf("hardware: %+v", view.Hardware)
	}

	eventsMu.Lock()
	gateway := gatewayURL
	eventsMu.Unlock()
	if gateway == "" {
		t.Fatal("gateway did not start")
	}
	for _, stream := range []bool{false, true} {
		body, _ := json.Marshal(map[string]any{"model": "Qwen/Qwen3-8B", "stream": stream, "messages": []map[string]string{{"role": "system", "content": "You are terse."}, {"role": "user", "content": secretPrompt}}})
		request, _ := http.NewRequest(http.MethodPost, gateway+"/chat/completions", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+localAPIKey)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || !strings.Contains(string(answer), "local") {
			t.Fatalf("gateway pass-through: %d %s", response.StatusCode, answer)
		}
	}
	if vllm.Completions() != 2 || !strings.Contains(strings.Join(vllm.Prompts, "|"), secretPrompt) {
		t.Fatalf("the engine did not receive the requests: %d %v", vllm.Completions(), vllm.Prompts)
	}

	// The second scrape after traffic gives window values.
	if err := runner.Step(ctx); err != nil {
		t.Fatalf("step: %v", err)
	}
	served := store.serverByEndpoint("qwen3-8b")
	relayed := map[string]float64{}
	for _, row := range store.measurementsFor(served.TargetID) {
		relayed[row.Name] = row.Value
		if row.ReplicaID != served.TargetID || row.EvidenceClass != "measured" || row.Source != "vllm_metrics" {
			t.Fatalf("relayed row is not keyed like a scrape: %+v", row)
		}
	}
	for _, name := range []string{metrics.MeasurementRequestsRunning, metrics.MeasurementRequestsWaiting, metrics.MeasurementKVCacheUsage, metrics.MeasurementTTFTP95, metrics.MeasurementQueueTimeP95, metrics.MeasurementRequestsCompleted} {
		if _, ok := relayed[name]; !ok {
			t.Fatalf("missing relayed measurement %s in %v", name, relayed)
		}
	}
	// A direct scrape of the same engine gives the same gauges.
	request, _ := http.NewRequest(http.MethodGet, vllm.URL+"/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+localAPIKey)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(response.Body)
	response.Body.Close()
	direct, err := metrics.ParseEngine(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := metrics.EngineMeasurements(nil, direct, metrics.EngineMeasurementOptions{ReplicaID: served.TargetID, ObservedAt: time.Now(), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if relayed[row.Name] != row.Value {
			t.Fatalf("relayed %s=%v, direct scrape %v", row.Name, relayed[row.Name], row.Value)
		}
	}
	if relayed[metrics.MeasurementRequestsCompleted] != 2 || relayed[metrics.MeasurementTTFTP95] <= 0 || relayed[metrics.MeasurementTTFTP95] > 250 {
		t.Fatalf("window values: %v", relayed)
	}
	for _, name := range []string{"llama-3.1-8b-instruct", "gemma-3-4b-it-q4_k_m.gguf"} {
		if len(store.measurementsFor(store.serverByEndpoint(name).TargetID)) == 0 {
			t.Fatalf("no relayed measurements for %s", name)
		}
	}
	if rows := store.measurementsFor(store.serverByEndpoint("laptop-qwen3-8b").TargetID); len(rows) != 0 {
		t.Fatalf("Ollama has no engine metrics, yet rows were stored: %+v", rows)
	}

	records := store.requestRecords()
	if len(records) != 2 {
		t.Fatalf("request records: %+v", records)
	}
	streamed := 0
	for _, record := range records {
		if record.EndpointID != served.EndpointID || record.TargetID != served.TargetID || record.OperationName != "chat" || record.ComputeMode != "agent-gateway" || record.StatusCode != 200 {
			t.Fatalf("record identity: %+v", record)
		}
		if record.InputTokens == nil || *record.InputTokens != 12 || record.OutputTokens == nil || *record.OutputTokens != 5 || record.TTFTMS == nil || record.SharedPrefixHash == "" {
			t.Fatalf("record shape: %+v", record)
		}
		if record.Streaming {
			streamed++
		}
	}
	if streamed != 1 {
		t.Fatalf("streamed records = %d", streamed)
	}

	// Nothing the agent sent carries a prompt, a completion, an unrelated
	// metric label, or the local server's key.
	everything := sent.all()
	for _, forbidden := range []string{secretPrompt, "You are terse", "local answer", secretLabel, "app_private", localAPIKey} {
		if strings.Contains(everything, forbidden) {
			t.Fatalf("agent sent %q to the control plane", forbidden)
		}
	}
	if !strings.Contains(everything, "vllm:time_to_first_token_seconds_bucket") {
		t.Fatal("filtered engine metrics were not relayed")
	}

	// An agent key cannot use the rest of the control API.
	if status, _ := call(t, http.MethodGet, control.URL+"/api/v1/agents", key, ""); status != http.StatusForbidden {
		t.Fatalf("agent key on the management API: %d", status)
	}
	if status, detail := call(t, http.MethodGet, control.URL+"/api/v1/agents/"+agentID, operatorKey, ""); status != http.StatusOK || len(detail["observed_servers"].([]any)) != 5 {
		t.Fatalf("agent detail: %d %v", status, detail)
	}

	// Revocation stops the agent on its next sync.
	if status, _ := call(t, http.MethodDelete, control.URL+"/api/v1/agents/"+agentID, operatorKey, ""); status != http.StatusOK {
		t.Fatalf("revoke: %d", status)
	}
	if err := runner.Step(ctx); !errors.Is(err, agent.ErrRevoked) && !errors.Is(err, agent.ErrUnauthenticated) {
		t.Fatalf("revoked agent kept syncing: %v", err)
	}
	if served = store.serverByEndpoint("qwen3-8b"); served.Health != "unhealthy" {
		t.Fatalf("revoked agent's server health = %s", served.Health)
	}
}

func TestRevokedAgentRunLoopStopsWithinOnePoll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := newMemoryStore()
	api := controlapi.API{Store: store, Authenticator: store, Agents: agentrelay.Service{Store: store, PollInterval: agentprotocol.MinPollInterval}}
	control := httptest.NewServer(api.Handler())
	defer control.Close()
	_, created := call(t, http.MethodPost, control.URL+"/api/v1/agents", operatorKey, `{"name":"rack-1"}`)
	key := created["key"].(string)
	engine := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.VLLM, Metrics: true})
	defer engine.Close()
	runner := agent.New(agent.Config{Client: agent.Client{ControlURL: control.URL, Key: key, HTTP: control.Client()}, Version: "1.0.0", Servers: []agent.ServerConfig{{URL: engine.URL}}, Hardware: agent.HardwareProbe{Runner: noAccelerators{}, GOOS: "linux"}, ProbeInterval: time.Hour})
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(ctx)
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(store.requestRecords()) == 0 && time.Now().Before(deadline) {
		agents, _ := store.AgentsForTenant(ctx, "tenant-a")
		if len(agents) == 1 && agents[0].LastSeenAt != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	revokedAt := time.Now()
	if status, _ := call(t, http.MethodDelete, control.URL+"/api/v1/agents/"+created["agent"].(map[string]any)["id"].(string), operatorKey, ""); status != http.StatusOK {
		t.Fatalf("revoke status %d", status)
	}
	select {
	case err := <-done:
		if !errors.Is(err, agent.ErrRevoked) && !errors.Is(err, agent.ErrUnauthenticated) {
			t.Fatalf("run ended with %v", err)
		}
		if elapsed := time.Since(revokedAt); elapsed > agentprotocol.MinPollInterval+2*time.Second {
			t.Fatalf("agent stopped %s after revocation, more than one poll", elapsed)
		}
	case <-ctx.Done():
		t.Fatal("revoked agent kept running")
	}
}
