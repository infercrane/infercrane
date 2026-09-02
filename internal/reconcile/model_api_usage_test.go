package reconcile

import (
	"context"
	"errors"
	"testing"

	"github.com/infercrane/infercrane/internal/modelapirouting"
)

type usageReconcileStore struct {
	pending  []modelapirouting.Reservation
	settled  []string
	released []string
}

func (s *usageReconcileStore) PendingModelAPIUsageReservations(context.Context, int) ([]modelapirouting.Reservation, error) {
	return s.pending, nil
}
func (s *usageReconcileStore) SettleModelAPIUsage(_ context.Context, tenant, id string, _ modelapirouting.Usage) (modelapirouting.Reservation, error) {
	s.settled = append(s.settled, tenant+"/"+id)
	return modelapirouting.Reservation{}, nil
}
func (s *usageReconcileStore) ConfirmNoChargeModelAPIUsage(_ context.Context, tenant, id, _ string) error {
	s.released = append(s.released, tenant+"/"+id)
	return nil
}

type usageResolverFunc func(context.Context, modelapirouting.Reservation) (ModelAPIUsageEvidence, error)

func (f usageResolverFunc) ResolveModelAPIUsage(ctx context.Context, reservation modelapirouting.Reservation) (ModelAPIUsageEvidence, error) {
	return f(ctx, reservation)
}

func TestModelAPIUsageReconcilerNeverGuessesUnknownUsage(t *testing.T) {
	store := &usageReconcileStore{pending: []modelapirouting.Reservation{{ID: "unknown", TenantID: "tenant"}}}
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
	store := &usageReconcileStore{pending: []modelapirouting.Reservation{{ID: "usage", TenantID: "tenant"}, {ID: "free", TenantID: "tenant"}}}
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
