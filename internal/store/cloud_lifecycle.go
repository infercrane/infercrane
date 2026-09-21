package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// SubmitCloudDeployment atomically persists desired deployment state and its
// lifecycle operation. A control-plane crash can therefore never leave a
// cloud deployment without durable work queued to realize it.
func (s *Store) SubmitCloudDeployment(ctx context.Context, deployment domain.Deployment, operation domain.Operation) (domain.Deployment, domain.Operation, bool, error) {
	deployed, queued, _, created, err := s.submitCloudDeployment(ctx, deployment, operation, nil)
	return deployed, queued, created, err
}

// SubmitManagedCloudDeployment adds an InferCrane-owned prepaid hold to the
// same transaction that persists the deployment and lifecycle operation.
// Provider work can therefore never be queued without spending authority.
func (s *Store) SubmitManagedCloudDeployment(ctx context.Context, deployment domain.Deployment, operation domain.Operation, reservation domain.ManagedSpendReservation) (domain.Deployment, domain.Operation, domain.ManagedSpendReservation, bool, error) {
	if reservation.Provider == "" || reservation.SupplierHourlyMicrousd < 1 || reservation.RetailHourlyMicrousd < reservation.SupplierHourlyMicrousd || reservation.ReservedMicrousd < 1 || reservation.GrossMarginBPS < 0 || reservation.GrossMarginBPS >= 10_000 || reservation.RuntimeLimitSeconds < 1 || reservation.CleanupAllowanceSeconds < 0 || len(reservation.PricingJSON) == 0 || len(reservation.PricingJSON) > 64<<10 || !json.Valid([]byte(reservation.PricingJSON)) {
		return domain.Deployment{}, domain.Operation{}, domain.ManagedSpendReservation{}, false, errors.New("managed deployment reservation is invalid")
	}
	return s.submitCloudDeployment(ctx, deployment, operation, &reservation)
}

