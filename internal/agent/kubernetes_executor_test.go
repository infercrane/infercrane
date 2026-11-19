package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/runtimecontract"
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
