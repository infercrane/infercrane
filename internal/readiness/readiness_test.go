package readiness

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGateFailsClosedUntilFirstSuccess(t *testing.T) {
	gate := &Gate{Probe: func(context.Context) error { return errors.New("database unavailable") }, StaleAfter: 30 * time.Second}
	if err := gate.Check(context.Background()); err == nil {
		t.Fatal("gate reported ready before its dependency succeeded")
	}
}

func TestGateToleratesOnlyBoundedInterruption(t *testing.T) {
	now := time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)
	failing := false
	gate := &Gate{
		Probe: func(context.Context) error {
			if failing {
				return errors.New("database unavailable")
			}
			return nil
		},
		StaleAfter: 30 * time.Second,
		Now:        func() time.Time { return now },
	}
	if err := gate.Check(context.Background()); err != nil {
		t.Fatalf("initial healthy probe: %v", err)
	}
	failing = true
	now = now.Add(20 * time.Second)
	if err := gate.Check(context.Background()); err != nil {
		t.Fatalf("short dependency interruption removed a previously healthy instance: %v", err)
	}
	now = now.Add(11 * time.Second)
	if err := gate.Check(context.Background()); err == nil {
		t.Fatal("gate remained ready after the stale window expired")
	}
}

func TestGateRecoversAndRefreshesHealthyWindow(t *testing.T) {
	now := time.Date(2026, 8, 30, 20, 0, 0, 0, time.UTC)
	failing := false
	gate := &Gate{Probe: func(context.Context) error {
		if failing {
			return errors.New("database unavailable")
		}
		return nil
	}, StaleAfter: 30 * time.Second, Now: func() time.Time { return now }}
	if err := gate.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(25 * time.Second)
	if err := gate.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	failing = true
	now = now.Add(25 * time.Second)
	if err := gate.Check(context.Background()); err != nil {
		t.Fatalf("recovered probe did not refresh the healthy window: %v", err)
	}
}
