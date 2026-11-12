package enginetelemetry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/metrics"
)

type staticTargets struct {
	mu      sync.Mutex
	targets []Target
	err     error
}

func (s *staticTargets) EngineMetricTargets(context.Context) ([]Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Target(nil), s.targets...), s.err
}

type recordedBatch struct {
	tenant, deployment string
	rows               []domain.OperationalMeasurement
}

type memoryRecorder struct {
	mu      sync.Mutex
	batches []recordedBatch
	fail    bool
}

func (m *memoryRecorder) RecordOperationalMeasurements(_ context.Context, tenant, deployment string, rows []domain.OperationalMeasurement) ([]domain.OperationalMeasurement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return nil, errors.New("database unavailable")
	}
	m.batches = append(m.batches, recordedBatch{tenant: tenant, deployment: deployment, rows: rows})
	return rows, nil
}

func (m *memoryRecorder) last(t *testing.T) map[string]domain.OperationalMeasurement {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.batches) == 0 {
		t.Fatal("no batch recorded")
	}
	out := map[string]domain.OperationalMeasurement{}
	for _, row := range m.batches[len(m.batches)-1].rows {
		out[row.Name] = row
	}
	return out
}

// engineServer serves fixture scrapes in order, repeating the last one.
type engineServer struct {
	mu            sync.Mutex
	pages         []string
	served        int
	authorization string
}

func newEngineServer(t *testing.T, fixtures ...string) (*engineServer, *httptest.Server) {
	t.Helper()
	engine := &engineServer{}
	for _, name := range fixtures {
		raw, err := os.ReadFile(filepath.Join("..", "metrics", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		engine.pages = append(engine.pages, string(raw))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		engine.mu.Lock()
		defer engine.mu.Unlock()
		engine.authorization = r.Header.Get("Authorization")
		page := engine.pages[min(engine.served, len(engine.pages)-1)]
		engine.served++
		_, _ = w.Write([]byte(page))
	}))
	t.Cleanup(server.Close)
	return engine, server
}

func newCollector(targets TargetSource, recorder Recorder, now *time.Time) *Collector {
	return &Collector{Targets: targets, Recorder: recorder, Interval: 30 * time.Second, Now: func() time.Time { return *now }}
}

func TestCollectorPersistsGaugesThenWindowSummary(t *testing.T) {
	engine, server := newEngineServer(t, "vllm_scrape0.prom", "vllm_scrape1.prom")
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	targets := &staticTargets{targets: []Target{{TenantID: "tenant-1", DeploymentID: "deployment-1", RevisionID: "revision-1", TargetID: "target-1", URL: server.URL + "/", Credential: "icw_secret"}}}
	recorder := &memoryRecorder{}
	collector := newCollector(targets, recorder, &now)

	if err := collector.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if engine.authorization != "Bearer icw_secret" {
		t.Fatalf("authorization=%q", engine.authorization)
	}
	first := recorder.last(t)
	if len(first) != 3 || first[metrics.MeasurementRequestsRunning].Value != 3 || first[metrics.MeasurementKVCacheUsage].Value != 42 {
		t.Fatalf("first scrape must persist gauges only: %+v", first)
	}
	if batch := recorder.batches[0]; batch.tenant != "tenant-1" || batch.deployment != "deployment-1" {
		t.Fatalf("batch identity=%+v", batch)
	}

	now = now.Add(30 * time.Second)
	if err := collector.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := recorder.last(t)
	if second[metrics.MeasurementQueueTimeP95].Value != 2250 || second[metrics.MeasurementPreemptions].Value != 5 || second[metrics.MeasurementSpecDecodeAccept].Value != .4 {
		t.Fatalf("window summary=%+v", second)
	}
	row := second[metrics.MeasurementRequestsRunning]
	if row.ReplicaID != "target-1" || row.Source != "vllm_metrics" || !row.ObservedAt.Equal(now) || !row.ValidUntil.Equal(now.Add(90*time.Second)) {
		t.Fatalf("row envelope=%+v", row)
	}
}

