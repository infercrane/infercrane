package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/provision"
)

// keptProvider keeps what it starts until it is deleted, and lists it.
type keptProvider struct {
	mu      sync.Mutex
	running map[string]int // external key -> port
	down    map[string]bool
	deleted []string
}

func (p *keptProvider) address(key string, port int) string {
	return "http://infercrane-" + strings.NewReplacer(":", "-").Replace(key)[12:30] + ".models.svc:" + itoa(port)
}

func (p *keptProvider) ServiceAddress(handle provision.ProviderHandle, port int) string {
	return p.address(handle.ExternalKey, port)
}

func (p *keptProvider) Check(context.Context) error { return nil }
func (p *keptProvider) Handle(key string) provision.ProviderHandle {
	return provision.ProviderHandle{ExternalKey: key}
}
func (p *keptProvider) EnsureReplica(_ context.Context, spec provision.ReplicaSpec) (provision.ProviderHandle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running == nil {
		p.running = map[string]int{}
	}
	p.running[spec.ExternalKey] = spec.Port
	return provision.ProviderHandle{ExternalKey: spec.ExternalKey}, nil
}
func (p *keptProvider) ObserveReplica(_ context.Context, handle provision.ProviderHandle, port int) (provision.Observation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.running[handle.ExternalKey]; !ok {
		return provision.Observation{}, nil
	}
	if p.down[handle.ExternalKey] {
		return provision.Observation{Exists: true, State: "provisioning"}, nil
	}
	return provision.Observation{Exists: true, State: "ready", Endpoint: p.address(handle.ExternalKey, port)}, nil
}
func (p *keptProvider) DeleteReplica(_ context.Context, handle provision.ProviderHandle) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.running, handle.ExternalKey)
	p.deleted = append(p.deleted, handle.ExternalKey)
	return nil
}
func (p *keptProvider) Inventory(_ context.Context, filter provision.InventoryFilter) ([]provision.Resource, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []provision.Resource
	for key := range p.running {
		if strings.HasPrefix(key, filter.Prefix) {
			out = append(out, provision.Resource{ExternalKey: key})
		}
	}
	return out, nil
}

func itoa(value int) string {
	digits := ""
	for {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
		if value == 0 {
			return digits
		}
	}
}

// A rollout's server keeps running after it is started, is found again by
// a restarted agent, and is stopped only by its own key; a key that names
// the customer's own server stops nothing.
func TestRolloutServersStartStayAndStopByTheirOwnKey(t *testing.T) {
	provider := &keptProvider{}
	executor := KubernetesExecutor{Provider: provider, WorkerKey: "worker-key", AllowedImages: []string{"vllm/vllm-openai"}, PollInterval: 1, ReadyTimeout: 1e9}
	spec := agentprotocol.StartServerSpec{Endpoint: "support-bot", Setup: *windowSpec().Candidate, TimeoutSecs: 600}
	launched, result, err := executor.StartServer(context.Background(), "command-1", spec, func(string) {})
	if err != nil || launched.Endpoint != "support-bot" || launched.Token != "worker-key" || result.ServerKey != ServerKey(launched.BaseURL) || result.Validate() != nil {
		t.Fatalf("start: %+v %+v %v", launched, result, err)
	}
	if len(provider.running) != 1 || len(provider.deleted) != 0 {
		t.Fatalf("the server was not kept: %+v", provider)
	}
	found, err := executor.LaunchedServers(context.Background())
	if err != nil || len(found) != 1 || found[0] != launched {
		t.Fatalf("found again: %+v %v", found, err)
	}
	var failure *CandidateError
	if _, err = executor.StopServer(context.Background(), agentprotocol.StopServerSpec{Endpoint: "support-bot", ServerKey: "qwen3-models-svc-8000"}); !errors.As(err, &failure) || failure.Code != "server_not_started_here" || len(provider.running) != 1 {
		t.Fatalf("stopping the customer's server: %v %+v", err, provider)
	}
	stopped, err := executor.StopServer(context.Background(), agentprotocol.StopServerSpec{Endpoint: "support-bot", ServerKey: result.ServerKey})
	if err != nil || !stopped.Stopped || len(provider.running) != 0 {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	spec.Setup.Workload.Image = "docker.io/library/busybox@sha256:" + strings.Repeat("e", 64)
	if _, _, err = executor.StartServer(context.Background(), "command-2", spec, func(string) {}); !errors.As(err, &failure) || failure.Code != "image_not_allowed" || len(provider.running) != 0 {
		t.Fatalf("refused image: %v", err)
	}
}

// A rollout's server that stopped serving is not served again by a
// restarted agent, and is still stopped by its key: a rollback stops a
// server because it stopped serving.
func TestARolloutServerThatStoppedServingIsStillStopped(t *testing.T) {
	provider := &keptProvider{down: map[string]bool{}}
	executor := KubernetesExecutor{Provider: provider, WorkerKey: "worker-key", AllowedImages: []string{"vllm/vllm-openai"}, PollInterval: 1, ReadyTimeout: 1e9}
	spec := agentprotocol.StartServerSpec{Endpoint: "support-bot", Setup: *windowSpec().Candidate, TimeoutSecs: 600}
	_, result, err := executor.StartServer(context.Background(), "command-1", spec, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	for key := range provider.running {
		provider.down[key] = true
	}
	if found, listErr := executor.LaunchedServers(context.Background()); listErr != nil || len(found) != 0 {
		t.Fatalf("a server that is not serving was found to serve: %+v %v", found, listErr)
	}
	stopped, err := executor.StopServer(context.Background(), agentprotocol.StopServerSpec{Endpoint: "support-bot", ServerKey: result.ServerKey})
	if err != nil || !stopped.Stopped || len(provider.running) != 0 || len(provider.deleted) != 1 {
		t.Fatalf("stop: %+v %v %+v", stopped, err, provider)
	}
}