func (s *Store) submitCloudDeployment(ctx context.Context, deployment domain.Deployment, operation domain.Operation, managed *domain.ManagedSpendReservation) (domain.Deployment, domain.Operation, domain.ManagedSpendReservation, bool, error) {
	emptyReservation := domain.ManagedSpendReservation{}
	if deployment.Name == "" || deployment.Model == "" {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, errors.New("deployment name and model are required")
	}
	serverless := operation.Kind == "deployment.serverless.converge"
	if (!serverless && deployment.MinReplicas < 1) || (serverless && deployment.MinReplicas != 0) || deployment.MaxReplicas < 1 || deployment.MaxReplicas < deployment.MinReplicas {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, errors.New("replica bounds are invalid for compute mode")
	}
	if operation.Kind == "" || operation.IdempotencyKey == "" {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, errors.New("operation kind and idempotency key are required")
	}
	if operation.TenantID == "" {
		operation.TenantID = "global"
	}
	if deployment.TenantID == "" {
		deployment.TenantID = operation.TenantID
	}
	if deployment.TenantID != operation.TenantID {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, errors.New("deployment and operation tenant must match")
	}
	operation.ResourceType, operation.ResourceName = "deployment", deployment.Name
	if operation.RequestJSON == "" {
		operation.RequestJSON = "{}"
	}

	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}
	defer func() { _ = tx.Rollback() }()

	existingOperation, lookupErr := operationByKeyQuery(ctx, tx, operation.TenantID, operation.Kind, operation.IdempotencyKey)
	if lookupErr == nil {
		existingDeployment, deploymentErr := deploymentByNameQuery(ctx, tx, deployment.TenantID, deployment.Name)
		if deploymentErr == nil && (!sameOperationIntent(existingOperation, operation) || !sameDeploymentSubmission(existingDeployment, deployment)) {
			return domain.Deployment{}, domain.Operation{}, emptyReservation, false, fmt.Errorf("%w: idempotency key was already used for a different deployment intent", ErrConflict)
		}
		if deploymentErr != nil || managed == nil {
			return existingDeployment, existingOperation, emptyReservation, false, deploymentErr
		}
		existingReservation, reservationErr := managedSpendReservationByResourceTx(ctx, tx, deployment.TenantID, "deployment", deployment.Name, false)
		return existingDeployment, existingOperation, existingReservation, false, reservationErr
	}
	if !errors.Is(lookupErr, ErrNotFound) {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, lookupErr
	}

	if existing, existingErr := deploymentByNameQuery(ctx, tx, deployment.TenantID, deployment.Name); existingErr == nil {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, fmt.Errorf("%w: deployment %s already exists", ErrConflict, existing.Name)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, existingErr
	}
	if err = enforceDeploymentQuota(ctx, tx, deployment.TenantID, "", deployment.MaxReplicas, true); err != nil {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}

	deployment.ID, err = newID()
	if err != nil {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
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
	createdReservation := emptyReservation
	if managed != nil {
		createdReservation, err = reserveManagedDeploymentTx(ctx, tx, deployment.TenantID, deployment.Name, stamp, *managed)
		if err != nil {
			return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployments(id,name,model,runtime,routing_strategy,desired_state,observed_state,min_replicas,max_replicas,autoscaling_enabled,tenant_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, deployment.ID, deployment.Name, deployment.Model, deployment.Runtime, deployment.RoutingStrategy, deployment.DesiredState, deployment.ObservedState, deployment.MinReplicas, deployment.MaxReplicas, deployment.AutoscalingEnabled, deployment.TenantID, stamp, stamp); err != nil {
		if isUniqueViolation(err) {
			return domain.Deployment{}, domain.Operation{}, emptyReservation, false, fmt.Errorf("%w: deployment already exists", ErrConflict)
		}
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO scaling_policies(deployment_id,enabled,min_replicas,max_replicas,queue_threshold,low_load_threshold,scale_up_intervals,scale_down_intervals,cooldown_seconds,updated_at) VALUES(?,?,?,?,1,0,2,6,60,?)`, deployment.ID, deployment.AutoscalingEnabled, deployment.MinReplicas, deployment.MaxReplicas, stamp); err != nil {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO autoscaling_state(deployment_id,consecutive_high,consecutive_low,desired_replicas,updated_at) VALUES(?,0,0,?,?)`, deployment.ID, deployment.MinReplicas, stamp); err != nil {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}
	eventID, err := newID()
	if err != nil {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO deployment_events(id,deployment_id,event_type,summary,payload_json,created_at) VALUES(?,?,?,?,?::jsonb,?)`, eventID, deployment.ID, "deployment_submitted", "Cloud deployment submitted", "{}", stamp); err != nil {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}

	operation.ID, err = newID()
	if err != nil {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}
	operation.Status, operation.Progress, operation.Attempt = "pending", 0, 0
	if operation.MaxAttempts == 0 {
		operation.MaxAttempts = 120
	}
	operation.CreatedAt, operation.UpdatedAt = parseTime(stamp), parseTime(stamp)
	if _, err = tx.ExecContext(ctx, `INSERT INTO operations(id,tenant_id,kind,resource_type,resource_name,idempotency_key,status,progress,message,request_json,result_json,attempt,max_attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,'{}'::jsonb,?,?,NOW(),NOW(),NOW())`, operation.ID, operation.TenantID, operation.Kind, operation.ResourceType, operation.ResourceName, operation.IdempotencyKey, operation.Status, 0, "queued", operation.RequestJSON, 0, operation.MaxAttempts); err != nil {
		if isUniqueViolation(err) {
			return domain.Deployment{}, domain.Operation{}, emptyReservation, false, fmt.Errorf("%w: lifecycle operation already exists", ErrConflict)
		}
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET spec_json=spec_json||jsonb_strip_nulls(jsonb_build_object('compute_mode',COALESCE(NULLIF(?::jsonb->>'compute_mode',''),'elastic'),'cloud',NULLIF(?::jsonb->>'cloud',''),'provider_adapter',NULLIF(?::jsonb->>'provider_adapter',''),'gpu',NULLIF(?::jsonb->>'gpu',''),'gpu_count',COALESCE(NULLIF(?::jsonb->>'gpu_count','')::integer,1),'region',NULLIF(?::jsonb->>'region',''),'runtime_version',NULLIF(?::jsonb->>'runtime_version',''),'runtime_args',?::jsonb->'runtime_args','model_revision',NULLIF(?::jsonb->>'model_revision',''),'model_secret_reference_id',NULLIF(?::jsonb->>'model_secret_reference_id',''),'port',NULLIF(?::jsonb->>'port','')::integer,'workload',?::jsonb->'workload','serving',?::jsonb->'serving')) WHERE id=?`, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, deployment.ID+"-rev-1"); err != nil {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}
	return deployment, operation, createdReservation, true, nil
}

func reserveManagedDeploymentTx(ctx context.Context, tx *tx, tenant, name, stamp string, reservation domain.ManagedSpendReservation) (domain.ManagedSpendReservation, error) {
	var balance, reserved, debt int64
	err := tx.QueryRowContext(ctx, `SELECT balance_microusd,reserved_microusd,debt_microusd FROM managed_wallets WHERE tenant_id=? FOR UPDATE`, tenant).Scan(&balance, &reserved, &debt)
	if errors.Is(err, sql.ErrNoRows) || balance-reserved-debt < reservation.ReservedMicrousd {
		return domain.ManagedSpendReservation{}, domain.ErrInsufficientCredits
	}
	if err != nil {
		return domain.ManagedSpendReservation{}, err
	}
	reservation.ID, err = newID()
	if err != nil {
		return domain.ManagedSpendReservation{}, err
	}
	reservation.TenantID, reservation.ResourceType, reservation.ResourceName = tenant, "deployment", name
	reservation.State, reservation.Currency = "reserved", "USD"
	reservation.ExpiresAt = parseTime(stamp).Add(time.Duration(reservation.RuntimeLimitSeconds) * time.Second)
	reservation.CreatedAt, reservation.UpdatedAt = parseTime(stamp), parseTime(stamp)
	if _, err = tx.ExecContext(ctx, `INSERT INTO managed_spend_reservations(id,tenant_id,resource_type,resource_name,provider,state,currency,supplier_hourly_microusd,retail_hourly_microusd,reserved_microusd,gross_margin_bps,runtime_limit_seconds,cleanup_allowance_seconds,pricing_json,expires_at,created_at,updated_at) VALUES(?,?, 'deployment', ?,?,'reserved','USD',?,?,?,?,?,?,?::jsonb,?,?,?)`, reservation.ID, tenant, name, reservation.Provider, reservation.SupplierHourlyMicrousd, reservation.RetailHourlyMicrousd, reservation.ReservedMicrousd, reservation.GrossMarginBPS, reservation.RuntimeLimitSeconds, reservation.CleanupAllowanceSeconds, reservation.PricingJSON, reservation.ExpiresAt.UTC(), stamp, stamp); err != nil {
		return domain.ManagedSpendReservation{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE managed_wallets SET reserved_microusd=reserved_microusd+?,updated_at=? WHERE tenant_id=?`, reservation.ReservedMicrousd, stamp, tenant); err != nil {
		return domain.ManagedSpendReservation{}, err
	}
	return reservation, nil
}

// SubmitDeploymentDelete atomically withdraws desired routing state and queues
// provider cleanup. A failed enqueue rolls back the state change.
func (s *Store) SubmitDeploymentDelete(ctx context.Context, tenant, name, deploymentID string, operation domain.Operation) (domain.Operation, bool, error) {
	if tenant == "" || name == "" || deploymentID == "" || operation.Kind == "" || operation.IdempotencyKey == "" {
		return domain.Operation{}, false, errors.New("tenant, deployment identity, operation kind, and idempotency key are required")
	}
	operation.TenantID, operation.ResourceType, operation.ResourceName = tenant, "deployment", name
	if operation.RequestJSON == "" {
		operation.RequestJSON = "{}"
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.Operation{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if existing, lookupErr := operationByKeyQuery(ctx, tx, tenant, operation.Kind, operation.IdempotencyKey); lookupErr == nil {
		if !sameOperationIntent(existing, operation) {
			return domain.Operation{}, false, fmt.Errorf("%w: idempotency key was already used for a different deletion intent", ErrConflict)
		}
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
	operation.Status = "pending"
	if operation.MaxAttempts == 0 {
		operation.MaxAttempts = 120
	}
	operation.CreatedAt, operation.UpdatedAt = parseTime(stamp), parseTime(stamp)
	if _, err = tx.ExecContext(ctx, `INSERT INTO operations(id,tenant_id,kind,resource_type,resource_name,idempotency_key,status,progress,message,request_json,result_json,attempt,max_attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,'{}'::jsonb,?,?,NOW(),NOW(),NOW())`, operation.ID, tenant, operation.Kind, operation.ResourceType, operation.ResourceName, operation.IdempotencyKey, operation.Status, 0, "queued", operation.RequestJSON, 0, operation.MaxAttempts); err != nil {
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

func sameDeploymentSubmission(existing, requested domain.Deployment) bool {
	runtime := requested.Runtime
	if runtime == "" {
		runtime = "vllm"
	}
	routing := requested.RoutingStrategy
	if routing == "" {
		routing = "round-robin"
	}
	return existing.TenantID == requested.TenantID && existing.Name == requested.Name && existing.Model == requested.Model && existing.Runtime == runtime && existing.RoutingStrategy == routing && existing.MinReplicas == requested.MinReplicas && existing.MaxReplicas == requested.MaxReplicas && existing.AutoscalingEnabled == requested.AutoscalingEnabled
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
