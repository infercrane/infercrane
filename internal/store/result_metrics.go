package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/infercrane/infercrane/internal/resultmetrics"
)

// ResultMetrics reads one workspace's Release Guard, rollback, routing, and
// optimization-run results for the Prometheus scrape route. It runs on the
// control path, never while routing an inference request. Each query is
// scoped to the tenant and the snapshot keeps at most
// resultmetrics.MaxSubjects deployments, endpoints, and optimized endpoints.
func (s *Store) ResultMetrics(ctx context.Context, tenant string) (resultmetrics.Snapshot, error) {
	snapshot := resultmetrics.Snapshot{SchemaVersion: resultmetrics.SchemaVersion, GeneratedAt: time.Now().UTC(), CampaignStates: resultmetrics.Counts(resultmetrics.CampaignStates)}
	deployments, ids, truncated, err := s.resultDeployments(ctx, tenant)
	if err != nil {
		return snapshot, err
	}
	snapshot.Truncated = truncated
	if err = s.resultGuardEvaluations(ctx, tenant, deployments, ids); err != nil {
		return snapshot, err
	}
	if err = s.resultObservations(ctx, tenant, deployments, ids); err != nil {
		return snapshot, err
	}
	if err = s.resultRollbacks(ctx, tenant, deployments, ids); err != nil {
		return snapshot, err
	}
	endpoints, endpointIDs, truncated, err := s.resultEndpoints(ctx, tenant)
	if err != nil {
		return snapshot, err
	}
	snapshot.Truncated = snapshot.Truncated || truncated
	if err = s.resultEndpointPlans(ctx, tenant, endpoints, endpointIDs); err != nil {
		return snapshot, err
	}
	if err = s.resultEndpointGuard(ctx, tenant, endpoints, endpointIDs); err != nil {
		return snapshot, err
	}
	campaigns, truncated, err := s.resultCampaigns(ctx, tenant, snapshot.CampaignStates)
	if err != nil {
		return snapshot, err
	}
	snapshot.Truncated = snapshot.Truncated || truncated
	snapshot.Campaigns = campaigns
	snapshot.Deployments = make([]resultmetrics.Deployment, 0, len(ids))
	for _, id := range ids {
		snapshot.Deployments = append(snapshot.Deployments, *deployments[id])
	}
	snapshot.Endpoints = make([]resultmetrics.Endpoint, 0, len(endpointIDs))
	for _, id := range endpointIDs {
		snapshot.Endpoints = append(snapshot.Endpoints, *endpoints[id])
	}
	return snapshot, nil
}

