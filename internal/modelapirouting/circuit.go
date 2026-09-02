package modelapirouting

import (
	"sync"
	"time"
)

// CandidateCircuit is deliberately request-path local. Durable health and
// qualification remain control-plane evidence; this circuit only prevents a
// recently failing target from receiving more canary traffic between route
// publications.
type CandidateCircuit interface {
	Allow(candidateID string, at time.Time) bool
	Observe(candidateID string, success bool, at time.Time)
}

type circuitState struct {
	consecutiveFailures int
	openUntil           time.Time
}

type CircuitBreaker struct {
	mu               sync.Mutex
	states           map[string]circuitState
	failureThreshold int
	openFor          time.Duration
}

func NewCircuitBreaker(failureThreshold int, openFor time.Duration) *CircuitBreaker {
	if failureThreshold < 1 {
		failureThreshold = 3
	}
	if openFor <= 0 {
		openFor = 30 * time.Second
	}
	return &CircuitBreaker{states: make(map[string]circuitState), failureThreshold: failureThreshold, openFor: openFor}
}

func (b *CircuitBreaker) Allow(candidateID string, at time.Time) bool {
	if b == nil || candidateID == "" {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.states[candidateID]
	return state.openUntil.IsZero() || !at.Before(state.openUntil)
}

func (b *CircuitBreaker) Observe(candidateID string, success bool, at time.Time) {
	if b == nil || candidateID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.states[candidateID]
	if success {
		delete(b.states, candidateID)
		return
	}
	state.consecutiveFailures++
	if state.consecutiveFailures >= b.failureThreshold {
		state.openUntil = at.UTC().Add(b.openFor)
		state.consecutiveFailures = 0
	}
	b.states[candidateID] = state
}
