package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
)

// ApplyDeployment converges an existing-target deployment to the requested
// model, routing policy, replica bounds, and target membership atomically.
func (s *Store) ApplyDeployment(ctx context.Context, deployment domain.Deployment, targetNames []string) (domain.Deployment, error) {
	return s.ApplyDeploymentForTenant(ctx, "global", deployment, targetNames)
}
func (s *Store) ApplyDeploymentForTenant(ctx context.Context, tenant string, deployment domain.Deployment, targetNames []string) (domain.Deployment, error) {
	if len(targetNames) == 0 {
		return domain.Deployment{}, errors.New("at least one target is required")
	}
	if deployment.Name == "" || deployment.Model == "" {
		return domain.Deployment{}, errors.New("deployment name and model are required")
	}
	if deployment.Runtime == "" {
		deployment.Runtime = "vllm"
	}
	if deployment.RoutingStrategy == "" {
		deployment.RoutingStrategy = "round-robin"
	}
	if _, ok := domain.RoutingStrategies[deployment.RoutingStrategy]; !ok {
		return domain.Deployment{}, fmt.Errorf("unsupported routing strategy %q", deployment.RoutingStrategy)
	}
	serverless := deployment.ComputeMode == "serverless"
	if deployment.MinReplicas == 0 && !serverless {
		deployment.MinReplicas = len(uniqueNames(targetNames))
	}
	if deployment.MaxReplicas == 0 {
		deployment.MaxReplicas = deployment.MinReplicas
	}
	if (!serverless && deployment.MinReplicas < 1) || (serverless && deployment.MinReplicas != 0) || deployment.MaxReplicas < 1 || deployment.MaxReplicas < deployment.MinReplicas {
		return domain.Deployment{}, errors.New("invalid replica bounds")
	}

	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.Deployment{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM deployments WHERE name=? AND tenant_id=? FOR UPDATE`, deployment.Name, tenant).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return s.CreateDeploymentForTenant(ctx, tenant, deployment, targetNames)
	}
	if err != nil {
		return domain.Deployment{}, err
	}
	if err = enforceDeploymentQuota(ctx, tx, tenant, id, deployment.MaxReplicas, false); err != nil {
		return domain.Deployment{}, err
	}
	targetIDs, err := validateTargetSet(ctx, tx, tenant, deployment, targetNames)
	if err != nil {
		return domain.Deployment{}, err
	}
	stamp := now()
	_, err = tx.ExecContext(ctx, `UPDATE deployments SET model=?,runtime=?,routing_strategy=?,desired_state='running',observed_state='pending',min_replicas=?,max_replicas=?,autoscaling_enabled=?,updated_at=? WHERE id=?`, deployment.Model, deployment.Runtime, deployment.RoutingStrategy, deployment.MinReplicas, deployment.MaxReplicas, deployment.AutoscalingEnabled, stamp, id)
	if err != nil {
		return domain.Deployment{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM deployment_targets WHERE deployment_id=?`, id); err != nil {
		return domain.Deployment{}, err
	}
	for _, targetID := range targetIDs {
		if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_targets(deployment_id,target_id) VALUES(?,?)`, id, targetID); err != nil {
			return domain.Deployment{}, err
		}
	}
	eventID, err := newID()
	if err != nil {
		return domain.Deployment{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_events(id,deployment_id,event_type,summary,payload_json,created_at) VALUES(?,?,?,?,?::jsonb,?)`, eventID, id, "deployment_applied", "Deployment "+deployment.Name+" converged", "{}", stamp); err != nil {
		return domain.Deployment{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Deployment{}, err
	}
	resolved, err := s.ResolveForTenant(ctx, tenant, deployment.Name)
	if err != nil {
		return domain.Deployment{}, err
	}
	return resolved.Deployment, nil
}

func validateTargetSet(ctx context.Context, tx *tx, tenant string, deployment domain.Deployment, names []string) ([]string, error) {
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(names))
	upstream := ""
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		var id, runtime string
		var targetModel sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT id,runtime,upstream_model_name FROM targets WHERE name=? AND tenant_id=?`, name, tenant).Scan(&id, &runtime, &targetModel)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w: target %s", ErrNotFound, name)
		}
		if err != nil {
			return nil, err
		}
		if runtime != deployment.Runtime {
			return nil, fmt.Errorf("target %s runtime mismatch", name)
		}
		model := deployment.Model
		if targetModel.Valid {
			model = targetModel.String
		}
		if upstream != "" && upstream != model {
			return nil, errors.New("all targets must expose one common upstream model")
		}
		upstream = model
		ids = append(ids, id)
	}
	return ids, nil
}

func uniqueNames(names []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			out = append(out, name)
		}
	}
	return out
}