func (s *Store) resultDeployments(ctx context.Context, tenant string) (map[string]*resultmetrics.Deployment, []string, bool, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,name FROM deployments WHERE tenant_id=? AND desired_state<>'deleted' ORDER BY name,id LIMIT ?`, tenant, resultmetrics.MaxSubjects+1)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	byID, ids := map[string]*resultmetrics.Deployment{}, []string{}
	truncated := false
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			return nil, nil, false, err
		}
		if len(ids) == resultmetrics.MaxSubjects {
			truncated = true
			continue
		}
		byID[id] = &resultmetrics.Deployment{Name: name, GuardDecisions: resultmetrics.Counts(resultmetrics.GuardDecisions), QualityVerdicts: resultmetrics.Counts(resultmetrics.QualityVerdicts), Rollbacks: resultmetrics.Counts(resultmetrics.RollbackTriggers)}
		ids = append(ids, id)
	}
	return byID, ids, truncated, rows.Err()
}

func (s *Store) resultGuardEvaluations(ctx context.Context, tenant string, deployments map[string]*resultmetrics.Deployment, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.QueryContext(ctx, `SELECT e.deployment_id,e.decision,COALESCE(e.metrics_json->'quality'->>'verdict',''),COUNT(*) FROM release_guard_evaluations e JOIN deployments d ON d.id=e.deployment_id WHERE d.tenant_id=? AND d.desired_state<>'deleted' GROUP BY 1,2,3`, tenant)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, decision, verdict string
		var count int
		if err = rows.Scan(&id, &decision, &verdict, &count); err != nil {
			rows.Close()
			return err
		}
		d := deployments[id]
		if d == nil {
			continue
		}
		if resultmetrics.Listed(resultmetrics.GuardDecisions, decision) {
			d.GuardDecisions[decision] += count
		}
		if resultmetrics.Listed(resultmetrics.QualityVerdicts, verdict) {
			d.QualityVerdicts[verdict] += count
		}
	}
	if err = closeRows(rows); err != nil {
		return err
	}
	rows, err = s.QueryContext(ctx, `SELECT DISTINCT ON (e.deployment_id) e.deployment_id,e.decision,e.metrics_json->'quality'->>'verdict',e.created_at FROM release_guard_evaluations e JOIN deployments d ON d.id=e.deployment_id WHERE d.tenant_id=? AND d.desired_state<>'deleted' ORDER BY e.deployment_id,e.created_at DESC,e.id DESC`, tenant)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, decision string
		var verdict sql.NullString
		var at time.Time
		if err = rows.Scan(&id, &decision, &verdict, &at); err != nil {
			return err
		}
		d := deployments[id]
		if d == nil {
			continue
		}
		result := &resultmetrics.GuardResult{Decision: decision, EvaluatedAt: at.UTC()}
		if verdict.Valid {
			result.QualityVerdict = &verdict.String
		}
		d.LatestGuard = result
	}
	return rows.Err()
}

func (s *Store) resultObservations(ctx context.Context, tenant string, deployments map[string]*resultmetrics.Deployment, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.QueryContext(ctx, `SELECT DISTINCT ON (deployment_id) deployment_id,status,updated_at FROM release_guard_monitors WHERE tenant_id=? ORDER BY deployment_id,created_at DESC,id DESC`, tenant)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, state string
		var at time.Time
		if err = rows.Scan(&id, &state, &at); err != nil {
			return err
		}
		if d := deployments[id]; d != nil {
			d.Observation = &resultmetrics.Observation{State: state, UpdatedAt: at.UTC()}
		}
	}
	return rows.Err()
}

// resultRollbacks counts the rollback events of each deployment. Deployment
// events are never pruned, so the counts only grow while the deployment
// exists.
func (s *Store) resultRollbacks(ctx context.Context, tenant string, deployments map[string]*resultmetrics.Deployment, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.QueryContext(ctx, `SELECT ev.deployment_id,ev.event_type,COUNT(*),MAX(ev.created_at) FROM deployment_events ev JOIN deployments d ON d.id=ev.deployment_id WHERE d.tenant_id=? AND d.desired_state<>'deleted' AND ev.event_type IN ('revision_auto_rolled_back','revision_rolled_back') GROUP BY 1,2`, tenant)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, eventType string
		var count int
		var last time.Time
		if err = rows.Scan(&id, &eventType, &count, &last); err != nil {
			return err
		}
		d := deployments[id]
		if d == nil {
			continue
		}
		trigger := "operator"
		if eventType == "revision_auto_rolled_back" {
			trigger = "automatic"
		}
		d.Rollbacks[trigger] += count
		last = last.UTC()
		if d.LastRollbackAt == nil || last.After(*d.LastRollbackAt) {
			d.LastRollbackAt = &last
		}
	}
	return rows.Err()
}

func (s *Store) resultEndpoints(ctx context.Context, tenant string) (map[string]*resultmetrics.Endpoint, []string, bool, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,name,candidate_serving_plan_id IS NOT NULL FROM endpoints WHERE tenant_id=? AND desired_state<>'deleted' ORDER BY name,id LIMIT ?`, tenant, resultmetrics.MaxSubjects+1)
	if err != nil {
		return nil, nil, false, err
	}
	defer rows.Close()
	byID, ids := map[string]*resultmetrics.Endpoint{}, []string{}
	truncated := false
	for rows.Next() {
		var id, name string
		var staged bool
		if err = rows.Scan(&id, &name, &staged); err != nil {
			return nil, nil, false, err
		}
		if len(ids) == resultmetrics.MaxSubjects {
			truncated = true
			continue
		}
		byID[id] = &resultmetrics.Endpoint{Name: name, CandidateStaged: staged, GuardDecisions: resultmetrics.Counts(resultmetrics.EndpointGuardDecisions)}
		ids = append(ids, id)
	}
	return byID, ids, truncated, rows.Err()
}

