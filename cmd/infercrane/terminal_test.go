package main

import (
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestOperationProgressExplainsDurableState(t *testing.T) {
	created := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	line := renderOperationProgress(domain.Operation{Status: "waiting", Progress: 55, Message: "Provider capacity: allocating", Attempt: 6, MaxAttempts: 120, CreatedAt: created}, created.Add(3*time.Minute+42*time.Second))
	for _, expected := range []string{"55%", "WAITING FOR CAPACITY", "Provider capacity: allocating", "3m42s", "check 6/120"} {
		if !strings.Contains(line, expected) {
			t.Fatalf("progress %q does not contain %q", line, expected)
		}
	}
}

func TestProgressSuppressesGenericRetryLeaseButKeepsWaitingHeartbeat(t *testing.T) {
	now := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	waiting := domain.Operation{Status: "waiting", Progress: 55, Message: "provider is allocating capacity", Attempt: 1}
	if !shouldRenderOperationProgress(nil, waiting, time.Time{}, now) {
		t.Fatal("initial meaningful waiting state was suppressed")
	}
	running := domain.Operation{Status: "running", Progress: 55, Message: "running", Attempt: 2}
	if shouldRenderOperationProgress(&waiting, running, now, now.Add(10*time.Second)) {
		t.Fatal("generic retry lease should not produce a duplicate progress row")
	}
	waiting.Attempt = 2
	if shouldRenderOperationProgress(&waiting, waiting, now, now.Add(20*time.Second)) {
		t.Fatal("unchanged waiting state printed before heartbeat interval")
	}
	if !shouldRenderOperationProgress(&waiting, waiting, now, now.Add(operationProgressHeartbeat)) {
		t.Fatal("waiting heartbeat was suppressed")
	}
}
