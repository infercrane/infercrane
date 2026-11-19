package agentrelay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/admission"
	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

type fakeDataPlaneStore struct {
	fakeStore
	bindings    []domain.AgentDataPlaneBinding
	credentials []domain.CredentialRecord
	policies    []admission.Policy
	quota       int
	limited     bool
	leased      int
}

func (f *fakeDataPlaneStore) AgentDataPlaneBindings(context.Context, string, string) ([]domain.AgentDataPlaneBinding, error) {
	return f.bindings, nil
}
func (f *fakeDataPlaneStore) AgentDataPlaneCredentials(context.Context, string) ([]domain.CredentialRecord, error) {
	return f.credentials, nil
}
func (f *fakeDataPlaneStore) AdmissionPoliciesForTenant(context.Context, string) ([]admission.Policy, error) {
	return f.policies, nil
}
func (f *fakeDataPlaneStore) RequestQuotaLimit(context.Context, string) (int, bool, error) {
	return f.quota, f.limited, nil
}
func (f *fakeDataPlaneStore) ReserveRequestQuota(_ context.Context, _ string, _ time.Time, requested int) (int, error) {
	if !f.limited {
		return 0, domain.ErrNotFound
	}
	f.leased += requested
	return requested / 2, nil
}

func TestDataPlaneCarriesOnlyWhatTheAgentNeedsToServeItsEndpoints(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	hash := func(seed byte) string {
		out := make([]byte, 64)
		for index := range out {
			out[index] = "0123456789abcdef"[(int(seed)+index)%16]
		}
		return string(out)
	}
	store := &fakeDataPlaneStore{
		bindings: []domain.AgentDataPlaneBinding{
			{EndpointID: "endpoint-1", EndpointName: "support-bot", LogicalModelID: "model-1", EnvironmentID: "production", ServingPlanID: "plan-1", RoutingPolicy: "manual", BindingID: "binding-1", TargetID: "target-1", ServerKey: "vllm-8000", UpstreamModel: "Qwen/Qwen3-8B", Runtime: "vllm", Health: "healthy", Weight: 100},
		},
		credentials: []domain.CredentialRecord{
			{Hash: hash(1), Principal: domain.Principal{ID: "app", TenantID: "tenant-a", Role: string(authz.Viewer), Scopes: authz.DefaultScopeNames(authz.Viewer)}},
			{Hash: hash(2), Principal: domain.Principal{ID: "scoped", TenantID: "tenant-a", Role: string(authz.Viewer), Scopes: authz.DefaultScopeNames(authz.Viewer), EndpointNames: []string{"support-bot", "other"}}},
			{Hash: hash(3), Principal: domain.Principal{ID: "elsewhere", TenantID: "tenant-a", Role: string(authz.Viewer), Scopes: authz.DefaultScopeNames(authz.Viewer), EndpointNames: []string{"other"}}},
			{Hash: hash(4), Principal: domain.Principal{ID: "agent-key", TenantID: "tenant-a", Role: string(authz.Viewer), Kind: domain.AgentPrincipalKind}},
			{Hash: hash(5), Principal: domain.Principal{ID: "foreign", TenantID: "tenant-b", Role: string(authz.Viewer)}},
			{Hash: hash(6), Principal: domain.Principal{ID: "disabled", TenantID: "tenant-a", Role: string(authz.Viewer), Disabled: true}},
		},
		policies: []admission.Policy{
			{Key: "tenant-a\x00support-bot", MaxConcurrency: 4, MaxQueueDepth: 8, QueueTimeout: 2 * time.Second, RequestTimeout: time.Minute, MaxRequestBytes: 1 << 20, MaxOutputTokens: 2048, AllowedPriorities: map[string]struct{}{"normal": {}, "high": {}}, Enabled: true, RetryBudget: 1},
			{Key: "tenant-a\x00hosted-endpoint", MaxConcurrency: 1, MaxQueueDepth: 0, QueueTimeout: time.Second, MaxRequestBytes: 1, MaxOutputTokens: 1, Enabled: true},
		},
		quota: 600, limited: true,
	}
	service := &Service{Store: store, Now: func() time.Time { return now }}
	agent := domain.Agent{ID: "agent-1", TenantID: "tenant-a", Name: "cluster-a"}
	snapshot, err := service.DataPlane(context.Background(), agent, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = snapshot.Validate(now); err != nil {
		t.Fatalf("the relay built an invalid snapshot: %v", err)
	}
	if len(snapshot.Routes) != 1 || snapshot.Routes[0].Bindings[0].ServerKey != "vllm-8000" || !snapshot.Routes[0].Bindings[0].Capabilities.ChatCompletions {
		t.Fatalf("routes=%+v", snapshot.Routes)
	}
	ids := map[string]bool{}
	for _, credential := range snapshot.Credentials {
		ids[credential.PrincipalID] = true
	}
	if len(ids) != 2 || !ids["app"] || !ids["scoped"] {
		t.Fatalf("credentials must be the tenant's live inference keys that may call a served endpoint: %v", ids)
	}
	if len(snapshot.Admission) != 1 || snapshot.Admission[0].Endpoint != "support-bot" || snapshot.Admission[0].QueueTimeoutMS != 2000 || len(snapshot.Admission[0].AllowedPriorities) != 2 {
		t.Fatalf("admission=%+v", snapshot.Admission)
	}
	if snapshot.RequestQuota == nil || snapshot.RequestQuota.MaxRequestsPerMinute != 600 || !snapshot.ValidUntil.Equal(now.Add(agentprotocol.DataPlaneValidity)) {
		t.Fatalf("quota=%+v valid_until=%s", snapshot.RequestQuota, snapshot.ValidUntil)
	}
	unchanged, err := service.DataPlane(context.Background(), agent, snapshot.Generation)
	if err != nil || !unchanged.Unchanged || len(unchanged.Credentials) != 0 || len(unchanged.Routes) != 0 || unchanged.Generation != snapshot.Generation {
		t.Fatalf("unchanged=%+v err=%v", unchanged, err)
	}
	// Revoking a key changes the generation.
	store.credentials = store.credentials[1:]
	changed, err := service.DataPlane(context.Background(), agent, snapshot.Generation)
	if err != nil || changed.Unchanged || changed.Generation == snapshot.Generation {
		t.Fatalf("a revoked key did not change the snapshot: %+v err=%v", changed, err)
	}
	// An agent with no agent-managed endpoint receives no key at all.
	store.bindings = nil
	empty, err := service.DataPlane(context.Background(), agent, "")
	if err != nil || len(empty.Credentials) != 0 || len(empty.Admission) != 0 || empty.RequestQuota != nil {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}

	lease, err := service.LeaseQuota(context.Background(), agent, agentprotocol.QuotaLeaseRequest{Window: now.Truncate(time.Minute), Requested: 40})
	if err != nil || lease.Granted != 20 || lease.Unlimited || store.leased != 40 {
		t.Fatalf("lease=%+v err=%v", lease, err)
	}
	if _, err = service.LeaseQuota(context.Background(), agent, agentprotocol.QuotaLeaseRequest{Window: now.Add(-time.Hour).Truncate(time.Minute), Requested: 40}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stale window: %v", err)
	}
	store.limited = false
	if lease, err = service.LeaseQuota(context.Background(), agent, agentprotocol.QuotaLeaseRequest{Window: now.Truncate(time.Minute), Requested: 40}); err != nil || !lease.Unlimited {
		t.Fatalf("unlimited=%+v err=%v", lease, err)
	}
	if _, err = (&Service{Store: &fakeStore{}}).DataPlane(context.Background(), agent, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("store without data plane: %v", err)
	}
}

func TestDataPlaneShapesMustNameTheAgentManagedBinding(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	agent := domain.Agent{ID: "agent-1", TenantID: "tenant-a"}
	server := domain.AgentServer{Key: "vllm-8000", TargetID: "target-1", EndpointID: "endpoint-1", BindingID: "binding-1", OwnershipMode: "agent-managed", Runtime: "vllm"}
	queue := 3.5
	shape := agentprotocol.TrafficShape{ID: "shape-0001", ServerKey: "vllm-8000", StartedAt: now, Operation: "chat", StatusCode: 200, LatencyMS: 120, Source: agentprotocol.ShapeSourceDataPlane, BindingID: "binding-1", QueueMS: &queue, RetryCount: 1, ResponseModel: "Qwen/Qwen3-8B"}
	if err := shape.Validate(); err != nil {
		t.Fatal(err)
	}
	record, err := requestRecord(agent, server, shape, 0, now)
	if err != nil || record.ComputeMode != agentprotocol.ComputeModeDataPlane || record.QueueMS == nil || *record.QueueMS != queue || record.RetryCount != 1 || record.ResponseModel != "Qwen/Qwen3-8B" || record.BindingID != "binding-1" {
		t.Fatalf("record=%+v err=%v", record, err)
	}
	shape.BindingID = "binding-of-someone-else"
	if _, err = requestRecord(agent, server, shape, 0, now); err == nil {
		t.Fatal("a data-plane shape for another binding was accepted")
	}
	server.OwnershipMode = "observe-only"
	shape.BindingID = "binding-1"
	if _, err = requestRecord(agent, server, shape, 0, now); err == nil {
		t.Fatal("a data-plane shape for an observe-only server was accepted")
	}
	// The pass-through gateway cannot claim data-plane fields.
	passthrough := shape
	passthrough.Source = ""
	if err = passthrough.Validate(); err == nil {
		t.Fatal("a gateway shape with data-plane fields was valid")
	}
}
