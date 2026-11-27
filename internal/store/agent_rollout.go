package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

// SwitchAgentRollout makes the endpoint's rollout server its primary, with
// its own server as the fallback, in one plan (primary-fallback). The switch
// rests on a Release Guard decision the caller made from the setup's measured
// evidence (decision PASS); it is recorded as the endpoint's guard evaluation
// for the plan, then the plan becomes active. A switch taken twice returns
// the first. The endpoint's own server is never removed.
func (s *Store) SwitchAgentRollout(ctx context.Context, tenant, endpoint, rolloutBindingID, decision string, reasons, metrics any) (domain.AgentRolloutSwitch, error) {
	if decision != "PASS" {
		return domain.AgentRolloutSwitch{}, fmt.Errorf("%w: a rollout switches only on a PASS decision", ErrConflict)
	}
	own, err := s.agentRolloutBindings(ctx, tenant, endpoint, rolloutBindingID)
	if err != nil {
		return domain.AgentRolloutSwitch{}, err
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpoint)
	if err != nil {
		return domain.AgentRolloutSwitch{}, err
	}
	plan, err := s.findOrCreatePlan(ctx, tenant, resolved, domain.ServingPlan{TenantID: tenant, EndpointID: resolved.Endpoint.ID, RoutingPolicy: "primary-fallback",
		Bindings: []domain.ServingPlanBinding{{BindingID: rolloutBindingID, Priority: 0, Weight: 100}, {BindingID: own, Priority: 1, Weight: 100}}})
	if err != nil {
		return domain.AgentRolloutSwitch{}, err
	}
	if resolved.ActivePlan.ID == plan.ID {
		// Taken before: the PASS that allowed it says from which plan and
		// when.
		switched := domain.AgentRolloutSwitch{PlanID: plan.ID, OwnBindingID: own}
		var at string
		err = s.QueryRowContext(ctx, `SELECT id,active_serving_plan_id,created_at FROM endpoint_release_guard_evaluations WHERE endpoint_id=? AND candidate_serving_plan_id=? AND decision='PASS' ORDER BY created_at DESC,id DESC LIMIT 1`, resolved.Endpoint.ID, plan.ID).Scan(&switched.EvaluationID, &switched.PreviousPlanID, &at)
		if err != nil {
			return domain.AgentRolloutSwitch{}, fmt.Errorf("the rollout's plan is active without its PASS: %w", err)
		}
		switched.SwitchedAt = parseTime(at)
		return switched, nil
	}
	previous := resolved.ActivePlan.ID
	if err = s.SetEndpointPlan(ctx, tenant, endpoint, plan.ID, "candidate"); err != nil {
		return domain.AgentRolloutSwitch{}, err
	}
	if resolved, err = s.ResolveEndpointForTenant(ctx, tenant, endpoint); err != nil {
		return domain.AgentRolloutSwitch{}, err
	}
	evaluation, err := s.recordEndpointGuardEvaluation(ctx, tenant, resolved, domain.EndpointReleaseGuardPolicy{}, decision, reasons, metrics)
	if err != nil {
		return domain.AgentRolloutSwitch{}, err
	}
	if err = s.SetEndpointPlan(ctx, tenant, endpoint, plan.ID, "active"); err != nil {
		return domain.AgentRolloutSwitch{}, err
	}
	return domain.AgentRolloutSwitch{PlanID: plan.ID, PreviousPlanID: previous, EvaluationID: evaluation.ID, OwnBindingID: own, SwitchedAt: evaluation.CreatedAt}, nil
}

