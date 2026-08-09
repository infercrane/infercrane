package operations

import (
	"bytes"
	"strings"
	"testing"
)

func TestTelemetryExportsCounters(t *testing.T) {
	telemetry := &Telemetry{}
	telemetry.claimed.Add(2)
	telemetry.completed.Add(1)
	var output bytes.Buffer
	telemetry.WritePrometheus(&output)
	if !strings.Contains(output.String(), "infercrane_operations_claimed_total 2") || !strings.Contains(output.String(), "infercrane_operations_completed_total 1") {
		t.Fatalf("metrics=%s", output.String())
	}
}
