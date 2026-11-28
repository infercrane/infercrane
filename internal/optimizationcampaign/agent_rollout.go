package optimizationcampaign

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

// A guarded rollout on an agent's endpoint, after a person activates the
// winner of a run the agent measured:
//
//  1. the agent starts the setup as a server it owns and keeps (start_server);
//  2. the server registers on the endpoint as a second agent-managed binding;
//  3. on the run's Release Guard PASS the endpoint's plan routes to it first,
//     the customer's own server as its fallback;
//  4. for the rollback window the live requests of the new server are
//     compared with the own server's before the switch (Release Guard,
//     AgentGuardPolicy). A rejection, or too little evidence by the end of
//     the window, switches back to the previous plan and stops the server;
//     a pass at the end promotes the setup.
//
// The customer's own server is never stopped or removed: InferCrane did not
// start it.

// Outcomes of activating an agent run's winner.
const (
	AgentRolloutPromoted   = "promoted"
	AgentRolloutRolledBack = "rolled_back"

	// FailureAgentRolloutRolledBack rejects a winner the watch window
	// rolled back.
	FailureAgentRolloutRolledBack = "release_guard_rejected_after_rollout"

	retryAgentRolloutStarting    = "optimization_agent_rollout_starting"
	retryAgentRolloutRegistering = "optimization_agent_rollout_registering"
	retryAgentRolloutObserving   = "optimization_agent_rollout_observing"

	// agentRolloutStartTime bounds how long the agent may take to start
	// the server.
	agentRolloutStartTime = time.Hour
	// agentRolloutBaseline is how much of the own server's traffic before
	// the switch the new server is compared with.
	agentRolloutBaseline = time.Hour
)

// AgentServerCommand is a rollout's start_server or stop_server as the
// relay reports it.
type AgentServerCommand struct {
	ID, State, FailureCode, FailureDetail string
	Result                                *agentprotocol.ServerResult
}

// AgentServerCommands queues a rollout's server commands, idempotent per key.
type AgentServerCommands interface {
	QueueServerCommand(ctx context.Context, tenant, agentID, key string, start *agentprotocol.StartServerSpec, stop *agentprotocol.StopServerSpec) (AgentServerCommand, error)
}

// AgentRolloutStore is the persistence a rollout on an agent's endpoint
// needs.
type AgentRolloutStore interface {
	AgentServers(context.Context, string, string) ([]domain.AgentServer, error)
	SwitchAgentRollout(ctx context.Context, tenant, endpoint, bindingID, decision string, reasons, metrics any) (domain.AgentRolloutSwitch, error)
	ActiveAgentRollout(ctx context.Context, tenant, endpoint, bindingID string) (domain.AgentRolloutSwitch, bool, error)
	RollBackAgentRollout(ctx context.Context, tenant, endpoint, rolloutPlanID, previousPlanID string, reasons, metrics any) error
	BindingMetricsBetween(ctx context.Context, tenant, bindingID string, from, to time.Time) (domain.RevisionMetrics, error)
}

// AgentRollout activates the winner of a run on an agent's endpoint.
type AgentRollout struct {
	Store    AgentRolloutStore
	Commands AgentServerCommands
	// Providers are the control plane's provider profiles, as the run's
	// AgentDriver reads them.
	Providers []integration.ProviderProfile
	Now       func() time.Time
}

func (r AgentRollout) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// AgentRolloutResult is what an activation step reached.
type AgentRolloutResult struct {
	Outcome string
	Switch  domain.AgentRolloutSwitch
	// Guard is the watch window's last Release Guard decision.
	Guard releaseguard.Result
}

