package provision

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeSkyRunner struct {
	exists          bool
	statusErr       bool
	launches, downs int
}

func (f *fakeSkyRunner) Run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	command := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(command, "launch "):
		f.launches++
		f.exists = true
		return []byte("request 12345678-1234-1234-1234-123456789abc submitted"), nil
	case strings.HasPrefix(command, "down "):
		f.downs++
		f.exists = false
		return []byte("submitted"), nil
	case strings.Contains(command, "--endpoint"):
		if !f.exists {
			return nil, errors.New("not found")
		}
		return []byte("https://worker.example\n"), nil
	case strings.HasPrefix(command, "status") && strings.Contains(command, "infercrane-prod-r0"):
		if f.statusErr {
			return nil, errors.New("SkyPilot API unavailable")
		}
		if !f.exists {
			return []byte(`[]`), nil
		}
		return []byte(`[{"name":"infercrane-prod-r0","status":"UP"}]`), nil
	case command == "status -o json":
		if !f.exists {
			return []byte(`[]`), nil
		}
		return []byte(`[{"name":"infercrane-prod-r0","status":"UP"},{"name":"unmanaged","status":"UP"}]`), nil
	default:
		return nil, errors.New("unexpected command: " + command)
	}
}

func TestEnsureDoesNotLaunchWhenDiscoveryFails(t *testing.T) {
	runner := &fakeSkyRunner{statusErr: true}
	provider := SkyPilot{Runner: runner}
	_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "prod-r0", Model: "model", Cloud: "runpod", GPU: "L40S"})
	if err == nil || runner.launches != 0 {
		t.Fatalf("launches=%d err=%v", runner.launches, err)
	}
}

func TestEnsureReplicaDiscoversExistingResource(t *testing.T) {
	runner := &fakeSkyRunner{}
	provider := SkyPilot{APIKey: "secret", Runner: runner}
	spec := ReplicaSpec{ExternalKey: "prod-r0", Model: "Qwen/Qwen3-8B", Cloud: "runpod", GPU: "L40S"}
	first, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || first.ResourceID != "infercrane-prod-r0" || first.RequestID == "" {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || second.ResourceID != first.ResourceID || runner.launches != 1 {
		t.Fatalf("second=%#v launches=%d err=%v", second, runner.launches, err)
	}
	observation, err := provider.ObserveReplica(context.Background(), second, 8000)
	if err != nil || !observation.Exists || observation.State != "ready" || observation.Endpoint != "https://worker.example" {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
}

func TestDeleteReplicaIsIdempotent(t *testing.T) {
	runner := &fakeSkyRunner{exists: true}
	provider := SkyPilot{Runner: runner}
	handle := ProviderHandle{ResourceID: "infercrane-prod-r0", ExternalKey: "prod-r0"}
	if err := provider.DeleteReplica(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if err := provider.DeleteReplica(context.Background(), handle); err != nil {
		t.Fatal(err)
	}
	if runner.downs != 1 {
		t.Fatalf("down calls=%d, want 1", runner.downs)
	}
}

func TestInventoryFiltersOwnedResources(t *testing.T) {
	provider := SkyPilot{Runner: &fakeSkyRunner{exists: true}}
	resources, err := provider.Inventory(context.Background(), InventoryFilter{Prefix: "infercrane-"})
	if err != nil || len(resources) != 1 || resources[0].ID != "infercrane-prod-r0" {
		t.Fatalf("resources=%#v err=%v", resources, err)
	}
}
