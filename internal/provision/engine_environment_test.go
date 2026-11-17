package provision

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/servingcontract"
	"github.com/infercrane/infercrane/internal/support"
)

const aiterSwitch = "VLLM_ROCM_USE_AITER"

// mi300xAITERSpec is a vLLM replica on an MI300X with AMD's AITER kernels on,
// carrying the pinned ROCm workload the converge fills in.
func mi300xAITERSpec(t *testing.T, cloud string) ReplicaSpec {
	t.Helper()
	workload, ok := support.RuntimeWorkload("vllm", "rocm-gfx942")
	if !ok {
		t.Fatal("no pinned vLLM ROCm workload")
	}
	return ReplicaSpec{ExternalKey: "deployment-r0", Model: "Qwen/Qwen3-8B", ModelRevision: "immutable", Cloud: cloud, GPU: "MI300X", GPUCount: 1, Region: "eu-central-1",
		Runtime: "vllm", RuntimeVersion: support.DefaultRuntimeVersion, Port: workload.Port, Workload: workload, RuntimeEnv: map[string]string{aiterSwitch: "1"}}
}

type refusingRunner struct{ t *testing.T }

func (r refusingRunner) Run(context.Context, []string, ...string) ([]byte, error) {
	r.t.Fatal("an adapter ran a provider command for a replica it must refuse")
	return nil, nil
}

// TestProviderAdaptersPassOrRefuseEngineEnvironment is the evidence for each
// adapter's engine_environment capability: RunPod Pods, Kubernetes, AWS EC2,
// and GCP Compute pass a reviewed engine variable to the runtime container;
// SkyPilot and Kubernetes Dynamo refuse it before any provider call.
func TestProviderAdaptersPassOrRefuseEngineEnvironment(t *testing.T) {
	t.Run("runpod-pods sets the Pod env and checks it on adoption", func(t *testing.T) {
		var mu sync.Mutex
		var pods []runPodRecord
		var created map[string]string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodGet {
				writeRunPodV2List(w, "pods", pods)
				return
			}
			body, ok := decodeRunPodV2PodCreate(t, w, r)
			if !ok {
				return
			}
			created = body.Env
			pods = []runPodRecord{body.pod("pod-amd", "EU-RO-1")}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(pods[0])
		}))
		defer server.Close()
		provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: server.URL, Client: server.Client()}
		spec := mi300xAITERSpec(t, "runpod")
		first, err := provider.EnsureReplica(context.Background(), spec)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		mu.Lock()
		if created[aiterSwitch] != "1" || created["INFERCRANE_WORKER_API_KEY"] != "worker-secret" {
			t.Fatalf("Pod env=%v", created)
		}
		mu.Unlock()
		if second, err := provider.EnsureReplica(context.Background(), spec); err != nil || second != first {
			t.Fatalf("adopt the matching Pod: handle=%#v err=%v", second, err)
		}
		// The same durable key without the switch is a different revision.
		without := spec
		without.RuntimeEnv = nil
		if _, err := provider.EnsureReplica(context.Background(), without); !errors.Is(err, ErrInvalidReplicaSpec) {
			t.Fatalf("adopted a Pod whose engine env differs: %v", err)
		}
		mu.Lock()
		pods[0].Environment[aiterSwitch] = "0"
		mu.Unlock()
		if _, err := provider.EnsureReplica(context.Background(), spec); !errors.Is(err, ErrInvalidReplicaSpec) {
			t.Fatalf("adopted a Pod with the switch off: %v", err)
		}
	})

	t.Run("kubernetes sets the container env and the intent hash covers it", func(t *testing.T) {
		provider := Kubernetes{Context: "kind-infercrane", Namespace: "infercrane-system", WorkloadAPI: "deployment", ServiceAccount: "infercrane-runtime", WorkerSecretName: "infercrane-worker", WorkerSecretKey: "api-key",
			ImageDigest: "vllm/vllm-openai@sha256:" + strings.Repeat("a", 64), GPUResource: "amd.com/gpu", GPUProductLabel: "amd.com/gpu.product-name", ArtifactCachePolicy: "disabled"}
		spec := mi300xAITERSpec(t, "kubernetes")
		if err := provider.validate(spec); err != nil {
			t.Fatal(err)
		}
		manifest, err := provider.manifest(spec, "")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(manifest), `{"name":"VLLM_ROCM_USE_AITER","value":"1"}`) {
			t.Fatalf("manifest has no engine env: %s", manifest)
		}
		without := spec
		without.RuntimeEnv = nil
		withHash, _ := provider.intentHash(spec)
		withoutHash, _ := provider.intentHash(without)
		if withHash == withoutHash {
			t.Fatal("intent hash ignores the engine env")
		}
		// A replica without engine env hashes exactly as before the field.
		if encoded, _ := json.Marshal(without); strings.Contains(string(encoded), "RuntimeEnv") {
			t.Fatalf("empty engine env changes existing intent hashes: %s", encoded)
		}
	})

	t.Run("aws-ec2 passes docker run -e in user data", func(t *testing.T) {
		provider := testAWSEC2(refusingRunner{t})
		provider.GPU = "MI300X"
		spec := mi300xAITERSpec(t, "aws")
		if err := provider.validate(spec); err != nil {
			t.Fatal(err)
		}
		if script := provider.userData(spec, spec.Port, ""); !strings.Contains(script, `-e 'VLLM_ROCM_USE_AITER=1' -p 8000:8000`) {
			t.Fatalf("user data has no engine env: %s", script)
		}
		// Without the pinned workload the adapter would start its own image,
		// which the variable was not reviewed for.
		spec.Workload = runtimecontract.Workload{}
		if err := provider.validate(spec); err == nil || !strings.Contains(err.Error(), "pinned runtime workload") {
			t.Fatal("AWS accepted engine env for its own default image")
		}
	})

	t.Run("gcp-compute passes docker run -e in the startup script and intent digest", func(t *testing.T) {
		provider := testGCPCompute(refusingRunner{t})
		provider.GPUType, provider.Zone = "MI300X", "eu-central-1-a"
		spec := mi300xAITERSpec(t, "gcp")
		if err := provider.validate(spec); err != nil {
			t.Fatal(err)
		}
		if script := provider.startup(spec, spec.Port); !strings.Contains(script, `-e 'VLLM_ROCM_USE_AITER=1' -p 8000:8000`) {
			t.Fatalf("startup script has no engine env: %s", script)
		}
		without := spec
		without.RuntimeEnv = nil
		if provider.intentDigest(spec) == provider.intentDigest(without) {
			t.Fatal("intent digest ignores the engine env")
		}
		if strings.Contains(provider.startup(without, without.Port), "VLLM_ROCM_USE_AITER") {
			t.Fatal("a replica without engine env got one")
		}
	})

	t.Run("skypilot refuses before launching", func(t *testing.T) {
		provider := SkyPilot{APIKey: "secret", Runner: refusingRunner{t}}
		spec := mi300xAITERSpec(t, "runpod")
		spec.GPU = "H100"
		if _, err := provider.EnsureReplica(context.Background(), spec); !errors.Is(err, ErrInvalidReplicaSpec) || !strings.Contains(err.Error(), "engine environment") {
			t.Fatalf("SkyPilot err=%v", err)
		}
	})

	t.Run("kubernetes-dynamo refuses before applying", func(t *testing.T) {
		provider := KubernetesDynamo{Context: "kind-infercrane", Namespace: "infercrane-system", ServiceAccount: "infercrane-runtime",
			VLLMImageDigest: "nvcr.io/nvidia/ai-dynamo/vllm-runtime:1.4.0@sha256:" + strings.Repeat("a", 64), VLLMRuntimeVersion: "1.4.0",
			ModelSecretName: "huggingface-models", GPUResource: "nvidia.com/gpu", GPUProductLabel: "nvidia.com/gpu.product", Runner: refusingRunner{t}}
		spec := mi300xAITERSpec(t, "kubernetes")
		spec.Serving = servingcontract.Topology{Backend: servingcontract.BackendDynamo, Profile: "baseline", Mode: servingcontract.ModeAggregated, Routing: servingcontract.RoutingDirect, Worker: servingcontract.Pool{Replicas: 1, TensorParallelism: 1}}
		if _, err := provider.EnsureReplica(context.Background(), spec); !errors.Is(err, ErrInvalidReplicaSpec) || !strings.Contains(err.Error(), "engine environment") {
			t.Fatalf("Dynamo err=%v", err)
		}
	})
}

