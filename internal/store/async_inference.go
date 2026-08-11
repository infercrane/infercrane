package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) CreateAsyncInferenceJob(ctx context.Context, tenant, endpointName string, job domain.AsyncInferenceJob) (domain.AsyncInferenceJob, bool, error) {
	endpoint, err := s.ResolveEndpointForTenant(ctx, tenant, endpointName)
	if err != nil {
		return domain.AsyncInferenceJob{}, false, err
	}
	if job.IdempotencyKey == "" || len(job.PayloadCiphertext) == 0 || len(job.PayloadNonce) == 0 || job.EncryptionKeyReference == "" || !job.ExecutionDeadline.Before(job.ExpiresAt) {
		return domain.AsyncInferenceJob{}, false, errors.New("invalid async inference job")
	}
	if job.ID == "" {
		job.ID, err = newID()
		if err != nil {
			return domain.AsyncInferenceJob{}, false, err
		}
	}
	if job.RequestID == "" {
		job.RequestID, err = newID()
		if err != nil {
			return domain.AsyncInferenceJob{}, false, err
		}
	}
	stamp := now()
	webhookStatus := "not_configured"
	if job.WebhookURL != "" {
		webhookStatus = "pending"
	}
	_, err = s.ExecContext(ctx, `INSERT INTO async_inference_jobs(id,tenant_id,endpoint_id,request_id,protocol,status,priority,idempotency_key,payload_ciphertext,payload_nonce,encryption_key_reference,webhook_url,webhook_secret_reference_id,webhook_status,execution_deadline,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,'queued',?,?,?,?,?,?,?,?,?,?,?,?)`, job.ID, tenant, endpoint.Endpoint.ID, job.RequestID, job.Protocol, job.Priority, job.IdempotencyKey, job.PayloadCiphertext, job.PayloadNonce, job.EncryptionKeyReference, nullIf(job.WebhookURL == "", job.WebhookURL), nullIf(job.WebhookSecretReferenceID == "", job.WebhookSecretReferenceID), webhookStatus, job.ExecutionDeadline.UTC(), job.ExpiresAt.UTC(), stamp, stamp)
	if err != nil {
		if isUniqueViolation(err) {
			existing, lookup := s.AsyncInferenceJobByIdempotency(ctx, tenant, job.IdempotencyKey)
			return existing, false, lookup
		}
		return domain.AsyncInferenceJob{}, false, err
	}
	created, err := s.AsyncInferenceJob(ctx, tenant, job.ID)
	return created, true, err
}

func (s *Store) AsyncInferenceJobByIdempotency(ctx context.Context, tenant, key string) (domain.AsyncInferenceJob, error) {
	return s.scanAsyncJob(s.QueryRowContext(ctx, asyncJobSelect+` WHERE tenant_id=? AND idempotency_key=?`, tenant, key))
}

func (s *Store) AsyncInferenceJob(ctx context.Context, tenant, id string) (domain.AsyncInferenceJob, error) {
	return s.scanAsyncJob(s.QueryRowContext(ctx, asyncJobSelect+` WHERE tenant_id=? AND id=?`, tenant, id))
}

const asyncJobSelect = `SELECT id,tenant_id,endpoint_id,request_id,protocol,status,priority,idempotency_key,payload_ciphertext,payload_nonce,COALESCE(result_ciphertext,'\\x'::bytea),COALESCE(result_nonce,'\\x'::bytea),encryption_key_reference,COALESCE(webhook_url,''),COALESCE(webhook_secret_reference_id,''),webhook_status,webhook_attempts,COALESCE(webhook_error_code,''),execution_deadline,expires_at,COALESCE(lease_owner,''),COALESCE(lease_token,''),attempt,COALESCE(error_code,''),COALESCE(error_message,''),created_at,started_at,completed_at,lease_expires_at,updated_at FROM async_inference_jobs`

type asyncRowScanner interface{ Scan(...any) error }

