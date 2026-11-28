package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
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
		existingDeployment, deploymentErr := latestDeploymentByNameQuery(ctx, tx, deployment.TenantID, deployment.Name)
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
		if existing.DesiredState == "deleted" {
			return domain.Deployment{}, domain.Operation{}, emptyReservation, false, fmt.Errorf("%w: deployment %s is still being deleted; deploy it again after cleanup finishes", ErrConflict, existing.Name)
		}
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
			return domain.Deployment{}, domain.Operation{}, emptyReservation, false, deploymentNameConflict(err, deployment.Name)
		}
		return domain.Deployment{}, domain.Operation{}, emptyReservation, false, err
	}
	// The deployment insert trigger creates and activates the initial revision.
	// Keep the returned object aligned with the durable row so callers can bind
	// evidence and follow-up operations to the exact revision immediately.
	deployment.ActiveRevisionID = deployment.ID + "-rev-1"
	if err = bindComputeConnectionTx(ctx, tx, deployment.TenantID, deployment.ID, deployment.ComputeConnectionID, stamp); err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.Deployment{}, domain.Operation{}, emptyReservation, false, fmt.Errorf("%w: verified compute connection was not found", ErrConflict)
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
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET spec_json=spec_json||jsonb_strip_nulls(jsonb_build_object('compute_mode',COALESCE(NULLIF(?::jsonb->>'compute_mode',''),'elastic'),'cloud',NULLIF(?::jsonb->>'cloud',''),'provider_adapter',NULLIF(?::jsonb->>'provider_adapter',''),'compute_connection_id',NULLIF(?::jsonb->>'compute_connection_id',''),'gpu',NULLIF(?::jsonb->>'gpu',''),'gpu_count',COALESCE(NULLIF(?::jsonb->>'gpu_count','')::integer,1),'region',NULLIF(?::jsonb->>'region',''),'runtime_version',NULLIF(?::jsonb->>'runtime_version',''),'runtime_args',?::jsonb->'runtime_args','runtime_env',?::jsonb->'runtime_env','model_revision',NULLIF(?::jsonb->>'model_revision',''),'model_secret_reference_id',NULLIF(?::jsonb->>'model_secret_reference_id',''),'model_adapters',?::jsonb->'model_adapters','port',NULLIF(?::jsonb->>'port','')::integer,'workload',?::jsonb->'workload','serving',?::jsonb->'serving')) WHERE id=?`, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, operation.RequestJSON, deployment.ActiveRevisionID); err != nil {
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

// OpenOperationError refuses a deletion because the deployment has an
// unfinished operation the deletion does not cancel by itself: one of
// another kind, or one a worker holds the lease of right now.
type OpenOperationError struct {
	ID, Kind, Status string
	// Running reports that a worker holds the operation's lease, so it may
	// be calling the provider at this moment.
	Running bool
}

func (e *OpenOperationError) Error() string {
	return fmt.Sprintf("%v: deployment already has an unresolved lifecycle operation %s (%s, %s)", ErrConflict, e.ID, e.Kind, e.Status)
}

func (e *OpenOperationError) Unwrap() error { return ErrConflict }

// DeploymentDeletion is a submitted deletion and, when it cancelled one
// first, the deployment's unfinished operation.
type DeploymentDeletion struct {
	Operation domain.Operation
	Created   bool
	Cancelled *domain.Operation
}

// SubmitDeploymentDelete atomically withdraws desired routing state and queues
// provider cleanup. A failed enqueue rolls back the state change.
func (s *Store) SubmitDeploymentDelete(ctx context.Context, tenant, name, deploymentID string, operation domain.Operation) (domain.Operation, bool, error) {
	deletion, err := s.SubmitDeploymentDeleteCancelling(ctx, tenant, name, deploymentID, operation, nil)
	return deletion.Operation, deletion.Created, err
}

// SubmitDeploymentDeleteCancelling is SubmitDeploymentDelete that first
// cancels the deployment's unfinished operation when its kind is one of
// cancelKinds and no worker holds its lease. The cancellation is recorded
// as an operation cancel records one that no worker has to run, and the
// queued deletion's cleanup then removes whatever the cancelled operation
// created, in the same transaction. Any other unfinished operation refuses
// the deletion with an *OpenOperationError and changes nothing.
func (s *Store) SubmitDeploymentDeleteCancelling(ctx context.Context, tenant, name, deploymentID string, operation domain.Operation, cancelKinds []string) (DeploymentDeletion, error) {
	if tenant == "" || name == "" || deploymentID == "" || operation.Kind == "" || operation.IdempotencyKey == "" {
		return DeploymentDeletion{}, errors.New("tenant, deployment identity, operation kind, and idempotency key are required")
	}
	operation.TenantID, operation.ResourceType, operation.ResourceName = tenant, "deployment", name
	if operation.RequestJSON == "" {
		operation.RequestJSON = "{}"
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return DeploymentDeletion{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if existing, lookupErr := operationByKeyQuery(ctx, tx, tenant, operation.Kind, operation.IdempotencyKey); lookupErr == nil {
		if !sameOperationIntent(existing, operation) {
			return DeploymentDeletion{}, fmt.Errorf("%w: idempotency key was already used for a different deletion intent", ErrConflict)
		}
		return DeploymentDeletion{Operation: existing}, nil
	} else if !errors.Is(lookupErr, ErrNotFound) {
		return DeploymentDeletion{}, lookupErr
	}
	var currentID, desired string
	if err = tx.QueryRowContext(ctx, `SELECT id,desired_state FROM deployments WHERE tenant_id=? AND name=? AND `+liveDeployment+` FOR UPDATE`, tenant, name).Scan(&currentID, &desired); errors.Is(err, sql.ErrNoRows) {
		return DeploymentDeletion{}, ErrNotFound
	} else if err != nil {
		return DeploymentDeletion{}, err
	}
	if currentID != deploymentID {
		return DeploymentDeletion{}, fmt.Errorf("%w: deployment identity changed", ErrConflict)
	}
	if desired == "deleted" {
		return DeploymentDeletion{}, fmt.Errorf("%w: deployment deletion is already in progress", ErrConflict)
	}
	cancelled, err := cancelOpenDeploymentOperationTx(ctx, s, tx, tenant, name, cancelKinds)
	if err != nil {
		return DeploymentDeletion{}, err
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `UPDATE deployments SET desired_state='deleted',observed_state='deleting',updated_at=? WHERE id=?`, stamp, deploymentID); err != nil {
		return DeploymentDeletion{}, err
	}
	operation.ID, err = newID()
	if err != nil {
		return DeploymentDeletion{}, err
	}
	operation.Status = "pending"
	if operation.MaxAttempts == 0 {
		operation.MaxAttempts = 120
	}
	operation.CreatedAt, operation.UpdatedAt = parseTime(stamp), parseTime(stamp)
	if _, err = tx.ExecContext(ctx, `INSERT INTO operations(id,tenant_id,kind,resource_type,resource_name,idempotency_key,status,progress,message,request_json,result_json,attempt,max_attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,'{}'::jsonb,?,?,NOW(),NOW(),NOW())`, operation.ID, tenant, operation.Kind, operation.ResourceType, operation.ResourceName, operation.IdempotencyKey, operation.Status, 0, "queued", operation.RequestJSON, 0, operation.MaxAttempts); err != nil {
		if isUniqueViolation(err) {
			return DeploymentDeletion{}, fmt.Errorf("%w: deployment already has an unresolved lifecycle operation", ErrConflict)
		}
		return DeploymentDeletion{}, err
	}
	if err = tx.Commit(); err != nil {
		return DeploymentDeletion{}, err
	}
	return DeploymentDeletion{Operation: operation, Created: true, Cancelled: cancelled}, nil
}

// cancelOpenDeploymentOperationTx finds the deployment's unfinished
// operation under the deployment row lock. It cancels one of cancelKinds
// that no worker holds, and refuses any other. A deployment has at most one
// (one_unresolved_deployment_operation).
func cancelOpenDeploymentOperationTx(ctx context.Context, s *Store, tx *tx, tenant, name string, cancelKinds []string) (*domain.Operation, error) {
	var id, kind, status string
	var leased bool
	err := tx.QueryRowContext(ctx, `SELECT id,kind,status,COALESCE(lease_expires_at>NOW(),FALSE) FROM operations WHERE tenant_id=? AND resource_type='deployment' AND resource_name=? AND status IN ('pending','leased','running','waiting','cancelling') FOR UPDATE`, tenant, name).Scan(&id, &kind, &status, &leased)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if refused := refuseOpenOperation(id, kind, status, leased, cancelKinds); refused != nil {
		return nil, refused
	}
	message := "cancelled by deployment delete"
	if status == "pending" {
		message = "cancelled before execution"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE operations SET cancel_requested=TRUE,status='cancelled',message=?,error_code=NULL,retryable=FALSE,waiting_reason=NULL,next_attempt_at=NULL,lease_owner=NULL,lease_expires_at=NULL,completed_at=NOW(),updated_at=NOW() WHERE id=?`, message, id); err != nil {
		return nil, err
	}
	cancelled, err := s.scanOperation(tx.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	return &cancelled, nil
}

