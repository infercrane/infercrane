package store

import (
	"context"
	"database/sql"
	"errors"
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
	_, err = s.ExecContext(ctx, `INSERT INTO operations(id,tenant_id,kind,resource_type,resource_name,idempotency_key,status,progress,message,request_json,result_json,attempt,max_attempts,next_attempt_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,'{}'::jsonb,?,?,?,?,?)`, operation.ID, operation.TenantID, operation.Kind, operation.ResourceType, operation.ResourceName, null(operation.IdempotencyKey), operation.Status, 0, "queued", operation.RequestJSON, 0, operation.MaxAttempts, stamp, stamp, stamp)
	if isUniqueViolation(err) && operation.IdempotencyKey != "" {
		existing, lookupErr := s.operationByKey(ctx, operation.TenantID, operation.Kind, operation.IdempotencyKey)
		return existing, false, lookupErr
	}
	return operation, err == nil, err
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
	err = tx.QueryRowContext(ctx, `SELECT id FROM operations WHERE ((status='pending' AND next_attempt_at<=NOW()) OR (status='running' AND lease_expires_at<NOW())) AND cancel_requested=FALSE AND attempt<max_attempts ORDER BY next_attempt_at,created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Operation{}, ErrNotFound
	}
	if err != nil {
		return domain.Operation{}, err
	}
	expires := time.Now().UTC().Add(lease)
	if _, err = tx.ExecContext(ctx, `UPDATE operations SET status='running',attempt=attempt+1,lease_owner=?,lease_expires_at=?,message='running',updated_at=? WHERE id=?`, owner, expires, now(), id); err != nil {
		return domain.Operation{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.Operation{}, err
	}
	return s.Operation(ctx, id)
}

func (s *Store) HeartbeatOperation(ctx context.Context, id, owner string, lease time.Duration) error {
	result, err := s.ExecContext(ctx, `UPDATE operations SET lease_expires_at=?,updated_at=? WHERE id=? AND status='running' AND lease_owner=?`, time.Now().UTC().Add(lease), now(), id, owner)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CompleteClaimedOperation(ctx context.Context, id, owner, resultJSON string) error {
	if resultJSON == "" {
		resultJSON = "{}"
	}
	result, err := s.ExecContext(ctx, `UPDATE operations SET status='succeeded',progress=100,result_json=?::jsonb,message='completed',lease_owner=NULL,lease_expires_at=NULL,updated_at=?,completed_at=? WHERE id=? AND status='running' AND lease_owner=?`, resultJSON, now(), now(), id, owner)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) FailClaimedOperation(ctx context.Context, id, owner, code, message string, retryable bool, next time.Time) error {
	status := "failed"
	var completed any = now()
	if retryable {
		status = "pending"
		completed = nil
	}
	result, err := s.ExecContext(ctx, `UPDATE operations SET status=?,error_code=?,message=?,retryable=?,next_attempt_at=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=?,completed_at=? WHERE id=? AND status='running' AND lease_owner=?`, status, code, message, retryable, next.UTC(), now(), completed, id, owner)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CancelClaimedOperation(ctx context.Context, id, owner, message string) error {
	result, err := s.ExecContext(ctx, `UPDATE operations SET status='cancelled',message=?,lease_owner=NULL,lease_expires_at=NULL,updated_at=?,completed_at=? WHERE id=? AND status='running' AND lease_owner=?`, message, now(), now(), id, owner)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
