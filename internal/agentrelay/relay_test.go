package agentrelay

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/domain"
)

type fakeStore struct {
	Store
	revoked      bool
	servers      []domain.AgentServer
	health       map[string]string
	measurements []domain.OperationalMeasurement
	requests     []domain.InferenceRecord
	heartbeat    domain.AgentHeartbeat
}

func (f *fakeStore) RecordAgentHeartbeat(_ context.Context, _, _ string, heartbeat domain.AgentHeartbeat) error {
	if f.revoked {
		return domain.ErrNotFound
	}
	f.heartbeat = heartbeat
	return nil
}
func (f *fakeStore) AgentServers(context.Context, string, string) ([]domain.AgentServer, error) {
	return f.servers, nil
}
func (f *fakeStore) ReportAgentServerHealth(_ context.Context, _, _, key, health string, _ time.Time) error {
	f.health[key] = health
	return nil
}
func (f *fakeStore) RecordTargetMeasurements(_ context.Context, _, _ string, rows []domain.OperationalMeasurement) ([]domain.OperationalMeasurement, error) {
	f.measurements = append(f.measurements, rows...)
	return rows, nil
}
func (f *fakeStore) RecordRequest(_ context.Context, record domain.InferenceRecord) error {
	f.requests = append(f.requests, record)
	return nil
}
func (f *fakeStore) AgentForPrincipal(context.Context, string) (domain.Agent, error) {
	state := "active"
	if f.revoked {
		state = "revoked"
	}
	return domain.Agent{ID: "agent-1", TenantID: "tenant-a", State: state}, nil
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../metrics/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSyncTurnsRelaysIntoScrapeEquivalentEvidence(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{health: map[string]string{}, servers: []domain.AgentServer{{AgentID: "agent-1", Key: "127.0.0.1-8000", TargetID: "target-1", EndpointID: "endpoint-1", BindingID: "binding-1", Runtime: "vllm", OwnershipMode: "observe-only"}}}
	service := Service{Store: store, Now: func() time.Time { return now }, RecommendedAgentVersion: "1.2.0"}
	agent := domain.Agent{ID: "agent-1", TenantID: "tenant-a", Name: "laptop"}
	ttft := 80.0
	input, output := 30, 9
	request := agentprotocol.SyncRequest{
		Protocol: agentprotocol.Version, AgentVersion: "1.1.0", OS: "linux", Arch: "amd64",
		// The agent's clock runs ten minutes slow.
		SentAt:   now.Add(-10 * time.Minute),
		Hardware: agentprotocol.Hardware{State: "unavailable", Accelerators: []agentprotocol.Accelerator{}, Limitations: []string{}},
		Servers: []agentprotocol.Server{
			{Key: "127.0.0.1-8000", Engine: agentprotocol.EngineVLLM, Address: "http://127.0.0.1:8000", Models: []string{"m"}, Health: agentprotocol.HealthHealthy},
			{Key: "127.0.0.1-11434", Engine: agentprotocol.EngineOllama, Address: "http://127.0.0.1:11434", Models: []string{"m"}, Health: agentprotocol.HealthHealthy},
		},
		Metrics: []agentprotocol.MetricRelay{
			{ServerKey: "127.0.0.1-8000", Current: agentprotocol.Exposition{AgeMS: 2000, Text: fixture(t, "vllm_scrape1.prom")}, Baseline: &agentprotocol.Exposition{AgeMS: 32000, Text: fixture(t, "vllm_scrape0.prom")}},
			{ServerKey: "127.0.0.1-9999", Current: agentprotocol.Exposition{AgeMS: 1, Text: "vllm:num_requests_running 1\n"}},
			{ServerKey: "127.0.0.1-8000", Current: agentprotocol.Exposition{AgeMS: int64(5 * time.Minute / time.Millisecond), Text: fixture(t, "vllm_scrape1.prom")}},
		},
		Traffic: []agentprotocol.TrafficShape{
			{ID: "shape-0001", ServerKey: "127.0.0.1-8000", StartedAt: now.Add(-10*time.Minute - time.Second), Operation: "chat", StatusCode: 200, LatencyMS: 400, TTFTMS: &ttft, InputTokens: &input, OutputTokens: &output, Streaming: true},
			{ID: "shape-0002", ServerKey: "127.0.0.1-11434", StartedAt: now.Add(-10 * time.Minute), Operation: "chat", StatusCode: 200, LatencyMS: 1},
		},
		Gateway: &agentprotocol.Gateway{Listen: "127.0.0.1:4000", ServerKey: "127.0.0.1-8000"},
	}
	response, err := service.Sync(context.Background(), agent, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Accepted.Requests != 1 || response.Accepted.Measurements == 0 || len(response.Rejected) != 3 || response.PollIntervalSeconds != 15 {
		t.Fatalf("response: %+v", response)
	}
	reasons := []string{}
	for _, rejection := range response.Rejected {
		reasons = append(reasons, rejection.Kind+":"+rejection.Reason)
	}
	joined := strings.Join(reasons, "|")
	if !strings.Contains(joined, "metrics:server is not registered") || !strings.Contains(joined, "older than its validity window") || !strings.Contains(joined, "traffic:server is not registered") {
		t.Fatalf("rejections: %v", reasons)
	}
	if store.health["127.0.0.1-8000"] != "healthy" || store.health["127.0.0.1-11434"] != "" {
		t.Fatalf("health: %v", store.health)
	}
	windowed := false
	for _, row := range store.measurements {
		if row.ReplicaID != "target-1" || !row.ObservedAt.Equal(now.Add(-2*time.Second)) || row.ValidUntil.Sub(row.ObservedAt) != 90*time.Second {
			t.Fatalf("row: %+v", row)
		}
		if strings.HasSuffix(row.Name, "_p95") || row.Name == "runtime_internal_itl" {
			windowed = true
		}
	}
	if !windowed {
		t.Fatalf("the baseline did not produce window values: %+v", store.measurements)
	}
	record := store.requests[0]
	if !record.StartedAt.Equal(now.Add(-time.Second)) || record.EndpointID != "endpoint-1" || record.GenerationMS == nil || *record.GenerationMS != 320 || !strings.HasPrefix(record.RequestID, "agent-") {
		t.Fatalf("record: %+v", record)
	}
	if len(response.Servers) != 1 || response.Servers[0].TrafficSource != "agent-gateway" || response.Upgrade.Detail == "" || response.Upgrade.Required {
		t.Fatalf("servers=%+v upgrade=%+v", response.Servers, response.Upgrade)
	}
	if store.heartbeat.GatewayServerKey != "127.0.0.1-8000" || !strings.Contains(store.heartbeat.ServersJSON, "127.0.0.1-11434") {
		t.Fatalf("heartbeat: %+v", store.heartbeat)
	}
}

func TestSyncRefusesRevokedOldAndInvalidAgents(t *testing.T) {
	store := &fakeStore{health: map[string]string{}, revoked: true}
	service := Service{Store: store, MinimumAgentVersion: "1.2.0"}
	base := agentprotocol.SyncRequest{Protocol: agentprotocol.Version, AgentVersion: "1.2.0", OS: "linux", Arch: "arm64", SentAt: time.Now(), Hardware: agentprotocol.Hardware{State: "unavailable"}}
	if _, err := service.Sync(context.Background(), domain.Agent{ID: "agent-1"}, base); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked: %v", err)
	}
	old := base
	old.AgentVersion = "v1.1.9-rc.1"
	if response, err := service.Sync(context.Background(), domain.Agent{}, old); !errors.Is(err, ErrUpgradeRequired) || !response.Upgrade.Required {
		t.Fatalf("old agent: %v %+v", err, response.Upgrade)
	}
	invalid := base
	invalid.Hardware.State = "excellent"
	if _, err := service.Sync(context.Background(), domain.Agent{}, invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid: %v", err)
	}
	if _, err := service.Authenticate(context.Background(), domain.Principal{ID: "p", TenantID: "tenant-a", Kind: domain.AgentPrincipalKind}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked principal: %v", err)
	}
	store.revoked = false
	if _, err := service.Authenticate(context.Background(), domain.Principal{ID: "p", TenantID: "tenant-b", Kind: domain.AgentPrincipalKind}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("cross-tenant principal: %v", err)
	}
	if _, err := service.Authenticate(context.Background(), domain.Principal{ID: "p", TenantID: "tenant-a", Kind: "service_account"}); !errors.Is(err, ErrRevoked) {
		t.Fatalf("non-agent principal: %v", err)
	}
	if _, _, err := service.Create(context.Background(), "tenant-a", "Not Valid", "owner"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid name: %v", err)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, item := range []struct {
		left, right string
		want        int
	}{{"1.2.0", "1.2", 0}, {"v1.10.0", "1.9.9", 1}, {"1.2.0-rc.1", "1.2.0", 0}, {"dev", "1.0.0", -1}, {"1.0.0", "dev", 1}} {
		if got := compareVersions(item.left, item.right); got != item.want {
			t.Fatalf("compare(%s,%s)=%d want %d", item.left, item.right, got, item.want)
		}
	}
}
