package provision

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

type fakeGCPRunner struct {
	instance         gcpInstance
	external         string
	creates, deletes int
	loseCreate       bool
	lastCreate       []string
}

func (f *fakeGCPRunner) Run(_ context.Context, _ []string, args ...string) ([]byte, error) {
	if len(args) < 3 || args[0] != "compute" || args[1] != "instances" {
		return nil, errors.New("unexpected gcloud command")
	}
	switch args[2] {
	case "describe":
		if f.instance.Name == "" {
			return []byte("was not found"), errors.New("exit 1")
		}
		return json.Marshal(f.instance)
	case "create":
		f.creates++
		f.lastCreate = append([]string(nil), args...)
		f.instance.Name = args[3]
		f.instance.Status = "RUNNING"
		f.instance.NetworkInterfaces = append(f.instance.NetworkInterfaces, struct {
			NetworkIP string `json:"networkIP"`
		}{NetworkIP: "10.20.0.8"})
		for i, arg := range args {
			if arg == "--metadata" && i+1 < len(args) {
				metadata := strings.TrimPrefix(args[i+1], "^|||^")
				for _, part := range strings.Split(metadata, "|||") {
					if strings.HasPrefix(part, "infercrane-external-key=") {
						f.external = strings.TrimPrefix(part, "infercrane-external-key=")
					}
				}
			}
		}
		f.instance.Metadata.Items = []struct{ Key, Value string }{{Key: "infercrane-external-key", Value: f.external}}
		if f.loseCreate {
			f.loseCreate = false
			return nil, errors.New("lost create response")
		}
		return json.Marshal([]gcpInstance{f.instance})
	case "delete":
		f.deletes++
		f.instance = gcpInstance{}
		return []byte(`{}`), nil
	case "list":
		if f.instance.Name == "" {
			return []byte(`[]`), nil
		}
		return json.Marshal([]gcpInstance{f.instance})
	default:
		return nil, errors.New("unexpected instances command")
	}
}

func testGCPCompute(runner CommandRunner) GCPCompute {
	return GCPCompute{Runner: runner, Project: "project", Zone: "europe-west4-a", Subnet: "private-inference", MachineType: "g2-standard-4", GPUType: "nvidia-l4", ServiceAccount: "runtime@project.iam.gserviceaccount.com", VMImage: "projects/cos-cloud/global/images/cos-immutable", ContainerImage: "vllm/vllm-openai@sha256:" + strings.Repeat("a", 64), WorkerSecret: "infercrane-worker"}
}

func TestGCPComputeLifecycleIsPrivateIdempotentAndAdoptable(t *testing.T) {
	runner := &fakeGCPRunner{loseCreate: true}
	provider := testGCPCompute(runner)
	spec := ReplicaSpec{ExternalKey: "prod-r0", Model: "Qwen/Qwen3-8B", Cloud: "gcp", Region: "europe-west4", GPU: "nvidia-l4"}
	handle := provider.Handle(spec.ExternalKey)
	if handle.ResourceID == "" || handle.RequestID == "" {
		t.Fatal("deterministic identity missing")
	}
	if _, err := provider.EnsureReplica(context.Background(), spec); err == nil {
		t.Fatal("lost create response not surfaced")
	}
	adopted, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || adopted.ResourceID != handle.ResourceID || runner.creates != 1 {
		t.Fatalf("adopted=%#v creates=%d err=%v", adopted, runner.creates, err)
	}
	joined := strings.Join(runner.lastCreate, " ")
	for _, required := range []string{"--no-address", "--service-account runtime@project.iam.gserviceaccount.com", "--labels infercrane-managed=true"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("create missing %q: %s", required, joined)
		}
	}
	observed, err := provider.ObserveReplica(context.Background(), adopted, 8000)
	if err != nil || observed.State != "ready" || observed.Endpoint != "http://10.20.0.8:8000" {
		t.Fatalf("observation=%#v err=%v", observed, err)
	}
	resources, err := provider.Inventory(context.Background(), InventoryFilter{Prefix: "prod-"})
	if err != nil || len(resources) != 1 || resources[0].ExternalKey != spec.ExternalKey {
		t.Fatalf("inventory=%#v err=%v", resources, err)
	}
	if err = provider.DeleteReplica(context.Background(), adopted); err != nil {
		t.Fatal(err)
	}
	if err = provider.DeleteReplica(context.Background(), adopted); err != nil || runner.deletes != 1 {
		t.Fatalf("delete replay calls=%d err=%v", runner.deletes, err)
	}
}

func TestGCPComputeRejectsMutableContainerBeforeProviderCall(t *testing.T) {
	runner := &fakeGCPRunner{}
	provider := testGCPCompute(runner)
	provider.ContainerImage = "mutable:latest"
	_, err := provider.EnsureReplica(context.Background(), ReplicaSpec{ExternalKey: "r0", Model: "model", Cloud: "gcp", Region: "europe-west4", GPU: "nvidia-l4"})
	if err == nil || runner.creates != 0 {
		t.Fatalf("mutable image accepted err=%v creates=%d", err, runner.creates)
	}
}

func TestGCPComputePortableWorkloadExpandsArgumentsSafely(t *testing.T) {
	provider := testGCPCompute(&fakeGCPRunner{})
	spec := ReplicaSpec{Model: "acme/model; touch /tmp/pwned", ModelRevision: "commit-123", RuntimeArgs: []string{"--trust-remote-code"}, Workload: runtimecontract.Workload{Image: "registry.example/runtime@sha256:" + strings.Repeat("b", 64), Command: []string{"serve", "${MODEL}", "--revision", "${MODEL_REVISION}", "--port", "${PORT}", "--key", "${WORKER_API_KEY}"}}}
	startup := provider.startup(spec, 9000)
	for _, expected := range []string{"registry.example/runtime@sha256:", "acme/model; touch /tmp/pwned", "commit-123", "9000", "--trust-remote-code", "INFERCRANE_WORKER_API_KEY"} {
		if !strings.Contains(startup, expected) {
			t.Fatalf("startup script omitted %q: %s", expected, startup)
		}
	}
	if strings.Contains(startup, "${MODEL}") || strings.Contains(startup, "${MODEL_REVISION}") || strings.Contains(startup, "${PORT}") || strings.Contains(startup, "${WORKER_API_KEY}") {
		t.Fatalf("startup retained an unresolved workload placeholder: %s", startup)
	}
	if strings.Contains(startup, " sh -c ") || !strings.Contains(startup, `'acme/model; touch /tmp/pwned'`) {
		t.Fatalf("portable argv was not passed as shell-quoted container argv: %s", startup)
	}
}
