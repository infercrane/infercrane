package agent

import (
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/enginetelemetry"
	"github.com/infercrane/infercrane/internal/metrics"
)

type scrape struct {
	at       time.Time
	text     string
	snapshot metrics.EngineSnapshot
}

// metricWindows keeps, per server, the scrapes of one engine process inside
// the control plane's trailing window, so each relay carries the same pair
// of readings the control plane would compare for a scrape it made itself.
type metricWindows struct {
	mu      sync.Mutex
	history map[string][]scrape
	pending map[string]time.Time
}

func newMetricWindows() *metricWindows {
	return &metricWindows{history: map[string][]scrape{}, pending: map[string]time.Time{}}
}

// Observe adds one filtered scrape. A scrape from a restarted engine starts a
// new window.
func (w *metricWindows) Observe(key, text string, at time.Time) error {
	snapshot, err := metrics.ParseEngine(text)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	history := w.history[key]
	if len(history) > 0 && !metrics.SameEngineProcess(&history[len(history)-1].snapshot, snapshot) {
		history = nil
	}
	history = append(history, scrape{at: at, text: text, snapshot: snapshot})
	for len(history) > 1 && at.Sub(history[0].at) > enginetelemetry.TrailingWindow {
		history = history[1:]
	}
	if len(history) > 64 {
		history = history[len(history)-64:]
	}
	w.history[key] = history
	w.pending[key] = at
	return nil
}

// Relays returns one relay per server with an unsent scrape, and the scrape
// times to pass to Sent once the control plane accepted them.
func (w *metricWindows) Relays(now time.Time) ([]agentprotocol.MetricRelay, map[string]time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []agentprotocol.MetricRelay{}
	sent := map[string]time.Time{}
	for key, pending := range w.pending {
		history := w.history[key]
		if pending.IsZero() || len(history) == 0 {
			continue
		}
		sent[key] = pending
		current := history[len(history)-1]
		relay := agentprotocol.MetricRelay{ServerKey: key, Current: agentprotocol.Exposition{AgeMS: now.Sub(current.at).Milliseconds(), Text: current.text}}
		if len(history) > 1 {
			baseline := history[0]
			relay.Baseline = &agentprotocol.Exposition{AgeMS: now.Sub(baseline.at).Milliseconds(), Text: baseline.text}
		}
		out = append(out, relay)
	}
	return out, sent
}

// Sent marks relayed scrapes as delivered so each is relayed once. A newer
// scrape taken meanwhile stays pending.
func (w *metricWindows) Sent(sent map[string]time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, at := range sent {
		if w.pending[key].Equal(at) {
			delete(w.pending, key)
		}
	}
}

// Forget drops a server that disappeared.
func (w *metricWindows) Forget(key string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.history, key)
	delete(w.pending, key)
}
