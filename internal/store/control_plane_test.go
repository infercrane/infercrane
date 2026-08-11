package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func TestControlPlaneMembershipAllowsOverlapAndRejectsIncompatibleLiveVersion(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	liveFor := time.Minute
	if err := s.RegisterControlPlaneInstance(ctx, domain.ControlPlaneInstance{ID: "node-n", BinaryVersion: "1.6.0", ProtocolMin: 1, ProtocolMax: 2}, liveFor); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterControlPlaneInstance(ctx, domain.ControlPlaneInstance{ID: "node-n-plus-one", BinaryVersion: "1.7.0", ProtocolMin: 2, ProtocolMax: 3}, liveFor); err != nil {
		t.Fatalf("overlapping rolling upgrade rejected: %v", err)
	}
	err := s.RegisterControlPlaneInstance(ctx, domain.ControlPlaneInstance{ID: "node-future", BinaryVersion: "3.0.0", ProtocolMin: 4, ProtocolMax: 4}, liveFor)
	if err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("disjoint mixed version accepted: %v", err)
	}
	instances, err := s.ControlPlaneInstances(ctx, liveFor)
	if err != nil || len(instances) != 2 {
		t.Fatalf("instances=%#v err=%v", instances, err)
	}
	if err = s.HeartbeatControlPlaneInstance(ctx, "node-n"); err != nil {
		t.Fatal(err)
	}
	if err = s.UnregisterControlPlaneInstance(ctx, "node-n"); err != nil {
		t.Fatal(err)
	}
	instances, err = s.ControlPlaneInstances(ctx, liveFor)
	if err != nil || len(instances) != 1 || instances[0].ID != "node-n-plus-one" {
		t.Fatalf("instances=%#v err=%v", instances, err)
	}
}

func TestControlPlaneMembershipIgnoresExpiredInstanceDuringCompatibilityCheck(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	if _, err := s.ExecContext(ctx, `INSERT INTO control_plane_instances(instance_id,binary_version,protocol_min,protocol_max,started_at,heartbeat_at) VALUES(?,?,?,?,?,?)`, "expired", "old", 1, 1, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterControlPlaneInstance(ctx, domain.ControlPlaneInstance{ID: "current", BinaryVersion: "new", ProtocolMin: 2, ProtocolMax: 2}, time.Minute); err != nil {
		t.Fatalf("expired member blocked startup: %v", err)
	}
}