func (s *Store) resultEndpointPlans(ctx context.Context, tenant string, endpoints map[string]*resultmetrics.Endpoint, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.QueryContext(ctx, `SELECT e.id,p.routing_policy,b.name,b.kind,spb.priority,spb.weight FROM endpoints e JOIN serving_plans p ON p.id=e.active_serving_plan_id JOIN serving_plan_bindings spb ON spb.serving_plan_id=p.id JOIN backend_bindings b ON b.id=spb.binding_id WHERE e.tenant_id=? AND e.desired_state<>'deleted' ORDER BY e.id,spb.priority,b.name`, tenant)
	if err != nil {
		return err
	}
	defer rows.Close()
	policies, bindings := map[string]string{}, map[string][]resultmetrics.PlanBinding{}
	for rows.Next() {
		var id, policy string
		var binding resultmetrics.PlanBinding
		if err = rows.Scan(&id, &policy, &binding.Name, &binding.Kind, &binding.Priority, &binding.Weight); err != nil {
			return err
		}
		if endpoints[id] == nil {
			continue
		}
		policies[id] = policy
		bindings[id] = append(bindings[id], binding)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for id, policy := range policies {
		endpoints[id].RoutingPolicy = policy
		endpoints[id].Bindings = resultmetrics.Shares(policy, bindings[id])
	}
	return nil
}

func (s *Store) resultEndpointGuard(ctx context.Context, tenant string, endpoints map[string]*resultmetrics.Endpoint, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.QueryContext(ctx, `SELECT endpoint_id,decision,COUNT(*) FROM endpoint_release_guard_evaluations WHERE tenant_id=? GROUP BY 1,2`, tenant)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, decision string
		var count int
		if err = rows.Scan(&id, &decision, &count); err != nil {
			rows.Close()
			return err
		}
		if e := endpoints[id]; e != nil && resultmetrics.Listed(resultmetrics.EndpointGuardDecisions, decision) {
			e.GuardDecisions[decision] += count
		}
	}
	if err = closeRows(rows); err != nil {
		return err
	}
	rows, err = s.QueryContext(ctx, `SELECT DISTINCT ON (endpoint_id) endpoint_id,decision,created_at FROM endpoint_release_guard_evaluations WHERE tenant_id=? ORDER BY endpoint_id,created_at DESC,id DESC`, tenant)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, decision string
		var at time.Time
		if err = rows.Scan(&id, &decision, &at); err != nil {
			return err
		}
		if e := endpoints[id]; e != nil {
			e.LatestGuard = &resultmetrics.EndpointGuardResult{Decision: decision, EvaluatedAt: at.UTC()}
		}
	}
	return rows.Err()
}

// resultCampaigns counts the workspace's optimization runs by state and
// returns the latest run of each endpoint a run targeted: the deployment an
// evolve run improves, or the endpoint a migration Proof replaces a closed
// API behind.
func (s *Store) resultCampaigns(ctx context.Context, tenant string, states map[string]int) ([]resultmetrics.Campaign, bool, error) {
	rows, err := s.QueryContext(ctx, `SELECT state,COUNT(*) FROM optimization_campaigns WHERE tenant_id=? GROUP BY state`, tenant)
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var state string
		var count int
		if err = rows.Scan(&state, &count); err != nil {
			rows.Close()
			return nil, false, err
		}
		if resultmetrics.Listed(resultmetrics.CampaignStates, state) {
			states[state] += count
		}
	}
	if err = closeRows(rows); err != nil {
		return nil, false, err
	}
	rows, err = s.QueryContext(ctx, `SELECT target,id,state,updated_at FROM (
	SELECT DISTINCT ON (target) target,id,state,updated_at FROM (
		SELECT COALESCE(NULLIF(target_deployment_name,''),proposal_json->'input'->'migration'->>'endpoint') AS target,id,state,created_at,updated_at FROM optimization_campaigns WHERE tenant_id=?
	) c WHERE target IS NOT NULL AND target<>'' ORDER BY target,created_at DESC,id DESC
) latest ORDER BY target LIMIT ?`, tenant, resultmetrics.MaxSubjects+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []resultmetrics.Campaign{}
	truncated := false
	for rows.Next() {
		var campaign resultmetrics.Campaign
		if err = rows.Scan(&campaign.Endpoint, &campaign.ID, &campaign.State, &campaign.UpdatedAt); err != nil {
			return nil, false, err
		}
		if len(out) == resultmetrics.MaxSubjects {
			truncated = true
			continue
		}
		campaign.UpdatedAt = campaign.UpdatedAt.UTC()
		out = append(out, campaign)
	}
	return out, truncated, rows.Err()
}

func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if closeErr := rows.Close(); err == nil {
		err = closeErr
	}
	return err
}
