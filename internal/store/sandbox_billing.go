package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/managedbilling"
)

// ReserveManagedSandboxSpend places the maximum-lifetime hold before any
// provider resource is created. A replay returns the original hold without
// reserving the wallet twice.
func (s *Store) ReserveManagedSandboxSpend(ctx context.Context, tenant, sandboxID string, reservation domain.ManagedSpendReservation) (domain.ManagedSpendReservation, bool, error) {
	if tenant == "" || sandboxID == "" || reservation.Provider != "brezel" || reservation.ResourceType != "sandbox" || reservation.ResourceName != sandboxID || reservation.SupplierHourlyMicrousd < 1 || reservation.RetailHourlyMicrousd < reservation.SupplierHourlyMicrousd || reservation.ReservedMicrousd < 1 || reservation.RuntimeLimitSeconds < 30 || reservation.RuntimeLimitSeconds > 30*24*60*60 || reservation.CleanupAllowanceSeconds != 0 || reservation.GrossMarginBPS < 0 || reservation.GrossMarginBPS >= 10_000 || len(reservation.PricingJSON) == 0 || len(reservation.PricingJSON) > 64<<10 || !json.Valid([]byte(reservation.PricingJSON)) {
		return domain.ManagedSpendReservation{}, false, errors.New("managed sandbox reservation is invalid")
	}
	var pricing struct {
		MaxActive   int `json:"max_active"`
		MaxRetained int `json:"max_retained"`
	}
	if err := json.Unmarshal([]byte(reservation.PricingJSON), &pricing); err != nil || pricing.MaxActive < 1 || pricing.MaxRetained < pricing.MaxActive {
		return domain.ManagedSpendReservation{}, false, errors.New("managed sandbox reservation quota is invalid")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ManagedSpendReservation{}, false, err
	}
	defer tx.Rollback()
	existing, err := managedSpendReservationByResourceTx(ctx, tx, tenant, "sandbox", sandboxID, true)
	if err == nil {
		if existing.State == "released" {
			return domain.ManagedSpendReservation{}, false, fmt.Errorf("%w: released sandbox reservation cannot be reused", ErrConflict)
		}
		if existing.Provider != reservation.Provider || existing.SupplierHourlyMicrousd != reservation.SupplierHourlyMicrousd || existing.RetailHourlyMicrousd != reservation.RetailHourlyMicrousd || existing.ReservedMicrousd != reservation.ReservedMicrousd || existing.RuntimeLimitSeconds != reservation.RuntimeLimitSeconds || !sameJSONDocument(existing.PricingJSON, reservation.PricingJSON) {
			return domain.ManagedSpendReservation{}, false, fmt.Errorf("%w: sandbox reservation changed", ErrConflict)
		}
		return existing, false, tx.Commit()
	}
	if !errors.Is(err, ErrNotFound) {
		return domain.ManagedSpendReservation{}, false, err
	}
	var present int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM native_sandboxes WHERE tenant_id=? AND id=? AND deleted_at IS NULL FOR UPDATE`, tenant, sandboxID).Scan(&present); errors.Is(err, sql.ErrNoRows) {
		return domain.ManagedSpendReservation{}, false, ErrNotFound
	} else if err != nil {
		return domain.ManagedSpendReservation{}, false, err
	}
	var balance, reserved, debt int64
	err = tx.QueryRowContext(ctx, `SELECT balance_microusd,reserved_microusd,debt_microusd FROM managed_wallets WHERE tenant_id=? FOR UPDATE`, tenant).Scan(&balance, &reserved, &debt)
	if errors.Is(err, sql.ErrNoRows) || balance-reserved-debt < reservation.ReservedMicrousd {
		return domain.ManagedSpendReservation{}, false, domain.ErrInsufficientCredits
	}
	if err != nil {
		return domain.ManagedSpendReservation{}, false, err
	}
	var active, retained int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FILTER(WHERE status IN ('creating_workspace','creating','running','pausing','resuming','deleting','unknown','cleanup_pending')),COUNT(*) FILTER(WHERE status NOT IN ('deleted','expired','failed')) FROM native_sandboxes WHERE tenant_id=? AND deleted_at IS NULL`, tenant).Scan(&active, &retained)
	if err != nil {
		return domain.ManagedSpendReservation{}, false, err
	}
	if active > pricing.MaxActive || retained > pricing.MaxRetained {
		return domain.ManagedSpendReservation{}, false, fmt.Errorf("%w: managed sandbox capacity is %d active and %d retained", ErrConflict, pricing.MaxActive, pricing.MaxRetained)
	}
	reservation.ID, err = newID()
	if err != nil {
		return domain.ManagedSpendReservation{}, false, err
	}
	stamp := now()
	reservation.TenantID, reservation.State, reservation.Currency = tenant, "reserved", "USD"
	reservation.ExpiresAt = parseTime(stamp).Add(time.Duration(reservation.RuntimeLimitSeconds) * time.Second)
	reservation.CreatedAt, reservation.UpdatedAt = parseTime(stamp), parseTime(stamp)
	if _, err = tx.ExecContext(ctx, `INSERT INTO managed_spend_reservations(id,tenant_id,resource_type,resource_name,provider,state,currency,supplier_hourly_microusd,retail_hourly_microusd,reserved_microusd,gross_margin_bps,runtime_limit_seconds,cleanup_allowance_seconds,pricing_json,expires_at,created_at,updated_at) VALUES(?,?, 'sandbox', ?,?,'reserved','USD',?,?,?,?,?,?,?::jsonb,?,?,?)`, reservation.ID, tenant, sandboxID, reservation.Provider, reservation.SupplierHourlyMicrousd, reservation.RetailHourlyMicrousd, reservation.ReservedMicrousd, reservation.GrossMarginBPS, reservation.RuntimeLimitSeconds, reservation.CleanupAllowanceSeconds, reservation.PricingJSON, reservation.ExpiresAt.UTC(), stamp, stamp); err != nil {
		return domain.ManagedSpendReservation{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_wallets SET reserved_microusd=reserved_microusd+?,updated_at=? WHERE tenant_id=?`, reservation.ReservedMicrousd, stamp, tenant); err != nil {
		return domain.ManagedSpendReservation{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ManagedSpendReservation{}, false, err
	}
	return reservation, true, nil
}

// PostgreSQL jsonb deliberately normalizes object key order and whitespace.
// Idempotency compares the commercial contract semantically so replay behavior
// is identical on PostgreSQL and SQLite.
func sameJSONDocument(left, right string) bool {
	var leftValue, rightValue any
	if json.Unmarshal([]byte(left), &leftValue) != nil || json.Unmarshal([]byte(right), &rightValue) != nil {
		return false
	}
	leftCanonical, leftErr := json.Marshal(leftValue)
	rightCanonical, rightErr := json.Marshal(rightValue)
	return leftErr == nil && rightErr == nil && string(leftCanonical) == string(rightCanonical)
}

func (s *Store) ActivateManagedSandboxSpend(ctx context.Context, tenant, sandboxID string, activatedAt time.Time) error {
	if tenant == "" || sandboxID == "" || activatedAt.IsZero() {
		return errors.New("managed sandbox activation identity is required")
	}
	result, err := s.ExecContext(ctx, `UPDATE managed_spend_reservations SET activated_at=COALESCE(activated_at,?::timestamptz),expires_at=CASE WHEN activated_at IS NULL THEN ?::timestamptz+(runtime_limit_seconds * INTERVAL '1 second') ELSE expires_at END,updated_at=? WHERE tenant_id=? AND resource_type='sandbox' AND resource_name=? AND state='reserved'`, activatedAt.UTC(), activatedAt.UTC(), now(), tenant, sandboxID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ReleaseManagedSandboxSpend(ctx context.Context, tenant, sandboxID, reason string) error {
	return s.finalizeManagedSandboxSpend(ctx, tenant, sandboxID, 0, "released", reason)
}

func (s *Store) SettleManagedSandboxSpend(ctx context.Context, tenant, sandboxID string, runningMilliseconds int64, reason string) error {
	if runningMilliseconds < 0 {
		return errors.New("managed sandbox running time cannot be negative")
	}
	return s.finalizeManagedSandboxSpend(ctx, tenant, sandboxID, time.Duration(runningMilliseconds)*time.Millisecond, "settled", reason)
}

// ManagedSandboxesForReconciliation returns every open sandbox authorization.
// The reconciler observes provider state, enforces expiry, and finalizes holds
// after cleanup. Keeping this query independent from customer traffic prevents
// abandoned sandboxes from retaining credit indefinitely.
func (s *Store) ManagedSandboxesForReconciliation(ctx context.Context, limit int) ([]domain.ManagedSpendReservation, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,resource_type,resource_name,provider,state,currency,supplier_hourly_microusd,retail_hourly_microusd,reserved_microusd,COALESCE(actual_microusd,0),gross_margin_bps,runtime_limit_seconds,cleanup_allowance_seconds,pricing_json::text,resolution,expires_at,activated_at,created_at,updated_at FROM managed_spend_reservations WHERE resource_type='sandbox' AND state IN ('reserved','pending_reconciliation') ORDER BY expires_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanManagedSpendRows(rows)
}

func (s *Store) finalizeManagedSandboxSpend(ctx context.Context, tenant, sandboxID string, running time.Duration, targetState, reason string) error {
	if tenant == "" || sandboxID == "" || reason == "" || targetState != "released" && targetState != "settled" {
		return errors.New("managed sandbox settlement identity is required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	reservation, err := managedSpendReservationByResourceTx(ctx, tx, tenant, "sandbox", sandboxID, true)
	if errors.Is(err, ErrNotFound) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if reservation.State != "reserved" && reservation.State != "pending_reconciliation" {
		return tx.Commit()
	}
	actual := int64(0)
	if targetState == "settled" {
		maximum := time.Duration(reservation.RuntimeLimitSeconds) * time.Second
		if running > maximum {
			running = maximum
		}
		actual, err = managedbilling.DurationCostMicrousd(reservation.RetailHourlyMicrousd, running)
		if err != nil {
			return err
		}
		if actual > reservation.ReservedMicrousd {
			actual = reservation.ReservedMicrousd
		}
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `UPDATE managed_wallets SET balance_microusd=balance_microusd-?,reserved_microusd=reserved_microusd-?,updated_at=? WHERE tenant_id=?`, actual, reservation.ReservedMicrousd, stamp, tenant); err != nil {
		return err
	}
	if actual > 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO managed_wallet_ledger(id,tenant_id,spend_reservation_id,kind,currency,amount_microusd,description,created_at) VALUES(?,?,?,'settlement','USD',?,?,?) ON CONFLICT(tenant_id,spend_reservation_id,kind) DO NOTHING`, "settlement_"+reservation.ID, tenant, reservation.ID, -actual, reason, stamp); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_spend_reservations SET actual_microusd=?,state=?,resolution=?,updated_at=? WHERE id=? AND tenant_id=?`, actual, targetState, reason, stamp, reservation.ID, tenant); err != nil {
		return err
	}
	if err = reconcileManagedDebtTx(ctx, tx, tenant, stamp); err != nil {
		return err
	}
	return tx.Commit()
}
