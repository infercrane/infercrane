// Package requestquota owns distributed hard request-rate authorization while
// keeping PostgreSQL off the inference data path.
package requestquota

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrExhausted = errors.New("tenant request-rate quota is exhausted")

type Policy struct {
	TenantID             string
	MaxRequestsPerMinute int
}

type Source interface {
	RequestQuotaPolicies(context.Context) ([]Policy, error)
	ReserveRequestQuota(context.Context, string, time.Time, int) (int, error)
}

type tenantState struct {
	limit       int
	tokens      int
	window      time.Time
	lastAttempt time.Time
}

type Pool struct {
	Source   Source
	Interval time.Duration
	Now      func() time.Time

	mu     sync.Mutex
	states map[string]tenantState
}

func New(source Source) *Pool {
	return &Pool{Source: source, Interval: 100 * time.Millisecond, states: map[string]tenantState{}}
}

// Authorize performs no I/O. A missing policy is unlimited; a configured zero
// limit denies every inference request.
func (p *Pool) Authorize(tenant string) error {
	if p == nil || tenant == "" {
		return ErrExhausted
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state, limited := p.states[tenant]
	if !limited {
		return nil
	}
	if state.limit == 0 || state.tokens < 1 {
		return ErrExhausted
	}
	state.tokens--
	p.states[tenant] = state
	return nil
}

func (p *Pool) Refresh(ctx context.Context) error {
	if p == nil || p.Source == nil {
		return errors.New("request quota source is required")
	}
	policies, err := p.Source.RequestQuotaPolicies(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if p.Now != nil {
		now = p.Now().UTC()
	}
	window := now.Truncate(time.Minute)
	next := make(map[string]tenantState, len(policies))
	p.mu.Lock()
	for _, policy := range policies {
		if policy.TenantID == "" || policy.MaxRequestsPerMinute < 0 {
			continue
		}
		state := p.states[policy.TenantID]
		if state.window != window {
			state.tokens = 0
			state.window = window
			state.lastAttempt = time.Time{}
		}
		state.limit = policy.MaxRequestsPerMinute
		next[policy.TenantID] = state
	}
	p.states = next
	p.mu.Unlock()

	return p.refillAll(ctx, window, now)
}

func (p *Pool) refillAll(ctx context.Context, window, now time.Time) error {
	p.mu.Lock()
	tenants := make([]string, 0, len(p.states))
	for tenant := range p.states {
		tenants = append(tenants, tenant)
	}
	p.mu.Unlock()
	for _, tenant := range tenants {
		if err := p.refill(ctx, tenant, window, now); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pool) refill(ctx context.Context, tenant string, window, now time.Time) error {
	p.mu.Lock()
	state, ok := p.states[tenant]
	if ok && state.window != window {
		state.window = window
		state.tokens = 0
		state.lastAttempt = time.Time{}
		p.states[tenant] = state
	}
	if !ok || state.limit == 0 || state.tokens > refillThreshold(state.limit) || now.Sub(state.lastAttempt) < 100*time.Millisecond {
		p.mu.Unlock()
		return nil
	}
	state.lastAttempt = now
	p.states[tenant] = state
	p.mu.Unlock()

	granted, err := p.Source.ReserveRequestQuota(ctx, tenant, window, leaseSize(state.limit))
	if err != nil {
		return err
	}
	p.mu.Lock()
	current, exists := p.states[tenant]
	if exists && current.window == window && current.limit == state.limit {
		current.tokens += granted
		p.states[tenant] = current
	}
	p.mu.Unlock()
	return nil
}

func leaseSize(limit int) int {
	size := limit / 8
	if size < 1 {
		size = 1
	}
	if size > 32 {
		size = 32
	}
	return size
}

func refillThreshold(limit int) int { return leaseSize(limit) / 2 }

func (p *Pool) Run(ctx context.Context) error {
	if err := p.Refresh(ctx); err != nil {
		return err
	}
	refillInterval := p.Interval
	if refillInterval <= 0 {
		refillInterval = 100 * time.Millisecond
	}
	refillTicker := time.NewTicker(refillInterval)
	policyTicker := time.NewTicker(time.Second)
	defer refillTicker.Stop()
	defer policyTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-policyTicker.C:
			_ = p.Refresh(ctx)
		case now := <-refillTicker.C:
			now = now.UTC()
			_ = p.refillAll(ctx, now.Truncate(time.Minute), now)
		}
	}
}
