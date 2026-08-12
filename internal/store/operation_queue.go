package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) EnqueueOperation(ctx context.Context, operation domain.Operation) (domain.Operation, bool, error) {
	if operation.Kind == "" || operation.ResourceType == "" || operation.ResourceName == "" {
		return domain.Operation{}, false, errors.New("operation kind and resource are required")
	}
	if operation.TenantID == "" {
		operation.TenantID = "global"
	}
	if operation.RequestJSON == "" {
		operation.RequestJSON = "{}"
	}
	if operation.MaxAttempts == 0 {
		operation.MaxAttempts = 5
	}
	if operation.IdempotencyKey != "" {
		existing, err := s.operationByKey(ctx, operation.TenantID, operation.Kind, operation.IdempotencyKey)
		if err == nil {
			if !sameOperationIntent(existing, operation) {
				return domain.Operation{}, false, fmt.Errorf("%w: idempotency key was already used for a different operation intent", ErrConflict)
			}
			return existing, false, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return domain.Operation{}, false, err
		}
	}
	id, err := newID()
	if err != nil {
		return domain.Operation{}, false, err
	}
	stamp := now()
	operation.ID = id
	operation.Status = "pending"
	operation.Attempt = 0
	operation.CreatedAt = parseTime(stamp)
	operation.UpdatedAt = operation.CreatedAt
	// Immediate queue eligibility must be based on the same clock used by
	// ClaimOperation. Application and PostgreSQL clocks can differ slightly in
	// real deployments (and when PostgreSQL runs in a VM/container); persisting
	// the application timestamp as next_attempt_at can therefore make a newly
	// queued operation temporarily invisible to workers.
	_, err = s.ExecContext(ctx, `INSERT INTO operations(id,tenant_id,kind,resource_type,resource_name,idempotency_key,status,progress,message,request_json,result_json,attempt,max_attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,'{}'::jsonb,?,?,NOW(),NOW(),NOW())`, operation.ID, operation.TenantID, operation.Kind, operation.ResourceType, operation.ResourceName, null(operation.IdempotencyKey), operation.Status, 0, "queued", operation.RequestJSON, 0, operation.MaxAttempts)
	if isUniqueViolation(err) && operation.IdempotencyKey != "" {
		existing, lookupErr := s.operationByKey(ctx, operation.TenantID, operation.Kind, operation.IdempotencyKey)
		if lookupErr == nil {
			if !sameOperationIntent(existing, operation) {
				return domain.Operation{}, false, fmt.Errorf("%w: idempotency key was concurrently used for a different operation intent", ErrConflict)
			}
			return existing, false, nil
		}
		if !errors.Is(lookupErr, ErrNotFound) {
			return domain.Operation{}, false, lookupErr
		}
		return domain.Operation{}, false, fmt.Errorf("%w: deployment already has an unresolved lifecycle operation", ErrConflict)
	}
	if isUniqueViolation(err) {
		return domain.Operation{}, false, fmt.Errorf("%w: deployment already has an unresolved lifecycle operation", ErrConflict)
	}
	return operation, err == nil, err
}

func sameOperationIntent(existing, requested domain.Operation) bool {
	return existing.TenantID == requested.TenantID && existing.Kind == requested.Kind && existing.ResourceType == requested.ResourceType && existing.ResourceName == requested.ResourceName && semanticJSONEqual(existing.RequestJSON, requested.RequestJSON)
}