// BindingMetricsBetween reads one binding's requests started in [from, to),
// as BindingMetrics reads a window ending now: a request that failed over
// away from the binding counts as one of its errors.
func (s *Store) BindingMetricsBetween(ctx context.Context, tenant, bindingID string, from, to time.Time) (domain.RevisionMetrics, error) {
	if !from.Before(to) {
		return domain.RevisionMetrics{}, errors.New("metrics window must end after it starts")
	}
	metrics, err := s.requestMetrics(ctx, bindingRequests, bindingID, `tenant_id=? AND started_at>=? AND started_at<?`, tenant, from.UTC().Format(time.RFC3339Nano), to.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return metrics, err
	}
	err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM backend_bindings b JOIN targets t ON t.id=b.target_id WHERE b.id=? AND t.health='healthy'`, bindingID).Scan(&metrics.ReadyReplicas)
	metrics.EvidenceSource = "binding_request_telemetry"
	return metrics, err
}

// RollBackAgentRollout makes the endpoint's previous plan active again,
// without a new PASS: a rollback returns to a plan that already served. The
// reason is recorded as a REJECT evaluation of the rollout's plan. Rolling
// back twice is a no-op.
func (s *Store) RollBackAgentRollout(ctx context.Context, tenant, endpoint, rolloutPlanID, previousPlanID string, reasons, metrics any) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var endpointID, active string
	if err = tx.QueryRowContext(ctx, `SELECT id,COALESCE(active_serving_plan_id,'') FROM endpoints WHERE tenant_id=? AND name=? AND desired_state<>'deleted' FOR UPDATE`, tenant, endpoint).Scan(&endpointID, &active); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if active == previousPlanID {
		return tx.Commit()
	}
	if active != rolloutPlanID {
		return fmt.Errorf("%w: the endpoint's active plan is no longer the rollout's", ErrConflict)
	}
	var owner string
	if err = tx.QueryRowContext(ctx, `SELECT endpoint_id FROM serving_plans WHERE tenant_id=? AND id=?`, tenant, previousPlanID).Scan(&owner); errors.Is(err, sql.ErrNoRows) || owner != endpointID {
		return fmt.Errorf("%w: the previous plan is not this endpoint's", ErrConflict)
	} else if err != nil {
		return err
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `UPDATE endpoints SET active_serving_plan_id=?,candidate_serving_plan_id=NULL,updated_at=? WHERE id=?`, previousPlanID, stamp, endpointID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpoint)
	if err != nil {
		return err
	}
	resolved.CandidatePlan = &domain.ServingPlan{ID: rolloutPlanID}
	_, err = s.recordEndpointGuardEvaluation(ctx, tenant, resolved, domain.EndpointReleaseGuardPolicy{}, "REJECT", reasons, metrics)
	return err
}

// agentRolloutBindings checks that rolloutBindingID is a rollout server of
// the endpoint and returns the binding of the endpoint's own server.
func (s *Store) agentRolloutBindings(ctx context.Context, tenant, endpoint, rolloutBindingID string) (string, error) {
	var own string
	err := s.QueryRowContext(ctx, `SELECT a.binding_id FROM endpoints e JOIN adopted_workloads a ON a.endpoint_id=e.id JOIN agent_rollout_servers r ON r.endpoint_id=e.id AND r.binding_id=? JOIN targets t ON t.id=a.target_id AND t.agent_id=r.agent_id WHERE e.tenant_id=? AND e.name=? AND e.desired_state<>'deleted' AND a.ownership_mode='agent-managed'`, rolloutBindingID, tenant, endpoint).Scan(&own)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: binding %s is not a rollout server of endpoint %s", ErrConflict, rolloutBindingID, endpoint)
	}
	return own, err
}

// ActiveAgentRollout reports the switch of a rollout whose plan is the
// endpoint's active plan, without switching anything.
func (s *Store) ActiveAgentRollout(ctx context.Context, tenant, endpoint, rolloutBindingID string) (domain.AgentRolloutSwitch, bool, error) {
	own, err := s.agentRolloutBindings(ctx, tenant, endpoint, rolloutBindingID)
	if err != nil {
		return domain.AgentRolloutSwitch{}, false, err
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpoint)
	if err != nil {
		return domain.AgentRolloutSwitch{}, false, err
	}
	_, body, err := normalizePlan("primary-fallback", []domain.ServingPlanBinding{{BindingID: rolloutBindingID, Priority: 0, Weight: 100}, {BindingID: own, Priority: 1, Weight: 100}}, "")
	if err != nil {
		return domain.AgentRolloutSwitch{}, false, err
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if resolved.ActivePlan.SpecDigest != digest {
		return domain.AgentRolloutSwitch{}, false, nil
	}
	switched := domain.AgentRolloutSwitch{PlanID: resolved.ActivePlan.ID, OwnBindingID: own}
	var at string
	if err = s.QueryRowContext(ctx, `SELECT id,active_serving_plan_id,created_at FROM endpoint_release_guard_evaluations WHERE endpoint_id=? AND candidate_serving_plan_id=? AND decision='PASS' ORDER BY created_at DESC,id DESC LIMIT 1`, resolved.Endpoint.ID, switched.PlanID).Scan(&switched.EvaluationID, &switched.PreviousPlanID, &at); err != nil {
		return domain.AgentRolloutSwitch{}, false, fmt.Errorf("the rollout's plan is active without its PASS: %w", err)
	}
	switched.SwitchedAt = parseTime(at)
	return switched, true, nil
}
