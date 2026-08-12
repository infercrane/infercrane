package admission

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testPolicy() Policy {
	return Policy{Key: "tenant/endpoint", MaxConcurrency: 1, MaxQueueDepth: 1, QueueTimeout: 30 * time.Millisecond, MaxRequestBytes: 100, MaxOutputTokens: 10, AllowedPriorities: map[string]struct{}{"normal": {}, "high": {}}, Enabled: true}
}

func TestPoolConcurrentAcquireNeverExceedsLimitAndReleaseIsExactlyOnce(t *testing.T) {
	pool := New()
	policy := testPolicy()
	policy.MaxConcurrency = 8
	policy.MaxQueueDepth = 128
	policy.QueueTimeout = 2 * time.Second
	pool.Replace([]Policy{policy})

	var active, maximum atomic.Int64
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 128 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			release, err := pool.Acquire(context.Background(), Request{Key: policy.Key, RequestBytes: 10, OutputTokens: 1})
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			current := active.Add(1)
			for observed := maximum.Load(); current > observed && !maximum.CompareAndSwap(observed, current); observed = maximum.Load() {
			}
			time.Sleep(100 * time.Microsecond)
			active.Add(-1)
			release()
			release()
		}()
	}
	close(start)
	workers.Wait()
	if got := maximum.Load(); got > int64(policy.MaxConcurrency) {
		t.Fatalf("maximum active=%d limit=%d", got, policy.MaxConcurrency)
	}
	for range policy.MaxConcurrency {
		release, err := pool.Acquire(context.Background(), Request{Key: policy.Key, RequestBytes: 10, OutputTokens: 1})
		if err != nil {
			t.Fatalf("lease leaked after stress: %v", err)
		}
		release()
	}
}

func TestPoolCancelledWaiterReleasesQueueSlotExactlyOnce(t *testing.T) {
	pool := New()
	policy := testPolicy()
	policy.QueueTimeout = time.Second
	pool.Replace([]Policy{policy})
	release, err := pool.Acquire(context.Background(), Request{Key: policy.Key, RequestBytes: 10})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, acquireErr := pool.Acquire(ctx, Request{Key: policy.Key, RequestBytes: 10})
		done <- acquireErr
	}()
	time.Sleep(5 * time.Millisecond)
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter error=%v", err)
	}
	// The cancelled waiter must have returned the sole queue slot.
	queued := make(chan error, 1)
	go func() {
		queuedRelease, acquireErr := pool.Acquire(context.Background(), Request{Key: policy.Key, RequestBytes: 10})
		if queuedRelease != nil {
			queuedRelease()
		}
		queued <- acquireErr
	}()
	release()
	if err = <-queued; err != nil {
		t.Fatalf("queue slot leaked after cancellation: %v", err)
	}
}

func TestPoolEnforcesLimitsWithoutOversubscription(t *testing.T) {
	pool := New()
	pool.Replace([]Policy{testPolicy()})
	release, err := pool.Acquire(context.Background(), Request{Key: "tenant/endpoint", RequestBytes: 10, OutputTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan error, 1)
	go func() {
		second, acquireErr := pool.Acquire(context.Background(), Request{Key: "tenant/endpoint", Priority: "high", RequestBytes: 10, OutputTokens: 5})
		if second != nil {
			second()
		}
		queued <- acquireErr
	}()
	time.Sleep(5 * time.Millisecond)
	if _, err = pool.Acquire(context.Background(), Request{Key: "tenant/endpoint", RequestBytes: 10, OutputTokens: 5}); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("overflow error=%v", err)
	}
	release()
	if err = <-queued; err != nil {
		t.Fatalf("queued acquire=%v", err)
	}
}

func TestPoolBoundsRequestOutputPriorityAndWait(t *testing.T) {
	pool := New()
	pool.Replace([]Policy{testPolicy()})
	for _, test := range []struct {
		request Request
		want    error
	}{
		{Request{Key: "tenant/endpoint", RequestBytes: 101}, ErrRequestSize},
		{Request{Key: "tenant/endpoint", RequestBytes: 10, OutputTokens: 11}, ErrOutputLimit},
		{Request{Key: "tenant/endpoint", RequestBytes: 10, Priority: "urgent"}, ErrPriority},
	} {
		if _, err := pool.Acquire(context.Background(), test.request); !errors.Is(err, test.want) {
			t.Fatalf("request=%+v error=%v want=%v", test.request, err, test.want)
		}
	}
	release, _ := pool.Acquire(context.Background(), Request{Key: "tenant/endpoint", RequestBytes: 10})
	defer release()
	if _, err := pool.Acquire(context.Background(), Request{Key: "tenant/endpoint", RequestBytes: 10}); !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("timeout error=%v", err)
	}
}
