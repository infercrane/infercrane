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
	for _, expected := range []string{"55%", "WAITING", "Provider capacity: allocating", "3m42s", "attempt 6/120"} {
		if !strings.Contains(line, expected) {
			t.Fatalf("progress %q does not contain %q", line, expected)
		}
	}
}
