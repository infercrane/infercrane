package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
)

// rolloutCommands answers server commands by key from a state table.
type rolloutCommands struct {
	start  string // state of the start command
	queued map[string]bool
}

func (c *rolloutCommands) QueueServerCommand(_ context.Context, _, _, key string, start *agentprotocol.StartServerSpec, stop *agentprotocol.StopServerSpec) (AgentServerCommand, error) {
	if c.queued == nil {
		c.queued = map[string]bool{}
	}
	c.queued[key] = true
	if stop != nil {
		return AgentServerCommand{ID: "stop-1", State: agentprotocol.CommandQueued}, nil
	}
	command := AgentServerCommand{ID: "start-1", State: c.start}
	if c.start == agentprotocol.CommandSucceeded {
		command.Result = &agentprotocol.ServerResult{ServerKey: "rollout-8000", ReadySeconds: 40}
	}
	return command, nil
}

// rolloutStore is the endpoint as the rollout sees it.
type rolloutStore struct {
	registered, switched, rolledBack bool
	// silent is an agent that stopped reporting: the control plane marks
	// its servers unhealthy.
	silent      bool
	switchedAt  time.Time
	own, rolled domain.RevisionMetrics
}

func (s *rolloutStore) AgentServers(context.Context, string, string) ([]domain.AgentServer, error) {
	servers := []domain.AgentServer{{Key: "own-8000", EndpointName: "support-bot", BindingID: "binding-own", Health: "healthy"}}
	if s.registered {
		health := "healthy"
		if s.silent {
			health = "unhealthy"
		}
		servers = append(servers, domain.AgentServer{Key: "rollout-8000", EndpointName: "support-bot", BindingID: "binding-rollout", Health: health, Rollout: true})
	}
	return servers, nil
}
func (s *rolloutStore) ActiveAgentRollout(context.Context, string, string, string) (domain.AgentRolloutSwitch, bool, error) {
	return s.current(), s.switched && !s.rolledBack, nil
}
func (s *rolloutStore) SwitchAgentRollout(_ context.Context, _, _, bindingID, decision string, _, _ any) (domain.AgentRolloutSwitch, error) {
	if decision != "PASS" || bindingID != "binding-rollout" {
		return domain.AgentRolloutSwitch{}, errors.New("bad switch")
	}
	s.switched = true
	return s.current(), nil
}
func (s *rolloutStore) current() domain.AgentRolloutSwitch {
	return domain.AgentRolloutSwitch{PlanID: "plan-rollout", PreviousPlanID: "plan-own", EvaluationID: "eval-1", OwnBindingID: "binding-own", SwitchedAt: s.switchedAt}
}
func (s *rolloutStore) RollBackAgentRollout(_ context.Context, _, _, rolloutPlanID, previousPlanID string, _, _ any) error {
	if rolloutPlanID != "plan-rollout" || previousPlanID != "plan-own" {
		return errors.New("bad rollback")
	}
	s.rolledBack = true
	return nil
}
func (s *rolloutStore) BindingMetricsBetween(_ context.Context, _, bindingID string, _, _ time.Time) (domain.RevisionMetrics, error) {
	if bindingID == "binding-rollout" {
		return s.rolled, nil
	}
	return s.own, nil
}

func liveMetrics(requests int, ttft float64) domain.RevisionMetrics {
	latency, speed := 4000.0, 30.0
	return domain.RevisionMetrics{Requests: requests, ReadyReplicas: 1, P95TTFTMS: &ttft, P95LatencyMS: &latency, OutputTokensPerSecond: &speed}
}

func rolloutFixture(t *testing.T) (agentRun, *rolloutStore, *rolloutCommands, *time.Time) {
	t.Helper()
	run := newAgentRun(t, "throughput", "--max-num-seqs", "256")
	run.store.campaign.Candidates[1].State = CandidateGuardPassed
	clock := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	store := &rolloutStore{switchedAt: clock, own: liveMetrics(400, 120), rolled: liveMetrics(300, 125)}
	return run, store, &rolloutCommands{start: agentprotocol.CommandRunning}, &clock
}

func activate(t *testing.T, run agentRun, store *rolloutStore, commands *rolloutCommands, clock *time.Time) (AgentRolloutResult, error) {
	t.Helper()
	campaign, _ := run.store.OptimizationCampaign(t.Context(), "tenant", "campaign-1")
	return AgentRollout{Store: store, Commands: commands, Now: func() time.Time { return *clock }}.Activate(t.Context(), campaign, run.store.candidate(run.setup.ID))
}

