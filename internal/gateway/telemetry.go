package gateway

import (
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

var durationBuckets = [...]float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type Telemetry struct {
	requests, failures, active, bytes, durationNanos atomic.Uint64
	durations                                        [len(durationBuckets)]atomic.Uint64
	Extra                                            func(io.Writer)
}
type observedWriter struct {
	http.ResponseWriter
	status int
	bytes  uint64
}

func (w *observedWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *observedWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(body)
	w.bytes += uint64(n)
	return n, err
}
func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (t *Telemetry) Observe(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		t.active.Add(1)
		wrapped := &observedWriter{ResponseWriter: w}
		defer func() {
			duration := time.Since(started)
			t.active.Add(^uint64(0))
			t.requests.Add(1)
			t.bytes.Add(wrapped.bytes)
			t.durationNanos.Add(uint64(duration))
			for i, boundary := range durationBuckets {
				if duration.Seconds() <= boundary {
					t.durations[i].Add(1)
				}
			}
			if wrapped.status >= 400 {
				t.failures.Add(1)
			}
		}()
		next(wrapped, r)
	}
}
func (t *Telemetry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	requests := t.requests.Load()
	fmt.Fprintf(w, "# TYPE infercrane_gateway_requests_total counter\ninfercrane_gateway_requests_total %d\n# TYPE infercrane_gateway_failures_total counter\ninfercrane_gateway_failures_total %d\n# TYPE infercrane_gateway_active_requests gauge\ninfercrane_gateway_active_requests %d\n# TYPE infercrane_gateway_response_bytes_total counter\ninfercrane_gateway_response_bytes_total %d\n", requests, t.failures.Load(), t.active.Load(), t.bytes.Load())
	fmt.Fprintln(w, "# TYPE infercrane_gateway_request_duration_seconds histogram")
	for i, boundary := range durationBuckets {
		fmt.Fprintf(w, "infercrane_gateway_request_duration_seconds_bucket{le=\"%g\"} %d\n", boundary, t.durations[i].Load())
	}
	fmt.Fprintf(w, "infercrane_gateway_request_duration_seconds_bucket{le=\"+Inf\"} %d\ninfercrane_gateway_request_duration_seconds_sum %g\ninfercrane_gateway_request_duration_seconds_count %d\n", requests, float64(t.durationNanos.Load())/float64(time.Second), requests)
	if t.Extra != nil {
		t.Extra(w)
	}
}
