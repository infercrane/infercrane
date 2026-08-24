package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
)

// PublishDeploymentEndpoint atomically publishes an existing deployment under
// a stable application endpoint. It is intentionally idempotent only for the
// exact endpoint/deployment pair; an existing alias is never rebound as a
// side effect of optimization.
func (s *Store) PublishDeploymentEndpoint(ctx context.Context, tenant, endpointName, deploymentName string) (domain.ResolvedEndpoint, error) {
	if tenant == "" || !validEndpointName(endpointName) || !validEndpointName(deploymentName) {
		return domain.ResolvedEndpoint{}, errors.New("tenant and valid endpoint and deployment names are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ResolvedEndpoint{}, err
	}
	defer tx.Rollback()

	var deploymentID string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM deployments WHERE tenant_id=? AND name=? AND desired_state='running' FOR SHARE`, tenant, deploymentName).Scan(&deploymentID); errors.Is(err, sql.ErrNoRows) {
		return domain.ResolvedEndpoint{}, fmt.Errorf("%w: running deployment", ErrNotFound)
	} else if err != nil {
		return domain.ResolvedEndpoint{}, err
	}

	var existingID string
	err = tx.QueryRowContext(ctx, `SELECT id FROM endpoints WHERE tenant_id=? AND name=? AND desired_state<>'deleted' FOR UPDATE`, tenant, endpointName).Scan(&existingID)
	if err == nil {
		if err = tx.Rollback(); err != nil {
			return domain.ResolvedEndpoint{}, err
		}
		return s.resolvePublishedEndpoint(ctx, tenant, endpointName, deploymentID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.ResolvedEndpoint{}, err
	}

	environmentID, err := newID()
	if err != nil {
		return domain.ResolvedEndpoint{}, err
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `INSERT INTO environments(id,tenant_id,name,policy_json,created_at,updated_at) VALUES(?,?,?,'{}'::jsonb,?,?) ON CONFLICT(tenant_id,name) DO NOTHING`, environmentID, tenant, "production", stamp, stamp); err != nil {
		return domain.ResolvedEndpoint{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT id FROM environments WHERE tenant_id=? AND name='production'`, tenant).Scan(&environmentID); err != nil {
		return domain.ResolvedEndpoint{}, err
	}

	logicalModelID, err := newID()
	if err != nil {
		return domain.ResolvedEndpoint{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO logical_models(id,tenant_id,name,description,created_at,updated_at) VALUES(?,?,?,?,?,?) ON CONFLICT(tenant_id,name) DO NOTHING`, logicalModelID, tenant, endpointName, "Published deployment endpoint", stamp, stamp); err != nil {
		return domain.ResolvedEndpoint{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT id FROM logical_models WHERE tenant_id=? AND name=?`, tenant, endpointName).Scan(&logicalModelID); err != nil {
		return domain.ResolvedEndpoint{}, err
	}

	endpointID, endpointErr := newID()
	if endpointErr != nil {
		return domain.ResolvedEndpoint{}, endpointErr
	}
	bindingID, bindingErr := newID()
	if bindingErr != nil {
		return domain.ResolvedEndpoint{}, bindingErr
	}
	planID, planErr := newID()
	if planErr != nil {
		return domain.ResolvedEndpoint{}, planErr
	}
	_, planBody, planErr := normalizePlan("manual", []domain.ServingPlanBinding{{BindingID: bindingID, Priority: 0, Weight: 100}})
	if planErr != nil {
		return domain.ResolvedEndpoint{}, planErr
	}
	digest := sha256.Sum256(planBody)
	planDigest := "sha256:" + hex.EncodeToString(digest[:])
	if _, err = tx.ExecContext(ctx, `INSERT INTO endpoints(id,tenant_id,logical_model_id,environment_id,name,desired_state,observed_state,created_at,updated_at) VALUES(?,?,?,?,?,'serving','pending',?,?)`, endpointID, tenant, logicalModelID, environmentID, endpointName, stamp, stamp); err != nil {
		if isUniqueViolation(err) {
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				return domain.ResolvedEndpoint{}, rollbackErr
			}
			return s.resolvePublishedEndpoint(ctx, tenant, endpointName, deploymentID)
		}
		return domain.ResolvedEndpoint{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO backend_bindings(id,tenant_id,endpoint_id,name,kind,ownership_mode,deployment_id,config_json,created_at,updated_at) VALUES(?,?,?,'primary','deployment','lifecycle-managed',?,'{}'::jsonb,?,?)`, bindingID, tenant, endpointID, deploymentID, stamp, stamp); err != nil {
		return domain.ResolvedEndpoint{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO serving_plans(id,tenant_id,endpoint_id,version,routing_policy,spec_json,spec_digest,created_at) VALUES(?,?,?,1,'manual',?::jsonb,?,?)`, planID, tenant, endpointID, string(planBody), planDigest, stamp); err != nil {
		return domain.ResolvedEndpoint{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO serving_plan_bindings(serving_plan_id,binding_id,priority,weight) VALUES(?,?,0,100)`, planID, bindingID); err != nil {
		return domain.ResolvedEndpoint{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE endpoints SET active_serving_plan_id=? WHERE id=? AND tenant_id=?`, planID, endpointID, tenant); err != nil {
		return domain.ResolvedEndpoint{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ResolvedEndpoint{}, err
	}
	return s.ResolveEndpointForTenant(ctx, tenant, endpointName)
}

func (s *Store) resolvePublishedEndpoint(ctx context.Context, tenant, endpointName, deploymentID string) (domain.ResolvedEndpoint, error) {
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpointName)
	if err != nil {
		return resolved, err
	}
	if resolved.ActivePlan.RoutingPolicy != "manual" || len(resolved.ActivePlan.Bindings) != 1 || len(resolved.Bindings) != 1 {
		return domain.ResolvedEndpoint{}, fmt.Errorf("%w: endpoint alias already belongs to another serving plan", ErrConflict)
	}
	planBinding := resolved.ActivePlan.Bindings[0]
	binding := resolved.Bindings[0]
	if planBinding.BindingID != binding.ID || binding.Kind != "deployment" || binding.OwnershipMode != "lifecycle-managed" || binding.DeploymentID != deploymentID {
		return domain.ResolvedEndpoint{}, fmt.Errorf("%w: endpoint alias already belongs to another serving plan", ErrConflict)
	}
	return resolved, nil
}
