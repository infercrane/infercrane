package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/external"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

func (s *Store) EndpointReleaseGuardPolicy(ctx context.Context, tenant, name string) (domain.EndpointReleaseGuardPolicy, error) {
	var policy domain.EndpointReleaseGuardPolicy
	err := s.QueryRowContext(ctx, `SELECT p.endpoint_id,p.enabled,p.minimum_requests,p.max_ttft_regression_percent,p.max_latency_regression_percent,p.max_error_rate_increase,p.max_output_throughput_drop_percent,p.require_compatibility_evidence FROM endpoint_release_guard_policies p JOIN endpoints e ON e.id=p.endpoint_id WHERE e.tenant_id=? AND e.name=? AND e.desired_state<>'deleted'`, tenant, name).Scan(&policy.EndpointID, &policy.Enabled, &policy.MinimumRequests, &policy.MaxTTFTRegressionPercent, &policy.MaxLatencyRegressionPercent, &policy.MaxErrorRateIncrease, &policy.MaxOutputThroughputDropPercent, &policy.RequireCompatibilityEvidence)
	if errors.Is(err, sql.ErrNoRows) {
		return policy, ErrNotFound
	}
	return policy, err
}

func (s *Store) SetEndpointReleaseGuardPolicy(ctx context.Context, tenant, name string, policy domain.EndpointReleaseGuardPolicy) (domain.EndpointReleaseGuardPolicy, error) {
	if policy.MinimumRequests < 1 || policy.MinimumRequests > 100000 || policy.MaxTTFTRegressionPercent < 0 || policy.MaxLatencyRegressionPercent < 0 || policy.MaxErrorRateIncrease < 0 || policy.MaxErrorRateIncrease > 1 || policy.MaxOutputThroughputDropPercent < 0 {
		return domain.EndpointReleaseGuardPolicy{}, errors.New("invalid endpoint Release Guard policy")
	}
	current, err := s.EndpointReleaseGuardPolicy(ctx, tenant, name)
	if err != nil {
		return domain.EndpointReleaseGuardPolicy{}, err
	}
	_, err = s.ExecContext(ctx, `UPDATE endpoint_release_guard_policies SET enabled=?,minimum_requests=?,max_ttft_regression_percent=?,max_latency_regression_percent=?,max_error_rate_increase=?,max_output_throughput_drop_percent=?,require_compatibility_evidence=?,updated_at=? WHERE endpoint_id=?`, policy.Enabled, policy.MinimumRequests, policy.MaxTTFTRegressionPercent, policy.MaxLatencyRegressionPercent, policy.MaxErrorRateIncrease, policy.MaxOutputThroughputDropPercent, policy.RequireCompatibilityEvidence, now(), current.EndpointID)
	if err != nil {
		return domain.EndpointReleaseGuardPolicy{}, err
	}
	return s.EndpointReleaseGuardPolicy(ctx, tenant, name)
}

type endpointGuardSubject struct {
	DeploymentID, RevisionID, Model, Runtime string
}

func (s *Store) endpointGuardSubject(ctx context.Context, tenant string, resolved domain.ResolvedEndpoint, plan domain.ServingPlan) (endpointGuardSubject, error) {
	if len(plan.Bindings) == 0 {
		return endpointGuardSubject{}, errors.New("serving plan has no bindings")
	}
	bindingID := plan.Bindings[0].BindingID
	var binding domain.BackendBinding
	for _, candidate := range resolved.Bindings {
		if candidate.ID == bindingID {
			binding = candidate
			break
		}
	}
	if binding.ID == "" || binding.Kind != "deployment" {
		return endpointGuardSubject{}, errors.New("endpoint Release Guard currently requires a deployment-backed primary binding")
	}
	var subject endpointGuardSubject
	err := s.QueryRowContext(ctx, `SELECT id,active_revision_id,model,runtime FROM deployments WHERE id=? AND tenant_id=? AND desired_state<>'deleted'`, binding.DeploymentID, tenant).Scan(&subject.DeploymentID, &subject.RevisionID, &subject.Model, &subject.Runtime)
	if errors.Is(err, sql.ErrNoRows) {
		return subject, ErrNotFound
	}
	return subject, err
}

