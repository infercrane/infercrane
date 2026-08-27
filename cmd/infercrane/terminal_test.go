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

func TestOperationPhaseDoesNotMislabelAmbiguousRuntimeStartupAsArtifactPreparation(t *testing.T) {
	operation := domain.Operation{
		Status:   "waiting",
		Progress: 70,
		Message:  "provider endpoint is assigned; vllm may be pulling artifacts, initializing, or restarting",
	}
	if got := operationPhase(operation); got != "STARTING RUNTIME" {
		t.Fatalf("operation phase = %q, want STARTING RUNTIME", got)
	}
}

func TestOperationPhasePrefersDurableStepOverMessageHeuristics(t *testing.T) {
	operation := domain.Operation{
		Status:      "waiting",
		Progress:    70,
		CurrentStep: "replica.0.runtime",
		Message:     "provider capacity may still be changing",
	}
	if got := operationPhase(operation); got != "STARTING RUNTIME" {
		t.Fatalf("operation phase = %q, want STARTING RUNTIME", got)
	}
}

func TestOperationPhaseDoesNotMislabelProviderDeletionAsCapacityWait(t *testing.T) {
	operation := domain.Operation{
		Kind:     "deployment.delete",
		Status:   "waiting",
		Progress: 0,
		Message:  "provider resource deletion is pending",
	}
	if got := operationPhase(operation); got != "DELETING" {
		t.Fatalf("operation phase = %q, want DELETING", got)
	}
	operation.Status = "succeeded"
	operation.Progress = 100
	operation.Message = "completed"
	if got := operationPhase(operation); got != "DELETED" {
		t.Fatalf("completed operation phase = %q, want DELETED", got)
	}
}
