package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
)

// CandidateExecutor runs one candidate run where the agent runs and returns
// numbers only.
type CandidateExecutor interface {
	RunCandidate(context.Context, CandidateRun, func(phase string)) (agentprotocol.CandidateRunResult, error)
}

// CandidateRun is one leased run with the current setup resolved to an
// address this agent found itself.
type CandidateRun struct {
	ID      string
	Spec    agentprotocol.CandidateRunSpec
	Control ControlServer
}

// ControlServer is the current setup a candidate is measured against.
type ControlServer struct {
	Endpoint, BaseURL, Model string
	// Token is the local server's bearer token, if it has one. It is used
	// only on this machine.
	Token string
}

// CandidateError is a run failure with a stable code for the control plane.
type CandidateError struct {
	Code, Detail string
	// Result carries what was measured before the failure, if anything.
	Result *agentprotocol.CandidateRunResult
}

func (e *CandidateError) Error() string { return e.Code + ": " + e.Detail }

func candidateFailure(code, format string, args ...any) *CandidateError {
	return &CandidateError{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// commandHeartbeat keeps a running command's lease while it works.
const commandHeartbeat = 2 * time.Minute

// RunCommands takes commands until ctx ends or the control plane refuses the
// agent. Run starts it when an executor is configured.
func (a *Agent) RunCommands(ctx context.Context) error {
	if a.config.Executor == nil {
		return errors.New("the agent has no candidate executor")
	}
	return a.runCommands(ctx)
}

// runCommands long-polls for commands and runs them one at a time, so two
// candidates never compete for the same accelerators. It returns only on a
// fatal error or when ctx ends.
func (a *Agent) runCommands(ctx context.Context) error {
	failures := 0
	for ctx.Err() == nil {
		response, err := a.config.Client.Commands(ctx, agentprotocol.MaxCommandWait)
		if fatal(err) {
			return err
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotImplemented {
			a.emit(Event{Kind: "rejected", Message: "this control plane does not queue candidate runs; the agent takes no commands"})
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			failures++
			wait := backoff(5*time.Second, failures)
			a.emit(Event{Kind: "retrying", Message: fmt.Sprintf("commands: %v; retrying in %s", err, wait.Round(time.Second))})
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
			}
			continue
		}
		failures = 0
		for _, command := range response.Commands {
			if err = a.runCommand(ctx, command); fatal(err) {
				return err
			}
		}
	}
	return nil
}

func (a *Agent) runCommand(ctx context.Context, command agentprotocol.Command) error {
	report := func(update agentprotocol.CommandUpdate) (agentprotocol.CommandUpdateResponse, error) {
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return a.config.Client.UpdateCommand(reportCtx, command.ID, update)
	}
	fail := func(code, detail string, result *agentprotocol.CandidateRunResult) error {
		if result != nil && result.Validate() != nil {
			result = nil
		}
		_, err := report(agentprotocol.CommandUpdate{State: agentprotocol.CommandFailed, FailureCode: code, FailureDetail: agentprotocol.CleanText(detail, agentprotocol.MaxText), Result: result})
		a.emit(Event{Kind: "candidate_failed", Message: fmt.Sprintf("%s: %s %s", command.ID, code, detail)})
		return err
	}
	if command.Kind != agentprotocol.CommandRunCandidate || command.RunCandidate == nil || command.Control == nil {
		return fail("unsupported_command", "this agent runs only run_candidate commands", nil)
	}
	if err := command.RunCandidate.Validate(); err != nil {
		return fail("invalid_command", err.Error(), nil)
	}
	control, ok := a.controlServer(*command.Control)
	if !ok {
		return fail("control_unavailable", "the current setup's server is not healthy on this agent", nil)
	}
	a.emit(Event{Kind: "candidate_started", Message: fmt.Sprintf("%s: %s against %s", command.ID, command.RunCandidate.Label, control.Endpoint)})
	timeout := time.Duration(command.RunCandidate.TimeoutSecs) * time.Second
	if !command.DeadlineAt.IsZero() && time.Until(command.DeadlineAt) < timeout {
		timeout = time.Until(command.DeadlineAt)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Progress reports keep the lease and learn about cancellation.
	phase := make(chan string, 8)
	done := make(chan struct{})
	reports := make(chan error, 1)
	go func() {
		defer close(reports)
		ticker := time.NewTicker(commandHeartbeat)
		defer ticker.Stop()
		current := "starting"
		for {
			select {
			case <-done:
				return
			case next := <-phase:
				current = next
			case <-ticker.C:
			}
			response, err := report(agentprotocol.CommandUpdate{State: agentprotocol.CommandRunning, Phase: current})
			if fatal(err) {
				reports <- err
				cancel()
				return
			}
			if err == nil && response.Cancelled {
				cancel()
			}
		}
	}()
	result, err := a.config.Executor.RunCandidate(runCtx, CandidateRun{ID: command.ID, Spec: *command.RunCandidate, Control: control}, func(next string) {
		a.emit(Event{Kind: "candidate_phase", Message: command.ID + ": " + next})
		select {
		case phase <- next:
		default:
		}
	})
	close(done)
	if reportErr := <-reports; reportErr != nil {
		return reportErr
	}
	if err != nil {
		var failure *CandidateError
		switch {
		case errors.As(err, &failure):
			return fail(failure.Code, failure.Detail, failure.Result)
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			return fail("deadline_exceeded", "the candidate run did not finish in time", nil)
		case runCtx.Err() != nil:
			return fail("cancelled", "the candidate run was cancelled", nil)
		default:
			return fail("candidate_failed", err.Error(), nil)
		}
	}
	if err = result.Validate(); err != nil {
		return fail("invalid_result", err.Error(), nil)
	}
	_, err = report(agentprotocol.CommandUpdate{State: agentprotocol.CommandSucceeded, Phase: "done", Result: &result})
	if err == nil {
		a.emit(Event{Kind: "candidate_done", Message: command.ID + ": measured and cleaned up"})
	}
	return err
}

// controlServer resolves the current setup to a healthy server this agent
// found itself. The command names it only by server key.
func (a *Agent) controlServer(control agentprotocol.CandidateRunServer) (ControlServer, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	server, ok := a.detected[control.ServerKey]
	status := a.status[control.ServerKey]
	if !ok || status == nil || !status.Registered || status.Endpoint != control.Endpoint || server.Health != agentprotocol.HealthHealthy {
		return ControlServer{}, false
	}
	return ControlServer{Endpoint: control.Endpoint, BaseURL: server.BaseURL, Model: control.Model, Token: bearerToken(server.authHeader)}, true
}
