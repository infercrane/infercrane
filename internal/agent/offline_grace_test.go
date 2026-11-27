package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
)

// Out of contact, inside the grace, the data plane routes only to the
// endpoint's own servers: a server a rollout started is dropped until the
// control plane is reached again. Past the grace nothing is served.
func TestOfflineDataPlaneRoutesOnlyTheEndpointsOwnServers(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 26, 18, 0, 0, 0, time.UTC)
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	a := New(Config{OfflineGrace: time.Hour, Now: clock})
	for key, model := range map[string]string{"own-8000": "Qwen/Qwen3-8B", "rollout-8000": "Qwen/Qwen3-8B"} {
		server := Detected{Server: agentprotocol.Server{Key: key, Health: agentprotocol.HealthHealthy, Models: []string{model}}, BaseURL: "http://" + key + ".models.svc:8000"}
		a.detected[key] = server
		a.status[key] = &ServerStatus{Server: server.Server, Registered: true}
	}
	dp := newDataPlane(a)
	binding := func(id, key string, rollout bool) agentprotocol.DataPlaneBinding {
		return agentprotocol.DataPlaneBinding{BindingID: id, TargetID: "target-" + id, ServerKey: key, UpstreamModel: "Qwen/Qwen3-8B", Runtime: "vllm", Weight: 100, Rollout: rollout}
	}
	dp.apply(agentprotocol.DataPlaneSnapshot{Generation: "sha256:" + strings.Repeat("a", 64), ValidUntil: now.Add(time.Minute), TenantID: "tenant-a",
		Routes: []agentprotocol.DataPlaneRoute{{Endpoint: "support-bot", EndpointID: "endpoint-1", LogicalModelID: "model-1", EnvironmentID: "env-1", ServingPlanID: "plan-2", RoutingPolicy: "primary-fallback",
			Bindings: []agentprotocol.DataPlaneBinding{binding("binding-rollout", "rollout-8000", true), binding("binding-own", "own-8000", false)}}}})
	dp.Compile()
	primary := func() string {
		snapshot, release, ok := dp.directory.AcquireForTenant("tenant-a", "support-bot")
		if !ok {
			return ""
		}
		release()
		return snapshot.BindingID
	}
	if err := dp.ready(context.Background()); err != nil || primary() != "binding-rollout" {
		t.Fatalf("current snapshot routes the rollout first: %v %q", err, primary())
	}
	mu.Lock()
	now = now.Add(10 * time.Minute)
	mu.Unlock()
	if err := dp.ready(context.Background()); err != nil || primary() != "binding-own" {
		t.Fatalf("offline: %v %q, want only the own server", err, primary())
	}
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	if err := dp.ready(context.Background()); err == nil {
		t.Fatal("served past the grace")
	}
}
