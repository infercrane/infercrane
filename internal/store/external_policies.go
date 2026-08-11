package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) SetExternalTargetPolicyForTenant(ctx context.Context, policy domain.ExternalTargetPolicy) (domain.ExternalTargetPolicy, error) {
	if policy.TenantID == "" || policy.DeploymentID == "" || policy.TargetID == "" || policy.SecretReferenceID == "" || policy.Adapter == "" || policy.RequestLimit < 1 {
		return domain.ExternalTargetPolicy{}, errors.New("tenant, deployment, target, adapter, secret reference, and positive request limit are required")
	}
	if policy.Adapter != "openai-compatible-external" && policy.Adapter != "openrouter" {
		return domain.ExternalTargetPolicy{}, fmt.Errorf("unsupported external adapter %q", policy.Adapter)
	}
	if policy.Enabled && !policy.PrivacyAcknowledged {
		return domain.ExternalTargetPolicy{}, errors.New("enabled external capacity requires explicit privacy acknowledgement")
	}
	if policy.CostLimitMicrousd < 1 || policy.MaxRequestCostMicrousd < 1 || policy.MaxRequestCostMicrousd > policy.CostLimitMicrousd {
		return domain.ExternalTargetPolicy{}, errors.New("cost budget and a bounded worst-case per-request reservation are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	defer tx.Rollback()
	var deploymentExists, targetExists, secretExists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deployments WHERE tenant_id=? AND id=?)`, policy.TenantID, policy.DeploymentID).Scan(&deploymentExists); err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM targets WHERE tenant_id=? AND id=? AND provider=?)`, policy.TenantID, policy.TargetID, policy.Adapter).Scan(&targetExists); err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM secret_references WHERE tenant_id=? AND id=?)`, policy.TenantID, policy.SecretReferenceID).Scan(&secretExists); err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	if !deploymentExists || !targetExists || !secretExists {
		return domain.ExternalTargetPolicy{}, ErrNotFound
	}
	if policy.ID == "" {
		policy.ID, err = newID()
		if err != nil {
			return domain.ExternalTargetPolicy{}, err
		}
	}
	stamp := now()
	_, err = tx.ExecContext(ctx, `INSERT INTO external_target_policies(id,tenant_id,deployment_id,target_id,adapter,secret_reference_id,enabled,privacy_acknowledged,request_limit,cost_limit_microusd,max_request_cost_microusd,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,deployment_id) DO UPDATE SET target_id=EXCLUDED.target_id,adapter=EXCLUDED.adapter,secret_reference_id=EXCLUDED.secret_reference_id,enabled=EXCLUDED.enabled,privacy_acknowledged=EXCLUDED.privacy_acknowledged,request_limit=EXCLUDED.request_limit,cost_limit_microusd=EXCLUDED.cost_limit_microusd,max_request_cost_microusd=EXCLUDED.max_request_cost_microusd,updated_at=EXCLUDED.updated_at`, policy.ID, policy.TenantID, policy.DeploymentID, policy.TargetID, policy.Adapter, policy.SecretReferenceID, policy.Enabled, policy.PrivacyAcknowledged, policy.RequestLimit, policy.CostLimitMicrousd, policy.MaxRequestCostMicrousd, stamp, stamp)
	if err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	return s.ExternalTargetPolicyForDeployment(ctx, policy.TenantID, policy.DeploymentID)
}

func (s *Store) ExternalTargetPolicyForDeployment(ctx context.Context, tenant, deploymentID string) (domain.ExternalTargetPolicy, error) {
	var out domain.ExternalTargetPolicy
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,deployment_id,target_id,adapter,secret_reference_id,enabled,privacy_acknowledged,request_limit,requests_reserved,cost_limit_microusd,max_request_cost_microusd,cost_reserved_microusd,created_at,updated_at FROM external_target_policies WHERE tenant_id=? AND deployment_id=?`, tenant, deploymentID).Scan(&out.ID, &out.TenantID, &out.DeploymentID, &out.TargetID, &out.Adapter, &out.SecretReferenceID, &out.Enabled, &out.PrivacyAcknowledged, &out.RequestLimit, &out.RequestsReserved, &out.CostLimitMicrousd, &out.MaxRequestCostMicrousd, &out.CostReservedMicrousd, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExternalTargetPolicy{}, ErrNotFound
	}
	if err != nil {
		return domain.ExternalTargetPolicy{}, err
	}
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	return out, nil
}

// LeaseExternalBudget atomically reserves a bounded request batch. Gateways
// consume leases in memory, preserving the no-PostgreSQL inference-path
// invariant. Unused reservations may reduce availability but can never exceed
// the hard persisted budget.
func (s *Store) LeaseExternalBudget(ctx context.Context, tenant, policyID string, requested int64) (domain.ExternalBudgetLease, error) {
	if tenant == "" || policyID == "" || requested < 1 || requested > 256 {
		return domain.ExternalBudgetLease{}, errors.New("tenant, policy, and lease size 1..256 are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	defer tx.Rollback()
	var enabled, privacy bool
	var limit, used, costLimit, costUsed, perRequest int64
	err = tx.QueryRowContext(ctx, `SELECT enabled,privacy_acknowledged,request_limit,requests_reserved,cost_limit_microusd,cost_reserved_microusd,max_request_cost_microusd FROM external_target_policies WHERE tenant_id=? AND id=? FOR UPDATE`, tenant, policyID).Scan(&enabled, &privacy, &limit, &used, &costLimit, &costUsed, &perRequest)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExternalBudgetLease{}, ErrNotFound
	}
	if err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	if !enabled || !privacy {
		return domain.ExternalBudgetLease{}, fmt.Errorf("%w: external policy is disabled or lacks privacy acknowledgement", ErrConflict)
	}
	available := limit - used
	if requested > available {
		requested = available
	}
	costAvailable := (costLimit - costUsed) / perRequest
	if requested > costAvailable {
		requested = costAvailable
	}
	if requested < 1 {
		return domain.ExternalBudgetLease{}, fmt.Errorf("%w: external hard budget is exhausted", ErrConflict)
	}
	reservedCost := requested * perRequest
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE external_target_policies SET requests_reserved=requests_reserved+?,cost_reserved_microusd=cost_reserved_microusd+?,updated_at=? WHERE tenant_id=? AND id=?`, requested, reservedCost, stamp, tenant, policyID); err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	return domain.ExternalBudgetLease{PolicyID: policyID, Requests: requested, ReservedCostMicrousd: reservedCost, MaxRequestCostMicrousd: perRequest}, nil
}