func TestCollectorUsesTrailingWindowBaseline(t *testing.T) {
	_, server := newEngineServer(t, "vllm_scrape0.prom", "vllm_scrape1.prom")
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	targets := &staticTargets{targets: []Target{{TenantID: "t", DeploymentID: "d", RevisionID: "r", TargetID: "target", URL: server.URL}}}
	recorder := &memoryRecorder{}
	collector := newCollector(targets, recorder, &now)
	for _, offset := range []time.Duration{0, 30 * time.Second, time.Minute} {
		now = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC).Add(offset)
		if err := collector.Once(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// The third scrape repeats the second page; its window still starts at
	// the first scrape, so recent pressure stays visible.
	if rows := recorder.last(t); rows[metrics.MeasurementPreemptions].Value != 5 || rows[metrics.MeasurementRequestsCompleted].Value != 100 {
		t.Fatalf("trailing window lost the first scrape: %+v", rows)
	}
	// Once the first scrape is older than the trailing window, the baseline
	// moves forward and the idle span reports measured zeros, not stale load.
	now = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC).Add(TrailingWindow + 10*time.Second)
	if err := collector.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := recorder.last(t)
	if rows[metrics.MeasurementPreemptions].Value != 0 || rows[metrics.MeasurementRequestsCompleted].Value != 0 {
		t.Fatalf("expired baseline was reused: %+v", rows)
	}
	if _, ok := rows[metrics.MeasurementQueueTimeP95]; ok {
		t.Fatalf("idle window reported latency: %+v", rows)
	}
}

func TestCollectorResetsWindowOnRolloutAndKeepsBaselineOnWriteFailure(t *testing.T) {
	_, server := newEngineServer(t, "sglang_scrape0.prom", "sglang_scrape1.prom")
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	target := Target{TenantID: "tenant-1", DeploymentID: "deployment-1", RevisionID: "revision-1", TargetID: "target-1", URL: server.URL}
	targets := &staticTargets{targets: []Target{target}}
	recorder := &memoryRecorder{}
	collector := newCollector(targets, recorder, &now)
	if err := collector.Once(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A rollout changes the active revision: the next scrape must not span it.
	target.RevisionID = "revision-2"
	targets.targets = []Target{target}
	now = now.Add(30 * time.Second)
	if err := collector.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := recorder.last(t)
	for _, name := range []string{metrics.MeasurementQueueTimeP95, metrics.MeasurementPrefixCacheHitRate, metrics.MeasurementRequestsCompleted} {
		if _, window := rows[name]; window {
			t.Fatalf("window value %s crossed a rollout: %+v", name, rows)
		}
	}
	if rows[metrics.MeasurementRequestsRunning].Value != 4 {
		t.Fatalf("gauges must still be recorded after a rollout: %+v", rows)
	}

	// A failed write keeps the old baseline instead of silently dropping it.
	recorder.fail = true
	now = now.Add(30 * time.Second)
	if err := collector.Once(context.Background()); err == nil {
		t.Fatal("persistence failure was swallowed")
	}
	collector.mu.Lock()
	history := collector.history[target.key()]
	collector.mu.Unlock()
	if len(history) != 1 || history[0].revisionID != "revision-2" {
		t.Fatalf("history after failed write=%d", len(history))
	}

	// Targets that leave the route are forgotten.
	targets.targets = nil
	recorder.fail = false
	if err := collector.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	collector.mu.Lock()
	remaining := len(collector.history)
	collector.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("baselines retained for removed targets: %d", remaining)
	}
}

func TestCollectorSkipsUnsupportedEnginesWithoutPersisting(t *testing.T) {
	unsupported := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tgi_queue_size 3\n"))
	}))
	defer unsupported.Close()
	missing := httptest.NewServer(http.NotFoundHandler())
	defer missing.Close()
	now := time.Now().UTC()
	recorder := &memoryRecorder{}
	targets := &staticTargets{targets: []Target{
		{TenantID: "t", DeploymentID: "d", RevisionID: "r", TargetID: "unsupported", URL: unsupported.URL},
		{TenantID: "t", DeploymentID: "d", RevisionID: "r", TargetID: "missing", URL: missing.URL},
	}}
	if err := newCollector(targets, recorder, &now).Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(recorder.batches) != 0 {
		t.Fatalf("unsupported engines produced evidence: %+v", recorder.batches)
	}
}

func TestCollectorUsesPartialTargetListsAndRequiresDependencies(t *testing.T) {
	_, server := newEngineServer(t, "llamacpp_scrape0.prom")
	now := time.Now().UTC()
	recorder := &memoryRecorder{}
	targets := &staticTargets{targets: []Target{{TenantID: "t", DeploymentID: "d", RevisionID: "r", TargetID: "target", URL: server.URL}}, err: errors.New("one deployment could not be resolved")}
	if err := newCollector(targets, recorder, &now).Once(context.Background()); err == nil {
		t.Fatal("partial target failure was swallowed")
	}
	if len(recorder.batches) != 1 {
		t.Fatalf("usable targets were not collected: %d batches", len(recorder.batches))
	}
	if err := (&Collector{}).Once(context.Background()); err == nil {
		t.Fatal("collector without dependencies ran")
	}
}
