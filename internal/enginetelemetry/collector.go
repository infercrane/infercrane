// Package enginetelemetry reads serving-engine Prometheus metrics from routed
// replicas and persists a bounded, content-free summary per scrape as
// operational measurements. It runs beside the control loops, never on the
// inference request path.
package enginetelemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/metrics"
)

const (
	// DefaultInterval is the scrape cadence. Each scrape persists one summary.
	DefaultInterval = 30 * time.Second
	// TrailingWindow is the longest span a window value covers. A summary
	// compares the current scrape with the oldest retained scrape of the same
	// engine process, so a quiet half minute does not hide recent pressure.
	TrailingWindow = 5 * time.Minute
	maxHistory     = 64
	// validityIntervals bounds how long one summary stays current. Missing
	// two consecutive scrapes turns the evidence stale instead of leaving an
	// old reading apparently live.
	validityIntervals = 3
	maxTargets        = 1000
	scrapeConcurrency = 8
	scrapeTimeout     = 5 * time.Second
)

// Target is one routed, self-hosted serving replica. Credential is the worker
// credential the route already presents to that replica. It stays in memory
// and is never logged or persisted.
type Target struct {
	TenantID, DeploymentID, RevisionID, TargetID, URL string
	Credential                                        string
}

// TargetSource lists scrapeable replicas. The reconciler implements it
// because it owns the per-target credential policy.
type TargetSource interface {
	EngineMetricTargets(context.Context) ([]Target, error)
}

// Recorder persists one bounded batch against the deployment's active
// revision.
type Recorder interface {
	RecordOperationalMeasurements(context.Context, string, string, []domain.OperationalMeasurement) ([]domain.OperationalMeasurement, error)
}

// Collector scrapes every target once per interval. A window value needs two
// readings of the same engine process at the same revision and URL; the
// first reading after start, a rollout, or an engine restart yields gauges
// only.
type Collector struct {
	Targets  TargetSource
	Recorder Recorder
	Client   *http.Client
	Interval time.Duration
	Logger   *slog.Logger
	Now      func() time.Time

	mu      sync.Mutex
	history map[string][]reading
}

type reading struct {
	revisionID, url string
	observedAt      time.Time
	snapshot        metrics.EngineSnapshot
}

// Run collects until the context ends. Failures are logged and retried on the
// next interval.
func (c *Collector) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval())
	defer ticker.Stop()
	for {
		if err := c.Once(ctx); err != nil && ctx.Err() == nil && c.Logger != nil {
			c.Logger.Warn("engine telemetry collection failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Once performs one bounded collection pass. Scrape failures are expected for
// runtimes without a supported metrics endpoint and are only logged at debug
// level; persistence failures are returned.
func (c *Collector) Once(ctx context.Context) error {
	if c.Targets == nil || c.Recorder == nil {
		return errors.New("engine telemetry requires a target source and a recorder")
	}
	var failures []error
	targets, err := c.Targets.EngineMetricTargets(ctx)
	if err != nil {
		if len(targets) == 0 {
			return fmt.Errorf("list engine metric targets: %w", err)
		}
		failures = append(failures, fmt.Errorf("list engine metric targets: %w", err))
	}
	if len(targets) > maxTargets {
		if c.Logger != nil {
			c.Logger.Warn("engine telemetry target list truncated", "targets", len(targets), "limit", maxTargets)
		}
		targets = targets[:maxTargets]
	}
	c.prune(targets)
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	semaphore := make(chan struct{}, scrapeConcurrency)
	for _, target := range targets {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Add(1)
		go func(target Target) {
			defer wg.Done()
			defer func() { <-semaphore }()
			if err := c.collect(ctx, target); err != nil {
				mu.Lock()
				failures = append(failures, err)
				mu.Unlock()
			}
		}(target)
	}
	wg.Wait()
	return errors.Join(failures...)
}

func (c *Collector) collect(ctx context.Context, target Target) error {
	snapshot, err := c.scrape(ctx, target)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Debug("engine metrics unavailable", "deployment_id", target.DeploymentID, "target_id", target.TargetID, "error", err)
		}
		return nil
	}
	observedAt := c.now().UTC()
	key := target.key()
	c.mu.Lock()
	history := append([]reading(nil), c.history[key]...)
	c.mu.Unlock()
	if len(history) > 0 {
		last := history[len(history)-1]
		if last.revisionID != target.RevisionID || last.url != target.URL || !metrics.SameEngineProcess(&last.snapshot, snapshot) {
			history = nil
		}
	}
	for len(history) > 0 && observedAt.Sub(history[0].observedAt) > TrailingWindow {
		history = history[1:]
	}
	var baseline *metrics.EngineSnapshot
	if len(history) > 0 {
		baseline = &history[0].snapshot
	}
	rows, err := metrics.EngineMeasurements(baseline, snapshot, metrics.EngineMeasurementOptions{
		ReplicaID: target.TargetID, ObservedAt: observedAt, TTL: validityIntervals * c.interval(),
	})
	if err != nil {
		return fmt.Errorf("summarize engine metrics for target %s: %w", target.TargetID, err)
	}
	if len(rows) > 0 {
		if _, err = c.Recorder.RecordOperationalMeasurements(ctx, target.TenantID, target.DeploymentID, rows); err != nil {
			// Keep the older baseline so the next window still covers the
			// unrecorded interval.
			return fmt.Errorf("record engine metrics for target %s: %w", target.TargetID, err)
		}
	}
	history = append(history, reading{revisionID: target.RevisionID, url: target.URL, observedAt: observedAt, snapshot: snapshot})
	if len(history) > maxHistory {
		history = history[len(history)-maxHistory:]
	}
	c.mu.Lock()
	if c.history == nil {
		c.history = map[string][]reading{}
	}
	c.history[key] = history
	c.mu.Unlock()
	return nil
}

// key separates one replica bound to two deployments so each deployment keeps
// its own window.
func (t Target) key() string {
	return t.DeploymentID + "\x00" + t.TargetID
}

func (c *Collector) scrape(ctx context.Context, target Target) (metrics.EngineSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, scrapeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(target.URL, "/")+"/metrics", nil)
	if err != nil {
		return metrics.EngineSnapshot{}, err
	}
	if target.Credential != "" {
		request.Header.Set("Authorization", "Bearer "+target.Credential)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: scrapeTimeout}
	}
	response, err := client.Do(request)
	if err != nil {
		return metrics.EngineSnapshot{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return metrics.EngineSnapshot{}, fmt.Errorf("metrics endpoint returned %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, metrics.MaxEnginePayloadBytes+1))
	if err != nil {
		return metrics.EngineSnapshot{}, err
	}
	return metrics.ParseEngine(string(body))
}

// prune forgets baselines for replicas that are no longer routed so the map
// stays bounded by the current target set.
func (c *Collector) prune(targets []Target) {
	current := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		current[target.key()] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.history {
		if _, ok := current[key]; !ok {
			delete(c.history, key)
		}
	}
}

func (c *Collector) interval() time.Duration {
	if c.Interval <= 0 {
		return DefaultInterval
	}
	return c.Interval
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
