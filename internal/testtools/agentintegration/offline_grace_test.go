package agentintegration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agent"
	"github.com/infercrane/infercrane/internal/agentrelay"
	"github.com/infercrane/infercrane/internal/controlapi"
	"github.com/infercrane/infercrane/internal/testtools/fakeengine"
)

// With an offline grace, an agent that cannot reach its control plane keeps
// serving the endpoint's own server past its snapshot's validity, until the
// grace ends; it keeps the snapshot on disk (0600, digests only), and an
// agent restarted while the control plane is down serves from that file.
// Without a grace it refuses at the validity, as before.
func TestDataPlaneServesItsOwnServerInsideTheOfflineGrace(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := newMemoryStore()
	store.noQuota = true
	now := time.Now()
	var clockMu sync.Mutex
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(by time.Duration) {
		clockMu.Lock()
		now = now.Add(by)
		clockMu.Unlock()
	}
	relay := &agentrelay.Service{Store: store, DataPlaneValidity: time.Minute}
	control := httptest.NewServer(controlapi.API{Store: store, Authenticator: store, Agents: relay}.Handler())
	_, created := call(t, http.MethodPost, control.URL+"/api/v1/agents", operatorKey, `{"name":"cluster-c"}`)
	vllm := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.VLLM, Models: []string{"Qwen/Qwen3-8B"}})
	defer vllm.Close()
	state := t.TempDir()
	config := agent.Config{
		Client: agent.Client{ControlURL: control.URL, Key: created["key"].(string), HTTP: control.Client()}, Version: "1.0.0",
		Servers: []agent.ServerConfig{{URL: vllm.URL}}, EndpointName: "chat", Serve: "127.0.0.1:0",
		Hardware: agent.HardwareProbe{Runner: noAccelerators{}, GOOS: "linux"}, Now: clock,
		OfflineGrace: 10 * time.Minute, StateDir: state,
	}
	runner := agent.New(config)
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
	// The snapshot is kept: mode 0600, key digests only.
	path := filepath.Join(state, "data-plane-snapshot.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("kept snapshot: %v %v", info, err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), appKey) || !strings.Contains(string(raw), `"credentials"`) {
		t.Fatalf("the kept snapshot holds a key or no digests: %s", raw)
	}

	// The control plane goes away. Past the validity, inside the grace, the
	// endpoint's own server still serves.
	control.Close()
	advance(2 * time.Minute)
	if code, body := chat(t, serve, appKey, "chat", "hello", false); code != http.StatusOK {
		t.Fatalf("inside the grace: %d %s", code, body)
	}
	// Past the grace it refuses.
	advance(10 * time.Minute)
	if code, body := chat(t, serve, appKey, "chat", "hello", false); code != http.StatusServiceUnavailable || !strings.Contains(body, "data_plane_stale") {
		t.Fatalf("past the grace: %d %s", code, body)
	}

	// A restart while the control plane is down, back inside the grace (a
	// kept snapshot valid until a minute ago), serves from the file.
	advance(-11 * time.Minute)
	restarted := agent.New(config)
	restartCtx, stop := context.WithCancel(ctx)
	defer stop()
	if _, err = restarted.Start(restartCtx); err != nil {
		t.Fatalf("restart with the control plane down: %v", err)
	}
	if code, body := chat(t, "http://"+restarted.ServeAddress(), appKey, "chat", "hello", false); code != http.StatusOK {
		t.Fatalf("restarted inside the grace: %d %s", code, body)
	}
	if code, _ := chat(t, "http://"+restarted.ServeAddress(), "ic_not_a_key", "chat", "hello", false); code != http.StatusUnauthorized {
		t.Fatalf("an unknown key while offline: %d", code)
	}

	stop()

	// A workspace with a hard request quota needs leases from the control
	// plane: offline, its requests get 429, never a free pass.
	raw, _ := os.ReadFile(path)
	limited := strings.Replace(string(raw), `"tenant_id"`, `"request_quota":{"max_requests_per_minute":600},"tenant_id"`, 1)
	if err = os.WriteFile(path, []byte(limited), 0o600); err != nil {
		t.Fatal(err)
	}
	quotaCtx, stopQuota := context.WithCancel(ctx)
	defer stopQuota()
	quotaLimited := agent.New(config)
	if _, err = quotaLimited.Start(quotaCtx); err != nil {
		t.Fatal(err)
	}
	if code, _ := chat(t, "http://"+quotaLimited.ServeAddress(), appKey, "chat", "hello", false); code != http.StatusTooManyRequests {
		t.Fatalf("a quota-limited workspace offline: %d, want 429", code)
	}
	stopQuota()

	// Without a grace, a restart with the control plane down does not start.
	config.OfflineGrace = 0
	if _, err = agent.New(config).Start(ctx); err == nil {
		t.Fatal("an agent without a grace started with its control plane down")
	}
}

// From the control plane's own snapshot rows to the agent's routes: while
// the control plane answers, an endpoint whose plan puts a rollout's server
// first is served by it; out of contact, inside the grace, the agent drops
// the rollout's server and serves only the endpoint's own.
func TestOfflineAgentDropsARolloutServerFromRealSnapshotRows(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := newMemoryStore()
	store.noQuota = true
	now := time.Now()
	var clockMu sync.Mutex
	clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	relay := &agentrelay.Service{Store: store, DataPlaneValidity: time.Minute}
	control := httptest.NewServer(controlapi.API{Store: store, Authenticator: store, Agents: relay}.Handler())
	_, created := call(t, http.MethodPost, control.URL+"/api/v1/agents", operatorKey, `{"name":"cluster-d"}`)
	own := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.VLLM, Models: []string{"Qwen/Qwen3-8B"}})
	defer own.Close()
	rollout := fakeengine.Start(fakeengine.Options{Flavor: fakeengine.VLLM, Models: []string{"Qwen/Qwen3-8B-FP8"}})
	defer rollout.Close()
	runner := agent.New(agent.Config{
		Client: agent.Client{ControlURL: control.URL, Key: created["key"].(string), HTTP: control.Client()}, Version: "1.0.0",
		Servers: []agent.ServerConfig{{URL: own.URL}, {URL: rollout.URL}}, Serve: "127.0.0.1:0",
		Hardware: agent.HardwareProbe{Runner: noAccelerators{}, GOOS: "linux"}, Now: clock, OfflineGrace: 10 * time.Minute, StateDir: t.TempDir(),
	})
	if _, err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	endpoint, rolloutEndpoint := agent.EndpointName("Qwen/Qwen3-8B"), agent.EndpointName("Qwen/Qwen3-8B-FP8")
	store.makeAgentManaged(endpoint)
	store.makeAgentManaged(rolloutEndpoint)
	store.makeRollout(rolloutEndpoint, endpoint)
	if err := runner.Step(ctx); err != nil {
		t.Fatal(err)
	}
	serve := "http://" + runner.ServeAddress()
	if code, body := chat(t, serve, appKey, endpoint, "hello", false); code != http.StatusOK || rollout.Completions() != 1 || own.Completions() != 0 {
		t.Fatalf("with the control plane: %d %s (rollout %d, own %d)", code, body, rollout.Completions(), own.Completions())
	}
	control.Close()
	clockMu.Lock()
	now = now.Add(2 * time.Minute)
	clockMu.Unlock()
	if code, body := chat(t, serve, appKey, endpoint, "hello", false); code != http.StatusOK || rollout.Completions() != 1 || own.Completions() != 1 {
		t.Fatalf("offline: %d %s (rollout %d, own %d), want only the own server", code, body, rollout.Completions(), own.Completions())
	}
}