// Activate advances the rollout by as much as is ready and returns a
// retryable failure while it waits: for the server to start, to register,
// or for the watch window to end. Every step is idempotent, so the durable
// activation operation can call it again after a restart.
//
// An approval that has expired stops a rollout before its switch, never
// after: a switched endpoint is always watched to its end and rolled back
// or promoted, so it is never left serving an unguarded setup.
func (r AgentRollout) Activate(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) (AgentRolloutResult, error) {
	if r.Store == nil || r.Commands == nil {
		return AgentRolloutResult{}, operations.Permanent("optimization_agent_rollout_unavailable", errors.New("this control plane cannot roll out on an agent's endpoint"))
	}
	if !AgentRun(campaign) || candidate.State != CandidateGuardPassed {
		return AgentRolloutResult{}, operations.Permanent("optimization_candidate_not_qualified", errors.New("a rollout on an agent's endpoint needs a setup that passed Release Guard"))
	}
	draft, err := candidateDraft(candidate)
	if err != nil {
		return AgentRolloutResult{}, operations.Permanent("optimization_candidate_spec_invalid", err)
	}
	// A setup the agent cannot start as it is named, such as a serving graph
	// a run measured before agents refused them, is never rolled out.
	setup, err := AgentSetup(draft, r.Providers)
	if err != nil {
		return AgentRolloutResult{}, agentSetupRefused(err)
	}
	if AgentSetupID(setup) != candidate.RevisionID {
		return AgentRolloutResult{}, operations.Permanent("optimization_candidate_identity_mismatch", errors.New("the setup no longer matches the identity it was measured under"))
	}
	started, err := r.Commands.QueueServerCommand(ctx, campaign.TenantID, campaign.AgentID, childKey(candidate.ID, "rollout-start"), &agentprotocol.StartServerSpec{Endpoint: campaign.TargetEndpoint, Setup: setup, TimeoutSecs: int(agentRolloutStartTime / time.Second)}, nil)
	if err != nil {
		return AgentRolloutResult{}, operations.Retryable("optimization_agent_rollout_queue_failed", err)
	}
	switch started.State {
	case agentprotocol.CommandQueued, agentprotocol.CommandRunning:
		return AgentRolloutResult{}, operations.Retryable(retryAgentRolloutStarting, fmt.Errorf("the agent is starting the setup (%s)", started.ID))
	case agentprotocol.CommandSucceeded:
	default:
		return AgentRolloutResult{}, operations.Permanent("optimization_agent_rollout_start_failed", fmt.Errorf("the agent could not start the setup: %s %s", started.FailureCode, started.FailureDetail))
	}
	if started.Result == nil {
		return AgentRolloutResult{}, operations.Permanent("optimization_agent_rollout_start_failed", errors.New("the agent started the setup but sent no server key"))
	}
	server, listed, err := r.rolloutServer(ctx, campaign, started.Result.ServerKey)
	if err != nil {
		return AgentRolloutResult{}, err
	}
	if !listed {
		return AgentRolloutResult{}, operations.Retryable(retryAgentRolloutRegistering, fmt.Errorf("waiting for the agent to register %s on %s", started.Result.ServerKey, campaign.TargetEndpoint))
	}
	switched, active, err := r.Store.ActiveAgentRollout(ctx, campaign.TenantID, campaign.TargetEndpoint, server.BindingID)
	if err != nil {
		return AgentRolloutResult{}, operations.Retryable("optimization_agent_rollout_lookup_failed", err)
	}
	if active && server.Health != agentprotocol.HealthHealthy {
		// Switched, and the new server is no longer known to serve: it
		// stopped, or its agent lost contact (a server whose agent stops
		// reporting turns unhealthy). Fail closed: the endpoint goes back
		// to the plan that served before, whatever the window says.
		result := AgentRolloutResult{Switch: switched, Guard: releaseguard.Result{Decision: "REJECT", Reasons: []releaseguard.Reason{{Code: "rollout_server_unhealthy", Message: "the rollout's server is not reported healthy"}}}}
		return r.rollBack(ctx, campaign, candidate, server, result)
	}
	if !active && server.Health != agentprotocol.HealthHealthy {
		return AgentRolloutResult{}, operations.Retryable(retryAgentRolloutRegistering, fmt.Errorf("waiting for the agent to report %s healthy on %s", server.Key, campaign.TargetEndpoint))
	}
	if !active {
		if campaign.ApprovalExpiresAt == nil || !campaign.ApprovalExpiresAt.After(r.now()) {
			// Not switched yet: the expired approval stops it here, and
			// the started server is stopped.
			_, _ = r.Commands.QueueServerCommand(ctx, campaign.TenantID, campaign.AgentID, childKey(candidate.ID, "rollout-stop"), nil, &agentprotocol.StopServerSpec{Endpoint: campaign.TargetEndpoint, ServerKey: server.Key})
			return AgentRolloutResult{}, operations.Permanent("optimization_approval_expired", ErrApprovalExpired)
		}
		switched, err = r.Store.SwitchAgentRollout(ctx, campaign.TenantID, campaign.TargetEndpoint, server.BindingID, "PASS",
			[]string{"agent_rollout_release_guard_passed"}, map[string]string{"campaign_id": campaign.ID, "candidate_id": candidate.ID, "setup": candidate.RevisionID, "benchmark_id": candidate.BenchmarkID, "quality_evidence_id": candidate.QualityEvidenceID})
		if err != nil {
			return AgentRolloutResult{}, operations.Retryable("optimization_agent_rollout_switch_failed", err)
		}
	}
	result := AgentRolloutResult{Switch: switched}
	policy := AgentGuardPolicy()
	// Live traffic is the evidence now; the synthetic windows were judged
	// before the switch.
	policy.RequireSyntheticEvidence = false
	now := r.now()
	deadline := switched.SwitchedAt.Add(time.Duration(policy.AutoRollbackWindowSeconds) * time.Second)
	if now.After(switched.SwitchedAt) {
		active, baselineErr := r.Store.BindingMetricsBetween(ctx, campaign.TenantID, switched.OwnBindingID, switched.SwitchedAt.Add(-agentRolloutBaseline), switched.SwitchedAt)
		rolled, candidateErr := r.Store.BindingMetricsBetween(ctx, campaign.TenantID, server.BindingID, switched.SwitchedAt, now)
		if err = errors.Join(baselineErr, candidateErr); err != nil {
			return result, operations.Retryable("optimization_agent_rollout_metrics_failed", err)
		}
		result.Guard = releaseguard.Evaluate(releaseguard.Input{Policy: policy, Active: active, Candidate: rolled})
	} else {
		result.Guard = releaseguard.Result{Decision: "WAIT"}
	}
	rejected := result.Guard.Decision == "REJECT" || result.Guard.Decision != "ACCEPT" && !now.Before(deadline)
	switch {
	case rejected:
		return r.rollBack(ctx, campaign, candidate, server, result)
	case now.Before(deadline):
		return result, operations.Retryable(retryAgentRolloutObserving, fmt.Errorf("watching the rollout until %s (Release Guard %s)", deadline.Format(time.RFC3339), result.Guard.Decision))
	default:
		result.Outcome = AgentRolloutPromoted
		return result, nil
	}
}

