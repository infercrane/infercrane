package provision

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

type fakeSkyRunner struct {
	exists          bool
	statusErr       bool
	missingErr      bool
	missingSuccess  bool
	statusPrefix    string
	requestID       string
	requestState    string
	endpoint        string
	launchTask      string
	launches, downs int
}

func (f *fakeSkyRunner) Run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	command := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(command, "api status "):
		if f.requestID == "" {
			return []byte(`[]`), nil
		}
		return []byte(`[{"request_id":"` + f.requestID + `","status":"` + f.requestState + `"}]`), nil
	case strings.HasPrefix(command, "launch "):
		f.launches++
		f.exists = true
		contents, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return nil, err
		}
		f.launchTask = string(contents)
		return []byte("request 12345678-1234-1234-1234-123456789abc submitted"), nil
	case strings.HasPrefix(command, "down "):
		f.downs++
		f.exists = false
		return []byte("submitted"), nil
	case strings.Contains(command, "--endpoint"):
		if !f.exists {
			return nil, errors.New("not found")
		}
		endpoint := f.endpoint
		if endpoint == "" {
			endpoint = "https://worker.example"
		}
		return []byte(endpoint + "\n"), nil
	case strings.HasPrefix(command, "status") && strings.Contains(command, "infercrane-prod-r0"):
		if f.statusErr {
			return nil, errors.New("SkyPilot API unavailable")
		}
		if !f.exists {
			if f.missingErr {
				return []byte("Cluster 'infercrane-prod-r0' not found."), errors.New("exit status 1")
			}
			if f.missingSuccess {
				return []byte("Cluster 'infercrane-prod-r0' not found."), nil
			}
			return []byte(`[]`), nil
		}
		return []byte(f.statusPrefix + `[{"name":"infercrane-prod-r0","status":"UP"}]`), nil
	case command == "status -o json":
		if !f.exists {
			return []byte(`[]`), nil
		}
		return []byte(`[{"name":"infercrane-prod-r0","status":"UP"},{"name":"unmanaged","status":"UP"}]`), nil
	default:
		return nil, errors.New("unexpected command: " + command)
	}
}

func TestEnsureUsesPinnedCompatibleRuntimeByDefault(t *testing.T) {
	runner := &fakeSkyRunner{}
	provider := SkyPilot{APIKey: "secret", Runner: runner}
	_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "prod-r0", Model: "model", Cloud: "runpod", GPU: "L40S"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runner.launchTask, "vllm=="+defaultVLLMVersion) || !strings.Contains(runner.launchTask, "transformers=="+defaultTransformersVersion) {
		t.Fatalf("launch task does not pin compatible default runtime: %s", runner.launchTask)
	}
}

func TestEnsureDoesNotConstrainExplicitRuntimeDependencies(t *testing.T) {
	runner := &fakeSkyRunner{}
	provider := SkyPilot{APIKey: "secret", Runner: runner}
	_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "prod-r0", Model: "model", Cloud: "runpod", GPU: "L40S", RuntimeVersion: "0.10.2"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runner.launchTask, "vllm==0.10.2") || strings.Contains(runner.launchTask, "transformers==") {
		t.Fatalf("launch task unexpectedly constrains explicit runtime: %s", runner.launchTask)
	}
}

func TestEnsureDoesNotRelaunchActiveRequest(t *testing.T) {
	runner := &fakeSkyRunner{requestID: "request-1", requestState: "RUNNING"}
	provider := SkyPilot{APIKey: "secret", Runner: runner}
	handle, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "prod-r0", RequestID: "request-1", Model: "model", Cloud: "runpod", GPU: "L40S"})
	if err != nil || runner.launches != 0 || handle.RequestID != "request-1" {
		t.Fatalf("handle=%#v launches=%d err=%v", handle, runner.launches, err)
	}
}

func TestRunCommandPinsImmutableModelRevision(t *testing.T) {
	command := runCommand("Qwen/Qwen3-8B", "0123456789abcdef0123456789abcdef01234567", 8000, nil)
	if !strings.Contains(command, "--revision '0123456789abcdef0123456789abcdef01234567'") {
		t.Fatalf("command does not pin revision: %s", command)
	}
}

func TestEnsureRelaunchesMissingOrFailedRequest(t *testing.T) {
	for _, state := range []string{"", "FAILED"} {
		runner := &fakeSkyRunner{requestID: "request-1", requestState: state}
		if state == "" {
			runner.requestID = ""
		}
		provider := SkyPilot{APIKey: "secret", Runner: runner}
		_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "prod-r0", RequestID: "request-1", Model: "model", Cloud: "runpod", GPU: "L40S"})
		if err != nil || runner.launches != 1 {
			t.Fatalf("state=%q launches=%d err=%v", state, runner.launches, err)
		}
	}
}

func TestEnsureTreatsSuccessfulMissingClusterMessageAsAbsent(t *testing.T) {
	runner := &fakeSkyRunner{missingSuccess: true}
	provider := SkyPilot{APIKey: "secret", Runner: runner}
	_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "prod-r0", Model: "model", Cloud: "runpod", GPU: "L40S"})
	if err != nil || runner.launches != 1 {
		t.Fatalf("launches=%d err=%v", runner.launches, err)
	}
}

func TestEnsureTreatsExplicitMissingClusterAsAbsent(t *testing.T) {
	runner := &fakeSkyRunner{missingErr: true}
	provider := SkyPilot{APIKey: "secret", Runner: runner}
	_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "prod-r0", Model: "model", Cloud: "runpod", GPU: "L40S"})
	if err != nil || runner.launches != 1 {
		t.Fatalf("launches=%d err=%v", runner.launches, err)
	}
}

func TestObserveParsesSkyPilotNoticeBeforeJSON(t *testing.T) {
	runner := &fakeSkyRunner{exists: true, statusPrefix: "Connecting to SkyPilot API server.\n"}
	provider := SkyPilot{Runner: runner}
	observation, err := provider.ObserveReplica(context.Background(), ProviderHandle{ResourceID: "infercrane-prod-r0"}, 8000)
	if err != nil || !observation.Exists || observation.Endpoint == "" {
		t.Fatalf("observation=%#v err=%v", observation, err)
	}
}

func TestObserveNormalizesBareSkyPilotEndpoint(t *testing.T) {
	runner := &fakeSkyRunner{exists: true, endpoint: "91.199.227.82:16889"}
	provider := SkyPilot{Runner: runner}
	observation, err := provider.ObserveReplica(context.Background(), ProviderHandle{ResourceID: "infercrane-prod-r0"}, 8000)
	if err != nil || observation.Endpoint != "http://91.199.227.82:16889" {
		t.Fatalf("observation=%#v err=%v", observation, err)
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
