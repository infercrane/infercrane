package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/engineingress"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/testtools/providerfixture"
)

func TestImageAllowlistMatchesRepositoriesNotPrefixesOfNames(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	allowed := []string{"vllm/vllm-openai", "ghcr.io/ggml-org/", "registry.internal:5000/team/sglang"}
	for image, want := range map[string]bool{
		"vllm/vllm-openai" + digest:                         true,
		"vllm/vllm-openai:v0.11.2" + digest:                 true,
		"vllm/vllm-openai-evil" + digest:                    false,
		"ghcr.io/ggml-org/llama.cpp:server" + digest:        true,
		"ghcr.io/ggml-org-evil/llama.cpp" + digest:          false,
		"registry.internal:5000/team/sglang" + digest:       true,
		"registry.internal:5000/team/sglang-other" + digest: false,
		"docker.io/library/busybox" + digest:                false,
	} {
		if got := ImageAllowed(allowed, image); got != want {
			t.Errorf("%s: allowed=%v, want %v", image, got, want)
		}
	}
	if ImageAllowed(nil, "vllm/vllm-openai"+digest) {
		t.Fatal("an empty allowlist admitted an image")
	}
}

type refusingProvider struct{ ensured int }

func (p *refusingProvider) Check(context.Context) error { return nil }
func (p *refusingProvider) Handle(key string) provision.ProviderHandle {
	return provision.ProviderHandle{ExternalKey: key}
}
func (p *refusingProvider) EnsureReplica(context.Context, provision.ReplicaSpec) (provision.ProviderHandle, error) {
	p.ensured++
	return provision.ProviderHandle{}, errors.New("unexpected launch")
}
func (p *refusingProvider) ObserveReplica(context.Context, provision.ProviderHandle, int) (provision.Observation, error) {
	return provision.Observation{}, nil
}
func (p *refusingProvider) DeleteReplica(context.Context, provision.ProviderHandle) error { return nil }

func TestExecutorRefusesBeforeApplyingAnything(t *testing.T) {
	provider := &refusingProvider{}
	executor := KubernetesExecutor{Provider: provider, WorkerKey: "k", AllowedImages: []string{"vllm/vllm-openai"}, AllowedAccelerators: []string{"NVIDIA-L4"}}
	spec := agentprotocol.CandidateRunSpec{
		Endpoint: "e", Label: "x", Model: "m", Accelerator: "NVIDIA-H100", GPUCount: 1, TimeoutSecs: 600,
		Workload: runtimecontract.Workload{Image: "vllm/vllm-openai@sha256:" + strings.Repeat("b", 64)},
		Quality:  agentprotocol.CandidateQuality{EvalSet: "local:missing"},
	}
	_, err := executor.RunCandidate(context.Background(), CandidateRun{ID: "command-0001", Spec: spec}, func(string) {})
	var failure *CandidateError
	if !errors.As(err, &failure) || failure.Code != "accelerator_not_allowed" {
		t.Fatalf("accelerator: %v", err)
	}
	spec.Accelerator = "NVIDIA-L4"
	if _, err = executor.RunCandidate(context.Background(), CandidateRun{ID: "command-0001", Spec: spec}, func(string) {}); !errors.As(err, &failure) || failure.Code != "eval_set_unknown" {
		t.Fatalf("eval set: %v", err)
	}
	if provider.ensured != 0 {
		t.Fatal("a refused candidate was applied")
	}
	if err = (KubernetesExecutor{Provider: provider, WorkerKey: "k"}).Check(context.Background()); err == nil {
		t.Fatal("an executor without an image allowlist started")
	}
}

func TestInClusterKubeconfigNeedsAPod(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	if _, err := WriteInClusterKubeconfig(t.TempDir()); err == nil {
		t.Fatal("wrote a kubeconfig outside a pod")
	}
}

// snapshotProvider records what the real Kubernetes provider applied for a
// candidate before the run deletes it.
type snapshotProvider struct {
	provision.Kubernetes
	fixture *providerfixture.KubernetesCLI
	applied map[string]map[string]any
}

func (p *snapshotProvider) EnsureReplica(ctx context.Context, spec provision.ReplicaSpec) (provision.ProviderHandle, error) {
	handle, err := p.Kubernetes.EnsureReplica(ctx, spec)
	p.applied = map[string]map[string]any{}
	for key, object := range p.fixture.Objects {
		p.applied[key] = object
	}
	return handle, err
}

