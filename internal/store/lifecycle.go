package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) StartOperation(ctx context.Context, operation domain.Operation) (domain.Operation, bool, error) {
	if operation.Kind == "" || operation.ResourceType == "" || operation.ResourceName == "" {
		return domain.Operation{}, false, errors.New("operation kind and resource are required")
	}
	if operation.TenantID == "" {
		operation.TenantID = "global"
	}
	if operation.RequestJSON == "" {
		operation.RequestJSON = "{}"
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
	operation.ID, operation.Status, operation.Progress, operation.Attempt = id, "running", 0, 1
	operation.CreatedAt, operation.UpdatedAt = parseTime(stamp), parseTime(stamp)
	_, err = s.ExecContext(ctx, `INSERT INTO operations(id,tenant_id,kind,resource_type,resource_name,idempotency_key,status,progress,message,request_json,result_json,attempt,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,'{}'::jsonb,?,?,?)`, operation.ID, operation.TenantID, operation.Kind, operation.ResourceType, operation.ResourceName, null(operation.IdempotencyKey), operation.Status, operation.Progress, operation.Message, operation.RequestJSON, operation.Attempt, stamp, stamp)
	if isUniqueViolation(err) && operation.IdempotencyKey != "" {
		existing, lookupErr := s.operationByKey(ctx, operation.TenantID, operation.Kind, operation.IdempotencyKey)
		if lookupErr == nil {
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

const operationColumns = `id,tenant_id,kind,resource_type,resource_name,COALESCE(idempotency_key,''),status,progress,message,request_json::text,result_json::text,COALESCE(error_code,''),retryable,cancel_requested,attempt,max_attempts,COALESCE(lease_owner,''),lease_generation,created_at,updated_at,completed_at,lease_expires_at,next_attempt_at`

func (s *Store) operationByKey(ctx context.Context, tenant, kind, key string) (domain.Operation, error) {
	return s.scanOperation(s.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE tenant_id=? AND kind=? AND idempotency_key=?`, tenant, kind, key))
}

func (s *Store) Operation(ctx context.Context, id string) (domain.Operation, error) {
	return s.scanOperation(s.QueryRowContext(ctx, `SELECT `+operationColumns+` FROM operations WHERE id=?`, id))
}

func (s *Store) scanOperation(row *sql.Row) (domain.Operation, error) {
	var out domain.Operation
	var created, updated string
	var completed, leaseExpires, nextAttempt sql.NullTime
	err := row.Scan(&out.ID, &out.TenantID, &out.Kind, &out.ResourceType, &out.ResourceName, &out.IdempotencyKey, &out.Status, &out.Progress, &out.Message, &out.RequestJSON, &out.ResultJSON, &out.ErrorCode, &out.Retryable, &out.CancelRequested, &out.Attempt, &out.MaxAttempts, &out.LeaseOwner, &out.LeaseGeneration, &created, &updated, &completed, &leaseExpires, &nextAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	if completed.Valid {
		stamp := completed.Time.UTC()
		out.CompletedAt = &stamp
	}
	if leaseExpires.Valid {
		stamp := leaseExpires.Time.UTC()
		out.LeaseExpiresAt = &stamp
	}
	if nextAttempt.Valid {
		stamp := nextAttempt.Time.UTC()
		out.NextAttemptAt = &stamp
	}
	return out, nil
}

func (s *Store) UpdateOperation(ctx context.Context, id string, progress int, message string) error {
	if progress < 0 || progress > 99 {
		return errors.New("running operation progress must be between 0 and 99")
	}
	result, err := s.ExecContext(ctx, `UPDATE operations SET progress=?,message=?,updated_at=? WHERE id=? AND status='running'`, progress, message, now(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) FinishOperation(ctx context.Context, id, resultJSON string) error {
	if resultJSON == "" {
		resultJSON = "{}"
	}
	result, err := s.ExecContext(ctx, `UPDATE operations SET status='succeeded',progress=100,result_json=?::jsonb,message='completed',updated_at=?,completed_at=? WHERE id=? AND status='running'`, resultJSON, now(), now(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) FailOperation(ctx context.Context, id, code, message string, retryable bool) error {
	result, err := s.ExecContext(ctx, `UPDATE operations SET status='failed',error_code=?,message=?,retryable=?,updated_at=?,completed_at=? WHERE id=? AND status='running'`, code, message, retryable, now(), now(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RequestOperationCancel(ctx context.Context, id string) error {
	stamp := now()
	result, err := s.ExecContext(ctx, `UPDATE operations SET cancel_requested=TRUE,status=CASE WHEN status IN ('pending','waiting') THEN 'cancelled' ELSE 'cancelling' END,message=CASE WHEN status IN ('pending','waiting') THEN 'cancelled before execution' ELSE 'cancellation requested' END,completed_at=CASE WHEN status IN ('pending','waiting') THEN ? ELSE completed_at END,updated_at=? WHERE id=? AND status IN ('pending','leased','running','waiting','cancelling')`, stamp, stamp, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CancelOperation(ctx context.Context, id, message string) error {
	result, err := s.ExecContext(ctx, `UPDATE operations SET status='cancelled',message=?,updated_at=?,completed_at=? WHERE id=? AND status IN ('pending','running')`, message, now(), now(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) OrphanedTargets(ctx context.Context) ([]domain.Orphan, error) {
	return s.OrphanedTargetsForTenant(ctx, "global")
}
func (s *Store) OrphanedTargetsForTenant(ctx context.Context, tenant string) ([]domain.Orphan, error) {
	rows, err := s.QueryContext(ctx, `SELECT t.id,t.name,t.provider,t.provider_resource_id,t.created_at FROM targets t LEFT JOIN deployment_targets dt ON dt.target_id=t.id LEFT JOIN deployments d ON d.id=dt.deployment_id AND d.desired_state!='deleted' WHERE t.provider_resource_id IS NOT NULL AND t.tenant_id=? GROUP BY t.id HAVING COUNT(d.id)=0 ORDER BY t.created_at`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Orphan
	for rows.Next() {
		var item domain.Orphan
		var stamp string
		if err := rows.Scan(&item.TargetID, &item.Name, &item.Provider, &item.ProviderResourceID, &stamp); err != nil {
			return nil, err
		}
		item.CreatedAt = parseTime(stamp)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) Audit(ctx context.Context, event domain.AuditEvent) error {
	if event.TenantID == "" {
		event.TenantID = "global"
	}
	if event.Actor == "" {
		event.Actor = "system"
	}
	if event.Payload == "" {
		event.Payload = "{}"
	}
	id, err := newID()
	if err != nil {
		return err
	}
	_, err = s.ExecContext(ctx, `INSERT INTO audit_events(id,tenant_id,actor,action,resource_type,resource_name,outcome,request_id,payload_json,created_at) VALUES(?,?,?,?,?,?,?,?,?::jsonb,?)`, id, event.TenantID, event.Actor, event.Action, event.ResourceType, event.ResourceName, event.Outcome, null(event.RequestID), event.Payload, now())
	return err
}

func (s *Store) AuditEventsForTenant(ctx context.Context, tenant string, before time.Time, limit int) ([]domain.AuditEvent, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	query := `SELECT id,tenant_id,actor,action,resource_type,resource_name,outcome,COALESCE(request_id,''),payload_json::text,created_at FROM audit_events WHERE tenant_id=?`
	args := []any{tenant}
	if !before.IsZero() {
		query += ` AND created_at<?`
		args = append(args, before.UTC())
	}
	query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AuditEvent
	for rows.Next() {
		var event domain.AuditEvent
		var stamp string
		if err := rows.Scan(&event.ID, &event.TenantID, &event.Actor, &event.Action, &event.ResourceType, &event.ResourceName, &event.Outcome, &event.RequestID, &event.Payload, &stamp); err != nil {
			return nil, err
		}
		event.CreatedAt = parseTime(stamp)
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *Store) RetryOperation(ctx context.Context, id string) (domain.Operation, error) {
	operation, err := s.Operation(ctx, id)
	if err != nil {
		return operation, err
	}
	if operation.Status != "failed" || !operation.Retryable {
		return operation, fmt.Errorf("operation is not retryable")
	}
	stamp := now()
	_, err = s.ExecContext(ctx, `UPDATE operations SET status='running',progress=0,message='retrying',error_code=NULL,retryable=FALSE,cancel_requested=FALSE,attempt=attempt+1,updated_at=?,completed_at=NULL WHERE id=?`, stamp, id)
	if err != nil {
		return operation, err
	}
	operation.Status = "running"
	operation.Progress = 0
	operation.Attempt++
	operation.UpdatedAt = parseTime(stamp)
	operation.CompletedAt = nil
	return operation, nil
}
