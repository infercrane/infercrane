package admission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func testPolicy() Policy {
	return Policy{Key: "tenant/endpoint", MaxConcurrency: 1, MaxQueueDepth: 1, QueueTimeout: 30 * time.Millisecond, MaxRequestBytes: 100, MaxOutputTokens: 10, AllowedPriorities: map[string]struct{}{"normal": {}, "high": {}}, Enabled: true}
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
