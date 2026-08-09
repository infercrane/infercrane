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
	serverless := operation.Kind == "deployment.serverless.converge"
	if (!serverless && deployment.MinReplicas < 1) || (serverless && deployment.MinReplicas != 0) || deployment.MaxReplicas < 1 || deployment.MaxReplicas < deployment.MinReplicas {
		return domain.Deployment{}, domain.Operation{}, false, errors.New("replica bounds are invalid for compute mode")
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
	if deployment.Runtime == "" {
		deployment.Runtime = "vllm"
	}
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO scaling_policies(deployment_id,enabled,min_replicas,max_replicas,queue_threshold,low_load_threshold,scale_up_intervals,scale_down_intervals,cooldown_seconds,updated_at) VALUES(?,?,?,?,1,0,2,6,60,?)`, deployment.ID, deployment.AutoscalingEnabled, deployment.MinReplicas, deployment.MaxReplicas, stamp); err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO autoscaling_state(deployment_id,consecutive_high,consecutive_low,desired_replicas,updated_at) VALUES(?,0,0,?,?)`, deployment.ID, deployment.MinReplicas, stamp); err != nil {
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
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET spec_json=spec_json||jsonb_strip_nulls(jsonb_build_object('compute_mode',COALESCE(NULLIF(?::jsonb->>'compute_mode',''),'elastic'),'cloud',NULLIF(?::jsonb->>'cloud',''),'gpu',NULLIF(?::jsonb->>'gpu',''),'region',NULLIF(?::jsonb->>'region',''),'runtime_version',NULLIF(?::jsonb->>'runtime_version',''),'runtime_args',?::jsonb->'runtime_args','model_revision',NULLIF(?::jsonb->>'model_revision',''),'port',NULLIF(?::jsonb->>'port','')::integer)) WHERE id=?`, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, deployment.ID+"-rev-1"); err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Deployment{}, domain.Operation{}, false, err
	}
	return deployment, operation, true, nil
}

// SubmitDeploymentDelete atomically withdraws desired routing state and queues
// provider cleanup. A failed enqueue rolls back the state change.
func (s *Store) SubmitDeploymentDelete(ctx context.Context, tenant, name, deploymentID string, operation domain.Operation) (domain.Operation, bool, error) {
	if tenant == "" || name == "" || deploymentID == "" || operation.Kind == "" || operation.IdempotencyKey == "" {
		return domain.Operation{}, false, errors.New("tenant, deployment identity, operation kind, and idempotency key are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.Operation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if existing, lookupErr := operationByKeyQuery(ctx, tx, tenant, operation.Kind, operation.IdempotencyKey); lookupErr == nil {
		return existing, false, nil
	} else if !errors.Is(lookupErr, ErrNotFound) {
		return domain.Operation{}, false, lookupErr
	}
	var currentID, desired string
	if err = tx.QueryRowContext(ctx, `SELECT id,desired_state FROM deployments WHERE tenant_id=? AND name=? FOR UPDATE`, tenant, name).Scan(&currentID, &desired); errors.Is(err, sql.ErrNoRows) {
		return domain.Operation{}, false, ErrNotFound
	} else if err != nil {
		return domain.Operation{}, false, err
	}
	if currentID != deploymentID {
		return domain.Operation{}, false, fmt.Errorf("%w: deployment identity changed", ErrConflict)
	}
	if desired == "deleted" {
		return domain.Operation{}, false, fmt.Errorf("%w: deployment deletion is already in progress", ErrConflict)
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `UPDATE deployments SET desired_state='deleted',observed_state='deleting',updated_at=? WHERE id=?`, stamp, deploymentID); err != nil {
		return domain.Operation{}, false, err
	}
	operation.ID, err = newID()
	if err != nil {
		return domain.Operation{}, false, err
	}
	operation.TenantID, operation.ResourceType, operation.ResourceName = tenant, "deployment", name
	operation.Status = "pending"
	if operation.MaxAttempts == 0 {
		operation.MaxAttempts = 120
	}
	if operation.RequestJSON == "" {
		operation.RequestJSON = "{}"
	}
	operation.CreatedAt, operation.UpdatedAt = parseTime(stamp), parseTime(stamp)
	if _, err = tx.ExecContext(ctx, `INSERT INTO operations(id,tenant_id,kind,resource_type,resource_name,idempotency_key,status,progress,message,request_json,result_json,attempt,max_attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,'{}'::jsonb,?,?,?,?,?)`, operation.ID, tenant, operation.Kind, operation.ResourceType, operation.ResourceName, operation.IdempotencyKey, operation.Status, 0, "queued", operation.RequestJSON, 0, operation.MaxAttempts, stamp, stamp, stamp); err != nil {
		if isUniqueViolation(err) {
			return domain.Operation{}, false, fmt.Errorf("%w: deployment already has an unresolved lifecycle operation", ErrConflict)
		}
		return domain.Operation{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Operation{}, false, err
	}
	return operation, true, nil
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
	err := tx.QueryRowContext(ctx, `SELECT id,tenant_id,name,model,runtime,routing_strategy,desired_state,observed_state,min_replicas,max_replicas,autoscaling_enabled,COALESCE(active_revision_id,''),COALESCE(candidate_revision_id,''),created_at,updated_at FROM deployments WHERE tenant_id=? AND name=?`, tenant, name).Scan(&out.ID, &out.TenantID, &out.Name, &out.Model, &out.Runtime, &out.RoutingStrategy, &out.DesiredState, &out.ObservedState, &out.MinReplicas, &out.MaxReplicas, &out.AutoscalingEnabled, &out.ActiveRevisionID, &out.CandidateRevisionID, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	return out, nil
}
