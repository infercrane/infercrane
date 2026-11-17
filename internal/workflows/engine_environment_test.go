package workflows

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/support"
)

func TestConvergePassesRuntimeEnvOnlyThroughAdaptersThatDeclareIt(t *testing.T) {
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	runtimeProfile, err := registry.Runtime(support.DefaultRuntime)
	if err != nil {
		t.Fatal(err)
	}
	podsProfile, err := registry.Provider("runpod-pods")
	if err != nil {
		t.Fatal(err)
	}
	converge := func(t *testing.T, profile integration.ProviderProfile) (*fakeReplicaProvider, *fakeCloudStore, error) {
		t.Helper()
		store := &fakeCloudStore{deployment: domain.Deployment{ID: "deployment-1", Name: "qwen", Model: "Qwen/Qwen3-8B", RoutingStrategy: "round-robin", MinReplicas: 1, MaxReplicas: 1}}
		provider := &fakeReplicaProvider{observation: provision.Observation{Exists: true, State: "ready", Endpoint: "http://gpu:8000", Details: `{}`}}
		backends, err := NewReplicaBackends(ReplicaBackend{Name: "runpod-pods", Cloud: "runpod", Runtime: "vllm", Profile: profile, Provider: provider})
		if err != nil {
			t.Fatal(err)
		}
		runtimes, err := integration.NewRuntimeBackends(integration.RuntimeBackend{Profile: runtimeProfile, Inspector: fakeInspector{ready: true}})
		if err != nil {
			t.Fatal(err)
		}
		operation := domain.Operation{ID: "operation-1", LeaseOwner: "worker", LeaseGeneration: 1, RequestJSON: `{"deployment_id":"deployment-1","name":"qwen","model":"Qwen/Qwen3-8B","cloud":"runpod","provider_adapter":"runpod-pods","gpu":"MI300X","tenant_id":"global","min_replicas":1,"max_replicas":1,"runtime_env":{"VLLM_ROCM_USE_AITER":"1"}}`}
		_, err = CloudHandlersWithBackendsAndDrain(store, backends, runtimes, &fakeDrainTracker{current: true})[ConvergeKind](context.Background(), operation)
		return provider, store, err
	}
	provider, _, err := converge(t, podsProfile)
	pinned, _ := support.RuntimeWorkload("vllm", "rocm-gfx942")
	if err != nil || provider.ensureCalls != 1 || provider.lastSpec.RuntimeEnv["VLLM_ROCM_USE_AITER"] != "1" || provider.lastSpec.Workload.Image != pinned.Image {
		t.Fatalf("runpod-pods: ensure_calls=%d spec=%+v err=%v", provider.ensureCalls, provider.lastSpec, err)
	}
	// An adapter that does not declare the capability is refused before any
	// provider call, so no replica ever starts without its reviewed switch.
	provider, store, err := converge(t, testElasticProfile("runpod-pods", "runpod"))
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Code != "runtime_env_unsupported" || failure.Retryable || provider.ensureCalls != 0 || store.target.Name != "" {
		t.Fatalf("undeclared adapter: failure=%+v ensure_calls=%d target=%+v err=%v", failure, provider.ensureCalls, store.target, err)
	}
}

func TestEveryRegisteredAdapterPassesOrRefusesRuntimeEnv(t *testing.T) {
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"VLLM_ROCM_USE_AITER": "1"}
	passes := map[string]bool{}
	for _, profile := range registry.Snapshot().Providers {
		passes[profile.Adapter] = refuseUnpassedRuntimeEnv(profile.Adapter, profile, env) == nil
		if refuseUnpassedRuntimeEnv(profile.Adapter, profile, nil) != nil {
			t.Fatalf("%s refused a revision without runtime env", profile.Adapter)
		}
	}
	for adapter, want := range map[string]bool{"runpod-pods": true, "kubernetes": true, "aws-ec2": true, "gcp-compute": true, "skypilot": false, "runpod-serverless": false, "kubernetes-dynamo": false} {
		if got, registered := passes[adapter]; !registered || got != want {
			t.Fatalf("%s passes runtime env=%t registered=%t, want %t", adapter, got, registered, want)
		}
	}
	// A declared SkyPilot cloud refuses it too.
	skyPilot := integration.SkyPilotProviderProfile("lambda", "skypilot-lambda")
	if err := refuseUnpassedRuntimeEnv(skyPilot.Adapter, skyPilot, env); err == nil || !strings.Contains(err.Error(), "skypilot-lambda") {
		t.Fatalf("SkyPilot cloud err=%v", err)
	}
}

func TestCloudRequestAdmitsOnlyReviewedRuntimeEnv(t *testing.T) {
	request := func(gpu string, env map[string]string) CloudRequest {
		return CloudRequest{Name: "qwen", Model: "Qwen/Qwen3-8B", Cloud: "runpod", ProviderAdapter: "runpod-pods", GPU: gpu, MinReplicas: 1, MaxReplicas: 1, RuntimeEnv: env}
	}
	for _, gpu := range []string{"MI300X", "AMD Instinct MI300X OAM", "MI350X"} {
		valid := request(gpu, map[string]string{"VLLM_ROCM_USE_AITER": "1"})
		if err := valid.Validate(); err != nil {
			t.Fatalf("%s: %v", gpu, err)
		}
	}
	for name, test := range map[string]CloudRequest{
		"CUDA GPU":   request("H100", map[string]string{"VLLM_ROCM_USE_AITER": "1"}),
		"credential": request("MI300X", map[string]string{"HF_TOKEN": "hf_secret"}),
		"value":      request("MI300X", map[string]string{"VLLM_ROCM_USE_AITER": "yes"}),
	} {
		if err := test.Validate(); err == nil || strings.Contains(err.Error(), "hf_secret") {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
	sglang := request("MI300X", map[string]string{"VLLM_ROCM_USE_AITER": "1"})
	sglang.Runtime, sglang.Cloud, sglang.ProviderAdapter = "sglang", "kubernetes", "kubernetes"
	if err := sglang.Validate(); err == nil {
		t.Fatal("SGLang accepted a vLLM switch")
	}
}