func TestAdaptersRefuseUnreviewedEngineEnvironmentWithoutEchoingValues(t *testing.T) {
	const pasted = "hf_secretvalue123"
	for name, env := range map[string]map[string]string{
		"unreviewed variable": {"HF_TOKEN": pasted},
		"unreviewed value":    {aiterSwitch: pasted},
		"value as a name":     {pasted: "1"},
	} {
		spec := mi300xAITERSpec(t, "runpod")
		spec.RuntimeEnv = env
		provider := RunPodPods{APIKey: "provider-secret", WorkerAPIKey: "worker-secret", BaseURL: "http://127.0.0.1:1", Client: http.DefaultClient}
		_, err := provider.EnsureReplica(context.Background(), spec)
		if !errors.Is(err, ErrInvalidReplicaSpec) {
			t.Fatalf("%s: err=%v", name, err)
		}
		if strings.Contains(err.Error(), pasted) {
			t.Fatalf("%s: error echoes the refused value: %v", name, err)
		}
	}
	// On a CUDA GPU no engine variable is reviewed at all.
	spec := mi300xAITERSpec(t, "runpod")
	spec.GPU, spec.Workload = "H100", support.VLLMWorkload()
	if _, err := engineEnvironment(spec); !errors.Is(err, ErrInvalidReplicaSpec) {
		t.Fatalf("CUDA engine env err=%v", err)
	}
}
