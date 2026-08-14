package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/external"
)

func (s *Store) ManagedExternalBindingPolicy(ctx context.Context, tenant, bindingID string) (domain.ManagedExternalBindingPolicy, error) {
	var out domain.ManagedExternalBindingPolicy
	var configJSON, created, updated string
	err := s.QueryRowContext(ctx, `SELECT b.id,b.tenant_id,b.target_id,b.config_json::text,x.requests_reserved,x.cost_reserved_microusd,x.created_at,x.updated_at FROM backend_bindings b JOIN managed_external_binding_budgets x ON x.binding_id=b.id AND x.tenant_id=b.tenant_id WHERE b.tenant_id=? AND b.id=?`, tenant, bindingID).Scan(&out.BindingID, &out.TenantID, &out.TargetID, &configJSON, &out.RequestsReserved, &out.CostReservedMicrousd, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ManagedExternalBindingPolicy{}, ErrNotFound
	}
	if err != nil {
		return domain.ManagedExternalBindingPolicy{}, err
	}
	config, managed, err := external.ParseManagedBindingConfig(configJSON)
	if err != nil {
		return domain.ManagedExternalBindingPolicy{}, fmt.Errorf("managed external binding policy is invalid: %w", err)
	}
	if !managed {
		return domain.ManagedExternalBindingPolicy{}, errors.New("managed external binding policy is missing")
	}
	out.ID = out.BindingID
	out.Adapter = config.Adapter
	out.SecretReferenceID = config.SecretReferenceID
	out.Enabled = config.Enabled
	out.PrivacyAcknowledged = config.PrivacyAcknowledged
	out.RequestLimit = config.RequestLimit
	out.CostLimitMicrousd = config.CostLimitMicrousd
	out.MaxRequestCostMicrousd = config.MaxRequestCostMicrousd
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	return out, nil
}

// LeaseManagedExternalBindingBudget reserves a bounded request batch in the
// control plane. Gateway requests consume the lease from memory, so the
// inference data path never reads PostgreSQL.
func (s *Store) LeaseManagedExternalBindingBudget(ctx context.Context, tenant, bindingID string, requested int64) (domain.ExternalBudgetLease, error) {
	if tenant == "" || bindingID == "" || requested < 1 || requested > 256 {
		return domain.ExternalBudgetLease{}, errors.New("tenant, binding, and lease size 1..256 are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	defer tx.Rollback()
	var configJSON string
	var used, costUsed int64
	err = tx.QueryRowContext(ctx, `SELECT b.config_json::text,x.requests_reserved,x.cost_reserved_microusd FROM managed_external_binding_budgets x JOIN backend_bindings b ON b.id=x.binding_id AND b.tenant_id=x.tenant_id WHERE x.tenant_id=? AND x.binding_id=? FOR UPDATE OF x`, tenant, bindingID).Scan(&configJSON, &used, &costUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ExternalBudgetLease{}, ErrNotFound
	}
	if err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	config, managed, err := external.ParseManagedBindingConfig(configJSON)
	if err != nil || !managed {
		return domain.ExternalBudgetLease{}, fmt.Errorf("%w: managed external binding policy is invalid", ErrConflict)
	}
	if !config.Enabled || !config.PrivacyAcknowledged {
		return domain.ExternalBudgetLease{}, fmt.Errorf("%w: managed external binding is disabled or lacks privacy acknowledgement", ErrConflict)
	}
	available := config.RequestLimit - used
	if requested > available {
		requested = available
	}
	costAvailable := (config.CostLimitMicrousd - costUsed) / config.MaxRequestCostMicrousd
	if requested > costAvailable {
		requested = costAvailable
	}
	if requested < 1 {
		return domain.ExternalBudgetLease{}, fmt.Errorf("%w: managed external binding hard budget is exhausted", ErrConflict)
	}
	reservedCost := requested * config.MaxRequestCostMicrousd
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE managed_external_binding_budgets SET requests_reserved=requests_reserved+?,cost_reserved_microusd=cost_reserved_microusd+?,updated_at=? WHERE tenant_id=? AND binding_id=?`, requested, reservedCost, stamp, tenant, bindingID); err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ExternalBudgetLease{}, err
	}
	return domain.ExternalBudgetLease{PolicyID: bindingID, Requests: requested, ReservedCostMicrousd: reservedCost, MaxRequestCostMicrousd: config.MaxRequestCostMicrousd}, nil
}
