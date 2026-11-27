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

// WindowExecutor runs one measured window of an optimization run where the
// agent runs and returns numbers only. KubernetesExecutor implements it; an
// executor that does not refuses measure_window commands.
type WindowExecutor interface {
	MeasureWindow(context.Context, WindowJob, func(phase string)) (agentprotocol.WindowResult, error)
}

// WindowJob is one leased window with the current setup resolved to an
// address this agent found itself.
type WindowJob struct {
	ID      string
	Spec    agentprotocol.WindowSpec
	Control ControlServer
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
	// Window carries a window's runs measured before the failure.
	Window *agentprotocol.WindowResult
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
	fail := func(code, detail string, result *agentprotocol.CandidateRunResult, window *agentprotocol.WindowResult) error {
		if result != nil && result.Validate() != nil {
			result = nil
		}
		if window != nil && window.Validate() != nil {
			window = nil
		}
		_, err := report(agentprotocol.CommandUpdate{State: agentprotocol.CommandFailed, FailureCode: code, FailureDetail: agentprotocol.CleanText(detail, agentprotocol.MaxText), Result: result, Window: window})
		a.emit(Event{Kind: "candidate_failed", Message: fmt.Sprintf("%s: %s %s", command.ID, code, detail)})
		return err
	}
	// Each kind names its timeout, its label, and how it runs; leasing,
	// heartbeats, cancellation, and reporting are shared.
	var (
		label       string
		timeoutSecs int
		execute     func(context.Context, ControlServer, func(string)) (agentprotocol.CommandUpdate, error)
	)
	switch {
	case command.Kind == agentprotocol.CommandRunCandidate && command.RunCandidate != nil:
		if err := command.RunCandidate.Validate(); err != nil {
			return fail("invalid_command", err.Error(), nil, nil)
		}
		label, timeoutSecs = command.RunCandidate.Label, command.RunCandidate.TimeoutSecs
		execute = func(runCtx context.Context, control ControlServer, progress func(string)) (agentprotocol.CommandUpdate, error) {
			result, err := a.config.Executor.RunCandidate(runCtx, CandidateRun{ID: command.ID, Spec: *command.RunCandidate, Control: control}, progress)
			if err == nil {
				err = result.Validate()
			}
			return agentprotocol.CommandUpdate{Result: &result}, err
		}
	case command.Kind == agentprotocol.CommandMeasureWindow && command.MeasureWindow != nil && command.Control != nil:
		windows, ok := a.config.Executor.(WindowExecutor)
		if !ok {
			return fail("unsupported_command", "this agent's executor does not run measured windows", nil, nil)
		}
		if err := command.MeasureWindow.Validate(); err != nil {
			return fail("invalid_command", err.Error(), nil, nil)
		}
		if command.MeasureWindow.Endpoint != command.Control.Endpoint {
			return fail("invalid_command", "the window's endpoint is not the current setup's", nil, nil)
		}
		label, timeoutSecs = command.MeasureWindow.Label, command.MeasureWindow.TimeoutSecs
		execute = func(runCtx context.Context, control ControlServer, progress func(string)) (agentprotocol.CommandUpdate, error) {
			result, err := windows.MeasureWindow(runCtx, WindowJob{ID: command.ID, Spec: *command.MeasureWindow, Control: control}, progress)
			if err == nil {
				err = result.Validate()
			}
			return agentprotocol.CommandUpdate{Window: &result}, err
		}
	case command.Kind == agentprotocol.CommandStartServer && command.StartServer != nil, command.Kind == agentprotocol.CommandStopServer && command.StopServer != nil:
		servers, ok := a.config.Executor.(ServerExecutor)
		if !ok {
			return fail("unsupported_command", "this agent's executor does not start servers for rollouts", nil, nil)
		}
		label, timeoutSecs = command.Kind, 600
		if start := command.StartServer; start != nil {
			if err := start.Validate(); err != nil {
				return fail("invalid_command", err.Error(), nil, nil)
			}
			timeoutSecs = start.TimeoutSecs
			execute = func(runCtx context.Context, _ ControlServer, progress func(string)) (agentprotocol.CommandUpdate, error) {
				launched, result, err := servers.StartServer(runCtx, command.ID, *start, progress)
				if err == nil {
					a.addLaunched(launched)
				}
				return agentprotocol.CommandUpdate{Server: &result}, err
			}
		} else {
			stop := command.StopServer
			if err := stop.Validate(); err != nil {
				return fail("invalid_command", err.Error(), nil, nil)
			}
			execute = func(runCtx context.Context, _ ControlServer, _ func(string)) (agentprotocol.CommandUpdate, error) {
				result, err := servers.StopServer(runCtx, *stop)
				if err == nil {
					a.forgetLaunched(stop.ServerKey)
				}
				return agentprotocol.CommandUpdate{Server: &result}, err
			}
		}
	default:
		return fail("unsupported_command", "this agent runs only run_candidate, measure_window, start_server, and stop_server commands", nil, nil)
	}
	// Measuring needs the current setup's server; starting or stopping a
	// rollout's server does not.
	var control ControlServer
	if command.Control != nil {
		resolved, ok := a.controlServer(*command.Control)
		if !ok {
			return fail("control_unavailable", "the current setup's server is not healthy on this agent", nil, nil)
		}
		control = resolved
	} else if command.Kind == agentprotocol.CommandRunCandidate || command.Kind == agentprotocol.CommandMeasureWindow {
		return fail("invalid_command", "a measurement names the current setup's server", nil, nil)
	}
	a.emit(Event{Kind: "candidate_started", Message: fmt.Sprintf("%s: %s against %s", command.ID, label, control.Endpoint)})
	timeout := time.Duration(timeoutSecs) * time.Second
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
	update, err := execute(runCtx, control, func(next string) {
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
			return fail(failure.Code, failure.Detail, failure.Result, failure.Window)
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			return fail("deadline_exceeded", "the candidate run did not finish in time", nil, nil)
		case runCtx.Err() != nil:
			return fail("cancelled", "the candidate run was cancelled", nil, nil)
		case update.Result != nil && update.Result.Validate() != nil, update.Window != nil && update.Window.Validate() != nil, update.Server != nil && update.Server.Validate() != nil:
			return fail("invalid_result", err.Error(), nil, nil)
		default:
			return fail("candidate_failed", err.Error(), nil, nil)
		}
	}
	update.State, update.Phase = agentprotocol.CommandSucceeded, "done"
	_, err = report(update)
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
