// Package operations executes durable, leased control-plane operations.
package operations

import (
	"context"
	"errors"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type Repository interface {
	ClaimOperation(context.Context, string, time.Duration) (domain.Operation, error)
	HeartbeatOperation(context.Context, string, string, time.Duration) error
	Operation(context.Context, string) (domain.Operation, error)
	CompleteClaimedOperation(context.Context, string, string, string) error
	FailClaimedOperation(context.Context, string, string, string, string, bool, time.Time) error
	CancelClaimedOperation(context.Context, string, string, string) error
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
	handler := w.Handlers[op.Kind]
	if handler == nil {
		if w.Telemetry != nil {
			w.Telemetry.failed.Add(1)
		}
		return true, w.Repository.FailClaimedOperation(ctx, op.ID, w.Owner, "unsupported_operation", "no handler registered for "+op.Kind, false, w.now())
	}
	handlerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	go w.maintainLease(handlerCtx, cancel, op.ID, lease, heartbeatDone)
	result, handlerErr := handler(handlerCtx, op)
	cancel()
	<-heartbeatDone
	current, currentErr := w.Repository.Operation(context.WithoutCancel(ctx), op.ID)
	if currentErr == nil && current.CancelRequested {
		if w.Telemetry != nil {
			w.Telemetry.cancelled.Add(1)
		}
		return true, w.Repository.CancelClaimedOperation(context.WithoutCancel(ctx), op.ID, w.Owner, "cancelled at safe execution boundary")
	}
	if handlerErr == nil {
		if w.Telemetry != nil {
			w.Telemetry.completed.Add(1)
		}
		return true, w.Repository.CompleteClaimedOperation(context.WithoutCancel(ctx), op.ID, w.Owner, result)
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
	return true, w.Repository.FailClaimedOperation(context.WithoutCancel(ctx), op.ID, w.Owner, code, handlerErr.Error(), retryable, next)
}

func (w Worker) maintainLease(ctx context.Context, cancel context.CancelFunc, id string, lease time.Duration, done chan<- struct{}) {
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
			if err := w.Repository.HeartbeatOperation(ctx, id, w.Owner, lease); err != nil {
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
		return max
	}
	return delay
}
func (w Worker) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}
func Retryable(code string, err error) error { return Failure{Code: code, Retryable: true, Err: err} }
func Permanent(code string, err error) error { return Failure{Code: code, Retryable: false, Err: err} }
