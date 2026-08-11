package requestquota

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeSource struct {
	mu       sync.Mutex
	policies []Policy
	reserved int
	calls    int
}

func (f *fakeSource) RequestQuotaPolicies(context.Context) ([]Policy, error) {
	return append([]Policy(nil), f.policies...), nil
}
func (f *fakeSource) ReserveRequestQuota(_ context.Context, _ string, _ time.Time, amount int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	remaining := 3 - f.reserved
	if remaining < amount {
		amount = remaining
	}
	if amount < 0 {
		amount = 0
	}
	f.reserved += amount
	return amount, nil
}

func TestPoolAuthorizesOnlyPrefetchedHardLeaseWithoutIO(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	source := &fakeSource{policies: []Policy{{TenantID: "team", MaxRequestsPerMinute: 3}}}
	pool := New(source)
	pool.Now = func() time.Time { return now }
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	source.mu.Lock()
	calls := source.calls
	source.mu.Unlock()
	if err := pool.Authorize("unconfigured"); err != nil {
		t.Fatalf("unconfigured tenant should be unlimited: %v", err)
	}
	if err := pool.Authorize("team"); err != nil {
		t.Fatal(err)
	}
	if err := pool.Authorize("team"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("second request should await the next lease, got %v", err)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.calls != calls {
		t.Fatalf("Authorize performed source I/O: calls=%d want=%d", source.calls, calls)
	}
}

func TestZeroQuotaFailsClosed(t *testing.T) {
	source := &fakeSource{policies: []Policy{{TenantID: "blocked", MaxRequestsPerMinute: 0}}}
	pool := New(source)
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := pool.Authorize("blocked"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("zero quota accepted request: %v", err)
	}
}

func TestPolicyChangeToZeroRevokesPreviouslyLeasedTokens(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	source := &fakeSource{policies: []Policy{{TenantID: "team", MaxRequestsPerMinute: 16}}}
	pool := New(source)
	pool.Now = func() time.Time { return now }
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	source.policies = []Policy{{TenantID: "team", MaxRequestsPerMinute: 0}}
	if err := pool.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := pool.Authorize("team"); !errors.Is(err, ErrExhausted) {
		t.Fatalf("zero policy accepted a previously leased token: %v", err)
	}
}
