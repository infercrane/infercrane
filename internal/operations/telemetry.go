package operations

import (
	"fmt"
	"io"
	"sync/atomic"
)

type Telemetry struct{ claimed, completed, failed, retried, cancelled atomic.Uint64 }

func (t *Telemetry) WritePrometheus(w io.Writer) {
	fmt.Fprintf(w, "# TYPE infercrane_operations_claimed_total counter\ninfercrane_operations_claimed_total %d\n# TYPE infercrane_operations_completed_total counter\ninfercrane_operations_completed_total %d\n# TYPE infercrane_operations_failed_total counter\ninfercrane_operations_failed_total %d\n# TYPE infercrane_operations_retried_total counter\ninfercrane_operations_retried_total %d\n# TYPE infercrane_operations_cancelled_total counter\ninfercrane_operations_cancelled_total %d\n", t.claimed.Load(), t.completed.Load(), t.failed.Load(), t.retried.Load(), t.cancelled.Load())
}