func retryCode(err error) string {
	var failure operations.Failure
	if errors.As(err, &failure) && failure.Retryable {
		return failure.Code
	}
	return ""
}

// The rollout waits for the server to start and register, switches on the
// run's PASS, watches the window, and promotes only at its end.
func TestAgentRolloutStartsSwitchesWatchesAndPromotes(t *testing.T) {
	run, store, commands, clock := rolloutFixture(t)
	if _, err := activate(t, run, store, commands, clock); retryCode(err) != retryAgentRolloutStarting {
		t.Fatalf("starting: %v", err)
	}
	commands.start = agentprotocol.CommandSucceeded
	if _, err := activate(t, run, store, commands, clock); retryCode(err) != retryAgentRolloutRegistering || store.switched {
		t.Fatalf("registering: %v switched=%v", err, store.switched)
	}
	store.registered = true
	*clock = clock.Add(time.Minute)
	result, err := activate(t, run, store, commands, clock)
	if retryCode(err) != retryAgentRolloutObserving || !store.switched || result.Guard.Decision != "ACCEPT" {
		t.Fatalf("observing: %+v %v", result.Guard, err)
	}
	*clock = clock.Add(5 * time.Minute)
	result, err = activate(t, run, store, commands, clock)
	if err != nil || result.Outcome != AgentRolloutPromoted || store.rolledBack || commands.queued[childKey(run.setup.ID, "rollout-stop")] {
		t.Fatalf("promoted: %+v %v", result, err)
	}
}

// A setup that serves much worse live is rolled back at once: the previous
// plan serves again and the rollout's server is stopped. So is one that got
// too little traffic by the end of the window.
func TestAgentRolloutRollsBackOnRejectionOrTooLittleEvidence(t *testing.T) {
	run, store, commands, clock := rolloutFixture(t)
	commands.start, store.registered = agentprotocol.CommandSucceeded, true
	store.rolled = liveMetrics(300, 400)
	*clock = clock.Add(time.Minute)
	result, err := activate(t, run, store, commands, clock)
	if err != nil || result.Outcome != AgentRolloutRolledBack || !store.rolledBack || !commands.queued[childKey(run.setup.ID, "rollout-stop")] || result.Guard.Decision != "REJECT" {
		t.Fatalf("rejection: %+v %v", result, err)
	}

	run, store, commands, clock = rolloutFixture(t)
	commands.start, store.registered = agentprotocol.CommandSucceeded, true
	store.rolled = liveMetrics(3, 125)
	*clock = clock.Add(time.Minute)
	if _, err = activate(t, run, store, commands, clock); retryCode(err) != retryAgentRolloutObserving || store.rolledBack {
		t.Fatalf("too little evidence inside the window: %v", err)
	}
	*clock = clock.Add(10 * time.Minute)
	if result, err = activate(t, run, store, commands, clock); err != nil || result.Outcome != AgentRolloutRolledBack || !store.rolledBack {
		t.Fatalf("too little evidence at the end: %+v %v", result, err)
	}
}

// An expired approval stops a rollout before its switch and stops the
// started server; a rollout already switched is still watched to its end.
func TestAgentRolloutApprovalExpiryStopsOnlyBeforeTheSwitch(t *testing.T) {
	run, store, commands, clock := rolloutFixture(t)
	commands.start, store.registered = agentprotocol.CommandSucceeded, true
	expired := clock.Add(-time.Minute)
	run.store.campaign.ApprovalExpiresAt = &expired
	_, err := activate(t, run, store, commands, clock)
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Retryable || store.switched || !commands.queued[childKey(run.setup.ID, "rollout-stop")] {
		t.Fatalf("expired before the switch: %v switched=%v", err, store.switched)
	}

	run, store, commands, clock = rolloutFixture(t)
	commands.start, store.registered, store.switched = agentprotocol.CommandSucceeded, true, true
	run.store.campaign.ApprovalExpiresAt = &expired
	*clock = clock.Add(10 * time.Minute)
	if result, err := activate(t, run, store, commands, clock); err != nil || result.Outcome != AgentRolloutPromoted {
		t.Fatalf("expired during the watch: %+v %v", result, err)
	}
}

