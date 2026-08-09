package accounting

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

type Sink interface {
	RecordRequest(context.Context, string, string, time.Time, int, float64, string) error
}
type record struct {
	id, deploymentID string
	started          time.Time
	status           int
	latency          float64
	errorType        string
}
type Recorder struct {
	sink    Sink
	logger  *slog.Logger
	queue   chan record
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	dropped atomic.Uint64
}

func New(sink Sink, logger *slog.Logger, capacity, workers int) *Recorder {
	if capacity < 1 {
		capacity = 4096
	}
	if workers < 1 {
		workers = 2
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Recorder{sink: sink, logger: logger, queue: make(chan record, capacity), ctx: ctx, cancel: cancel}
	for range workers {
		r.wg.Add(1)
		go r.run()
	}
	return r
}
func (r *Recorder) RecordRequest(_ context.Context, id, deploymentID string, started time.Time, status int, latency float64, errorType string) error {
	item := record{id: id, deploymentID: deploymentID, started: started, status: status, latency: latency, errorType: errorType}
	select {
	case r.queue <- item:
	default:
		r.dropped.Add(1)
		if r.logger != nil {
			r.logger.Warn("request accounting queue full", "dropped_total", r.dropped.Load())
		}
	}
	return nil
}
func (r *Recorder) run() {
	defer r.wg.Done()
	for item := range r.queue {
		ctx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
		err := r.sink.RecordRequest(ctx, item.id, item.deploymentID, item.started, item.status, item.latency, item.errorType)
		cancel()
		if err != nil && r.logger != nil {
			r.logger.Error("persist request accounting", "error", err, "request_id", item.id)
		}
	}
}
func (r *Recorder) Close(ctx context.Context) error {
	close(r.queue)
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		r.cancel()
		return nil
	case <-ctx.Done():
		r.cancel()
		return ctx.Err()
	}
}
