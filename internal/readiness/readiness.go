package readiness

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Probe is a dependency check used to decide whether an instance may receive
// traffic.
type Probe func(context.Context) error

// Gate keeps a previously healthy instance routable through a short dependency
// interruption. It never reports ready before the probe has succeeded, and it
// fails closed once the bounded stale window expires.
type Gate struct {
	Probe      Probe
	StaleAfter time.Duration
	Now        func() time.Time

	mu          sync.Mutex
	lastSuccess time.Time
}

func (g *Gate) Check(ctx context.Context) error {
	if g == nil || g.Probe == nil {
		return nil
	}
	now := time.Now
	if g.Now != nil {
		now = g.Now
	}
	if err := g.Probe(ctx); err == nil {
		g.mu.Lock()
		g.lastSuccess = now()
		g.mu.Unlock()
		return nil
	} else {
		g.mu.Lock()
		lastSuccess := g.lastSuccess
		g.mu.Unlock()
		if !lastSuccess.IsZero() && g.StaleAfter > 0 && now().Sub(lastSuccess) <= g.StaleAfter {
			return nil
		}
		return errors.Join(errors.New("readiness dependency unavailable"), err)
	}
}
