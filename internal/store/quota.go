package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (s *Store) SetTenantQuota(ctx context.Context, tenant string, maxDeployments, maxReplicas, maxRequestsPerMinute int) error {
	if tenant == "" || maxDeployments < 0 || maxReplicas < 0 || maxRequestsPerMinute < 0 {
		return errors.New("tenant and non-negative quota limits are required")
	}
	_, err := s.ExecContext(ctx, `INSERT INTO tenant_quotas(tenant_id,max_deployments,max_replicas,max_requests_per_minute,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(tenant_id) DO UPDATE SET max_deployments=EXCLUDED.max_deployments,max_replicas=EXCLUDED.max_replicas,max_requests_per_minute=EXCLUDED.max_requests_per_minute,updated_at=EXCLUDED.updated_at`, tenant, maxDeployments, maxReplicas, maxRequestsPerMinute, now())
	return err
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
