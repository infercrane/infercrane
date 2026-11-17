package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/metrics"
)

func activeCredentialKind(t *testing.T, ctx context.Context, s *Store, token string) (string, bool) {
	t.Helper()
	records, err := s.ActiveCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.Hash == credentialHash(token) {
			return record.Principal.Kind, true
		}
	}
	return "", false
}

func TestAgentRegistersObserveOnlyServersAndRevocationStopsIt(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if err := s.CreateTenant(ctx, "tenant-a", "Tenant A"); err != nil {
		t.Fatal(err)
	}
	agent, key, err := s.CreateAgent(ctx, "tenant-a", "laptop", "owner")
	if err != nil || agent.State != "active" || agent.Name != "laptop" || key == "" {
		t.Fatalf("agent=%#v err=%v", agent, err)
	}
	if kind, ok := activeCredentialKind(t, ctx, s, key); !ok || kind != domain.AgentPrincipalKind {
		t.Fatalf("agent key kind=%q active=%v", kind, ok)
	}
	if _, _, err = s.CreateAgent(ctx, "tenant-a", "laptop", "owner"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate active agent name: %v", err)
	}
	other, _, err := s.CreateAgent(ctx, "tenant-a", "workstation", "owner")
	if err != nil {
		t.Fatal(err)
	}

	registration := domain.AgentServerRegistration{Key: "127.0.0.1-8000", EndpointName: "qwen3-8b", UpstreamModel: "Qwen/Qwen3-8B", Runtime: "vllm", Address: "http://127.0.0.1:8000"}
	server, created, err := s.RegisterAgentServer(ctx, "tenant-a", agent.ID, registration)
	if err != nil || !created || server.EndpointName != "qwen3-8b" || server.OwnershipMode != "observe-only" || server.AgentID != agent.ID {
		t.Fatalf("server=%#v created=%v err=%v", server, created, err)
	}
	again, created, err := s.RegisterAgentServer(ctx, "tenant-a", agent.ID, registration)
	if err != nil || created || again.TargetID != server.TargetID {
		t.Fatalf("repeat registration=%#v created=%v err=%v", again, created, err)
	}
	changed := registration
	changed.UpstreamModel = "Qwen/Qwen3-32B"
	if _, _, err = s.RegisterAgentServer(ctx, "tenant-a", agent.ID, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed model registration: %v", err)
	}
	// Another machine's loopback server is a different target.
	twin := registration
	twin.EndpointName = "qwen3-8b-workstation"
	twinServer, _, err := s.RegisterAgentServer(ctx, "tenant-a", other.ID, twin)
	if err != nil || twinServer.TargetID == server.TargetID {
		t.Fatalf("twin=%#v err=%v", twinServer, err)
	}
	target, err := s.TargetForTenantByID(ctx, "tenant-a", server.TargetID)
	if err != nil || target.AgentID != agent.ID || target.URL != "http://127.0.0.1:8000" {
		t.Fatalf("target=%#v err=%v", target, err)
	}
	if _, err = s.PromoteAdoptionOwnership(ctx, "tenant-a", "qwen3-8b", "traffic-managed"); !errors.Is(err, ErrConflict) {
		t.Fatalf("agent endpoint promotion: %v", err)
	}
	// connect cannot reuse an agent's target by URL.
	_, adopted, err := s.AdoptEndpoint(ctx, "tenant-a", "direct-qwen", "direct-qwen", "Qwen/Qwen3-8B", "http://127.0.0.1:8000/v1", "vllm", "observe-only", "vllm")
	if err != nil || adopted.TargetID == server.TargetID {
		t.Fatalf("direct adoption shared the agent target: %#v err=%v", adopted, err)
	}

	seen := time.Now().UTC()
	if err = s.RecordAgentHeartbeat(ctx, "tenant-a", agent.ID, domain.AgentHeartbeat{AgentVersion: "1.0.0", ProtocolVersion: "infercrane.agent/v1", OS: "linux", Arch: "amd64", HardwareJSON: `{"state":"ready"}`, ServersJSON: `[{"key":"127.0.0.1-8000"}]`, GatewayServerKey: "127.0.0.1-8000", SeenAt: seen}); err != nil {
		t.Fatal(err)
	}
	if err = s.ReportAgentServerHealth(ctx, "tenant-a", agent.ID, "127.0.0.1-8000", "healthy", seen); err != nil {
		t.Fatal(err)
	}
	servers, err := s.AgentServers(ctx, "tenant-a", agent.ID)
	if err != nil || len(servers) != 1 || servers[0].Health != "healthy" || servers[0].ReportedAt == nil {
		t.Fatalf("servers=%#v err=%v", servers, err)
	}
	loaded, err := s.AgentForTenant(ctx, "tenant-a", "laptop")
	if err != nil || loaded.ID != agent.ID || loaded.AgentVersion != "1.0.0" || loaded.LastSeenAt == nil || loaded.GatewayServerKey != "127.0.0.1-8000" {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}

	rows := []domain.OperationalMeasurement{
		{ReplicaID: server.TargetID, Name: metrics.MeasurementRequestsRunning, Value: 3, Unit: "count", EvidenceClass: "measured", Source: "vllm_metrics", SampleCount: 1, ObservedAt: seen, ValidUntil: seen.Add(90 * time.Second)},
		{ReplicaID: server.TargetID, Name: metrics.MeasurementTTFTP95, Value: 180, Unit: "milliseconds", EvidenceClass: "measured", Source: "vllm_metrics", SampleCount: 20, ObservedAt: seen, ValidUntil: seen.Add(90 * time.Second)},
	}
	if _, err = s.RecordTargetMeasurements(ctx, "tenant-a", server.TargetID, rows); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RecordTargetMeasurements(ctx, "tenant-a", server.TargetID, rows); err != nil {
		t.Fatalf("a retried relay must be idempotent: %v", err)
	}
	bad := rows[:1]
	bad[0].Unit = "percent"
	if _, err = s.RecordTargetMeasurements(ctx, "tenant-a", server.TargetID, bad); err == nil {
		t.Fatal("a non-canonical unit was accepted")
	}
	if _, err = s.RecordTargetMeasurements(ctx, "tenant-b", server.TargetID, rows[1:]); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant measurement: %v", err)
	}
	snapshot, err := s.EndpointMonitoring(ctx, "tenant-a", "qwen3-8b", time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]float64{}
	for _, measurement := range snapshot.Evidence.Measurements {
		if measurement.Availability == "available" && measurement.Value != nil {
			found[measurement.Name] = *measurement.Value
		}
	}
	if found[metrics.MeasurementRequestsRunning] != 3 || found[metrics.MeasurementTTFTP95] != 180 {
		t.Fatalf("agent endpoint monitoring = %v", found)
	}
	twinSnapshot, err := s.EndpointMonitoring(ctx, "tenant-a", "qwen3-8b-workstation", time.Hour, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, measurement := range twinSnapshot.Evidence.Measurements {
		if measurement.Name == metrics.MeasurementRequestsRunning && measurement.Value != nil {
			t.Fatalf("another agent's endpoint read this target's measurements: %#v", measurement)
		}
	}

	if marked, err := s.MarkSilentAgentTargets(ctx, seen.Add(-time.Minute)); err != nil || marked != 1 {
		// Only the workstation twin has never reported.
		t.Fatalf("silent targets marked=%d err=%v", marked, err)
	}
	if marked, err := s.MarkSilentAgentTargets(ctx, seen.Add(time.Minute)); err != nil || marked != 1 {
		t.Fatalf("stale reporter marked=%d err=%v", marked, err)
	}

	revoked, err := s.RevokeAgent(ctx, "tenant-a", agent.ID)
	if err != nil || revoked.State != "revoked" || revoked.RevokedAt == nil {
		t.Fatalf("revoked=%#v err=%v", revoked, err)
	}
	if _, ok := activeCredentialKind(t, ctx, s, key); ok {
		t.Fatal("a revoked agent key is still an active credential")
	}
	if err = s.RecordAgentHeartbeat(ctx, "tenant-a", agent.ID, domain.AgentHeartbeat{AgentVersion: "1.0.0", SeenAt: time.Now()}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked heartbeat: %v", err)
	}
	if _, _, err = s.RegisterAgentServer(ctx, "tenant-a", agent.ID, domain.AgentServerRegistration{Key: "127.0.0.1-11434", EndpointName: "llama", UpstreamModel: "llama3.2", Runtime: "ollama", Address: "http://127.0.0.1:11434"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoked registration: %v", err)
	}
	if _, err = s.RevokeAgent(ctx, "tenant-a", agent.ID); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	if _, _, err = s.CreateAgent(ctx, "tenant-a", "laptop", "owner"); err != nil {
		t.Fatalf("a revoked name could not be reused: %v", err)
	}
	listed, err := s.AgentsForTenant(ctx, "tenant-a")
	if err != nil || len(listed) != 3 || listed[len(listed)-1].State != "revoked" {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	if purged, err := s.PurgeTargetMeasurements(ctx, seen.Add(time.Hour), 100); err != nil || purged != 2 {
		t.Fatalf("purged=%d err=%v", purged, err)
	}
}

func TestDirectAdoptionStoresOnlySealedCredential(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	sealed := &domain.SealedCredential{Ciphertext: []byte("ciphertext-1"), Nonce: []byte("nonce-000001")}
	_, adoption, err := s.AdoptEndpointWithCredential(ctx, "global", "private-vllm", "private-vllm", "Qwen/Qwen3-8B", "https://vllm.internal.example.test/v1", "vllm", "observe-only", "vllm", sealed)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.AdoptedCredential(ctx, "global", adoption.TargetID)
	if err != nil || !bytes.Equal(stored.Sealed.Ciphertext, sealed.Ciphertext) || stored.EndpointName != "private-vllm" {
		t.Fatalf("stored=%#v err=%v", stored, err)
	}
	rotated := &domain.SealedCredential{Ciphertext: []byte("ciphertext-2"), Nonce: []byte("nonce-000002")}
	if _, _, err = s.AdoptEndpointWithCredential(ctx, "global", "private-vllm", "private-vllm", "Qwen/Qwen3-8B", "https://vllm.internal.example.test", "vllm", "observe-only", "vllm", rotated); err != nil {
		t.Fatal(err)
	}
	if stored, err = s.AdoptedCredential(ctx, "global", adoption.TargetID); err != nil || !bytes.Equal(stored.Sealed.Ciphertext, rotated.Ciphertext) {
		t.Fatalf("rotated=%#v err=%v", stored, err)
	}
	_, plain, err := s.AdoptEndpoint(ctx, "global", "public-vllm", "public-vllm", "Qwen/Qwen3-8B", "https://vllm.public.example.test", "vllm", "observe-only", "vllm")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdoptedCredential(ctx, "global", plain.TargetID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("credential for an adoption without one: %v", err)
	}
}