// refuseOpenOperation decides whether a deletion may cancel an unfinished
// operation. It may when the kind is one of cancelKinds and no worker runs
// it: a pending or waiting operation holds no lease, and an expired lease is
// one any worker may claim. leased reports an unexpired lease.
func refuseOpenOperation(id, kind, status string, leased bool, cancelKinds []string) *OpenOperationError {
	running := leased && status != "pending" && status != "waiting"
	if running || !slices.Contains(cancelKinds, kind) {
		return &OpenOperationError{ID: id, Kind: kind, Status: status, Running: running}
	}
	return nil
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
	return existing.TenantID == requested.TenantID && existing.Name == requested.Name && existing.Model == requested.Model && existing.Runtime == runtime && existing.RoutingStrategy == routing && existing.ComputeConnectionID == requested.ComputeConnectionID && existing.MinReplicas == requested.MinReplicas && existing.MaxReplicas == requested.MaxReplicas && existing.AutoscalingEnabled == requested.AutoscalingEnabled
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

// liveDeployment and liveDeploymentD match every deployment that still owns
// its name: all rows except fully deleted ones, which stay only as history.
// The deployments_tenant_live_name_key index allows at most one such row per
// tenant and name, so a by-name lookup filtered with them is unambiguous.
const (
	liveDeployment  = `NOT (desired_state='deleted' AND observed_state='deleted')`
	liveDeploymentD = `NOT (d.desired_state='deleted' AND d.observed_state='deleted')`
)

// deploymentNameConflict explains a unique violation raised while inserting a
// deployment. The insert trigger also creates the deployment's endpoint, so
// the name can be taken by an endpoint as well as by another deployment.
func deploymentNameConflict(err error, name string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "endpoints_tenant_live_name_key" {
		return fmt.Errorf("%w: endpoint %s already exists; delete it or choose another deployment name", ErrConflict, name)
	}
	return fmt.Errorf("%w: deployment %s already exists", ErrConflict, name)
}

// deploymentByNameQuery returns the deployment that currently owns name.
func deploymentByNameQuery(ctx context.Context, tx *tx, tenant, name string) (domain.Deployment, error) {
	return scanDeploymentByName(ctx, tx, tenant, name, ` AND `+liveDeploymentD)
}

// latestDeploymentByNameQuery returns the deployment that owns name or, when
// every deployment with that name was deleted, the most recent one. It lets an
// idempotent retry return the deployment its submission created.
func latestDeploymentByNameQuery(ctx context.Context, tx *tx, tenant, name string) (domain.Deployment, error) {
	return scanDeploymentByName(ctx, tx, tenant, name, ` ORDER BY (`+liveDeploymentD+`) DESC,d.created_at DESC,d.id DESC LIMIT 1`)
}

func scanDeploymentByName(ctx context.Context, tx *tx, tenant, name, suffix string) (domain.Deployment, error) {
	var out domain.Deployment
	var created, updated string
	err := tx.QueryRowContext(ctx, `SELECT d.id,d.tenant_id,d.name,d.model,d.runtime,d.routing_strategy,d.desired_state,d.observed_state,d.min_replicas,d.max_replicas,d.autoscaling_enabled,COALESCE(d.active_revision_id,''),COALESCE(d.candidate_revision_id,''),COALESCE(dc.compute_connection_id,''),d.created_at,d.updated_at FROM deployments d LEFT JOIN deployment_compute_connections dc ON dc.tenant_id=d.tenant_id AND dc.deployment_id=d.id WHERE d.tenant_id=? AND d.name=?`+suffix, tenant, name).Scan(&out.ID, &out.TenantID, &out.Name, &out.Model, &out.Runtime, &out.RoutingStrategy, &out.DesiredState, &out.ObservedState, &out.MinReplicas, &out.MaxReplicas, &out.AutoscalingEnabled, &out.ActiveRevisionID, &out.CandidateRevisionID, &out.ComputeConnectionID, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	return out, nil
}
