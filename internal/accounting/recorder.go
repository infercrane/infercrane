package accounting

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type Sink interface {
	RecordRequest(context.Context, domain.InferenceRecord) error
}
type record struct {
	domain.InferenceRecord
}
type Recorder struct {
	sink      Sink
	logger    *slog.Logger
	queue     chan record
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	dropped   atomic.Uint64
	persisted atomic.Uint64
	failures  atomic.Uint64
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
func (r *Recorder) RecordRequest(_ context.Context, value domain.InferenceRecord) error {
	item := record{InferenceRecord: value}
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
		err := r.sink.RecordRequest(ctx, item.InferenceRecord)
		cancel()
		if err != nil {
			r.failures.Add(1)
			if r.logger != nil {
				r.logger.Error("persist request accounting", "error", err, "request_id", item.RequestID)
			}
		} else {
			r.persisted.Add(1)
		}
	}
}

func (r *Recorder) WritePrometheus(w io.Writer) {
	fmt.Fprintf(w, "# TYPE infercrane_accounting_queue_depth gauge\ninfercrane_accounting_queue_depth %d\n# TYPE infercrane_accounting_queue_capacity gauge\ninfercrane_accounting_queue_capacity %d\n# TYPE infercrane_accounting_records_persisted_total counter\ninfercrane_accounting_records_persisted_total %d\n# TYPE infercrane_accounting_records_dropped_total counter\ninfercrane_accounting_records_dropped_total %d\n# TYPE infercrane_accounting_persist_failures_total counter\ninfercrane_accounting_persist_failures_total %d\n", len(r.queue), cap(r.queue), r.persisted.Load(), r.dropped.Load(), r.failures.Load())
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
