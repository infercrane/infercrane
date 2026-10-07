package reconcile

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/modelapirouting"
)

type usageReconcileStore struct {
	pending  []modelapirouting.Reservation
	settled  []string
	released []string
}

func (s *usageReconcileStore) ReconcileableModelAPIUsageReservations(context.Context, time.Time, int) ([]modelapirouting.Reservation, error) {
	rows := make([]modelapirouting.Reservation, 0, len(s.pending))
	for _, row := range s.pending {
		if row.State == "pending_reconciliation" || row.State == "transmitted" || row.State == "response_started" {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
func (s *usageReconcileStore) ModelAPIUsageReconciliationBacklog(context.Context) (int64, time.Time, error) {
	var count int64
	var oldest time.Time
	for _, row := range s.pending {
		if row.State == "pending_reconciliation" {
			count++
			if oldest.IsZero() || row.UpdatedAt.Before(oldest) {
				oldest = row.UpdatedAt
			}
		}
	}
	return count, oldest, nil
}
func (s *usageReconcileStore) RecordModelAPIUsageReconciliationAttempt(context.Context, string, string, time.Time) error {
	return nil
}
func (s *usageReconcileStore) SettleModelAPIUsage(_ context.Context, tenant, id string, usage modelapirouting.Usage) (modelapirouting.Reservation, error) {
	for index := range s.pending {
		if s.pending[index].TenantID != tenant || s.pending[index].ID != id {
			continue
		}
		if usage.InputTokens == nil || usage.OutputTokens == nil {
			s.pending[index].State = "pending_reconciliation"
			return s.pending[index], nil
		}
		s.pending[index].State = "settled"
		s.settled = append(s.settled, tenant+"/"+id)
		return s.pending[index], nil
	}
	return modelapirouting.Reservation{}, errors.New("reservation not found")
}
func (s *usageReconcileStore) ConfirmNoChargeModelAPIUsage(_ context.Context, tenant, id, _ string) error {
	s.released = append(s.released, tenant+"/"+id)
	for index := range s.pending {
		if s.pending[index].TenantID == tenant && s.pending[index].ID == id {
			s.pending[index].State = "released"
		}
	}
	return nil
}

type usageResolverFunc func(context.Context, modelapirouting.Reservation) (ModelAPIUsageEvidence, error)

func (f usageResolverFunc) ResolveModelAPIUsage(ctx context.Context, reservation modelapirouting.Reservation) (ModelAPIUsageEvidence, error) {
	return f(ctx, reservation)
}

func TestModelAPIUsageReconcilerNeverGuessesUnknownUsage(t *testing.T) {
	store := &usageReconcileStore{pending: []modelapirouting.Reservation{{ID: "unknown", TenantID: "tenant", State: "pending_reconciliation"}}}
	reconciler := ModelAPIUsageReconciler{Store: store, Resolver: usageResolverFunc(func(context.Context, modelapirouting.Reservation) (ModelAPIUsageEvidence, error) {
		return ModelAPIUsageEvidence{}, ErrUsageNotReady
	})}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.settled) != 0 || len(store.released) != 0 {
		t.Fatalf("unknown usage mutated money: settled=%v released=%v", store.settled, store.released)
	}
}

func TestModelAPIUsageReconcilerRequiresCompleteEvidence(t *testing.T) {
	store := &usageReconcileStore{pending: []modelapirouting.Reservation{{ID: "usage", TenantID: "tenant", State: "pending_reconciliation"}, {ID: "free", TenantID: "tenant", State: "pending_reconciliation"}}}
	input, output := 10, 2
	reconciler := ModelAPIUsageReconciler{Store: store, Resolver: usageResolverFunc(func(_ context.Context, reservation modelapirouting.Reservation) (ModelAPIUsageEvidence, error) {
		switch reservation.ID {
		case "usage":
			return ModelAPIUsageEvidence{Usage: &modelapirouting.Usage{InputTokens: &input, OutputTokens: &output}}, nil
		case "free":
			return ModelAPIUsageEvidence{NoChargeConfirmed: true, Evidence: "supplier request ledger"}, nil
		default:
			return ModelAPIUsageEvidence{}, errors.New("unexpected")
		}
	})}
	if err := reconciler.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.settled) != 1 || store.settled[0] != "tenant/usage" || len(store.released) != 1 || store.released[0] != "tenant/free" {
		t.Fatalf("settled=%v released=%v", store.settled, store.released)
	}
}

type usageEvidenceSource struct {
	evidence map[string]modelapirouting.ReconciliationEvidence
}

func (s *usageEvidenceSource) ModelAPIUsageReconciliationEvidence(_ context.Context, tenant, reservation string) (modelapirouting.ReconciliationEvidence, bool, error) {
	evidence, found := s.evidence[tenant+"/"+reservation]
	return evidence, found, nil
}

func TestModelAPIUsageReconcilerSurvivesRestartAndUsesOnlyDurableSupplierEvidence(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	store := &usageReconcileStore{pending: []modelapirouting.Reservation{{ID: "usage", TenantID: "tenant", Supplier: "supplier-a", State: "transmitted", UpdatedAt: now.Add(-time.Hour)}, {ID: "free", TenantID: "tenant", Supplier: "supplier-a", State: "pending_reconciliation", UpdatedAt: now.Add(-time.Hour)}}}
	source := &usageEvidenceSource{evidence: map[string]modelapirouting.ReconciliationEvidence{}}
	registry, err := NewSupplierUsageResolverRegistry(StoredSupplierUsageResolver{Supplier: "supplier-a", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	first := ModelAPIUsageReconciler{Store: store, Resolver: registry, InFlightGrace: time.Minute, now: func() time.Time { return now }}
	if err = first.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.pending[0].State != "pending_reconciliation" || len(store.settled) != 0 || len(store.released) != 0 {
		t.Fatalf("first pass guessed money outcome: rows=%+v settled=%v released=%v", store.pending, store.settled, store.released)
	}
	input, cached, output := 19, 4, 7
	source.evidence["tenant/usage"] = modelapirouting.ReconciliationEvidence{ReservationID: "usage", TenantID: "tenant", Supplier: "supplier-a", SupplierRequestID: "supplier-usage", Outcome: modelapirouting.ReconciliationUsage, Authority: modelapirouting.EvidenceOperatorVerified, Reference: "invoice://usage-1", InputTokens: &input, CachedInputTokens: &cached, OutputTokens: &output, ObservedAt: now}
	source.evidence["tenant/free"] = modelapirouting.ReconciliationEvidence{ReservationID: "free", TenantID: "tenant", Supplier: "supplier-a", SupplierRequestID: "supplier-free", Outcome: modelapirouting.ReconciliationNoCharge, Authority: modelapirouting.EvidenceOperatorVerified, Reference: "invoice://free-1", ObservedAt: now}

	// A new reconciler value represents a restarted control-plane process. It
	// reconstructs all authority from durable reservation and evidence state.
	telemetry := &ModelAPIUsageTelemetry{}
	restarted := ModelAPIUsageReconciler{Store: store, Resolver: registry, Telemetry: telemetry, InFlightGrace: time.Minute, now: func() time.Time { return now.Add(time.Minute) }}
	if err = restarted.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.settled) != 1 || store.settled[0] != "tenant/usage" || len(store.released) != 1 || store.released[0] != "tenant/free" {
		t.Fatalf("settled=%v released=%v", store.settled, store.released)
	}
	if err = restarted.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(store.settled) != 1 || len(store.released) != 1 {
		t.Fatalf("replayed reconciliation was not idempotent: settled=%v released=%v", store.settled, store.released)
	}
	var metrics bytes.Buffer
	telemetry.WritePrometheus(&metrics)
	for _, expected := range []string{"infercrane_model_api_reconciliation_settled_total 1", "infercrane_model_api_reconciliation_released_total 1", "infercrane_model_api_reconciliation_backlog 0"} {
		if !strings.Contains(metrics.String(), expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, metrics.String())
		}
	}
}

func TestSupplierUsageResolverRegistryFailsClosedWithoutExactSupplierResolver(t *testing.T) {
	registry, err := NewSupplierUsageResolverRegistry(StoredSupplierUsageResolver{Supplier: "supplier-a", Source: &usageEvidenceSource{evidence: map[string]modelapirouting.ReconciliationEvidence{}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.ResolveModelAPIUsage(context.Background(), modelapirouting.Reservation{ID: "reservation", TenantID: "tenant", Supplier: "supplier-b"})
	if !errors.Is(err, ErrUsageNotReady) {
		t.Fatalf("unsupported supplier did not fail closed: %v", err)
	}
}
