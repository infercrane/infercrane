package agentrelay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/domain"
)

// leaseHookStore runs afterLease once a poll has leased commands, standing in
// for an agent that goes away at that moment.
type leaseHookStore struct {
	fakeCommandStore
	afterLease func()
}

func (h *leaseHookStore) LeaseAgentCommands(ctx context.Context, tenant, agentID string, at time.Time, lease time.Duration, limit int) ([]domain.AgentCommand, error) {
	leased, err := h.fakeCommandStore.LeaseAgentCommands(ctx, tenant, agentID, at, lease, limit)
	if len(leased) > 0 && h.afterLease != nil {
		h.afterLease()
	}
	return leased, err
}

func (h *leaseHookStore) state() (domain.AgentCommand, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.commands[0], h.released
}

// A long poll whose agent goes away after its commands are leased, or whose
// answer cannot be written, hands them back: the next poll gets them at once,
// not after the 10-minute lease, and the lost attempt is not counted. An
// answer that reached the agent keeps its lease.
func TestALongPollThatCannotDeliverHandsItsCommandsBack(t *testing.T) {
	agent := domain.Agent{ID: "agent-1", TenantID: "tenant-a", Name: "cluster-a"}
	setup := func(t *testing.T) (*Service, *leaseHookStore) {
		t.Helper()
		store := &leaseHookStore{fakeCommandStore: fakeCommandStore{fakeStore: fakeStore{servers: []domain.AgentServer{{AgentID: "agent-1", Key: "vllm-8000", EndpointName: "support-bot", UpstreamModel: "Qwen/Qwen3-8B"}}}}}
		return &Service{Store: store}, store
	}
	queue := func(t *testing.T, service *Service) domain.AgentCommand {
		t.Helper()
		_, command, err := service.CreateCandidateRun(context.Background(), "tenant-a", "cluster-a", "owner", candidateSpec())
		if err != nil {
			t.Fatal(err)
		}
		return command
	}
	// The next poll takes the command at once, as its first attempt; once
	// delivered it keeps its lease, so the poll after that gets nothing.
	redelivered := func(t *testing.T, service *Service, store *leaseHookStore, id string) {
		t.Helper()
		started := time.Now()
		response, err := poll(context.Background(), service, agent, 0)
		if err != nil || len(response.Commands) != 1 || response.Commands[0].ID != id || response.Commands[0].Attempt != 1 {
			t.Fatalf("next poll: %+v %v", response, err)
		}
		if waited := time.Since(started); waited > time.Second {
			t.Fatalf("the released command took %s to come back", waited)
		}
		again, err := poll(context.Background(), service, agent, 0)
		if err != nil || len(again.Commands) != 0 {
			t.Fatalf("a delivered command was handed out again: %+v %v", again, err)
		}
		if command, released := store.state(); command.State != "running" || command.Phase != "leased" || command.LeaseExpiresAt == nil || released != 1 {
			t.Fatalf("delivered command: %+v, %d releases", command, released)
		}
	}

	t.Run("the agent goes away after the lease", func(t *testing.T) {
		service, store := setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store.afterLease = cancel
		answered := make(chan agentprotocol.CommandsResponse, 1)
		done := make(chan error, 1)
		go func() {
			done <- service.Commands(ctx, agent, 20*time.Second, func(response agentprotocol.CommandsResponse) error {
				answered <- response
				return errors.New("write: broken pipe")
			})
		}()
		// Queued while the poll is held, so the held poll leases it.
		time.Sleep(50 * time.Millisecond)
		command := queue(t, service)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("poll: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the held poll did not return")
		}
		if response := <-answered; len(response.Commands) != 0 {
			t.Fatalf("an answer for an agent that went away carried commands: %+v", response)
		}
		redelivered(t, service, store, command.ID)
	})

	t.Run("the answer cannot be written", func(t *testing.T) {
		service, store := setup(t)
		command := queue(t, service)
		offered := 0
		err := service.Commands(context.Background(), agent, 0, func(response agentprotocol.CommandsResponse) error {
			offered = len(response.Commands)
			return errors.New("write: connection reset by peer")
		})
		if err != nil || offered != 1 {
			t.Fatalf("poll: offered %d, err %v", offered, err)
		}
		redelivered(t, service, store, command.ID)
	})
}
