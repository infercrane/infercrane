package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type transientUnregisterer struct {
	failures int
	attempts int
}

func (u *transientUnregisterer) UnregisterControlPlaneInstance(context.Context, string) error {
	u.attempts++
	if u.attempts <= u.failures {
		return errors.New("transient database failure")
	}
	return nil
}

func TestControlPlaneMembershipWithdrawalRetriesTransientFailure(t *testing.T) {
	membership := &transientUnregisterer{failures: 2}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := unregisterControlPlaneInstance(ctx, membership, "node-a", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if membership.attempts != 3 {
		t.Fatalf("attempts=%d want 3", membership.attempts)
	}
}

func TestControlPlaneMembershipWithdrawalIsBoundedByContext(t *testing.T) {
	membership := &transientUnregisterer{failures: 1000}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := unregisterControlPlaneInstance(ctx, membership, "node-a", time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v want context deadline", err)
	}
	if membership.attempts < 2 {
		t.Fatalf("attempts=%d want retry", membership.attempts)
	}
}