// A candidate starts like a deployment replica: behind the engine
// NetworkPolicy the operator configured, and, when its command starts the
// SGLang server, through the key launcher, so it requires the worker key.
// The candidate is deleted with its policy when the run ends.
func TestCandidateGetsTheEngineNetworkPolicyAndKey(t *testing.T) {
	fixture := providerfixture.NewKubernetesCLI()
	provider := &snapshotProvider{fixture: fixture, Kubernetes: provision.Kubernetes{
		Runner: fixture, Context: "kind-agent", Namespace: "models", WorkloadAPI: "deployment", ServiceAccount: "infercrane-runtime",
		WorkerSecretName: "agent-worker", WorkerSecretKey: "api-key", GPUResource: "nvidia.com/gpu", GPUProductLabel: "nvidia.com/gpu.product",
		ArtifactCachePolicy: "disabled",
		EngineIngress:       []engineingress.Peer{{NamespaceLabels: map[string]string{"kubernetes.io/metadata.name": "models"}, PodLabels: map[string]string{"app.kubernetes.io/name": "infercrane-agent"}}},
	}}
	executor := KubernetesExecutor{
		Provider: provider, WorkerKey: "worker-key", AllowedImages: []string{"lmsysorg/sglang"}, PollInterval: time.Millisecond, ReadyTimeout: time.Second,
		Benchmark: func(context.Context, benchmark.Config) (benchmark.Result, error) {
			return benchmark.Result{}, errors.New("stop after launch")
		},
	}
	spec := agentprotocol.CandidateRunSpec{
		Endpoint: "e", Label: "x", Model: "Qwen/Qwen3-8B", Accelerator: "NVIDIA-L4", GPUCount: 1, TimeoutSecs: 600,
		Workload: runtimecontract.Workload{Image: "lmsysorg/sglang@sha256:" + strings.Repeat("c", 64), Command: []string{"sglang", "serve", "--model-path", "${MODEL}", "--port", "30000"},
			Protocol: "openai", Port: 30000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30},
		Quality: agentprotocol.CandidateQuality{Skip: true},
	}
	result, err := executor.RunCandidate(context.Background(), CandidateRun{ID: "command-0001", Spec: spec, Control: ControlServer{Endpoint: "e", BaseURL: "http://current.models.svc:8000", Model: "Qwen/Qwen3-8B"}}, func(string) {})
	var failure *CandidateError
	if !errors.As(err, &failure) || failure.Code != "control_measurement_failed" {
		t.Fatalf("run: %v", err)
	}
	if !result.CleanedUp || len(fixture.Objects) != 0 {
		t.Fatalf("candidate left behind: cleaned=%v objects=%v", result.CleanedUp, fixture.Objects)
	}
	applied, _ := json.Marshal(provider.applied)
	for _, want := range []string{`"kind":"NetworkPolicy"`, `"policyTypes":["Ingress"]`, `"kubernetes.io/metadata.name":"models"`, `"port":30000`, "infercrane-engine-key", `"sglang"`} {
		if !strings.Contains(string(applied), want) {
			t.Fatalf("applied objects lack %s: %s", want, applied)
		}
	}
	if strings.Contains(string(applied), "--api-key") {
		t.Fatalf("the key is in argv: %s", applied)
	}
	// Behind a prefix such as numactl, an SGLang candidate starts through the
	// launcher too, with its command kept as it was after the launcher.
	spec.Workload.Command = []string{"numactl", "--interleave=all", "sglang", "serve", "--model-path", "${MODEL}", "--port", "30000"}
	if _, err = executor.RunCandidate(context.Background(), CandidateRun{ID: "command-0003", Spec: spec, Control: ControlServer{Endpoint: "e", BaseURL: "http://current.models.svc:8000", Model: "Qwen/Qwen3-8B"}}, func(string) {}); !errors.As(err, &failure) {
		t.Fatalf("prefixed SGLang run: %v", err)
	}
	applied, _ = json.Marshal(provider.applied)
	if !strings.Contains(string(applied), "infercrane-engine-key\",\"numactl\",\"--interleave=all\",\"sglang\",\"serve\"") {
		t.Fatalf("prefixed SGLang candidate was not keyed: %s", applied)
	}
	// A vLLM candidate reads the key from VLLM_API_KEY and starts unchanged.
	spec.Workload.Image, spec.Workload.Command, spec.Workload.Port = "vllm/vllm-openai@sha256:"+strings.Repeat("d", 64), []string{"vllm", "serve", "${MODEL}", "--port", "8000"}, 8000
	executor.AllowedImages = []string{"vllm/vllm-openai"}
	if _, err = executor.RunCandidate(context.Background(), CandidateRun{ID: "command-0002", Spec: spec, Control: ControlServer{Endpoint: "e", BaseURL: "http://current.models.svc:8000", Model: "Qwen/Qwen3-8B"}}, func(string) {}); !errors.As(err, &failure) {
		t.Fatalf("vLLM run: %v", err)
	}
	applied, _ = json.Marshal(provider.applied)
	if strings.Contains(string(applied), "infercrane-engine-key") || !strings.Contains(string(applied), `"command":["vllm"]`) || !strings.Contains(string(applied), `"kind":"NetworkPolicy"`) {
		t.Fatalf("vLLM candidate: %s", applied)
	}
}
