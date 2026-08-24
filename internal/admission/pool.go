// Package admission owns bounded, database-free request admission for the
// inference data path. Policy refresh is control-plane work; Acquire performs
// only in-memory synchronization.
package admission

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

var (
	ErrConcurrency  = errors.New("endpoint concurrency and queue capacity are exhausted")
	ErrQueueTimeout = errors.New("endpoint admission queue timed out")
	ErrRequestSize  = errors.New("request exceeds endpoint byte limit")
	ErrOutputLimit  = errors.New("requested output exceeds endpoint token limit")
	ErrPriority     = errors.New("request priority is not allowed")
)

type Policy struct {
	Key               string
	MaxConcurrency    int
	MaxQueueDepth     int
	QueueTimeout      time.Duration
	RequestTimeout    time.Duration
	MaxRequestBytes   int
	MaxOutputTokens   int
	AllowedPriorities map[string]struct{}
	Enabled           bool
	RetryBudget       int
}

type Request struct {
	Key, Priority              string
	RequestBytes, OutputTokens int
}

type Source interface {
	AdmissionPolicies(context.Context) ([]Policy, error)
}

type endpointState struct {
	policy        Policy
	active        int
	waiting       int
	rejected      uint64
	queueTimeouts uint64
	notify        chan struct{}
}

// State is an instantaneous view of admission pressure for one gateway
// instance. It stays in memory so observability never adds database work to
// the inference request path.
type State struct {
	CapacityState string `json:"capacity_state"`
	Scope         string `json:"scope"`
	Active        int    `json:"active"`
	Waiting       int    `json:"waiting"`
	MaxConcurrent int    `json:"max_concurrent"`
	MaxQueueDepth int    `json:"max_queue_depth"`
	Rejected      uint64 `json:"rejected"`
	QueueTimeouts uint64 `json:"queue_timeouts"`
}

type Pool struct {
	mu     sync.Mutex
	states map[string]*endpointState
}

func New() *Pool { return &Pool{states: map[string]*endpointState{}} }

func (p *Pool) Refresh(ctx context.Context, source Source) error {
	if source == nil {
		return errors.New("admission policy source is required")
	}
	policies, err := source.AdmissionPolicies(ctx)
	if err != nil {
		return err
	}
	p.Replace(policies)
	return nil
}

func (p *Pool) Run(ctx context.Context, source Source, interval time.Duration) error {
	if err := p.Refresh(ctx, source); err != nil {
		return err
	}
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = p.Refresh(ctx, source)
		}
	}
}

// Replace atomically publishes a complete policy snapshot. Existing leases
// keep their release function; removed policies become unlimited for new work.
func (p *Pool) Replace(policies []Policy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := make(map[string]*endpointState, len(policies))
	for _, policy := range policies {
		if policy.Key == "" || policy.MaxConcurrency < 1 || policy.MaxQueueDepth < 0 || policy.QueueTimeout <= 0 || policy.MaxRequestBytes < 1 || policy.MaxOutputTokens < 1 {
			continue
		}
		state := p.states[policy.Key]
		if state == nil {
			state = &endpointState{notify: make(chan struct{})}
		}
		state.policy = clonePolicy(policy)
		next[policy.Key] = state
	}
	p.states = next
}

func clonePolicy(policy Policy) Policy {
	copy := policy
	copy.AllowedPriorities = make(map[string]struct{}, len(policy.AllowedPriorities))
	for priority := range policy.AllowedPriorities {
		copy.AllowedPriorities[priority] = struct{}{}
	}
	return copy
}

