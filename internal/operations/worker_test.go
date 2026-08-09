package operations

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type fakeRepo struct {
	op                           domain.Operation
	completed, failed, cancelled bool
	retryable                    bool
	next                         time.Time
}

func (f *fakeRepo) ClaimOperation(context.Context, string, time.Duration) (domain.Operation, error) {
	if f.op.ID == "" {
		return domain.Operation{}, domain.ErrNotFound
	}
	return f.op, nil
}
func (f *fakeRepo) HeartbeatOperation(context.Context, string, string, time.Duration) error {
	return nil
}
func (f *fakeRepo) Operation(context.Context, string) (domain.Operation, error) { return f.op, nil }
func (f *fakeRepo) CompleteClaimedOperation(context.Context, string, string, string) error {
	f.completed = true
	return nil
}
func (f *fakeRepo) FailClaimedOperation(_ context.Context, _, _, _, _ string, retryable bool, next time.Time) error {
	f.failed = true
	f.retryable = retryable
	f.next = next
	return nil
}
func (f *fakeRepo) CancelClaimedOperation(context.Context, string, string, string) error {
	f.cancelled = true
	return nil
}

func TestWorkerCompletesClaimedOperation(t *testing.T) {
	repo := &fakeRepo{op: domain.Operation{ID: "1", Kind: "apply", Attempt: 1, MaxAttempts: 3}}
	worked, err := (Worker{Repository: repo, Owner: "worker", Handlers: map[string]Handler{"apply": func(context.Context, domain.Operation) (string, error) { return `{}`, nil }}}).Once(context.Background())
	if err != nil || !worked || !repo.completed {
		t.Fatalf("worked=%t completed=%t err=%v", worked, repo.completed, err)
	}
}
func TestWorkerSchedulesBoundedRetry(t *testing.T) {
	now := time.Unix(100, 0)
	repo := &fakeRepo{op: domain.Operation{ID: "1", Kind: "apply", Attempt: 2, MaxAttempts: 3}}
	_, err := (Worker{Repository: repo, Owner: "worker", Now: func() time.Time { return now }, BaseBackoff: time.Second, Handlers: map[string]Handler{"apply": func(context.Context, domain.Operation) (string, error) {
		return "", Retryable("busy", errors.New("busy"))
	}}}).Once(context.Background())
	if err != nil || !repo.failed || !repo.retryable || !repo.next.Equal(now.Add(2*time.Second)) {
		t.Fatalf("repo=%#v err=%v", repo, err)
	}
}
func TestWorkerStopsRetryAtMaxAttempts(t *testing.T) {
	repo := &fakeRepo{op: domain.Operation{ID: "1", Kind: "apply", Attempt: 3, MaxAttempts: 3}}
	_, err := (Worker{Repository: repo, Owner: "worker", Handlers: map[string]Handler{"apply": func(context.Context, domain.Operation) (string, error) {
		return "", Retryable("busy", errors.New("busy"))
	}}}).Once(context.Background())
	if err != nil || repo.retryable {
		t.Fatalf("retryable=%t err=%v", repo.retryable, err)
	}
}
