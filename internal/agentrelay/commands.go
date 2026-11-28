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
	// ReleaseAgentCommand undoes one lease (tenant, agent, command, attempt)
	// whose answer never reached the agent.
	ReleaseAgentCommand(context.Context, string, string, string, int) error
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
	RunCandidate  *agentprotocol.CandidateRunSpec   `json:"run_candidate,omitempty"`
	MeasureWindow *agentprotocol.WindowSpec         `json:"measure_window,omitempty"`
	StartServer   *agentprotocol.StartServerSpec    `json:"start_server,omitempty"`
	StopServer    *agentprotocol.StopServerSpec     `json:"stop_server,omitempty"`
	Control       *agentprotocol.CandidateRunServer `json:"control,omitempty"`
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
	control, err := s.controlFor(ctx, tenant, agent, spec.Endpoint)
	if err != nil {
		return domain.Agent{}, domain.AgentCommand{}, err
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

// controlFor resolves the server of endpoint on agent: the current setup a
// candidate is measured against, named by the agent's own server key.
func (s *Service) controlFor(ctx context.Context, tenant string, agent domain.Agent, endpoint string) (*agentprotocol.CandidateRunServer, error) {
	servers, err := s.Store.AgentServers(ctx, tenant, agent.ID)
	if err != nil {
		return nil, err
	}
	var control *agentprotocol.CandidateRunServer
	for _, server := range servers {
		// The current setup is the endpoint's own server, never a server
		// a rollout started beside it.
		if server.EndpointName == endpoint && !server.Rollout {
			control = &agentprotocol.CandidateRunServer{Endpoint: server.EndpointName, ServerKey: server.Key, Model: server.UpstreamModel}
		}
	}
	if control == nil {
		return nil, fmt.Errorf("%w: endpoint %s is not served by agent %s", ErrInvalid, endpoint, agent.Name)
	}
	return control, nil
}

// QueueWindow queues one measured window of an optimization run on an agent.
// key makes it idempotent: queuing again with the same key returns the
// window queued first, in whatever state it is, so a retried step never
// measures twice. Only the control plane's campaign worker calls it.
func (s *Service) QueueWindow(ctx context.Context, tenant, agentID, actor, key string, spec agentprotocol.WindowSpec) (domain.AgentCommand, error) {
	store, ok := s.commandStore()
	if !ok {
		return domain.AgentCommand{}, ErrUnavailable
	}
	if strings.TrimSpace(key) == "" {
		return domain.AgentCommand{}, fmt.Errorf("%w: a measured window needs an idempotency key", ErrInvalid)
	}
	if err := spec.Validate(); err != nil {
		return domain.AgentCommand{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	agent, err := s.Store.AgentForTenant(ctx, tenant, agentID)
	if err != nil {
		return domain.AgentCommand{}, err
	}
	control, err := s.controlFor(ctx, tenant, agent, spec.Endpoint)
	if err != nil {
		return domain.AgentCommand{}, err
	}
	encoded, err := json.Marshal(commandSpec{MeasureWindow: &spec, Control: control})
	if err != nil {
		return domain.AgentCommand{}, err
	}
	command, err := store.EnqueueAgentCommand(ctx, domain.AgentCommand{
		TenantID: tenant, AgentID: agent.ID, Kind: agentprotocol.CommandMeasureWindow, SpecJSON: string(encoded), CreatedBy: actor, IdempotencyKey: key,
		// The deadline includes time for the window to wait in the queue
		// behind the agent's other work, which runs one at a time.
		DeadlineAt: s.now().Add(time.Duration(spec.TimeoutSecs)*time.Second + 2*time.Hour),
	})
	if err != nil {
		return domain.AgentCommand{}, err
	}
	s.wakeAgent(agent.ID)
	return command, nil
}

// QueueServerCommand queues a rollout's start_server or stop_server on an
// agent, idempotent per key like a measured window. Only the control plane's
// rollout operation calls it.
func (s *Service) QueueServerCommand(ctx context.Context, tenant, agentID, actor, key string, start *agentprotocol.StartServerSpec, stop *agentprotocol.StopServerSpec) (domain.AgentCommand, error) {
	store, ok := s.commandStore()
	if !ok {
		return domain.AgentCommand{}, ErrUnavailable
	}
	if strings.TrimSpace(key) == "" || (start == nil) == (stop == nil) {
		return domain.AgentCommand{}, fmt.Errorf("%w: a server command needs a key and exactly one of start or stop", ErrInvalid)
	}
	kind, timeout, err := agentprotocol.CommandStopServer, 10*time.Minute, error(nil)
	if start != nil {
		kind, timeout, err = agentprotocol.CommandStartServer, time.Duration(start.TimeoutSecs)*time.Second, start.Validate()
	} else {
		err = stop.Validate()
	}
	if err != nil {
		return domain.AgentCommand{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	agent, err := s.Store.AgentForTenant(ctx, tenant, agentID)
	if err != nil {
		return domain.AgentCommand{}, err
	}
	encoded, err := json.Marshal(commandSpec{StartServer: start, StopServer: stop})
	if err != nil {
		return domain.AgentCommand{}, err
	}
	command, err := store.EnqueueAgentCommand(ctx, domain.AgentCommand{TenantID: tenant, AgentID: agent.ID, Kind: kind, SpecJSON: string(encoded), CreatedBy: actor, IdempotencyKey: key,
		DeadlineAt: s.now().Add(timeout + 2*time.Hour)})
	if err != nil {
		return domain.AgentCommand{}, err
	}
	s.wakeAgent(agent.ID)
	return command, nil
}

// DecodeServerResult reads a start_server or stop_server command's result.
func DecodeServerResult(command domain.AgentCommand) (*agentprotocol.ServerResult, error) {
	if strings.TrimSpace(command.ResultJSON) == "" {
		return nil, nil
	}
	var result agentprotocol.ServerResult
	if err := json.Unmarshal([]byte(command.ResultJSON), &result); err != nil {
		return nil, fmt.Errorf("the server result could not be read: %w", err)
	}
	return &result, nil
}

// Window reads one measured window of an agent.
func (s *Service) Window(ctx context.Context, tenant, agentID, id string) (domain.AgentCommand, error) {
	store, ok := s.commandStore()
	if !ok {
		return domain.AgentCommand{}, ErrUnavailable
	}
	return store.AgentCommand(ctx, tenant, agentID, id)
}

// CancelWindow cancels a queued or running measured window; the agent removes
// its candidate on its next report.
func (s *Service) CancelWindow(ctx context.Context, tenant, agentID, id string) (domain.AgentCommand, error) {
	store, ok := s.commandStore()
	if !ok {
		return domain.AgentCommand{}, ErrUnavailable
	}
	return store.CancelAgentCommand(ctx, tenant, agentID, id)
}

// DecodeWindow reads a window command's spec and, once it has one, its
// result.
func DecodeWindow(command domain.AgentCommand) (agentprotocol.WindowSpec, *agentprotocol.WindowResult, error) {
	var spec commandSpec
	if command.Kind != agentprotocol.CommandMeasureWindow || json.Unmarshal([]byte(command.SpecJSON), &spec) != nil || spec.MeasureWindow == nil {
		return agentprotocol.WindowSpec{}, nil, errors.New("the command is not a measured window")
	}
	if strings.TrimSpace(command.ResultJSON) == "" {
		return *spec.MeasureWindow, nil, nil
	}
	var result agentprotocol.WindowResult
	if err := json.Unmarshal([]byte(command.ResultJSON), &result); err != nil {
		return *spec.MeasureWindow, nil, fmt.Errorf("the window result could not be read: %w", err)
	}
	return *spec.MeasureWindow, &result, nil
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

// Commands answers an agent's long poll. It leases commands as soon as there
// are any, or none after wait, and hands the answer to deliver, which writes
// it to the agent. Commands whose answer never reached the agent, because the
// agent went away first or deliver failed, are released at once: the next
// poll takes them instead of waiting out agentprotocol.CommandLease. An error
// means deliver was not called.
func (s *Service) Commands(ctx context.Context, agent domain.Agent, wait time.Duration, deliver func(agentprotocol.CommandsResponse) error) error {
	store, ok := s.commandStore()
	if !ok {
		return ErrUnavailable
	}
	if wait < 0 || wait > agentprotocol.MaxCommandWait {
		wait = agentprotocol.MaxCommandWait
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		leased, err := store.LeaseAgentCommands(ctx, agent.TenantID, agent.ID, s.now(), agentprotocol.CommandLease, agentprotocol.MaxCommandsPerPoll)
		if err != nil {
			return err
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
			if ctx.Err() != nil {
				// The agent went away while its commands were being leased.
				s.releaseCommands(ctx, store, agent, out.Commands)
				out.Commands = []agentprotocol.Command{}
			}
			if deliver(out) != nil {
				s.releaseCommands(ctx, store, agent, out.Commands)
			}
			return nil
		}
		check := time.NewTimer(commandPollInterval)
		select {
		case <-ctx.Done():
			check.Stop()
			_ = deliver(agentprotocol.CommandsResponse{Commands: []agentprotocol.Command{}})
			return nil
		case <-deadline.C:
			check.Stop()
			wait = 0
		case <-s.waiter(agent.ID):
			check.Stop()
		case <-check.C:
		}
	}
}

// releaseTimeout bounds handing back an undelivered answer's commands.
const releaseTimeout = 5 * time.Second

// releaseCommands hands back the commands of an answer that never reached the
// agent. The poll's context is usually done by now, so the release runs on its
// own deadline. A command it cannot release is handed out again when its lease
// ends, as before.
func (s *Service) releaseCommands(ctx context.Context, store CommandStore, agent domain.Agent, commands []agentprotocol.Command) {
	if len(commands) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	for _, command := range commands {
		_ = store.ReleaseAgentCommand(ctx, agent.TenantID, agent.ID, command.ID, command.Attempt)
	}
}

func wireCommand(command domain.AgentCommand) (agentprotocol.Command, error) {
	var spec commandSpec
	if err := json.Unmarshal([]byte(command.SpecJSON), &spec); err != nil {
		return agentprotocol.Command{}, errors.New("the command spec could not be read")
	}
	wire := agentprotocol.Command{ID: command.ID, Kind: command.Kind, DeadlineAt: command.DeadlineAt, Control: spec.Control, Attempt: command.Attempts}
	switch {
	case command.Kind == agentprotocol.CommandStartServer && spec.StartServer != nil:
		wire.StartServer = spec.StartServer
		return wire, spec.StartServer.Validate()
	case command.Kind == agentprotocol.CommandStopServer && spec.StopServer != nil:
		wire.StopServer = spec.StopServer
		return wire, spec.StopServer.Validate()
	case spec.Control == nil:
		return agentprotocol.Command{}, errors.New("the command spec could not be read")
	case command.Kind == agentprotocol.CommandRunCandidate && spec.RunCandidate != nil:
		wire.RunCandidate = spec.RunCandidate
		return wire, spec.RunCandidate.Validate()
	case command.Kind == agentprotocol.CommandMeasureWindow && spec.MeasureWindow != nil:
		wire.MeasureWindow = spec.MeasureWindow
		return wire, spec.MeasureWindow.Validate()
	default:
		return agentprotocol.Command{}, errors.New("the command spec could not be read")
	}
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
	var report any
	switch {
	case update.Result != nil:
		report = update.Result
	case update.Window != nil:
		report = update.Window
	case update.Server != nil:
		report = update.Server
	}
	if report != nil {
		encoded, err := json.Marshal(report)
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
		"id": command.ID, "kind": command.Kind, "agent": agent.Name, "agent_id": agent.ID, "state": command.State, "phase": nullable(command.Phase),
		"spec": spec.RunCandidate, "control": spec.Control, "result": result, "attempts": command.Attempts,
		"failure_code": nullable(command.FailureCode), "failure_detail": nullable(command.FailureDetail),
		"deadline_at": command.DeadlineAt, "created_by": command.CreatedBy, "created_at": command.CreatedAt, "updated_at": command.UpdatedAt,
		"started_at": command.StartedAt, "finished_at": command.FinishedAt,
		"runs_in": "the agent's own cluster; prompts, answers, and engine logs stay there and only the numbers in result are sent",
	}
	if spec.MeasureWindow != nil {
		// A measured window belongs to an optimization run; the run's own
		// pages judge it. Its spec and numbers are shown as they are.
		view["spec"] = spec.MeasureWindow
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
