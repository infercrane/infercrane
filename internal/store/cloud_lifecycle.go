package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
)

// SubmitCloudDeployment atomically persists desired deployment state and its
// lifecycle operation. A control-plane crash can therefore never leave a
// cloud deployment without durable work queued to realize it.
func (s *Store) SubmitCloudDeployment(ctx context.Context, deployment domain.Deployment, operation domain.Operation) (domain.Deployment, domain.Operation, bool, error) {
	if deployment.Name == "" || deployment.Model == "" {
		return domain.Deployment{}, domain.Operation{}, false, errors.New("deployment name and model are required")
	}
	if deployment.MinReplicas < 1 || deployment.MaxReplicas < deployment.MinReplicas {
		return domain.Deployment{}, domain.Operation{}, false, errors.New("replica bounds must satisfy 1 <= min <= max")
	}
	if operation.Kind == "" || operation.IdempotencyKey == "" {
		return domain.Deployment{}, domain.Operation{}, false, errors.New("operation kind and idempotency key are required")
	}
	if operation.TenantID == "" {
		operation.TenantID = "global"
	}
	if deployment.TenantID == "" {
		deployment.TenantID = operation.TenantID
	}
	if deployment.TenantID != operation.TenantID {
		return domain.Deployment{}, domain.Operation{}, false, errors.New("deployment and operation tenant must match")
	}

	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	existingOperation, lookupErr := operationByKeyQuery(ctx, tx, operation.TenantID, operation.Kind, operation.IdempotencyKey)
	if lookupErr == nil {
		existingDeployment, deploymentErr := deploymentByNameQuery(ctx, tx, deployment.TenantID, deployment.Name)
		return existingDeployment, existingOperation, false, deploymentErr
	}
	if !errors.Is(lookupErr, ErrNotFound) {
		return domain.Deployment{}, domain.Operation{}, false, lookupErr
	}

	if existing, existingErr := deploymentByNameQuery(ctx, tx, deployment.TenantID, deployment.Name); existingErr == nil {
		return domain.Deployment{}, domain.Operation{}, false, fmt.Errorf("%w: deployment %s already exists", ErrConflict, existing.Name)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return domain.Deployment{}, domain.Operation{}, false, existingErr
	}
	if err = enforceDeploymentQuota(ctx, tx, deployment.TenantID, "", deployment.MaxReplicas, true); err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}

	deployment.ID, err = newID()
	if err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	deployment.Runtime = "vllm"
	if deployment.RoutingStrategy == "" {
		deployment.RoutingStrategy = "round-robin"
	}
	deployment.DesiredState, deployment.ObservedState = "running", "pending"
	stamp := now()
	deployment.CreatedAt, deployment.UpdatedAt = parseTime(stamp), parseTime(stamp)
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployments(id,name,model,runtime,routing_strategy,desired_state,observed_state,min_replicas,max_replicas,autoscaling_enabled,tenant_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, deployment.ID, deployment.Name, deployment.Model, deployment.Runtime, deployment.RoutingStrategy, deployment.DesiredState, deployment.ObservedState, deployment.MinReplicas, deployment.MaxReplicas, deployment.AutoscalingEnabled, deployment.TenantID, stamp, stamp); err != nil {
		if isUniqueViolation(err) {
			return domain.Deployment{}, domain.Operation{}, false, fmt.Errorf("%w: deployment already exists", ErrConflict)
		}
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	eventID, err := newID()
	if err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_events(id,deployment_id,event_type,summary,payload_json,created_at) VALUES(?,?,?,?,?::jsonb,?)`, eventID, deployment.ID, "deployment_submitted", "Cloud deployment submitted", "{}", stamp); err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}

	operation.ID, err = newID()
	if err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	operation.ResourceType, operation.ResourceName = "deployment", deployment.Name
	operation.Status, operation.Progress, operation.Attempt = "pending", 0, 0
	if operation.MaxAttempts == 0 {
		operation.MaxAttempts = 120
	}
	if operation.RequestJSON == "" {
		operation.RequestJSON = "{}"
	}
	operation.CreatedAt, operation.UpdatedAt = parseTime(stamp), parseTime(stamp)
	if _, err = tx.ExecContext(ctx, `INSERT INTO operations(id,tenant_id,kind,resource_type,resource_name,idempotency_key,status,progress,message,request_json,result_json,attempt,max_attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,'{}'::jsonb,?,?,?,?,?)`, operation.ID, operation.TenantID, operation.Kind, operation.ResourceType, operation.ResourceName, operation.IdempotencyKey, operation.Status, 0, "queued", operation.RequestJSON, 0, operation.MaxAttempts, stamp, stamp, stamp); err != nil {
		if isUniqueViolation(err) {
			return domain.Deployment{}, domain.Operation{}, false, fmt.Errorf("%w: lifecycle operation already exists", ErrConflict)
		}
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	return deployment, operation, true, nil
}

func operationByKeyQuery(ctx context.Context, tx *tx, tenant, kind, key string) (domain.Operation, error) {
	var out domain.Operation
	var created, updated string
	var completed, leaseExpires, nextAttempt sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE tenant_id=? AND kind=? AND idempotency_key=?`, tenant, kind, key).Scan(&out.ID, &out.TenantID, &out.Kind, &out.ResourceType, &out.ResourceName, &out.IdempotencyKey, &out.Status, &out.Progress, &out.Message, &out.RequestJSON, &out.ResultJSON, &out.ErrorCode, &out.Retryable, &out.CancelRequested, &out.Attempt, &out.MaxAttempts, &out.LeaseOwner, &out.LeaseGeneration, &created, &updated, &completed, &leaseExpires, &nextAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	return out, nil
}

func deploymentByNameQuery(ctx context.Context, tx *tx, tenant, name string) (domain.Deployment, error) {
	var out domain.Deployment
	var created, updated string
	err := tx.QueryRowContext(ctx, `SELECT id,tenant_id,name,model,runtime,routing_strategy,desired_state,observed_state,min_replicas,max_replicas,autoscaling_enabled,created_at,updated_at FROM deployments WHERE tenant_id=? AND name=?`, tenant, name).Scan(&out.ID, &out.TenantID, &out.Name, &out.Model, &out.Runtime, &out.RoutingStrategy, &out.DesiredState, &out.ObservedState, &out.MinReplicas, &out.MaxReplicas, &out.AutoscalingEnabled, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	return out, nil
}
