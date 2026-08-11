package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/requestquota"
)

func (s *Store) SetTenantQuota(ctx context.Context, tenant string, maxDeployments, maxReplicas, maxRequestsPerMinute int) error {
	if tenant == "" || maxDeployments < 0 || maxReplicas < 0 || maxRequestsPerMinute < 0 {
		return errors.New("tenant and non-negative quota limits are required")
	}
	_, err := s.ExecContext(ctx, `INSERT INTO tenant_quotas(tenant_id,max_deployments,max_replicas,max_requests_per_minute,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(tenant_id) DO UPDATE SET max_deployments=EXCLUDED.max_deployments,max_replicas=EXCLUDED.max_replicas,max_requests_per_minute=EXCLUDED.max_requests_per_minute,updated_at=EXCLUDED.updated_at`, tenant, maxDeployments, maxReplicas, maxRequestsPerMinute, now())
	return err
}

func (s *Store) RequestQuotaPolicies(ctx context.Context) ([]requestquota.Policy, error) {
	rows, err := s.QueryContext(ctx, `SELECT tenant_id,max_requests_per_minute FROM tenant_quotas ORDER BY tenant_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var policies []requestquota.Policy
	for rows.Next() {
		var policy requestquota.Policy
		if err := rows.Scan(&policy.TenantID, &policy.MaxRequestsPerMinute); err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

// ReserveRequestQuota allocates a bounded lease from one UTC minute window.
// Gateway requests consume that lease in memory and never query PostgreSQL.
func (s *Store) ReserveRequestQuota(ctx context.Context, tenant string, window time.Time, requested int) (granted int, err error) {
	if tenant == "" || requested < 1 || !window.Equal(window.UTC().Truncate(time.Minute)) {
		return 0, errors.New("tenant, minute-aligned UTC window, and positive request count are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var limit int
	if err = tx.QueryRowContext(ctx, `SELECT max_requests_per_minute FROM tenant_quotas WHERE tenant_id=? FOR UPDATE`, tenant).Scan(&limit); errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	} else if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tenant_request_quota_windows(tenant_id,window_start,reserved_requests) VALUES(?,?,0) ON CONFLICT(tenant_id,window_start) DO NOTHING`, tenant, window); err != nil {
		return 0, err
	}
	var reserved int
	if err = tx.QueryRowContext(ctx, `SELECT reserved_requests FROM tenant_request_quota_windows WHERE tenant_id=? AND window_start=? FOR UPDATE`, tenant, window).Scan(&reserved); err != nil {
		return 0, err
	}
	remaining := limit - reserved
	if remaining < 0 {
		remaining = 0
	}
	granted = requested
	if granted > remaining {
		granted = remaining
	}
	if granted > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE tenant_request_quota_windows SET reserved_requests=reserved_requests+? WHERE tenant_id=? AND window_start=?`, granted, tenant, window); err != nil {
			return 0, err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM tenant_request_quota_windows WHERE window_start < ?`, window.Add(-2*time.Minute)); err != nil {
		return 0, err
	}
	err = tx.Commit()
	return granted, err
}

func enforceDeploymentQuota(ctx context.Context, tx *tx, tenant, excludeID string, requestedReplicas int, adding bool) error {
	var maxDeployments, maxReplicas int
	err := tx.QueryRowContext(ctx, `SELECT max_deployments,max_replicas FROM tenant_quotas WHERE tenant_id=?`, tenant).Scan(&maxDeployments, &maxReplicas)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var deployments, replicas int
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(max_replicas),0) FROM deployments WHERE tenant_id=? AND desired_state!='deleted' AND id!=?`, tenant, excludeID).Scan(&deployments, &replicas)
	if err != nil {
		return err
	}
	if adding {
		deployments++
	}
	replicas += requestedReplicas
	if deployments > maxDeployments {
		return fmt.Errorf("%w: tenant deployment quota exceeded (%d/%d)", ErrConflict, deployments, maxDeployments)
	}
	if replicas > maxReplicas {
		return fmt.Errorf("%w: tenant replica quota exceeded (%d/%d)", ErrConflict, replicas, maxReplicas)
	}
	return nil
}
