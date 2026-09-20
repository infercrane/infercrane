package controlapi

import (
	"testing"
	"time"
)

func TestSandboxPreviewLeaseExpiresAndCannotCrossBoundaries(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	broker := NewSandboxPreviewBroker()
	broker.clock = func() time.Time { return now }
	token, ok := broker.issue("tenant-a", "computer-a", "/provider/lease/secret", now.Add(time.Minute))
	if !ok || token == "" {
		t.Fatal("preview lease was not issued")
	}
	if _, ok = broker.resolve(token, "tenant-b", "computer-a"); ok {
		t.Fatal("preview lease crossed the tenant boundary")
	}
	if _, ok = broker.resolve(token, "tenant-a", "computer-b"); ok {
		t.Fatal("preview lease crossed the computer boundary")
	}
	lease, ok := broker.resolve(token, "tenant-a", "computer-a")
	if !ok || lease.ProviderPath != "/provider/lease/secret" {
		t.Fatalf("preview lease=%#v ok=%v", lease, ok)
	}
	now = now.Add(time.Minute)
	if _, ok = broker.resolve(token, "tenant-a", "computer-a"); ok {
		t.Fatal("expired preview lease remained valid")
	}
}
