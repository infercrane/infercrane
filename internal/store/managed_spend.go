package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func managedSpendReservationByResourceTx(ctx context.Context, tx *tx, tenant, resourceType, resourceName string, forUpdate bool) (domain.ManagedSpendReservation, error) {
	var out domain.ManagedSpendReservation
	var actual sql.NullInt64
	var activated sql.NullTime
	var created, updated time.Time
	query := `SELECT id,tenant_id,resource_type,resource_name,provider,state,currency,supplier_hourly_microusd,retail_hourly_microusd,reserved_microusd,actual_microusd,gross_margin_bps,runtime_limit_seconds,cleanup_allowance_seconds,pricing_json::text,resolution,expires_at,activated_at,created_at,updated_at FROM managed_spend_reservations WHERE tenant_id=? AND resource_type=? AND resource_name=?`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	err := tx.QueryRowContext(ctx, query, tenant, resourceType, resourceName).Scan(&out.ID, &out.TenantID, &out.ResourceType, &out.ResourceName, &out.Provider, &out.State, &out.Currency, &out.SupplierHourlyMicrousd, &out.RetailHourlyMicrousd, &out.ReservedMicrousd, &actual, &out.GrossMarginBPS, &out.RuntimeLimitSeconds, &out.CleanupAllowanceSeconds, &out.PricingJSON, &out.Resolution, &out.ExpiresAt, &activated, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if actual.Valid {
		out.ActualMicrousd = actual.Int64
	}
	if activated.Valid {
		value := activated.Time.UTC()
		out.ActivatedAt = &value
	}
	out.ExpiresAt, out.CreatedAt, out.UpdatedAt = out.ExpiresAt.UTC(), created.UTC(), updated.UTC()
	return out, nil
}

func (s *Store) ManagedSpendReservation(ctx context.Context, tenant, resourceType, resourceName string) (domain.ManagedSpendReservation, error) {
	if tenant == "" || resourceType == "" || resourceName == "" {
		return domain.ManagedSpendReservation{}, errors.New("tenant and resource identity are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ManagedSpendReservation{}, err
	}
	defer tx.Rollback()
	row, err := managedSpendReservationByResourceTx(ctx, tx, tenant, resourceType, resourceName, false)
	if err != nil {
		return row, err
	}
	return row, tx.Commit()
}

// ExpiredManagedDeployments returns tenant-owned deployment identities whose
// prepaid runtime window has elapsed. Callers submit ordinary durable delete
// operations; settlement occurs only after provider deletion succeeds.
func (s *Store) ExpiredManagedDeployments(ctx context.Context, current time.Time, limit int) ([]domain.ManagedSpendReservation, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.QueryContext(ctx, `SELECT r.id,r.tenant_id,r.resource_type,r.resource_name,r.provider,r.state,r.currency,r.supplier_hourly_microusd,r.retail_hourly_microusd,r.reserved_microusd,COALESCE(r.actual_microusd,0),r.gross_margin_bps,r.runtime_limit_seconds,r.cleanup_allowance_seconds,r.pricing_json::text,r.resolution,r.expires_at,r.activated_at,r.created_at,r.updated_at FROM managed_spend_reservations r JOIN deployments d ON d.tenant_id=r.tenant_id AND d.name=r.resource_name WHERE r.resource_type='deployment' AND r.state='reserved' AND r.activated_at IS NOT NULL AND r.expires_at<=? AND d.desired_state<>'deleted' ORDER BY r.expires_at,r.id LIMIT ?`, current.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanManagedSpendRows(rows)
}

// FailedUnactivatedManagedDeployments returns reservations whose latest create
// operation ended permanently before serving and which have no live provider
// replica. The caller queues the ordinary delete workflow so the hold is
// released only through the same durable cleanup boundary as every other
// deployment.
func (s *Store) FailedUnactivatedManagedDeployments(ctx context.Context, limit int) ([]domain.ManagedSpendReservation, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.QueryContext(ctx, `SELECT r.id,r.tenant_id,r.resource_type,r.resource_name,r.provider,r.state,r.currency,r.supplier_hourly_microusd,r.retail_hourly_microusd,r.reserved_microusd,COALESCE(r.actual_microusd,0),r.gross_margin_bps,r.runtime_limit_seconds,r.cleanup_allowance_seconds,r.pricing_json::text,r.resolution,r.expires_at,r.activated_at,r.created_at,r.updated_at FROM managed_spend_reservations r JOIN deployments d ON d.tenant_id=r.tenant_id AND d.name=r.resource_name JOIN operations o ON o.tenant_id=r.tenant_id AND o.resource_type='deployment' AND o.resource_name=r.resource_name WHERE r.resource_type='deployment' AND r.state='reserved' AND r.activated_at IS NULL AND d.desired_state<>'deleted' AND o.kind IN ('deployment.converge','deployment.serverless.converge') AND o.status IN ('failed','cancelled') AND NOT EXISTS (SELECT 1 FROM operations newer WHERE newer.tenant_id=o.tenant_id AND newer.resource_type=o.resource_type AND newer.resource_name=o.resource_name AND newer.created_at>o.created_at) AND NOT EXISTS (SELECT 1 FROM replicas p WHERE p.deployment_id=d.id AND p.lifecycle_state<>'deleted') ORDER BY o.completed_at,o.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanManagedSpendRows(rows)
}

func scanManagedSpendRows(rows *sql.Rows) ([]domain.ManagedSpendReservation, error) {
	out := make([]domain.ManagedSpendReservation, 0)
	for rows.Next() {
		var item domain.ManagedSpendReservation
		var activated sql.NullTime
		if err := rows.Scan(&item.ID, &item.TenantID, &item.ResourceType, &item.ResourceName, &item.Provider, &item.State, &item.Currency, &item.SupplierHourlyMicrousd, &item.RetailHourlyMicrousd, &item.ReservedMicrousd, &item.ActualMicrousd, &item.GrossMarginBPS, &item.RuntimeLimitSeconds, &item.CleanupAllowanceSeconds, &item.PricingJSON, &item.Resolution, &item.ExpiresAt, &activated, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if activated.Valid {
			value := activated.Time.UTC()
			item.ActivatedAt = &value
		}
		item.ExpiresAt, item.CreatedAt, item.UpdatedAt = item.ExpiresAt.UTC(), item.CreatedAt.UTC(), item.UpdatedAt.UTC()
		out = append(out, item)
	}
	return out, rows.Err()
}