func (s *Store) endpointSubjectMetrics(ctx context.Context, subject endpointGuardSubject, window time.Duration) (domain.RevisionMetrics, error) {
	metrics, err := s.RevisionMetrics(ctx, subject.RevisionID, window)
	if err != nil {
		return metrics, err
	}
	if err = s.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployment_targets dt JOIN targets t ON t.id=dt.target_id WHERE dt.deployment_id=? AND t.health='healthy'`, subject.DeploymentID).Scan(&metrics.ReadyReplicas); err != nil {
		return domain.RevisionMetrics{}, err
	}
	metrics.EvidenceSource = "deployment_request_telemetry"
	metrics.EvidenceID = subject.RevisionID
	return metrics, nil
}

func endpointGuardPolicy(policy domain.EndpointReleaseGuardPolicy) domain.ReleaseGuardPolicy {
	return domain.ReleaseGuardPolicy{Enabled: policy.Enabled, MinimumRequests: policy.MinimumRequests, MaxTTFTRegressionPercent: policy.MaxTTFTRegressionPercent, MaxLatencyRegressionPercent: policy.MaxLatencyRegressionPercent, MaxErrorRateIncrease: policy.MaxErrorRateIncrease, MaxOutputThroughputDropPercent: policy.MaxOutputThroughputDropPercent, RequireCompatibilityEvidence: policy.RequireCompatibilityEvidence}
}

func samePlanBinding(left, right domain.ServingPlanBinding) bool {
	return left.BindingID == right.BindingID && left.Priority == right.Priority && left.Weight == right.Weight
}

func managedFallbackBinding(resolved domain.ResolvedEndpoint, id string) bool {
	for _, binding := range resolved.Bindings {
		if binding.ID != id {
			continue
		}
		config, managed, err := external.ParseManagedBindingConfig(binding.ConfigJSON)
		return err == nil && managed && binding.Kind == "external" && binding.OwnershipMode == "traffic-managed" && config.Enabled && config.PrivacyAcknowledged
	}
	return false
}

func deploymentBackedBinding(resolved domain.ResolvedEndpoint, id string) bool {
	for _, binding := range resolved.Bindings {
		if binding.ID == id {
			return binding.Kind == "deployment" && binding.DeploymentID != ""
		}
	}
	return false
}

// endpointPlanTopologyEvidence prevents Release Guard from applying one
// deployment's metrics to an unmeasured routing graph. Today it can qualify a
// single primary change, an unchanged primary-fallback graph, or an append-only
// governed fallback that receives no traffic while the primary is healthy.
func endpointPlanTopologyEvidence(resolved domain.ResolvedEndpoint) (bool, string) {
	active, candidate := resolved.ActivePlan, resolved.CandidatePlan
	if candidate == nil || len(active.Bindings) == 0 || len(candidate.Bindings) == 0 {
		return false, "missing active or candidate binding graph"
	}
	if !deploymentBackedBinding(resolved, active.Bindings[0].BindingID) || !deploymentBackedBinding(resolved, candidate.Bindings[0].BindingID) {
		return false, "primary comparison requires deployment-backed bindings"
	}
	if active.RoutingPolicy == "weighted" || candidate.RoutingPolicy == "weighted" {
		return false, "weighted routing requires per-binding candidate evidence"
	}
	if active.RoutingPolicy == "manual" && candidate.RoutingPolicy == "manual" && len(active.Bindings) == 1 && len(candidate.Bindings) == 1 {
		return true, "single deployment-backed primary comparison"
	}
	if active.RoutingPolicy == "primary-fallback" && candidate.RoutingPolicy == "primary-fallback" && len(active.Bindings) == len(candidate.Bindings) {
		for index := 1; index < len(active.Bindings); index++ {
			if !samePlanBinding(active.Bindings[index], candidate.Bindings[index]) {
				return false, "fallback graph changed without per-binding evidence"
			}
		}
		return true, "primary comparison with unchanged fallback graph"
	}
	if candidate.RoutingPolicy != "primary-fallback" || len(candidate.Bindings) <= len(active.Bindings) {
		return false, "routing topology change is not locally qualified"
	}
	for index := range active.Bindings {
		if !samePlanBinding(active.Bindings[index], candidate.Bindings[index]) {
			return false, "candidate does not preserve the active routing prefix"
		}
	}
	for _, addition := range candidate.Bindings[len(active.Bindings):] {
		if !managedFallbackBinding(resolved, addition.BindingID) {
			return false, "new fallback lacks immutable consent and hard-budget policy"
		}
	}
	return true, "active routing preserved; append-only governed fallback"
}

func (s *Store) recordEndpointGuardEvaluation(ctx context.Context, tenant string, resolved domain.ResolvedEndpoint, policy domain.EndpointReleaseGuardPolicy, decision string, reasons, metrics any) (domain.EndpointReleaseGuardEvaluation, error) {
	reasonJSON, _ := json.Marshal(reasons)
	metricJSON, _ := json.Marshal(metrics)
	policyJSON, _ := json.Marshal(policy)
	id, err := newID()
	if err != nil {
		return domain.EndpointReleaseGuardEvaluation{}, err
	}
	evaluation := domain.EndpointReleaseGuardEvaluation{ID: id, TenantID: tenant, EndpointID: resolved.Endpoint.ID, ActiveServingPlanID: resolved.ActivePlan.ID, CandidateServingPlanID: resolved.CandidatePlan.ID, Decision: decision, ReasonCodesJSON: string(reasonJSON), MetricsJSON: string(metricJSON), PolicyJSON: string(policyJSON), CreatedAt: time.Now().UTC()}
	_, err = s.ExecContext(ctx, `INSERT INTO endpoint_release_guard_evaluations(id,tenant_id,endpoint_id,active_serving_plan_id,candidate_serving_plan_id,decision,reason_codes_json,metrics_json,policy_json,created_at) VALUES(?,?,?,?,?,?,?::jsonb,?::jsonb,?::jsonb,?)`, evaluation.ID, evaluation.TenantID, evaluation.EndpointID, evaluation.ActiveServingPlanID, evaluation.CandidateServingPlanID, evaluation.Decision, evaluation.ReasonCodesJSON, evaluation.MetricsJSON, evaluation.PolicyJSON, evaluation.CreatedAt.Format(time.RFC3339Nano))
	return evaluation, err
}

func (s *Store) EvaluateEndpointReleaseGuard(ctx context.Context, tenant, name string, window time.Duration) (domain.EndpointReleaseGuardEvaluation, error) {
	if window <= 0 || window > 30*24*time.Hour {
		return domain.EndpointReleaseGuardEvaluation{}, errors.New("endpoint guard window must be between zero and 30 days")
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, name)
	if err != nil {
		return domain.EndpointReleaseGuardEvaluation{}, err
	}
	if resolved.CandidatePlan == nil {
		return domain.EndpointReleaseGuardEvaluation{}, fmt.Errorf("%w: endpoint has no candidate serving plan", ErrConflict)
	}
	policy, err := s.EndpointReleaseGuardPolicy(ctx, tenant, name)
	if err != nil {
		return domain.EndpointReleaseGuardEvaluation{}, err
	}
	topologyQualified, topologyEvidence := endpointPlanTopologyEvidence(resolved)
	if !topologyQualified {
		return s.recordEndpointGuardEvaluation(ctx, tenant, resolved, policy, "INCONCLUSIVE", []string{"serving_plan_topology_unqualified"}, map[string]any{"plan_topology": topologyEvidence})
	}
	activeSubject, err := s.endpointGuardSubject(ctx, tenant, resolved, resolved.ActivePlan)
	if err != nil {
		return domain.EndpointReleaseGuardEvaluation{}, err
	}
	candidateSubject, err := s.endpointGuardSubject(ctx, tenant, resolved, *resolved.CandidatePlan)
	if err != nil {
		return domain.EndpointReleaseGuardEvaluation{}, err
	}
	active, err := s.endpointSubjectMetrics(ctx, activeSubject, window)
	if err != nil {
		return domain.EndpointReleaseGuardEvaluation{}, err
	}
	candidate, err := s.endpointSubjectMetrics(ctx, candidateSubject, window)
	if err != nil {
		return domain.EndpointReleaseGuardEvaluation{}, err
	}
	compatible := activeSubject.Model == candidateSubject.Model && activeSubject.Runtime == candidateSubject.Runtime
	active.Compatible, candidate.Compatible = &compatible, &compatible
	active.CompatibilityEvidence = "persisted deployment model and runtime identity"
	candidate.CompatibilityEvidence = active.CompatibilityEvidence
	result := releaseguard.Evaluate(releaseguard.Input{Policy: endpointGuardPolicy(policy), Active: active, Candidate: candidate})
	decision := map[string]string{"ACCEPT": "PASS", "REJECT": "REJECT", "WAIT": "INCONCLUSIVE"}[result.Decision]
	return s.recordEndpointGuardEvaluation(ctx, tenant, resolved, policy, decision, result.Reasons, map[string]any{"active": active, "candidate": candidate, "plan_topology": topologyEvidence})
}

func (s *Store) EndpointReleaseGuardEvaluations(ctx context.Context, tenant, name string, limit int) ([]domain.EndpointReleaseGuardEvaluation, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.QueryContext(ctx, `SELECT g.id,g.tenant_id,g.endpoint_id,g.active_serving_plan_id,g.candidate_serving_plan_id,g.decision,g.reason_codes_json::text,g.metrics_json::text,g.policy_json::text,g.created_at FROM endpoint_release_guard_evaluations g JOIN endpoints e ON e.id=g.endpoint_id WHERE e.tenant_id=? AND e.name=? ORDER BY g.created_at DESC LIMIT ?`, tenant, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EndpointReleaseGuardEvaluation
	for rows.Next() {
		var item domain.EndpointReleaseGuardEvaluation
		var created string
		if err = rows.Scan(&item.ID, &item.TenantID, &item.EndpointID, &item.ActiveServingPlanID, &item.CandidateServingPlanID, &item.Decision, &item.ReasonCodesJSON, &item.MetricsJSON, &item.PolicyJSON, &created); err != nil {
			return nil, err
		}
		item.CreatedAt = parseTime(created)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) EndpointReleaseGuardAccepted(ctx context.Context, tenant, name, candidatePlanID string) (bool, error) {
	var decision, currentActive, currentCandidate, evaluatedActive string
	err := s.QueryRowContext(ctx, `SELECT g.decision,e.active_serving_plan_id,e.candidate_serving_plan_id,g.active_serving_plan_id FROM endpoints e JOIN endpoint_release_guard_evaluations g ON g.endpoint_id=e.id AND g.candidate_serving_plan_id=? WHERE e.tenant_id=? AND e.name=? ORDER BY g.created_at DESC LIMIT 1`, candidatePlanID, tenant, name).Scan(&decision, &currentActive, &currentCandidate, &evaluatedActive)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return decision == "PASS" && currentCandidate == candidatePlanID && currentActive == evaluatedActive, nil
}
