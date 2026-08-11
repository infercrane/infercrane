package external

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/overflow"
)

type coordinatorStore struct {
	policy domain.ExternalTargetPolicy
	secret domain.SecretReference
	leases atomic.Int64
	state  overflow.State
}

func (s *coordinatorStore) EvaluateOverflow(_ context.Context, _ string, _ string, signal overflow.Signal, budget bool, now time.Time) (overflow.Decision, error) {
	policy := overflow.Policy{Mode: s.policy.OverflowMode, QueueThreshold: value(s.policy.QueueThreshold), BreachIntervals: s.policy.BreachIntervals, RecoveryIntervals: s.policy.RecoveryIntervals, Cooldown: time.Duration(s.policy.CooldownSeconds) * time.Second, SignalMaxAge: time.Duration(s.policy.SignalMaxAgeSeconds) * time.Second, PrivacyAcknowledged: s.policy.PrivacyAcknowledged, BudgetAvailable: budget}
	decision, err := overflow.Evaluate(policy, s.state, signal, now)
	if err == nil {
		s.state.External = decision.Route == "external"
		s.state.ConsecutiveHigh = decision.ConsecutiveHigh
		s.state.ConsecutiveLow = decision.ConsecutiveLow
		if decision.Action != "hold" {
			s.state.LastChangedAt = now
		}
	}
	return decision, err
}
func value(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func (s *coordinatorStore) ExternalTargetPolicyForDeployment(context.Context, string, string) (domain.ExternalTargetPolicy, error) {
	return s.policy, nil
}
func (s *coordinatorStore) SecretReferenceForTenant(context.Context, string, string) (domain.SecretReference, error) {
	return s.secret, nil
}
func (s *coordinatorStore) LeaseExternalBudget(context.Context, string, string, int64) (domain.ExternalBudgetLease, error) {
	s.leases.Add(1)
	return domain.ExternalBudgetLease{PolicyID: s.policy.ID, Requests: 2, ReservedCostMicrousd: 200, MaxRequestCostMicrousd: 100}, nil
}

type staticSecret string

func (s staticSecret) Resolve(context.Context, domain.SecretReference) (string, error) {
	return string(s), nil
}

func TestCoordinatorSelectsOnlyHealthyExplicitBudgetedFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer external-key" {
			t.Fatalf("request=%s authorization=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"provider/model"}]}`))
	}))
	defer server.Close()
	store := &coordinatorStore{
		policy: domain.ExternalTargetPolicy{ID: "policy", TenantID: "tenant", DeploymentID: "deployment", TargetID: "target", Adapter: "openrouter", SecretReferenceID: "secret", Enabled: true, PrivacyAcknowledged: true, RequestLimit: 2, CostLimitMicrousd: 200, MaxRequestCostMicrousd: 100},
		secret: domain.SecretReference{ID: "secret", Resolver: "env", Reference: "OPENROUTER_API_KEY"},
	}
	coordinator := &Coordinator{Store: store, Secrets: staticSecret("external-key"), Budgets: NewBudgetPool(), Client: server.Client(), LeaseBatch: 2}
	deployment := domain.Deployment{ID: "deployment", TenantID: "tenant", Name: "prod", Model: "alias", ActiveRevisionID: "revision"}
	target := domain.Target{ID: "target", Provider: "openrouter", Runtime: "openai", URL: server.URL, UpstreamModel: "provider/model"}
	route, err := coordinator.Resolve(context.Background(), deployment, []domain.Target{target})
	if err != nil || route.ComputeMode != "external" || route.Provider != "openrouter" || route.UpstreamAPIKey != "external-key" || route.ExternalPolicyID != "policy" || store.leases.Load() != 1 {
		t.Fatalf("route=%#v leases=%d err=%v", route, store.leases.Load(), err)
	}
	if _, err := coordinator.Budgets.Authorize("policy"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Resolve(context.Background(), deployment, []domain.Target{target}); err != nil {
		t.Fatalf("existing lease was not reused: leases=%d err=%v", store.leases.Load(), err)
	}
}

func TestCoordinatorRejectsUnacknowledgedPolicyBeforeCredentialResolution(t *testing.T) {
	store := &coordinatorStore{policy: domain.ExternalTargetPolicy{ID: "policy", Adapter: "openrouter", Enabled: true}}
	coordinator := &Coordinator{Store: store, Secrets: staticSecret("must-not-use"), Budgets: NewBudgetPool()}
	if _, err := coordinator.Resolve(context.Background(), domain.Deployment{ID: "deployment", TenantID: "tenant"}, nil); err == nil {
		t.Fatal("unacknowledged external policy was accepted")
	}
}

func TestCoordinatorQueueOverflowUsesExplicitPolicyAndOneExternalRoute(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"provider/model"}]}`))
	}))
	defer server.Close()
	threshold := 2.0
	store := &coordinatorStore{policy: domain.ExternalTargetPolicy{ID: "policy", TenantID: "tenant", DeploymentID: "deployment", TargetID: "target", Adapter: "openrouter", SecretReferenceID: "secret", Enabled: true, PrivacyAcknowledged: true, RequestLimit: 10, CostLimitMicrousd: 1000, MaxRequestCostMicrousd: 100, OverflowMode: "health_and_queue", QueueThreshold: &threshold, BreachIntervals: 1, RecoveryIntervals: 2, CooldownSeconds: 0, SignalMaxAgeSeconds: 30}, secret: domain.SecretReference{ID: "secret"}}
	coordinator := &Coordinator{Store: store, Secrets: staticSecret("external-key"), Budgets: NewBudgetPool(), Client: server.Client(), LeaseBatch: 2}
	deployment := domain.Deployment{ID: "deployment", TenantID: "tenant", Name: "prod", ActiveRevisionID: "revision"}
	targets := []domain.Target{{ID: "target", Provider: "openrouter", Runtime: "openai", URL: server.URL, UpstreamModel: "provider/model"}}
	waiting := 3.0
	now := time.Now().UTC()
	route, decision, err := coordinator.ResolveHybrid(context.Background(), deployment, targets, overflow.Signal{PrimaryHealthy: true, Waiting: &waiting, ObservedAt: now}, now)
	if err != nil || decision.Route != "external" || route.ExternalPolicyID != "policy" || store.leases.Load() != 1 {
		t.Fatalf("route=%#v decision=%#v leases=%d err=%v", route, decision, store.leases.Load(), err)
	}
}
