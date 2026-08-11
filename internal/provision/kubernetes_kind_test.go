package provision_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/provision"
)

func TestKubernetesKindLifecycle(t *testing.T) {
	contextName := os.Getenv("INFERCRANE_KIND_CONTEXT")
	if contextName == "" {
		t.Skip("INFERCRANE_KIND_CONTEXT is not set")
	}
	provider := provision.Kubernetes{Context: contextName, Namespace: "infercrane-system", WorkloadAPI: "deployment", ServiceAccount: "infercrane-runtime", WorkerSecretName: "infercrane-worker", WorkerSecretKey: "api-key", ImageDigest: testKubernetesImage, GPUResource: "nvidia.com/gpu", GPUProductLabel: "nvidia.com/gpu.product"}
	if err := provider.Check(context.Background()); err != nil {
		t.Fatalf("cluster capability check: %v", err)
	}
	spec := provision.ReplicaSpec{ExternalKey: "kind-lifecycle-r0", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", Cloud: "kubernetes", GPU: "NVIDIA-L40S", Port: 8000}
	handle := provider.Handle(spec.ExternalKey)
	_ = provider.DeleteReplica(context.Background(), handle)

	created, err := provider.EnsureReplica(context.Background(), spec)
	if err != nil || created != handle {
		t.Fatalf("ensure handle=%#v err=%v", created, err)
	}
	observation, err := provider.ObserveReplica(context.Background(), handle, 8000)
	if err != nil || !observation.Exists || observation.State != "provisioning" || observation.Endpoint != "" {
		t.Fatalf("unscheduled GPU observation=%#v err=%v", observation, err)
	}

	// A new adapter value models a control-plane restart: persisted external
	// identity remains sufficient for observation and adoption.
	restarted := provider
	restartedObservation, err := restarted.ObserveReplica(context.Background(), handle, 8000)
	if err != nil || !restartedObservation.Exists {
		t.Fatalf("restart observation=%#v err=%v", restartedObservation, err)
	}
	resources, err := restarted.Inventory(context.Background(), provision.InventoryFilter{Prefix: "kind-lifecycle"})
	if err != nil || len(resources) != 1 || resources[0].ExternalKey != spec.ExternalKey {
		t.Fatalf("inventory=%#v err=%v", resources, err)
	}

	name := resourceNameFromHandle(handle)
	runKubectl(t, contextName, "--namespace", provider.Namespace, "delete", "service/"+name, "--wait=true")
	partial, err := provider.Inventory(context.Background(), provision.InventoryFilter{Prefix: "kind-lifecycle"})
	if err != nil || len(partial) != 1 || partial[0].State != "degraded" {
		t.Fatalf("partial inventory=%#v err=%v", partial, err)
	}
	if _, err = provider.EnsureReplica(context.Background(), spec); err != nil {
		t.Fatalf("repair partial owner set: %v", err)
	}
	runKubectl(t, contextName, "--namespace", provider.Namespace, "get", "service/"+name)

	// Another server-side field manager taking replicas must produce a
	// conflict; InferCrane never force-steals it.
	foreign := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: " + name + "\n  namespace: " + provider.Namespace + "\nspec:\n  replicas: 2\n"
	path := filepath.Join(t.TempDir(), "foreign.yaml")
	if err = os.WriteFile(path, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	runKubectl(t, contextName, "apply", "--server-side", "--field-manager=foreign-owner", "--force-conflicts", "-f", path)
	if _, err = provider.EnsureReplica(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "server-side validate") {
		t.Fatalf("field conflict was not preserved: %v", err)
	}

	if err = provider.DeleteReplica(context.Background(), handle); err != nil {
		t.Fatalf("delete: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		remaining, listErr := provider.Inventory(context.Background(), provision.InventoryFilter{Prefix: "kind-lifecycle"})
		if listErr == nil && len(remaining) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("resources remain after delete: %#v err=%v", remaining, listErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err = provider.DeleteReplica(context.Background(), handle); err != nil {
		t.Fatalf("delete replay: %v", err)
	}
}

func runKubectl(t *testing.T, contextName string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"--context", contextName}, args...)
	output, err := exec.Command("kubectl", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v: %s", strings.Join(args, " "), err, output)
	}
}
