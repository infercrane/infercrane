// Package managedsandbox reconciles prepaid Brezel lifecycle state without
// depending on customer requests. It is the fail-safe that expires compute,
// confirms provider cleanup, and releases or settles wallet holds.
package managedsandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/sandboxprovider"
)

type Repository interface {
	ManagedSandboxesForReconciliation(context.Context, int) ([]domain.ManagedSpendReservation, error)
	CleanupPendingSandboxesForReconciliation(context.Context, int) ([]domain.NativeSandbox, error)
	NativeSandbox(context.Context, string, string) (domain.NativeSandbox, error)
	RecordNativeSandboxTransition(context.Context, string, string, string, string, string, string, time.Time) (domain.NativeSandbox, error)
	SandboxRunningMilliseconds(context.Context, string, string) (int64, error)
	ReleaseManagedSandboxSpend(context.Context, string, string, string) error
	SettleManagedSandboxSpend(context.Context, string, string, int64, string) error
}

type Reconciler struct {
	Store    Repository
	Provider sandboxprovider.Provider
	Now      func() time.Time
	Limit    int
}

func (r Reconciler) Once(ctx context.Context) error {
	if r.Store == nil || r.Provider == nil {
		return errors.New("managed sandbox reconciler requires storage and provider")
	}
	rows, err := r.Store.ManagedSandboxesForReconciliation(ctx, r.Limit)
	if err != nil {
		return err
	}
	var failures []error
	for _, reservation := range rows {
		if err = r.reconcile(ctx, reservation); err != nil {
			failures = append(failures, fmt.Errorf("sandbox %s/%s: %w", reservation.TenantID, reservation.ResourceName, err))
		}
	}
	orphans, orphanErr := r.Store.CleanupPendingSandboxesForReconciliation(ctx, r.Limit)
	if orphanErr != nil {
		failures = append(failures, fmt.Errorf("list unreserved cleanup-pending sandboxes: %w", orphanErr))
	} else {
		observedAt := time.Now().UTC()
		if r.Now != nil {
			observedAt = r.Now().UTC()
		}
		for _, row := range orphans {
			if err = r.cleanup(ctx, row, "unreserved:"+row.ID); err != nil {
				failures = append(failures, fmt.Errorf("sandbox %s/%s: %w", row.TenantID, row.ID, err))
				continue
			}
			if _, err = r.recordTransition(ctx, row, "deleted", "", observedAt, "unreserved-cleanup"); err != nil {
				failures = append(failures, fmt.Errorf("sandbox %s/%s: record confirmed cleanup: %w", row.TenantID, row.ID, err))
			}
		}
	}
	return errors.Join(failures...)
}