func (s *Store) ClaimOperation(ctx context.Context, owner string, lease time.Duration) (domain.Operation, error) {
	if owner == "" || lease <= 0 {
		return domain.Operation{}, errors.New("owner and positive lease are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.Operation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id FROM operations WHERE ((((status IN ('pending','waiting') AND next_attempt_at<=NOW()) OR (status IN ('leased','running') AND lease_expires_at<NOW())) AND cancel_requested=FALSE) OR (status='cancelling' AND (lease_expires_at IS NULL OR lease_expires_at<NOW()))) AND attempt<max_attempts ORDER BY next_attempt_at,created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Operation{}, ErrNotFound
	}
	if err != nil {
		return domain.Operation{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE operations SET status='leased',attempt=attempt+1,lease_owner=?,lease_generation=lease_generation+1,lease_expires_at=NOW()+(? * INTERVAL '1 microsecond'),last_heartbeat_at=?,message='leased',updated_at=? WHERE id=?`, owner, lease.Microseconds(), now(), now(), id); err != nil {
		return domain.Operation{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Operation{}, err
	}
	return s.Operation(ctx, id)
}

func (s *Store) StartClaimedOperation(ctx context.Context, id, owner string, generation int64) error {
	return s.updateClaim(ctx, `UPDATE operations SET status='running',message='running',updated_at=? WHERE id=? AND status='leased' AND lease_owner=? AND lease_generation=? AND lease_expires_at>NOW()`, id, owner, generation)
}

func (s *Store) HeartbeatOperation(ctx context.Context, id, owner string, generation int64, lease time.Duration) error {
	result, err := s.ExecContext(ctx, `UPDATE operations SET lease_expires_at=NOW()+(? * INTERVAL '1 microsecond'),last_heartbeat_at=?,updated_at=? WHERE id=? AND status IN ('leased','running','cancelling') AND lease_owner=? AND lease_generation=? AND lease_expires_at>NOW()`, lease.Microseconds(), now(), now(), id, owner, generation)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CompleteClaimedOperation(ctx context.Context, id, owner string, generation int64, resultJSON string) error {
	if resultJSON == "" {
		resultJSON = "{}"
	}
	result, err := s.ExecContext(ctx, `UPDATE operations SET status='succeeded',progress=100,result_json=?::jsonb,message='completed',lease_owner=NULL,lease_expires_at=NULL,updated_at=?,completed_at=? WHERE id=? AND status='running' AND lease_owner=? AND lease_generation=? AND lease_expires_at>NOW()`, resultJSON, now(), now(), id, owner, generation)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) FailClaimedOperation(ctx context.Context, id, owner string, generation int64, code, message string, retryable bool, next time.Time) error {
	status := "failed"
	var completed any = now()
	if retryable {
		status = "waiting"
		completed = nil
	}
	delay := time.Until(next)
	if delay < 0 {
		delay = 0
	}
	result, err := s.ExecContext(ctx, `UPDATE operations SET status=CASE WHEN cancel_requested AND ? THEN 'cancelling' ELSE ? END,error_code=?,message=?,retryable=?,waiting_reason=?,next_attempt_at=NOW()+(? * INTERVAL '1 microsecond'),lease_owner=NULL,lease_expires_at=NULL,updated_at=NOW(),completed_at=? WHERE id=? AND status IN ('running','cancelling') AND lease_owner=? AND lease_generation=? AND lease_expires_at>NOW()`, retryable, status, code, message, retryable, nullIf(!retryable, message), delay.Microseconds(), completed, id, owner, generation)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CancelClaimedOperation(ctx context.Context, id, owner string, generation int64, message string) error {
	result, err := s.ExecContext(ctx, `UPDATE operations SET status='cancelled',message=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=?,completed_at=? WHERE id=? AND status IN ('running','cancelling') AND lease_owner=? AND lease_generation=? AND lease_expires_at>NOW()`, message, now(), now(), id, owner, generation)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CheckpointClaimedOperation(ctx context.Context, id, owner string, generation int64, step, status, checkpoint string, progress int, message string) error {
	if step == "" || progress < 0 || progress > 99 {
		return errors.New("step and progress between 0 and 99 are required")
	}
	switch status {
	case "pending", "running", "waiting", "succeeded", "failed", "cancelled":
	default:
		return fmt.Errorf("invalid operation step status %q", status)
	}
	if checkpoint == "" {
		checkpoint = "{}"
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// A retry replays completed workflow steps before it reaches the blocking
	// boundary. Keep the public operation cursor monotonic: replaying a 15%
	// identity checkpoint after a 55% capacity wait must not make clients think
	// the deployment moved backwards. Retain the message as well when the
	// replayed checkpoint is behind the durable high-water mark.
	result, err := tx.ExecContext(ctx, `UPDATE operations SET progress=GREATEST(progress,?),message=CASE WHEN ?>=progress THEN ? ELSE message END,updated_at=? WHERE id=? AND status IN ('running','cancelling') AND lease_owner=? AND lease_generation=? AND lease_expires_at>NOW()`, progress, progress, message, now(), id, owner, generation)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	stamp := now()
	var completed any
	if status == "succeeded" || status == "failed" || status == "cancelled" {
		completed = stamp
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO operation_steps(operation_id,step_name,status,attempt,checkpoint_json,started_at,updated_at,completed_at) VALUES(?,?,?,?,?::jsonb,?,?,?) ON CONFLICT(operation_id,step_name) DO UPDATE SET status=EXCLUDED.status,attempt=operation_steps.attempt+1,checkpoint_json=EXCLUDED.checkpoint_json,updated_at=EXCLUDED.updated_at,completed_at=EXCLUDED.completed_at`, id, step, status, 1, checkpoint, stamp, stamp, completed)
	if err != nil {
		return err
	}
	var sequence int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM operation_events WHERE operation_id=?`, id).Scan(&sequence); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_events(operation_id,sequence,level,event_type,message,payload_json,created_at) VALUES(?,?,?,?,?,?::jsonb,?)`, id, sequence, "info", "step."+status, message, checkpoint, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) OperationEvents(ctx context.Context, id string, limit int) ([]domain.OperationEvent, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	rows, err := s.QueryContext(ctx, `SELECT operation_id,sequence,level,event_type,message,payload_json::text,created_at FROM operation_events WHERE operation_id=? ORDER BY sequence DESC LIMIT ?`, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []domain.OperationEvent
	for rows.Next() {
		var event domain.OperationEvent
		var stamp string
		if err := rows.Scan(&event.OperationID, &event.Sequence, &event.Level, &event.Type, &event.Message, &event.Payload, &stamp); err != nil {
			return nil, err
		}
		event.CreatedAt = parseTime(stamp)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) updateClaim(ctx context.Context, query, id, owner string, generation int64) error {
	result, err := s.ExecContext(ctx, query, now(), id, owner, generation)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func nullIf(condition bool, value string) any {
	if condition {
		return nil
	}
	return value
}
