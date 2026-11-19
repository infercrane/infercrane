package agentrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/domain"
)

// CommandStore is the persistence of the agent command channel.
type CommandStore interface {
	EnqueueAgentCommand(context.Context, domain.AgentCommand) (domain.AgentCommand, error)
	AgentCommand(context.Context, string, string, string) (domain.AgentCommand, error)
	AgentCommands(context.Context, string, string, int) ([]domain.AgentCommand, error)
	LeaseAgentCommands(context.Context, string, string, time.Time, time.Duration, int) ([]domain.AgentCommand, error)
	UpdateAgentCommand(context.Context, string, string, string, domain.AgentCommandUpdate) (domain.AgentCommand, error)
	CancelAgentCommand(context.Context, string, string, string) (domain.AgentCommand, error)
}

func (s *Service) commandStore() (CommandStore, bool) {
	store, ok := s.Store.(CommandStore)
	return store, ok
}

// commandSpec is what a queued command stores: the candidate spec and the
// current setup it is measured against, resolved when it was queued.
type commandSpec struct {
	RunCandidate *agentprotocol.CandidateRunSpec   `json:"run_candidate"`
	Control      *agentprotocol.CandidateRunServer `json:"control"`
}

// CreateCandidateRun queues one candidate run on an agent. The endpoint must
// be one this agent serves or observes; its server is the current setup the
// candidate is measured against.
func (s *Service) CreateCandidateRun(ctx context.Context, tenant, agentIDOrName, actor string, spec agentprotocol.CandidateRunSpec) (domain.Agent, domain.AgentCommand, error) {
	store, ok := s.commandStore()
	if !ok {
		return domain.Agent{}, domain.AgentCommand{}, ErrUnavailable
	}
	if spec.TimeoutSecs == 0 {
		spec.TimeoutSecs = int(agentprotocol.DefaultCandidateRunTime / time.Second)
	}
	if spec.Quality.EvalSet == "" && !spec.Quality.Skip {
		spec.Quality.EvalSet = "default"
	}
	if err := spec.Validate(); err != nil {
		return domain.Agent{}, domain.AgentCommand{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	agent, err := s.Store.AgentForTenant(ctx, tenant, agentIDOrName)
	if err != nil {
		return domain.Agent{}, domain.AgentCommand{}, err
	}
	if agent.State != "active" {
		return domain.Agent{}, domain.AgentCommand{}, fmt.Errorf("%w: the agent is revoked", domain.ErrConflict)
	}
	servers, err := s.Store.AgentServers(ctx, tenant, agent.ID)
	if err != nil {
		return domain.Agent{}, domain.AgentCommand{}, err
	}
	var control *agentprotocol.CandidateRunServer
	for _, server := range servers {
		if server.EndpointName == spec.Endpoint {
			control = &agentprotocol.CandidateRunServer{Endpoint: server.EndpointName, ServerKey: server.Key, Model: server.UpstreamModel}
		}
	}
	if control == nil {
		return domain.Agent{}, domain.AgentCommand{}, fmt.Errorf("%w: endpoint %s is not served by agent %s", ErrInvalid, spec.Endpoint, agent.Name)
	}
	encoded, err := json.Marshal(commandSpec{RunCandidate: &spec, Control: control})
	if err != nil {
		return domain.Agent{}, domain.AgentCommand{}, err
	}
	command, err := store.EnqueueAgentCommand(ctx, domain.AgentCommand{
		TenantID: tenant, AgentID: agent.ID, Kind: agentprotocol.CommandRunCandidate, SpecJSON: string(encoded), CreatedBy: actor,
		// The deadline includes time for the command to wait in the queue.
		DeadlineAt: s.now().Add(time.Duration(spec.TimeoutSecs)*time.Second + 2*time.Hour),
	})
	if err != nil {
		return domain.Agent{}, domain.AgentCommand{}, err
	}
	s.wakeAgent(agent.ID)
	return agent, command, nil
}

// CandidateRuns lists an agent's candidate runs, newest first.
func (s *Service) CandidateRuns(ctx context.Context, tenant, agentIDOrName string) (domain.Agent, []domain.AgentCommand, error) {
	store, ok := s.commandStore()
	if !ok {
		return domain.Agent{}, nil, ErrUnavailable
	}
	agent, err := s.Store.AgentForTenant(ctx, tenant, agentIDOrName)
	if err != nil {
		return domain.Agent{}, nil, err
	}
	commands, err := store.AgentCommands(ctx, tenant, agent.ID, 50)
	return agent, commands, err
}

// CandidateRun reads one candidate run.
func (s *Service) CandidateRun(ctx context.Context, tenant, agentIDOrName, id string) (domain.Agent, domain.AgentCommand, error) {
	store, ok := s.commandStore()
	if !ok {
		return domain.Agent{}, domain.AgentCommand{}, ErrUnavailable
	}
	agent, err := s.Store.AgentForTenant(ctx, tenant, agentIDOrName)
	if err != nil {
		return domain.Agent{}, domain.AgentCommand{}, err
	}
	command, err := store.AgentCommand(ctx, tenant, agent.ID, id)
	return agent, command, err
}

// CancelCandidateRun cancels a queued or running candidate run.
func (s *Service) CancelCandidateRun(ctx context.Context, tenant, agentIDOrName, id string) (domain.Agent, domain.AgentCommand, error) {
	store, ok := s.commandStore()
	if !ok {
		return domain.Agent{}, domain.AgentCommand{}, ErrUnavailable
	}
	agent, err := s.Store.AgentForTenant(ctx, tenant, agentIDOrName)
	if err != nil {
		return domain.Agent{}, domain.AgentCommand{}, err
	}
	command, err := store.CancelAgentCommand(ctx, tenant, agent.ID, id)
	return agent, command, err
}

// wakeAgent ends a long poll this replica holds for the agent, so a new
// command reaches it at once. Other replicas find it on their next check.
func (s *Service) wakeAgent(agentID string) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if waiter, ok := s.waiters[agentID]; ok {
		close(waiter)
		delete(s.waiters, agentID)
	}
}

func (s *Service) waiter(agentID string) <-chan struct{} {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.waiters == nil {
		s.waiters = map[string]chan struct{}{}
	}
	waiter, ok := s.waiters[agentID]
	if !ok {
		waiter = make(chan struct{})
		s.waiters[agentID] = waiter
	}
	return waiter
}

// commandPollInterval is how often one held long poll checks the database.
const commandPollInterval = 2 * time.Second

// Commands answers an agent's long poll: leased commands as soon as there are
// any, or none after wait.
func (s *Service) Commands(ctx context.Context, agent domain.Agent, wait time.Duration) (agentprotocol.CommandsResponse, error) {
	store, ok := s.commandStore()
	if !ok {
		return agentprotocol.CommandsResponse{}, ErrUnavailable
	}
	if wait < 0 || wait > agentprotocol.MaxCommandWait {
		wait = agentprotocol.MaxCommandWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		leased, err := store.LeaseAgentCommands(ctx, agent.TenantID, agent.ID, s.now(), agentprotocol.CommandLease, agentprotocol.MaxCommandsPerPoll)
		if err != nil {
			return agentprotocol.CommandsResponse{}, err
		}
		if len(leased) > 0 || wait == 0 {
			out := agentprotocol.CommandsResponse{Commands: make([]agentprotocol.Command, 0, len(leased))}
			for _, command := range leased {
				wire, convertErr := wireCommand(command)
				if convertErr != nil {
					// A stored spec this build cannot read fails on its own
					// instead of blocking the agent's queue.
					_, _ = store.UpdateAgentCommand(ctx, agent.TenantID, agent.ID, command.ID, domain.AgentCommandUpdate{State: agentprotocol.CommandFailed, FailureCode: "invalid_command", FailureDetail: convertErr.Error()})
					continue
				}
				out.Commands = append(out.Commands, wire)
			}
			return out, nil
		}
		check := time.NewTimer(commandPollInterval)
		select {
		case <-ctx.Done():
			check.Stop()
			return agentprotocol.CommandsResponse{Commands: []agentprotocol.Command{}}, nil
		case <-deadline.C:
			check.Stop()
			wait = 0
		case <-s.waiter(agent.ID):
			check.Stop()
		case <-check.C:
		}
	}
}

func wireCommand(command domain.AgentCommand) (agentprotocol.Command, error) {
	var spec commandSpec
	if err := json.Unmarshal([]byte(command.SpecJSON), &spec); err != nil || spec.RunCandidate == nil || spec.Control == nil {
		return agentprotocol.Command{}, errors.New("the command spec could not be read")
	}
	if err := spec.RunCandidate.Validate(); err != nil {
		return agentprotocol.Command{}, err
	}
	return agentprotocol.Command{ID: command.ID, Kind: command.Kind, DeadlineAt: command.DeadlineAt, RunCandidate: spec.RunCandidate, Control: spec.Control, Attempt: command.Attempts}, nil
}

// UpdateCommand applies an agent's report on a command it leased.
func (s *Service) UpdateCommand(ctx context.Context, agent domain.Agent, id string, update agentprotocol.CommandUpdate) (agentprotocol.CommandUpdateResponse, error) {
	store, ok := s.commandStore()
	if !ok {
		return agentprotocol.CommandUpdateResponse{}, ErrUnavailable
	}
	if !agentprotocol.ValidCommandID(id) {
		return agentprotocol.CommandUpdateResponse{}, domain.ErrNotFound
	}
	if err := update.Validate(); err != nil {
		return agentprotocol.CommandUpdateResponse{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	result := ""
	if update.Result != nil {
		encoded, err := json.Marshal(update.Result)
		if err != nil {
			return agentprotocol.CommandUpdateResponse{}, err
		}
		result = string(encoded)
	}
	command, err := store.UpdateAgentCommand(ctx, agent.TenantID, agent.ID, id, domain.AgentCommandUpdate{
		State: update.State, Phase: update.Phase, FailureCode: update.FailureCode,
		FailureDetail: agentprotocol.CleanText(update.FailureDetail, agentprotocol.MaxText), ResultJSON: result, Lease: agentprotocol.CommandLease,
	})
	if err != nil {
		return agentprotocol.CommandUpdateResponse{}, err
	}
	response := agentprotocol.CommandUpdateResponse{State: command.State, Cancelled: command.State == agentprotocol.CommandCancelled || command.State == agentprotocol.CommandExpired}
	if command.LeaseExpiresAt != nil {
		response.LeaseTill = *command.LeaseExpiresAt
	}
	return response, nil
}

// CandidateRunView is the API view of one candidate run.
func CandidateRunView(agent domain.Agent, command domain.AgentCommand) map[string]any {
	var spec commandSpec
	_ = json.Unmarshal([]byte(command.SpecJSON), &spec)
	var result any
	if command.ResultJSON != "" {
		_ = json.Unmarshal([]byte(command.ResultJSON), &result)
	}
	view := map[string]any{
		"id": command.ID, "agent": agent.Name, "agent_id": agent.ID, "state": command.State, "phase": nullable(command.Phase),
		"spec": spec.RunCandidate, "control": spec.Control, "result": result, "attempts": command.Attempts,
		"failure_code": nullable(command.FailureCode), "failure_detail": nullable(command.FailureDetail),
		"deadline_at": command.DeadlineAt, "created_by": command.CreatedBy, "created_at": command.CreatedAt, "updated_at": command.UpdatedAt,
		"started_at": command.StartedAt, "finished_at": command.FinishedAt,
		"runs_in": "the agent's own cluster; prompts, answers, and engine logs stay there and only the numbers in result are sent",
	}
	if strings.TrimSpace(command.ResultJSON) == "" {
		view["result"] = nil
	}
	return view
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