// rolloutServer is the rollout's server as the agent registered it on the
// endpoint, healthy or not, and whether it is registered.
func (r AgentRollout) rolloutServer(ctx context.Context, campaign domain.OptimizationCampaign, key string) (domain.AgentServer, bool, error) {
	servers, err := r.Store.AgentServers(ctx, campaign.TenantID, campaign.AgentID)
	if err != nil {
		return domain.AgentServer{}, false, operations.Retryable("optimization_agent_rollout_lookup_failed", err)
	}
	for _, server := range servers {
		if server.Rollout && server.Key == key && server.EndpointName == campaign.TargetEndpoint {
			return server, true, nil
		}
	}
	return domain.AgentServer{}, false, nil
}

// rollBack switches the endpoint back to the plan that served before and
// stops the rollout's server. The customer's own server is untouched.
func (r AgentRollout) rollBack(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, server domain.AgentServer, result AgentRolloutResult) (AgentRolloutResult, error) {
	reasons := []string{"agent_rollout_" + result.Guard.Decision}
	for _, reason := range result.Guard.Reasons {
		reasons = append(reasons, reason.Code)
	}
	if err := r.Store.RollBackAgentRollout(ctx, campaign.TenantID, campaign.TargetEndpoint, result.Switch.PlanID, result.Switch.PreviousPlanID, reasons, result.Guard); err != nil {
		return result, operations.Retryable("optimization_agent_rollback_failed", err)
	}
	stop, err := r.Commands.QueueServerCommand(ctx, campaign.TenantID, campaign.AgentID, childKey(candidate.ID, "rollout-stop"), nil, &agentprotocol.StopServerSpec{Endpoint: campaign.TargetEndpoint, ServerKey: server.Key})
	if err != nil {
		// The endpoint already serves from its own server again; the stop
		// is queued on the next attempt.
		return result, operations.Retryable("optimization_agent_rollout_stop_queue_failed", err)
	}
	_ = stop
	result.Outcome = AgentRolloutRolledBack
	return result, nil
}