// The activation operation records the rollout's outcome on the setup.
func TestActivationRecordsAnAgentRolloutsOutcome(t *testing.T) {
	run, store, commands, clock := rolloutFixture(t)
	commands.start, store.registered = agentprotocol.CommandSucceeded, true
	store.rolled = liveMetrics(300, 400)
	*clock = clock.Add(time.Minute)
	transitions := &transitionRecorder{agentStore: run.store}
	payload, _ := json.Marshal(ActivateRequest{TenantID: "tenant", CampaignID: "campaign-1", CandidateID: run.setup.ID, Actor: "operator"})
	rollout := AgentRollout{Store: store, Commands: commands, Now: func() time.Time { return *clock }}
	result, err := ActivationHandlersWith(transitions, nil, func() time.Time { return *clock }, &rollout)[ActivateKind](t.Context(), domain.Operation{RequestJSON: string(payload)})
	if err != nil || transitions.to != CandidateRejected || transitions.failure != FailureAgentRolloutRolledBack {
		t.Fatalf("activation: %s %v %+v", result, err, transitions)
	}
}

type transitionRecorder struct {
	*agentStore
	to, failure string
}

func (r *transitionRecorder) TransitionOptimizationCandidate(_ context.Context, _, _, _, _, to string, updates domain.OptimizationCandidateRun) (domain.OptimizationCandidateRun, error) {
	r.to, r.failure = to, updates.FailureCode
	return domain.OptimizationCandidateRun{State: to}, nil
}
func (r *transitionRecorder) EnqueueOperation(context.Context, domain.Operation) (domain.Operation, bool, error) {
	return domain.Operation{}, false, errors.New("an agent rollout enqueues no deployment rollout")
}
func (r *transitionRecorder) PublishDeploymentEndpoint(context.Context, string, string, string) (domain.ResolvedEndpoint, error) {
	return domain.ResolvedEndpoint{}, errors.New("an agent rollout publishes no deployment endpoint")
}

// The agent stops reporting during the watch window: no heartbeat, no
// data-plane records. The rollout fails closed and is never promoted on
// silence. Once the control plane marks the agent's servers unhealthy, the
// endpoint switches back at once (the agent applies the old plan when it
// next pulls one) and the stop is queued for when the agent returns. Before
// that, silence is too little evidence: watched, then rolled back at the
// end of the window.
func TestAgentRolloutFailsClosedWhenTheAgentGoesSilent(t *testing.T) {
	run, store, commands, clock := rolloutFixture(t)
	commands.start, store.registered = agentprotocol.CommandSucceeded, true
	*clock = clock.Add(time.Minute)
	if _, err := activate(t, run, store, commands, clock); retryCode(err) != retryAgentRolloutObserving || !store.switched {
		t.Fatalf("watching: %v", err)
	}
	// The agent goes silent: its servers turn unhealthy.
	store.silent = true
	*clock = clock.Add(4 * time.Minute)
	result, err := activate(t, run, store, commands, clock)
	if err != nil || result.Outcome != AgentRolloutRolledBack || !store.rolledBack || !commands.queued[childKey(run.setup.ID, "rollout-stop")] || result.Guard.Reasons[0].Code != "rollout_server_unhealthy" {
		t.Fatalf("silent agent: %+v %v", result, err)
	}

	// Silent before the servers turn unhealthy: no records at all is no
	// evidence, never a pass.
	run, store, commands, clock = rolloutFixture(t)
	commands.start, store.registered = agentprotocol.CommandSucceeded, true
	store.rolled = domain.RevisionMetrics{ReadyReplicas: 1}
	for _, step := range []time.Duration{time.Minute, 2 * time.Minute, time.Minute} {
		*clock = clock.Add(step)
		if result, err = activate(t, run, store, commands, clock); retryCode(err) != retryAgentRolloutObserving || result.Outcome == AgentRolloutPromoted {
			t.Fatalf("silent inside the window: %+v %v", result, err)
		}
	}
	*clock = clock.Add(2 * time.Minute)
	if result, err = activate(t, run, store, commands, clock); err != nil || result.Outcome != AgentRolloutRolledBack || !store.rolledBack {
		t.Fatalf("silent to the end of the window: %+v %v", result, err)
	}

	// A server that is not yet healthy before the switch is waited for,
	// never switched to.
	run, store, commands, clock = rolloutFixture(t)
	commands.start, store.registered, store.silent = agentprotocol.CommandSucceeded, true, true
	if _, err = activate(t, run, store, commands, clock); retryCode(err) != retryAgentRolloutRegistering || store.switched {
		t.Fatalf("unhealthy before the switch: %v switched=%v", err, store.switched)
	}
}