func (s *Store) scanAsyncJob(row asyncRowScanner) (domain.AsyncInferenceJob, error) {
	var j domain.AsyncInferenceJob
	var execution, expires, created, updated string
	var started, completed, lease sql.NullString
	err := row.Scan(&j.ID, &j.TenantID, &j.EndpointID, &j.RequestID, &j.Protocol, &j.Status, &j.Priority, &j.IdempotencyKey, &j.PayloadCiphertext, &j.PayloadNonce, &j.ResultCiphertext, &j.ResultNonce, &j.EncryptionKeyReference, &j.WebhookURL, &j.WebhookSecretReferenceID, &j.WebhookStatus, &j.WebhookAttempts, &j.WebhookErrorCode, &execution, &expires, &j.LeaseOwner, &j.LeaseToken, &j.Attempt, &j.ErrorCode, &j.ErrorMessage, &created, &started, &completed, &lease, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	j.ExecutionDeadline, j.ExpiresAt, j.CreatedAt, j.UpdatedAt = parseTime(execution), parseTime(expires), parseTime(created), parseTime(updated)
	if started.Valid {
		v := parseTime(started.String)
		j.StartedAt = &v
	}
	if completed.Valid {
		v := parseTime(completed.String)
		j.CompletedAt = &v
	}
	if lease.Valid {
		v := parseTime(lease.String)
		j.LeaseExpiresAt = &v
	}
	return j, nil
}

func (s *Store) ClaimAsyncInferenceJob(ctx context.Context, owner, token string, lease time.Duration) (domain.AsyncInferenceJob, error) {
	if owner == "" || token == "" || lease <= 0 {
		return domain.AsyncInferenceJob{}, errors.New("owner, token, and positive lease are required")
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.AsyncInferenceJob{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var id, tenant string
	err = tx.QueryRowContext(ctx, `SELECT id,tenant_id FROM async_inference_jobs WHERE ((status='queued') OR (status='running' AND lease_expires_at<NOW())) AND execution_deadline>NOW() AND expires_at>NOW() AND attempt<3 ORDER BY priority DESC,created_at FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&id, &tenant)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AsyncInferenceJob{}, ErrNotFound
	}
	if err != nil {
		return domain.AsyncInferenceJob{}, err
	}
	stamp := now()
	_, err = tx.ExecContext(ctx, `UPDATE async_inference_jobs SET status='running',lease_owner=?,lease_token=?,lease_expires_at=?,attempt=attempt+1,started_at=COALESCE(started_at,?),updated_at=? WHERE id=?`, owner, token, time.Now().UTC().Add(lease), stamp, stamp, id)
	if err != nil {
		return domain.AsyncInferenceJob{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.AsyncInferenceJob{}, err
	}
	return s.AsyncInferenceJob(ctx, tenant, id)
}

func (s *Store) CompleteAsyncInferenceJob(ctx context.Context, id, owner, token string, ciphertext, nonce []byte) error {
	r, err := s.ExecContext(ctx, `UPDATE async_inference_jobs SET status='succeeded',result_ciphertext=?,result_nonce=?,lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=?,updated_at=? WHERE id=? AND status='running' AND lease_owner=? AND lease_token=? AND lease_expires_at>NOW()`, ciphertext, nonce, now(), now(), id, owner, token)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) FailAsyncInferenceJob(ctx context.Context, id, owner, token, code, message string, retryable bool) error {
	status := "failed"
	var completed any = now()
	if retryable {
		status = "queued"
		completed = nil
	}
	r, err := s.ExecContext(ctx, `UPDATE async_inference_jobs SET status=?,error_code=?,error_message=?,lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=?,updated_at=? WHERE id=? AND status='running' AND lease_owner=? AND lease_token=? AND lease_expires_at>NOW()`, status, code, message, completed, now(), id, owner, token)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ExpireAsyncInferenceJobs(ctx context.Context) (int64, error) {
	r, err := s.ExecContext(ctx, `UPDATE async_inference_jobs SET status='expired',error_code='expired',error_message='job retention or execution deadline elapsed',lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=?,updated_at=? WHERE status IN ('queued','running') AND (execution_deadline<=NOW() OR expires_at<=NOW())`, now(), now())
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

func (s *Store) RecordAsyncWebhookAttempt(ctx context.Context, tenant, id string, delivered bool, code string) error {
	status := "pending"
	if delivered {
		status = "delivered"
	}
	r, err := s.ExecContext(ctx, `UPDATE async_inference_jobs SET webhook_attempts=webhook_attempts+1,webhook_status=CASE WHEN ? THEN 'delivered' WHEN webhook_attempts+1>=3 THEN 'failed' ELSE ? END,webhook_error_code=?,updated_at=? WHERE tenant_id=? AND id=? AND webhook_url IS NOT NULL AND webhook_attempts<3`, delivered, status, nullIf(code == "", code), now(), tenant, id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CancelAsyncInferenceJob(ctx context.Context, tenant, id string) error {
	r, err := s.ExecContext(ctx, `UPDATE async_inference_jobs SET status='cancelled',lease_owner=NULL,lease_token=NULL,lease_expires_at=NULL,completed_at=?,updated_at=? WHERE tenant_id=? AND id=? AND status IN ('queued','running')`, now(), now(), tenant, id)
	if err != nil {
		return err
	}
	n, _ := r.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
