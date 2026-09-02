package reconcile

import (
	"context"
	"errors"
	"log/slog"

	"github.com/infercrane/infercrane/internal/modelapirouting"
)

var ErrUsageNotReady = errors.New("supplier usage is not ready")

type ModelAPIUsageStore interface {
	PendingModelAPIUsageReservations(context.Context, int) ([]modelapirouting.Reservation, error)
	SettleModelAPIUsage(context.Context, string, string, modelapirouting.Usage) (modelapirouting.Reservation, error)
	ConfirmNoChargeModelAPIUsage(context.Context, string, string, string) error
}

type ModelAPIUsageEvidence struct {
	Usage             *modelapirouting.Usage
	NoChargeConfirmed bool
	Evidence          string
}

type ModelAPIUsageResolver interface {
	ResolveModelAPIUsage(context.Context, modelapirouting.Reservation) (ModelAPIUsageEvidence, error)
}

// ModelAPIUsageReconciler resolves ambiguous supplier usage without guessing.
// Unknown evidence remains reserved; an explicit no-charge confirmation or
// complete token usage is required to mutate customer money.
type ModelAPIUsageReconciler struct {
	Store    ModelAPIUsageStore
	Resolver ModelAPIUsageResolver
	Logger   *slog.Logger
	Limit    int
}

func (r ModelAPIUsageReconciler) Once(ctx context.Context) error {
	reservations, err := r.Store.PendingModelAPIUsageReservations(ctx, r.Limit)
	if err != nil {
		return err
	}
	var failures []error
	for _, reservation := range reservations {
		evidence, resolveErr := r.Resolver.ResolveModelAPIUsage(ctx, reservation)
		if resolveErr != nil {
			if !errors.Is(resolveErr, ErrUsageNotReady) {
				failures = append(failures, resolveErr)
			}
			continue
		}
		if evidence.NoChargeConfirmed {
			if evidence.Evidence == "" {
				failures = append(failures, errors.New("supplier no-charge result omitted evidence"))
				continue
			}
			if releaseErr := r.Store.ConfirmNoChargeModelAPIUsage(ctx, reservation.TenantID, reservation.ID, evidence.Evidence); releaseErr != nil {
				failures = append(failures, releaseErr)
			}
			continue
		}
		if evidence.Usage == nil || evidence.Usage.InputTokens == nil || evidence.Usage.OutputTokens == nil {
			continue
		}
		if _, settleErr := r.Store.SettleModelAPIUsage(ctx, reservation.TenantID, reservation.ID, *evidence.Usage); settleErr != nil {
			failures = append(failures, settleErr)
		}
	}
	if len(failures) > 0 && r.Logger != nil {
		r.Logger.Error("hosted model API usage reconciliation incomplete", "failures", len(failures))
	}
	return errors.Join(failures...)
}