// Acquire blocks only inside the bounded in-memory queue. It performs no I/O.
func (p *Pool) Acquire(ctx context.Context, request Request) (func(), error) {
	if p == nil {
		return func() {}, nil
	}
	p.mu.Lock()
	state := p.states[request.Key]
	if state == nil || !state.policy.Enabled {
		p.mu.Unlock()
		return func() {}, nil
	}
	policy := state.policy
	if request.RequestBytes > policy.MaxRequestBytes {
		state.rejected++
		p.mu.Unlock()
		return nil, ErrRequestSize
	}
	if request.OutputTokens > policy.MaxOutputTokens {
		state.rejected++
		p.mu.Unlock()
		return nil, ErrOutputLimit
	}
	priority := request.Priority
	if priority == "" {
		priority = "normal"
	}
	if _, allowed := policy.AllowedPriorities[priority]; !allowed {
		state.rejected++
		p.mu.Unlock()
		return nil, ErrPriority
	}
	if state.active < policy.MaxConcurrency {
		state.active++
		p.mu.Unlock()
		return p.release(request.Key, state), nil
	}
	if state.waiting >= policy.MaxQueueDepth {
		state.rejected++
		p.mu.Unlock()
		return nil, ErrConcurrency
	}
	state.waiting++
	p.mu.Unlock()

	timer := time.NewTimer(policy.QueueTimeout)
	defer timer.Stop()
	for {
		p.mu.Lock()
		current := p.states[request.Key]
		if current != state || !state.policy.Enabled {
			state.waiting--
			p.mu.Unlock()
			return func() {}, nil
		}
		if state.active < state.policy.MaxConcurrency {
			state.waiting--
			state.active++
			p.mu.Unlock()
			return p.release(request.Key, state), nil
		}
		notify := state.notify
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			p.removeWaiter(request.Key, state)
			return nil, ctx.Err()
		case <-timer.C:
			p.removeWaiter(request.Key, state)
			p.mu.Lock()
			state.rejected++
			state.queueTimeouts++
			p.mu.Unlock()
			return nil, ErrQueueTimeout
		case <-notify:
		}
	}
}

// State returns saturation independently from liveness/readiness. A saturated
// engine is busy, not necessarily unhealthy.
func (p *Pool) State(key string) (State, bool) {
	if p == nil {
		return State{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.states[key]
	if state == nil || !state.policy.Enabled {
		return State{}, false
	}
	capacity := "accepting"
	if state.active >= state.policy.MaxConcurrency {
		capacity = "queueing"
	}
	if state.active >= state.policy.MaxConcurrency && state.waiting >= state.policy.MaxQueueDepth {
		capacity = "saturated"
	}
	return State{CapacityState: capacity, Scope: "gateway_instance", Active: state.active, Waiting: state.waiting, MaxConcurrent: state.policy.MaxConcurrency, MaxQueueDepth: state.policy.MaxQueueDepth, Rejected: state.rejected, QueueTimeouts: state.queueTimeouts}, true
}

// WritePrometheus exports only bounded aggregate gauges/counters. Endpoint
// names are intentionally omitted to avoid an unbounded label surface.
func (p *Pool) WritePrometheus(w io.Writer) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var active, waiting int
	var rejected, queueTimeouts uint64
	for _, state := range p.states {
		if !state.policy.Enabled {
			continue
		}
		active += state.active
		waiting += state.waiting
		rejected += state.rejected
		queueTimeouts += state.queueTimeouts
	}
	fmt.Fprintf(w, "# TYPE infercrane_admission_active gauge\ninfercrane_admission_active %d\n", active)
	fmt.Fprintf(w, "# TYPE infercrane_admission_waiting gauge\ninfercrane_admission_waiting %d\n", waiting)
	fmt.Fprintf(w, "# TYPE infercrane_admission_rejected_total counter\ninfercrane_admission_rejected_total %d\n", rejected)
	fmt.Fprintf(w, "# TYPE infercrane_admission_queue_timeouts_total counter\ninfercrane_admission_queue_timeouts_total %d\n", queueTimeouts)
}

// RetryBudget returns the current in-memory budget for a qualified endpoint.
func (p *Pool) RetryBudget(key string) int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.states[key]
	if state == nil || !state.policy.Enabled {
		return 0
	}
	return state.policy.RetryBudget
}

// RequestTimeout returns the absolute end-to-end request budget from gateway
// arrival through admission, all retry attempts, and response streaming. A
// zero value keeps compatibility with an absent or disabled policy.
func (p *Pool) RequestTimeout(key string) time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.states[key]
	if state == nil || !state.policy.Enabled {
		return 0
	}
	return state.policy.RequestTimeout
}

func (p *Pool) release(key string, state *endpointState) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			if state.active > 0 {
				state.active--
			}
			close(state.notify)
			state.notify = make(chan struct{})
			p.mu.Unlock()
		})
	}
}

func (p *Pool) removeWaiter(key string, state *endpointState) {
	p.mu.Lock()
	if state.waiting > 0 {
		state.waiting--
	}
	p.mu.Unlock()
}
