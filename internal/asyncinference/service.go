package asyncinference

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/safehttp"
	"github.com/infercrane/infercrane/internal/secrets"
)

type Store interface {
	CreateAsyncInferenceJob(context.Context, string, string, domain.AsyncInferenceJob) (domain.AsyncInferenceJob, bool, error)
	AsyncInferenceJob(context.Context, string, string) (domain.AsyncInferenceJob, error)
	ClaimAsyncInferenceJob(context.Context, string, string, time.Duration) (domain.AsyncInferenceJob, error)
	HeartbeatAsyncInferenceJob(context.Context, string, string, string, time.Duration) error
	CompleteAsyncInferenceJob(context.Context, string, string, string, []byte, []byte) error
	FailAsyncInferenceJob(context.Context, string, string, string, string, string, bool) error
	ExpireAsyncInferenceJobs(context.Context) (int64, error)
	CancelAsyncInferenceJob(context.Context, string, string) error
	SecretReferenceForTenant(context.Context, string, string) (domain.SecretReference, error)
	RecordAsyncWebhookAttempt(context.Context, string, string, bool, string) error
}

type Service struct {
	Store                                   Store
	Cipher                                  Cipher
	KeyReference, GatewayURL, APIKey, Owner string
	Client                                  *http.Client
	Secrets                                 secrets.Resolver
	Lease                                   time.Duration
}

type SubmitRequest struct {
	Tenant, Endpoint, Protocol, IdempotencyKey string
	Payload                                    []byte
	Priority                                   int
	ExecutionDeadline, ExpiresAt               time.Time
	WebhookURL, WebhookSecretReferenceID       string
}

func (s Service) Submit(ctx context.Context, request SubmitRequest) (domain.AsyncInferenceJob, bool, error) {
	if s.Store == nil || s.KeyReference == "" {
		return domain.AsyncInferenceJob{}, false, errors.New("async inference is not configured")
	}
	if request.Tenant == "" || request.Endpoint == "" || request.IdempotencyKey == "" || len(request.Payload) == 0 {
		return domain.AsyncInferenceJob{}, false, errors.New("tenant, endpoint, idempotency key, and payload are required")
	}
	if request.Protocol == "" {
		request.Protocol = "chat"
	}
	if request.ExecutionDeadline.IsZero() {
		request.ExecutionDeadline = time.Now().UTC().Add(15 * time.Minute)
	}
	if request.ExpiresAt.IsZero() {
		request.ExpiresAt = request.ExecutionDeadline.Add(24 * time.Hour)
	}
	jobID, err := randomID()
	if err != nil {
		return domain.AsyncInferenceJob{}, false, err
	}
	aad := []byte(request.Tenant + "/" + jobID)
	ciphertext, nonce, err := s.Cipher.Encrypt(request.Payload, aad)
	if err != nil {
		return domain.AsyncInferenceJob{}, false, err
	}
	payloadDigest := sha256.Sum256(request.Payload)
	return s.Store.CreateAsyncInferenceJob(ctx, request.Tenant, request.Endpoint, domain.AsyncInferenceJob{ID: jobID, Protocol: request.Protocol, Priority: request.Priority, IdempotencyKey: request.IdempotencyKey, PayloadDigest: hex.EncodeToString(payloadDigest[:]), PayloadCiphertext: ciphertext, PayloadNonce: nonce, EncryptionKeyReference: s.KeyReference, WebhookURL: request.WebhookURL, WebhookSecretReferenceID: request.WebhookSecretReferenceID, ExecutionDeadline: request.ExecutionDeadline, ExpiresAt: request.ExpiresAt})
}

func (s Service) Result(ctx context.Context, tenant, id string) (domain.AsyncInferenceJob, []byte, error) {
	job, err := s.Store.AsyncInferenceJob(ctx, tenant, id)
	if err != nil {
		return job, nil, err
	}
	if job.Status != "succeeded" {
		return job, nil, nil
	}
	result, err := s.Cipher.Decrypt(job.ResultCiphertext, job.ResultNonce, []byte(job.TenantID+"/"+job.ID))
	return job, result, err
}

func (s Service) Cancel(ctx context.Context, tenant, id string) error {
	return s.Store.CancelAsyncInferenceJob(ctx, tenant, id)
}

