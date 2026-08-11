package external

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestBudgetPoolNeverAuthorizesBeyondLease(t *testing.T) {
	pool := NewBudgetPool()
	if err := pool.Add(domain.ExternalBudgetLease{PolicyID: "policy", Requests: 10, ReservedCostMicrousd: 1000, MaxRequestCostMicrousd: 100}); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int64
	var wait sync.WaitGroup
	for range 64 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := pool.Authorize("policy"); err == nil {
				accepted.Add(1)
			}
		}()
	}
	wait.Wait()
	if accepted.Load() != 10 || pool.Remaining("policy") != 0 {
		t.Fatalf("accepted=%d remaining=%d", accepted.Load(), pool.Remaining("policy"))
	}
}

func TestBudgetPoolPrefetchesWithoutPuttingStorageOnAuthorizePath(t *testing.T) {
	pool := NewBudgetPool()
	if err := pool.Add(domain.ExternalBudgetLease{PolicyID: "policy", Requests: 2, ReservedCostMicrousd: 200, MaxRequestCostMicrousd: 100}); err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{}, 1)
	pool.RegisterRefill("policy", 1, func(context.Context) (domain.ExternalBudgetLease, error) {
		called <- struct{}{}
		return domain.ExternalBudgetLease{PolicyID: "policy", Requests: 4, ReservedCostMicrousd: 400, MaxRequestCostMicrousd: 100}, nil
	})
	if _, err := pool.Authorize("policy"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("background refill was not requested")
	}
	deadline := time.Now().Add(time.Second)
	for pool.Remaining("policy") < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pool.Remaining("policy") != 5 {
		t.Fatalf("remaining=%d", pool.Remaining("policy"))
	}
}
