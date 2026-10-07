package reconcile

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/infercrane/infercrane/internal/modelapirouting"
)

var ErrUsageNotReady = errors.New("supplier usage is not ready")

type ModelAPIUsageStore interface {
	ReconcileableModelAPIUsageReservations(context.Context, time.Time, int) ([]modelapirouting.Reservation, error)
	ModelAPIUsageReconciliationBacklog(context.Context) (int64, time.Time, error)
	RecordModelAPIUsageReconciliationAttempt(context.Context, string, string, time.Time) error
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

type ModelAPIUsageEvidenceSource interface {
	ModelAPIUsageReconciliationEvidence(context.Context, string, string) (modelapirouting.ReconciliationEvidence, bool, error)
}

// StoredSupplierUsageResolver accepts only immutable evidence for its exact
// supplier. Each supplier remains an explicit launch boundary even though the
// durable evidence shape is shared.
type StoredSupplierUsageResolver struct {
	Supplier string
	Source   ModelAPIUsageEvidenceSource
}

func (r StoredSupplierUsageResolver) ResolveModelAPIUsage(ctx context.Context, reservation modelapirouting.Reservation) (ModelAPIUsageEvidence, error) {
	if strings.TrimSpace(r.Supplier) == "" || r.Source == nil {
		return ModelAPIUsageEvidence{}, errors.New("supplier usage resolver is not configured")
	}
	if reservation.Supplier != r.Supplier {
		return ModelAPIUsageEvidence{}, fmt.Errorf("supplier resolver %q cannot resolve %q", r.Supplier, reservation.Supplier)
	}
	evidence, found, err := r.Source.ModelAPIUsageReconciliationEvidence(ctx, reservation.TenantID, reservation.ID)
	if err != nil {
		return ModelAPIUsageEvidence{}, err
	}
	if !found {
		return ModelAPIUsageEvidence{}, ErrUsageNotReady
	}
	if evidence.Supplier != reservation.Supplier || evidence.ReservationID != reservation.ID || evidence.TenantID != reservation.TenantID {
		return ModelAPIUsageEvidence{}, errors.New("reconciliation evidence does not match the reserved supplier attempt")
	}
	if evidence.Authority != modelapirouting.EvidenceSupplierAdapter && evidence.Authority != modelapirouting.EvidenceOperatorVerified {
		return ModelAPIUsageEvidence{}, errors.New("reconciliation evidence has no supported authority")
	}
	reference := evidence.Authority + ":" + evidence.Reference
	if evidence.SupplierRequestID != "" {
		reference += ":supplier_request=" + evidence.SupplierRequestID
	}
	switch evidence.Outcome {
	case modelapirouting.ReconciliationNoCharge:
		return ModelAPIUsageEvidence{NoChargeConfirmed: true, Evidence: reference}, nil
	case modelapirouting.ReconciliationUsage:
		if evidence.InputTokens == nil || evidence.OutputTokens == nil {
			return ModelAPIUsageEvidence{}, errors.New("supplier usage evidence is incomplete")
		}
		return ModelAPIUsageEvidence{Usage: &modelapirouting.Usage{InputTokens: evidence.InputTokens, CachedInputTokens: evidence.CachedInputTokens, OutputTokens: evidence.OutputTokens}, Evidence: reference}, nil
	default:
		return ModelAPIUsageEvidence{}, errors.New("supplier usage evidence has an unsupported outcome")
	}
}

// SupplierUsageResolverRegistry prevents supplier names from implicitly
// selecting executable reconciliation code.
type SupplierUsageResolverRegistry struct {
	resolvers map[string]ModelAPIUsageResolver
}

func NewSupplierUsageResolverRegistry(resolvers ...StoredSupplierUsageResolver) (*SupplierUsageResolverRegistry, error) {
	registry := &SupplierUsageResolverRegistry{resolvers: make(map[string]ModelAPIUsageResolver, len(resolvers))}
	for _, resolver := range resolvers {
		name := strings.TrimSpace(resolver.Supplier)
		if name == "" || resolver.Source == nil {
			return nil, errors.New("supplier reconciliation resolver requires a supplier and evidence source")
		}
		if _, exists := registry.resolvers[name]; exists {
			return nil, fmt.Errorf("supplier reconciliation resolver %q is duplicated", name)
		}
		resolver.Supplier = name
		registry.resolvers[name] = resolver
	}
	return registry, nil
}

func (r *SupplierUsageResolverRegistry) ResolveModelAPIUsage(ctx context.Context, reservation modelapirouting.Reservation) (ModelAPIUsageEvidence, error) {
	if r == nil {
		return ModelAPIUsageEvidence{}, errors.New("supplier reconciliation registry is unavailable")
	}
	resolver := r.resolvers[reservation.Supplier]
	if resolver == nil {
		return ModelAPIUsageEvidence{}, fmt.Errorf("%w: supplier %q has no authoritative resolver", ErrUsageNotReady, reservation.Supplier)
	}
	return resolver.ResolveModelAPIUsage(ctx, reservation)
}

type ModelAPIUsageTelemetry struct {
	runs, failures, settled, released, waiting atomic.Uint64
	backlog, oldestAgeSeconds                  atomic.Int64
}

func (t *ModelAPIUsageTelemetry) observeRun()      { t.runs.Add(1) }
func (t *ModelAPIUsageTelemetry) observeFailure()  { t.failures.Add(1) }
func (t *ModelAPIUsageTelemetry) observeSettled()  { t.settled.Add(1) }
func (t *ModelAPIUsageTelemetry) observeReleased() { t.released.Add(1) }
func (t *ModelAPIUsageTelemetry) observeWaiting()  { t.waiting.Add(1) }
func (t *ModelAPIUsageTelemetry) observeBacklog(count int64, oldest time.Time, now time.Time) {
	t.backlog.Store(count)
	age := int64(0)
	if !oldest.IsZero() && now.After(oldest) {
		age = int64(now.Sub(oldest) / time.Second)
	}
	t.oldestAgeSeconds.Store(age)
}

func (t *ModelAPIUsageTelemetry) WritePrometheus(w io.Writer) {
	if t == nil {
		return
	}
	fmt.Fprintf(w, "# TYPE infercrane_model_api_reconciliation_runs_total counter\ninfercrane_model_api_reconciliation_runs_total %d\n", t.runs.Load())
	fmt.Fprintf(w, "# TYPE infercrane_model_api_reconciliation_failures_total counter\ninfercrane_model_api_reconciliation_failures_total %d\n", t.failures.Load())
	fmt.Fprintf(w, "# TYPE infercrane_model_api_reconciliation_settled_total counter\ninfercrane_model_api_reconciliation_settled_total %d\n", t.settled.Load())
	fmt.Fprintf(w, "# TYPE infercrane_model_api_reconciliation_released_total counter\ninfercrane_model_api_reconciliation_released_total %d\n", t.released.Load())
	fmt.Fprintf(w, "# TYPE infercrane_model_api_reconciliation_waiting_total counter\ninfercrane_model_api_reconciliation_waiting_total %d\n", t.waiting.Load())
	fmt.Fprintf(w, "# TYPE infercrane_model_api_reconciliation_backlog gauge\ninfercrane_model_api_reconciliation_backlog %d\n", t.backlog.Load())
	fmt.Fprintf(w, "# TYPE infercrane_model_api_reconciliation_oldest_age_seconds gauge\ninfercrane_model_api_reconciliation_oldest_age_seconds %d\n", t.oldestAgeSeconds.Load())
}

// ModelAPIUsageReconciler resolves ambiguous supplier usage without guessing.
// Unknown evidence remains reserved; an explicit no-charge confirmation or
// complete token usage is required to mutate customer money.
type ModelAPIUsageReconciler struct {
	Store         ModelAPIUsageStore
	Resolver      ModelAPIUsageResolver
	Logger        *slog.Logger
	Telemetry     *ModelAPIUsageTelemetry
	Limit         int
	InFlightGrace time.Duration
	now           func() time.Time
}

func (r ModelAPIUsageReconciler) Once(ctx context.Context) error {
	if r.Store == nil || r.Resolver == nil {
		return errors.New("hosted Model API usage reconciler requires storage and a supplier resolver")
	}
	now := time.Now().UTC()
	if r.now != nil {
		now = r.now().UTC()
	}
	grace := r.InFlightGrace
	if grace <= 0 {
		grace = 10 * time.Minute
	}
	if r.Telemetry != nil {
		r.Telemetry.observeRun()
	}
	reservations, err := r.Store.ReconcileableModelAPIUsageReservations(ctx, now.Add(-grace), r.Limit)
	if err != nil {
		if r.Telemetry != nil {
			r.Telemetry.observeFailure()
		}
		return err
	}
	var failures []error
	for _, reservation := range reservations {
		if reservation.State != "pending_reconciliation" {
			retained, retainErr := r.Store.SettleModelAPIUsage(ctx, reservation.TenantID, reservation.ID, modelapirouting.Usage{})
			if retainErr != nil {
				failures = append(failures, retainErr)
				continue
			}
			reservation = retained
		}
		if attemptErr := r.Store.RecordModelAPIUsageReconciliationAttempt(ctx, reservation.TenantID, reservation.ID, now); attemptErr != nil {
			failures = append(failures, attemptErr)
			continue
		}
		evidence, resolveErr := r.Resolver.ResolveModelAPIUsage(ctx, reservation)
		if resolveErr != nil {
			if errors.Is(resolveErr, ErrUsageNotReady) {
				if r.Telemetry != nil {
					r.Telemetry.observeWaiting()
				}
			} else {
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
			} else if r.Telemetry != nil {
				r.Telemetry.observeReleased()
			}
			continue
		}
		if evidence.Usage == nil || evidence.Usage.InputTokens == nil || evidence.Usage.OutputTokens == nil {
			if r.Telemetry != nil {
				r.Telemetry.observeWaiting()
			}
			continue
		}
		if _, settleErr := r.Store.SettleModelAPIUsage(ctx, reservation.TenantID, reservation.ID, *evidence.Usage); settleErr != nil {
			failures = append(failures, settleErr)
		} else if r.Telemetry != nil {
			r.Telemetry.observeSettled()
		}
	}
	count, oldest, backlogErr := r.Store.ModelAPIUsageReconciliationBacklog(ctx)
	if backlogErr != nil {
		failures = append(failures, backlogErr)
	} else if r.Telemetry != nil {
		r.Telemetry.observeBacklog(count, oldest, now)
	}
	if len(failures) > 0 {
		if r.Telemetry != nil {
			r.Telemetry.observeFailure()
		}
		if r.Logger != nil {
			r.Logger.Error("hosted model API usage reconciliation incomplete", "failures", len(failures))
		}
	}
	return errors.Join(failures...)
}
