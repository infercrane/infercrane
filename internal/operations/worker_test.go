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
	heartbeatErr                 error
}

func (f *fakeRepo) ClaimOperation(context.Context, string, time.Duration) (domain.Operation, error) {
	if f.op.ID == "" {
		return domain.Operation{}, domain.ErrNotFound
	}
	return f.op, nil
}
func (f *fakeRepo) StartClaimedOperation(context.Context, string, string, int64) error { return nil }
func (f *fakeRepo) HeartbeatOperation(context.Context, string, string, int64, time.Duration) error {
	return f.heartbeatErr
}

func TestWorkerDoesNotFinalizeAfterLeaseMaintenanceFailure(t *testing.T) {
	repo := &fakeRepo{
		op:           domain.Operation{ID: "1", Kind: "apply", Attempt: 1, MaxAttempts: 3},
		heartbeatErr: errors.New("database connection reset"),
	}
	worked, err := (Worker{
		Repository: repo,
		Owner:      "worker",
		Lease:      30 * time.Millisecond,
		Handlers: map[string]Handler{"apply": func(ctx context.Context, _ domain.Operation) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}},
	}).Once(context.Background())
	if !worked || !errors.Is(err, repo.heartbeatErr) {
		t.Fatalf("worked=%t err=%v", worked, err)
	}
	if repo.completed || repo.failed || repo.cancelled {
		t.Fatalf("lease-lost worker finalized operation: %#v", repo)
	}
}
func (f *fakeRepo) Operation(context.Context, string) (domain.Operation, error) { return f.op, nil }
func (f *fakeRepo) CompleteClaimedOperation(context.Context, string, string, int64, string) error {
	f.completed = true
	return nil
}
func (f *fakeRepo) FailClaimedOperation(_ context.Context, _, _ string, _ int64, _, _ string, retryable bool, next time.Time) error {
	f.failed = true
	f.retryable = retryable
	f.next = next
	return nil
}
func (f *fakeRepo) CancelClaimedOperation(context.Context, string, string, int64, string) error {
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
	_, err := (Worker{Repository: repo, Owner: "worker", Now: func() time.Time { return now }, BaseBackoff: time.Second, Jitter: func(delay time.Duration) time.Duration { return delay }, Handlers: map[string]Handler{"apply": func(context.Context, domain.Operation) (string, error) {
		return "", Retryable("busy", errors.New("busy"))
	}}}).Once(context.Background())
	if err != nil || !repo.failed || !repo.retryable || !repo.next.Equal(now.Add(2*time.Second)) {
		t.Fatalf("repo=%#v err=%v", repo, err)
	}
}

func TestWorkerAppliesConfiguredJitter(t *testing.T) {
	now := time.Unix(100, 0)
	repo := &fakeRepo{op: domain.Operation{ID: "1", Kind: "apply", Attempt: 2, MaxAttempts: 3}}
	_, err := (Worker{Repository: repo, Owner: "worker", Now: func() time.Time { return now }, BaseBackoff: time.Second, Jitter: func(delay time.Duration) time.Duration { return delay + 250*time.Millisecond }, Handlers: map[string]Handler{"apply": func(context.Context, domain.Operation) (string, error) {
		return "", Retryable("busy", errors.New("busy"))
	}}}).Once(context.Background())
	if err != nil || !repo.next.Equal(now.Add(2250*time.Millisecond)) {
		t.Fatalf("next=%s err=%v", repo.next, err)
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

func TestWorkerCancelsRecoveredClaimBeforeHandler(t *testing.T) {
	repo := &fakeRepo{op: domain.Operation{ID: "1", Kind: "apply", Attempt: 2, MaxAttempts: 3, CancelRequested: true}}
	called, cleaned := false, false
	worked, err := (Worker{Repository: repo, Owner: "worker", Handlers: map[string]Handler{"apply": func(context.Context, domain.Operation) (string, error) {
		called = true
		return `{}`, nil
	}, "apply.cancel": func(context.Context, domain.Operation) (string, error) {
		cleaned = true
		return `{}`, nil
	}}}).Once(context.Background())
	if err != nil || !worked || !repo.cancelled || called || !cleaned {
		t.Fatalf("worked=%t cancelled=%t handler_called=%t cleaned=%t err=%v", worked, repo.cancelled, called, cleaned, err)
	}
}

func TestWorkerRetriesFailedCancellationCleanup(t *testing.T) {
	repo := &fakeRepo{op: domain.Operation{ID: "1", Kind: "apply", Attempt: 1, MaxAttempts: 3, CancelRequested: true}}
	_, err := (Worker{Repository: repo, Owner: "worker", Handlers: map[string]Handler{
		"apply.cancel": func(context.Context, domain.Operation) (string, error) {
			return "", Retryable("cleanup_failed", errors.New("provider unavailable"))
		},
	}}).Once(context.Background())
	if err != nil || repo.cancelled || !repo.failed || !repo.retryable {
		t.Fatalf("repo=%#v err=%v", repo, err)
	}
}