func (s Service) Run(ctx context.Context) error {
	if s.Store == nil || s.GatewayURL == "" {
		return errors.New("async worker is not configured")
	}
	owner := s.Owner
	if owner == "" {
		owner = "async-worker"
	}
	lease := s.Lease
	if lease <= 0 {
		lease = time.Minute
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		_, _ = s.Store.ExpireAsyncInferenceJobs(ctx)
		token, err := randomID()
		if err != nil {
			return err
		}
		job, err := s.Store.ClaimAsyncInferenceJob(ctx, owner, token, lease)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(250 * time.Millisecond):
				continue
			}
		}
		executionCtx, cancelExecution := context.WithCancel(ctx)
		leaseResult := make(chan error, 1)
		go s.maintainLease(executionCtx, cancelExecution, job.ID, owner, token, lease, leaseResult)
		executeErr := s.execute(executionCtx, owner, token, job)
		cancelExecution()
		leaseErr := <-leaseResult
		if leaseErr != nil {
			// Ownership is uncertain. Never issue a stale terminal mutation; the
			// durable lease can be reclaimed and fenced by another worker.
			continue
		}
		if executeErr != nil {
			retryable := job.Attempt < 3 && time.Now().UTC().Before(job.ExecutionDeadline)
			_ = s.Store.FailAsyncInferenceJob(ctx, job.ID, owner, token, "execution_failed", executeErr.Error(), retryable)
		}
	}
}

func (s Service) maintainLease(ctx context.Context, cancel context.CancelFunc, jobID, owner, token string, lease time.Duration, result chan<- error) {
	interval := lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			result <- nil
			return
		case <-ticker.C:
			if err := s.Store.HeartbeatAsyncInferenceJob(ctx, jobID, owner, token, lease); err != nil {
				cancel()
				result <- err
				return
			}
		}
	}
}

func (s Service) execute(ctx context.Context, owner, token string, job domain.AsyncInferenceJob) error {
	payload, err := s.Cipher.Decrypt(job.PayloadCiphertext, job.PayloadNonce, []byte(job.TenantID+"/"+job.ID))
	if err != nil {
		return err
	}
	path, ok := map[string]string{"chat": "/v1/chat/completions", "responses": "/v1/responses", "embeddings": "/v1/embeddings", "completions": "/v1/completions", "batch": "/v1/chat/completions/batch"}[job.Protocol]
	if !ok {
		return fmt.Errorf("unsupported async protocol %q", job.Protocol)
	}
	base, err := url.Parse(s.GatewayURL)
	if err != nil {
		return err
	}
	base.Path = path
	deadline := job.ExecutionDeadline
	requestCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, base.String(), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Idempotency-Key", job.IdempotencyKey)
	req.Header.Set("X-InferCrane-Request-ID", job.RequestID)
	client := s.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gateway returned HTTP %d", resp.StatusCode)
	}
	ciphertext, nonce, err := s.Cipher.Encrypt(body, []byte(job.TenantID+"/"+job.ID))
	if err != nil {
		return err
	}
	if err = s.Store.CompleteAsyncInferenceJob(ctx, job.ID, owner, token, ciphertext, nonce); err != nil {
		return err
	}
	if job.WebhookURL != "" {
		_ = s.deliverWebhook(ctx, job, body)
	}
	return nil
}

func (s Service) deliverWebhook(ctx context.Context, job domain.AsyncInferenceJob, result []byte) error {
	parsed, err := url.Parse(job.WebhookURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return errors.New("async webhook must be HTTPS")
	}
	if s.Secrets == nil {
		return errors.New("async webhook secret resolver is unavailable")
	}
	reference, err := s.Store.SecretReferenceForTenant(ctx, job.TenantID, job.WebhookSecretReferenceID)
	if err != nil {
		return err
	}
	secret, err := s.Secrets.Resolve(ctx, reference)
	if err != nil {
		return errors.New("async webhook signing secret is unavailable")
	}
	var decoded any
	if json.Unmarshal(result, &decoded) != nil {
		decoded = string(result)
	}
	body, err := json.Marshal(map[string]any{"schema_version": 1, "event": "infercrane.async_inference.completed", "job": map[string]any{"id": job.ID, "request_id": job.RequestID, "status": "succeeded", "protocol": job.Protocol}, "result": decoded})
	if err != nil {
		return err
	}
	client := s.Client
	if client == nil {
		client = safehttp.WebhookClient(nil, false)
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		timestamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(timestamp + "."))
		_, _ = mac.Write(body)
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, job.WebhookURL, bytes.NewReader(body))
		if requestErr != nil {
			return requestErr
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("InferCrane-Delivery", job.ID)
		req.Header.Set("InferCrane-Timestamp", timestamp)
		req.Header.Set("InferCrane-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
		response, sendErr := client.Do(req)
		code, delivered := "network_error", false
		if sendErr == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			_ = response.Body.Close()
			delivered = response.StatusCode >= 200 && response.StatusCode < 300
			code = "http_" + strconv.Itoa(response.StatusCode)
			if delivered {
				code = ""
			}
		}
		if recordErr := s.Store.RecordAsyncWebhookAttempt(ctx, job.TenantID, job.ID, delivered, code); recordErr != nil {
			return recordErr
		}
		if delivered {
			return nil
		}
		if sendErr != nil {
			last = sendErr
		} else {
			last = errors.New(code)
		}
	}
	return last
}

func randomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}
