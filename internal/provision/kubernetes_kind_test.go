package provision_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func TestKubernetesKWOKFleetLifecycle(t *testing.T) {
	contextName := os.Getenv("INFERCRANE_KWOK_CONTEXT")
	if contextName == "" {
		t.Skip("INFERCRANE_KWOK_CONTEXT is not set")
	}
	count := 100
	if raw := os.Getenv("INFERCRANE_KWOK_WORKLOADS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 1000 {
			t.Fatalf("invalid INFERCRANE_KWOK_WORKLOADS %q", raw)
		}
		count = parsed
	}
	provider := provision.Kubernetes{Context: contextName, Namespace: "infercrane-system", WorkloadAPI: "deployment", ServiceAccount: "infercrane-runtime", WorkerSecretName: "infercrane-worker", WorkerSecretKey: "api-key", ImageDigest: testKubernetesImage, GPUResource: "nvidia.com/gpu", GPUProductLabel: "nvidia.com/gpu.product"}
	if err := provider.Check(context.Background()); err != nil {
		t.Fatalf("cluster capability check: %v", err)
	}

	runBounded := func(action func(int) error) {
		t.Helper()
		jobs := make(chan int)
		errorsByIndex := make([]error, count)
		var workers sync.WaitGroup
		for range 16 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for index := range jobs {
					errorsByIndex[index] = action(index)
				}
			}()
		}
		for index := range count {
			jobs <- index
		}
		close(jobs)
		workers.Wait()
		for index, err := range errorsByIndex {
			if err != nil {
				t.Fatalf("fleet item %d: %v", index, err)
			}
		}
	}

	spec := func(index int) provision.ReplicaSpec {
		return provision.ReplicaSpec{ExternalKey: fmt.Sprintf("kwok-fleet-%04d", index), Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", Cloud: "kubernetes", GPU: "NVIDIA-L40S", Port: 8000}
	}
	runBounded(func(index int) error {
		_, err := provider.EnsureReplica(context.Background(), spec(index))
		return err
	})

	started := time.Now()
	resources, err := provider.Inventory(context.Background(), provision.InventoryFilter{Prefix: "kwok-fleet-"})
	if err != nil || len(resources) != count {
		t.Fatalf("inventory count=%d want=%d err=%v", len(resources), count, err)
	}
	for _, resource := range resources {
		if resource.State != "provisioning" || resource.Endpoint != "" {
			t.Fatalf("unscheduled GPU workload was routable: %#v", resource)
		}
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("fleet inventory took %s for %d workloads", elapsed, count)
	}

	// Removing half the simulated nodes models a large scheduler disruption.
	// Pending GPU workloads must remain present and unroutable rather than
	// disappearing or being inferred healthy from cluster-level node state.
	nodeNames := kubectlOutput(t, contextName, "get", "nodes", "-o", "name")
	nodes := strings.Fields(nodeNames)
	for _, node := range nodes[:len(nodes)/2] {
		runKubectl(t, contextName, "delete", node, "--wait=false")
	}
	resources, err = provider.Inventory(context.Background(), provision.InventoryFilter{Prefix: "kwok-fleet-"})
	if err != nil || len(resources) != count {
		t.Fatalf("post-disruption inventory count=%d want=%d err=%v", len(resources), count, err)
	}
	for _, resource := range resources {
		if resource.State != "provisioning" || resource.Endpoint != "" {
			t.Fatalf("node disruption made pending workload routable: %#v", resource)
		}
	}

	runBounded(func(index int) error {
		return provider.DeleteReplica(context.Background(), provider.Handle(spec(index).ExternalKey))
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		remaining, listErr := provider.Inventory(context.Background(), provision.InventoryFilter{Prefix: "kwok-fleet-"})
		if listErr == nil && len(remaining) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fleet resources remain after cleanup: count=%d err=%v", len(remaining), listErr)
		}
		time.Sleep(250 * time.Millisecond)
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

func kubectlOutput(t *testing.T, contextName string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"--context", contextName}, args...)
	output, err := exec.Command("kubectl", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
