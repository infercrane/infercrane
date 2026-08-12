package asyncinference

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type failingLeaseStore struct{ heartbeatErr error }

func (f failingLeaseStore) CreateAsyncInferenceJob(context.Context, string, string, domain.AsyncInferenceJob) (domain.AsyncInferenceJob, bool, error) {
	return domain.AsyncInferenceJob{}, false, nil
}
func (f failingLeaseStore) AsyncInferenceJob(context.Context, string, string) (domain.AsyncInferenceJob, error) {
	return domain.AsyncInferenceJob{}, nil
}
func (f failingLeaseStore) ClaimAsyncInferenceJob(context.Context, string, string, time.Duration) (domain.AsyncInferenceJob, error) {
	return domain.AsyncInferenceJob{}, nil
}
func (f failingLeaseStore) HeartbeatAsyncInferenceJob(context.Context, string, string, string, time.Duration) error {
	return f.heartbeatErr
}
func (f failingLeaseStore) CompleteAsyncInferenceJob(context.Context, string, string, string, []byte, []byte) error {
	return nil
}
func (f failingLeaseStore) FailAsyncInferenceJob(context.Context, string, string, string, string, string, bool) error {
	return nil
}
func (f failingLeaseStore) ExpireAsyncInferenceJobs(context.Context) (int64, error) { return 0, nil }
func (f failingLeaseStore) CancelAsyncInferenceJob(context.Context, string, string) error {
	return nil
}
func (f failingLeaseStore) SecretReferenceForTenant(context.Context, string, string) (domain.SecretReference, error) {
	return domain.SecretReference{}, nil
}
func (f failingLeaseStore) RecordAsyncWebhookAttempt(context.Context, string, string, bool, string) error {
	return nil
}

func TestAsyncLeaseFailureCancelsExecutionAndSurfacesOwnershipLoss(t *testing.T) {
	want := errors.New("database connection reset")
	service := Service{Store: failingLeaseStore{heartbeatErr: want}}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go service.maintainLease(ctx, cancel, "job", "worker", "token", 30*time.Millisecond, result)
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("lease failure did not cancel execution")
	}
	if err := <-result; !errors.Is(err, want) {
		t.Fatalf("lease error=%v want=%v", err, want)
	}
}
