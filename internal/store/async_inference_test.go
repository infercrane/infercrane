package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestAsyncInferenceIsIdempotentFencedCancellableAndExpirable(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := time.Now().UTC().Format("150405.000000000")
	target, err := s.AddTarget(ctx, domain.Target{Name: "async-target-" + suffix, URL: "http://async-" + suffix, Provider: "existing", Runtime: "vllm", UpstreamModel: "model"})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.CreateDeployment(ctx, domain.Deployment{Name: "async-" + suffix, Model: "model"}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	request := domain.AsyncInferenceJob{ID: "job-" + suffix, RequestID: "request-" + suffix, Protocol: "chat", IdempotencyKey: "key-" + suffix, PayloadDigest: "sha256-payload", PayloadCiphertext: []byte("sealed"), PayloadNonce: []byte("nonce"), EncryptionKeyReference: "test-key", ExecutionDeadline: time.Now().UTC().Add(time.Hour), ExpiresAt: time.Now().UTC().Add(2 * time.Hour)}
	created, wasCreated, err := s.CreateAsyncInferenceJob(ctx, "global", deployment.Name, request)
	if err != nil || !wasCreated {
		t.Fatalf("create=(%#v,%t,%v)", created, wasCreated, err)
	}
	replay, replayCreated, err := s.CreateAsyncInferenceJob(ctx, "global", deployment.Name, domain.AsyncInferenceJob{ID: "different", RequestID: "different", Protocol: "chat", IdempotencyKey: request.IdempotencyKey, PayloadDigest: request.PayloadDigest, PayloadCiphertext: []byte("other"), PayloadNonce: []byte("other"), EncryptionKeyReference: "test-key", ExecutionDeadline: request.ExecutionDeadline, ExpiresAt: request.ExpiresAt})
	if err != nil || replayCreated || replay.ID != created.ID {
		t.Fatalf("replay=(%#v,%t,%v)", replay, replayCreated, err)
	}
	shiftedReplay := request
	shiftedReplay.ID = "shifted-deadline"
	shiftedReplay.RequestID = "shifted-deadline"
	shiftedReplay.ExecutionDeadline = request.ExecutionDeadline.Add(time.Minute)
	shiftedReplay.ExpiresAt = request.ExpiresAt.Add(time.Minute)
	shifted, shiftedCreated, err := s.CreateAsyncInferenceJob(ctx, "global", deployment.Name, shiftedReplay)
	if err != nil || shiftedCreated || shifted.ID != created.ID {
		t.Fatalf("network retry with derived deadlines=(%#v,%t,%v)", shifted, shiftedCreated, err)
	}
	conflict := request
	conflict.ID, conflict.RequestID, conflict.PayloadDigest = "conflict", "conflict", "sha256-other"
	if _, _, err = s.CreateAsyncInferenceJob(ctx, "global", deployment.Name, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("different async payload reused idempotency key: %v", err)
	}
	first, err := s.ClaimAsyncInferenceJob(ctx, "worker-a", "lease-a", time.Minute)
	if err != nil || first.ID != created.ID || first.Attempt != 1 {
		t.Fatalf("first claim=%#v %v", first, err)
	}
	var leaseSeconds float64
	if err = s.db.QueryRowContext(ctx, `SELECT EXTRACT(EPOCH FROM (lease_expires_at-clock_timestamp())) FROM async_inference_jobs WHERE id=$1`, first.ID).Scan(&leaseSeconds); err != nil || leaseSeconds < 55 || leaseSeconds > 61 {
		t.Fatalf("database-clock async lease duration=%g err=%v", leaseSeconds, err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE async_inference_jobs SET lease_expires_at=NOW()-INTERVAL '1 second' WHERE id=?`, created.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimAsyncInferenceJob(ctx, "worker-b", "lease-b", time.Minute)
	if err != nil || second.ID != created.ID || second.Attempt != 2 {
		t.Fatalf("adopted claim=%#v %v", second, err)
	}
	if err = s.HeartbeatAsyncInferenceJob(ctx, created.ID, "worker-a", "lease-a", time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale heartbeat=%v", err)
	}
	if err = s.HeartbeatAsyncInferenceJob(ctx, created.ID, "worker-b", "lease-b", time.Minute); err != nil {
		t.Fatalf("current heartbeat=%v", err)
	}
	if err = s.CompleteAsyncInferenceJob(ctx, created.ID, "worker-a", "lease-a", []byte("stale"), []byte("nonce")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale completion=%v", err)
	}
	if err = s.CompleteAsyncInferenceJob(ctx, created.ID, "worker-b", "lease-b", []byte("result"), []byte("result-nonce")); err != nil {
		t.Fatal(err)
	}
	completed, err := s.AsyncInferenceJob(ctx, "global", created.ID)
	if err != nil || completed.Status != "succeeded" || string(completed.ResultCiphertext) != "result" {
		t.Fatalf("completed=%#v %v", completed, err)
	}

	cancelRequest := request
	cancelRequest.ID = "cancel-" + suffix
	cancelRequest.RequestID = "cancel-request-" + suffix
	cancelRequest.IdempotencyKey = "cancel-key-" + suffix
	cancelled, _, err := s.CreateAsyncInferenceJob(ctx, "global", deployment.Name, cancelRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CancelAsyncInferenceJob(ctx, "global", cancelled.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, err = s.AsyncInferenceJob(ctx, "global", cancelled.ID)
	if err != nil || cancelled.Status != "cancelled" {
		t.Fatalf("cancelled=%#v %v", cancelled, err)
	}

	expireRequest := request
	expireRequest.ID = "expire-" + suffix
	expireRequest.RequestID = "expire-request-" + suffix
	expireRequest.IdempotencyKey = "expire-key-" + suffix
	expired, _, err := s.CreateAsyncInferenceJob(ctx, "global", deployment.Name, expireRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExecContext(ctx, `UPDATE async_inference_jobs SET execution_deadline=NOW()-INTERVAL '1 second' WHERE id=?`, expired.ID); err != nil {
		t.Fatal(err)
	}
	count, err := s.ExpireAsyncInferenceJobs(ctx)
	if err != nil || count < 1 {
		t.Fatalf("expire count=%d err=%v", count, err)
	}
	expired, err = s.AsyncInferenceJob(ctx, "global", expired.ID)
	if err != nil || expired.Status != "expired" {
		t.Fatalf("expired=%#v %v", expired, err)
	}
}
