// Package operations executes durable, leased control-plane operations.
package operations

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type Repository interface {
	ClaimOperation(context.Context, string, time.Duration) (domain.Operation, error)
	StartClaimedOperation(context.Context, string, string, int64) error
	HeartbeatOperation(context.Context, string, string, int64, time.Duration) error
	Operation(context.Context, string) (domain.Operation, error)
	CompleteClaimedOperation(context.Context, string, string, int64, string) error
	FailClaimedOperation(context.Context, string, string, int64, string, string, bool, time.Time) error
	CancelClaimedOperation(context.Context, string, string, int64, string) error
}
type Handler func(context.Context, domain.Operation) (string, error)
type Failure struct {
	Code      string
	Retryable bool
	Err       error
}

func (f Failure) Error() string {
	if f.Err != nil {
		return f.Err.Error()
	}
	return f.Code
}
func (f Failure) Unwrap() error { return f.Err }

type Worker struct {
	Repository                                   Repository
	Handlers                                     map[string]Handler
	Owner                                        string
	Lease, PollInterval, BaseBackoff, MaxBackoff time.Duration
	Now                                          func() time.Time
	Telemetry                                    *Telemetry
	Jitter                                       func(time.Duration) time.Duration
}

func (w Worker) Run(ctx context.Context) error {
	poll := w.PollInterval
	if poll <= 0 {
		poll = time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		worked, err := w.Once(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			worked = false
		}
		if worked {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (w Worker) Once(ctx context.Context) (bool, error) {
	if w.Repository == nil || w.Owner == "" {
		return false, errors.New("operation repository and worker owner are required")
	}
	lease := w.Lease
	if lease <= 0 {
		lease = 30 * time.Second
	}
	op, err := w.Repository.ClaimOperation(ctx, w.Owner, lease)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if w.Telemetry != nil {
		w.Telemetry.claimed.Add(1)
	}
	if err := w.Repository.StartClaimedOperation(ctx, op.ID, w.Owner, op.LeaseGeneration); err != nil {
		return true, err
	}
	op.Status = "running"
	if op.CancelRequested {
		if cleanup := w.Handlers[op.Kind+".cancel"]; cleanup != nil {
			if _, cleanupErr := cleanup(ctx, op); cleanupErr != nil {
				return true, w.fail(ctx, op, cleanupErr)
			}
		}
		if w.Telemetry != nil {
			w.Telemetry.cancelled.Add(1)
		}
		return true, w.Repository.CancelClaimedOperation(ctx, op.ID, w.Owner, op.LeaseGeneration, "cancellation cleanup completed")
	}
	handler := w.Handlers[op.Kind]
	if handler == nil {
		if w.Telemetry != nil {
			w.Telemetry.failed.Add(1)
		}
		return true, w.Repository.FailClaimedOperation(ctx, op.ID, w.Owner, op.LeaseGeneration, "unsupported_operation", "no handler registered for "+op.Kind, false, w.now())
	}
	handlerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go w.maintainLease(handlerCtx, cancel, op.ID, op.LeaseGeneration, lease, heartbeatDone)
	result, handlerErr := handler(handlerCtx, op)
	cancel()
	<-heartbeatDone
	current, currentErr := w.Repository.Operation(context.WithoutCancel(ctx), op.ID)
	if currentErr == nil && current.CancelRequested {
		if cleanup := w.Handlers[op.Kind+".cancel"]; cleanup != nil {
			if _, cleanupErr := cleanup(context.WithoutCancel(ctx), op); cleanupErr != nil {
				return true, w.fail(context.WithoutCancel(ctx), op, cleanupErr)
			}
		}
		if w.Telemetry != nil {
			w.Telemetry.cancelled.Add(1)
		}
		return true, w.Repository.CancelClaimedOperation(context.WithoutCancel(ctx), op.ID, w.Owner, op.LeaseGeneration, "cancelled at safe execution boundary")
	}
	if handlerErr == nil {
		if w.Telemetry != nil {
			w.Telemetry.completed.Add(1)
		}
		return true, w.Repository.CompleteClaimedOperation(context.WithoutCancel(ctx), op.ID, w.Owner, op.LeaseGeneration, result)
	}
	code, retryable := "operation_failed", false
	var failure Failure
	if errors.As(handlerErr, &failure) {
		if failure.Code != "" {
			code = failure.Code
		}
		retryable = failure.Retryable
	}
	if op.Attempt >= op.MaxAttempts {
		retryable = false
		code = "attempts_exhausted"
	}
	if w.Telemetry != nil {
		if retryable {
			w.Telemetry.retried.Add(1)
		} else {
			w.Telemetry.failed.Add(1)
		}
	}
	next := w.now().Add(w.backoff(op.Attempt))
	return true, w.Repository.FailClaimedOperation(context.WithoutCancel(ctx), op.ID, w.Owner, op.LeaseGeneration, code, handlerErr.Error(), retryable, next)
}

func (w Worker) fail(ctx context.Context, op domain.Operation, handlerErr error) error {
	code, retryable := "operation_failed", false
	var failure Failure
	if errors.As(handlerErr, &failure) {
		if failure.Code != "" {
			code = failure.Code
		}
		retryable = failure.Retryable
	}
	if op.Attempt >= op.MaxAttempts {
		retryable = false
		code = "attempts_exhausted"
	}
	next := w.now().Add(w.backoff(op.Attempt))
	return w.Repository.FailClaimedOperation(ctx, op.ID, w.Owner, op.LeaseGeneration, code, handlerErr.Error(), retryable, next)
}

func (w Worker) maintainLease(ctx context.Context, cancel context.CancelFunc, id string, generation int64, lease time.Duration, done chan<- struct{}) {
	defer close(done)
	interval := lease / 3
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.Repository.HeartbeatOperation(ctx, id, w.Owner, generation, lease); err != nil {
				cancel()
				return
			}
			op, err := w.Repository.Operation(ctx, id)
			if err != nil || op.CancelRequested {
				cancel()
				return
			}
		}
	}
}
func (w Worker) backoff(attempt int) time.Duration {
	base := w.BaseBackoff
	if base <= 0 {
		base = time.Second
	}
	max := w.MaxBackoff
	if max <= 0 {
		max = time.Minute
	}
	delay := base
	for i := 1; i < attempt && delay < max; i++ {
		delay *= 2
	}
	if delay > max {
		delay = max
	}
	if w.Jitter != nil {
		return w.Jitter(delay)
	}
	return randomJitter(delay)
}

func randomJitter(delay time.Duration) time.Duration {
	// Full determinism is available to tests through Worker.Jitter. Production uses
	// a bounded ±25% spread so replicas do not synchronize provider retries.
	n, err := rand.Int(rand.Reader, big.NewInt(501))
	if err != nil {
		return delay
	}
	permille := int64(750) + n.Int64()
	return time.Duration(int64(delay) * permille / 1000)
}
func (w Worker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}
func Retryable(code string, err error) error { return Failure{Code: code, Retryable: true, Err: err} }
func Permanent(code string, err error) error { return Failure{Code: code, Retryable: false, Err: err} }