func (r Reconciler) reconcile(ctx context.Context, reservation domain.ManagedSpendReservation) error {
	row, err := r.Store.NativeSandbox(ctx, reservation.TenantID, reservation.ResourceName)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}

	// A crash before billing activation must never become a free provider
	// resource. Confirm deletion first, then release the untouched hold.
	if reservation.ActivatedAt == nil {
		terminal := row.Status == "failed" || row.Status == "deleted" || row.Status == "expired" || row.Status == "cleanup_pending"
		if !terminal && now.Before(reservation.CreatedAt.Add(5*time.Minute)) {
			return nil
		}
		if err = r.cleanup(ctx, row, reservation.ID); err != nil {
			return err
		}
		if _, err = r.recordTransition(ctx, row, "deleted", "", now, "unactivated"); err != nil {
			return err
		}
		return r.Store.ReleaseManagedSandboxSpend(ctx, row.TenantID, row.ID, "provider cleanup confirmed before sandbox billing activation")
	}

	observed, observeErr := r.Provider.Get(ctx, row.TenantID, row.BrezelSandboxID)
	providerAbsent := errors.Is(observeErr, sandboxprovider.ErrNotFound)
	if observeErr != nil && !providerAbsent {
		return observeErr
	}
	terminal := providerAbsent
	if observeErr == nil {
		observedStatus := customerStatus(observed.State)
		terminal = observedStatus == "deleted" || observedStatus == "expired" || observedStatus == "failed"
		if observedStatus != "unknown" && observedStatus != row.Status {
			transitionAt := boundedTransitionTime(observed.UpdatedAt, row, now)
			row, err = r.recordTransition(ctx, row, observedStatus, failureCode(observed.Failure), transitionAt, "observed")
			if err != nil {
				return err
			}
		}
	}
	expired := !reservation.ExpiresAt.After(now)
	if !terminal && !expired {
		return nil
	}
	if err = r.cleanup(ctx, row, reservation.ID); err != nil {
		return err
	}
	endedAt := now
	if expired && reservation.ExpiresAt.Before(endedAt) {
		endedAt = reservation.ExpiresAt
	}
	if !row.BillingStateSince.IsZero() && endedAt.Before(row.BillingStateSince) {
		endedAt = row.BillingStateSince
	}
	if _, err = r.recordTransition(ctx, row, "deleted", "", endedAt, "terminal"); err != nil {
		return err
	}
	runningMilliseconds, err := r.Store.SandboxRunningMilliseconds(ctx, row.TenantID, row.ID)
	if err != nil {
		return err
	}
	return r.Store.SettleManagedSandboxSpend(ctx, row.TenantID, row.ID, runningMilliseconds, "metered active sandbox runtime; provider cleanup confirmed by lifecycle reconciler")
}

func (r Reconciler) cleanup(ctx context.Context, row domain.NativeSandbox, reservationID string) error {
	if row.BrezelSandboxID != "" {
		if _, err := r.Provider.Delete(ctx, row.TenantID, row.BrezelSandboxID, "managed-reconcile:"+reservationID+":sandbox"); !cleanupConfirmed(err) {
			return err
		}
	}
	if row.BrezelWorkspaceID != "" {
		if _, err := r.Provider.DeleteWorkspace(ctx, row.TenantID, row.BrezelWorkspaceID, "managed-reconcile:"+reservationID+":workspace"); !cleanupConfirmed(err) {
			return err
		}
	}
	return nil
}

// Brezel returns a conflict when a deletion retry reaches an already terminal
// resource. That response confirms the cleanup goal just as strongly as a 404.
// Other conflicts, including active mutations or attached workspaces, remain
// failures and are retried without releasing capacity or wallet holds.
func cleanupConfirmed(err error) bool {
	return err == nil || errors.Is(err, sandboxprovider.ErrNotFound) ||
		(errors.Is(err, sandboxprovider.ErrConflict) && strings.Contains(err.Error(), "already terminal"))
}

func (r Reconciler) recordTransition(ctx context.Context, row domain.NativeSandbox, status, failure string, occurredAt time.Time, source string) (domain.NativeSandbox, error) {
	return r.Store.RecordNativeSandboxTransition(ctx, row.TenantID, row.ID, status, failure, "managed-reconcile:"+source+":"+occurredAt.UTC().Format(time.RFC3339Nano), "sandbox.lifecycle."+status, occurredAt)
}

func boundedTransitionTime(providerUpdatedAt time.Time, row domain.NativeSandbox, now time.Time) time.Time {
	if providerUpdatedAt.IsZero() || providerUpdatedAt.After(now) {
		return now
	}
	startedAt := row.BillingStateSince
	if startedAt.IsZero() {
		startedAt = row.LastActiveAt
	}
	if providerUpdatedAt.Before(startedAt) {
		return startedAt
	}
	return providerUpdatedAt.UTC()
}

func customerStatus(state string) string {
	switch state {
	case "creating", "running", "standby", "pausing", "resuming", "deleting", "deleted", "expired", "failed":
		return state
	default:
		return "unknown"
	}
}

func failureCode(failure *sandboxprovider.Failure) string {
	if failure == nil {
		return ""
	}
	return failure.Code
}
