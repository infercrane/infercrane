package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

func (s *Store) EnsureReleaseGuardMonitor(ctx context.Context, tenant, deploymentName, promotedRevisionID, rollbackRevisionID string, window time.Duration) (domain.ReleaseGuardMonitor, error) {
	if tenant == "" {
		tenant = "global"
	}
	if promotedRevisionID == "" || rollbackRevisionID == "" || promotedRevisionID == rollbackRevisionID || window < 30*time.Second || window > time.Hour {
		return domain.ReleaseGuardMonitor{}, errors.New("invalid release monitor")
	}
	resolved, err := s.ResolveForTenant(ctx, tenant, deploymentName)
	if err != nil {
		return domain.ReleaseGuardMonitor{}, err
	}
	if resolved.Deployment.ActiveRevisionID != promotedRevisionID {
		return domain.ReleaseGuardMonitor{}, fmt.Errorf("%w: promoted revision is not active", ErrConflict)
	}
	id, err := newID()
	if err != nil {
		return domain.ReleaseGuardMonitor{}, err
	}
	stamp := time.Now().UTC()
	deadline := stamp.Add(window)
	policy, err := s.ReleaseGuardPolicy(ctx, tenant, deploymentName)
	if err != nil {
		return domain.ReleaseGuardMonitor{}, err
	}
	policyJSON, _ := json.Marshal(policy)
	_, err = s.ExecContext(ctx, `INSERT INTO release_guard_monitors(id,tenant_id,deployment_id,promoted_revision_id,rollback_revision_id,status,deadline,policy_json,created_at,updated_at) VALUES(?,?,?,?,?,'observing',?,?::jsonb,?,?) ON CONFLICT(deployment_id,promoted_revision_id) DO NOTHING`, id, tenant, resolved.Deployment.ID, promotedRevisionID, rollbackRevisionID, deadline.Format(time.RFC3339Nano), string(policyJSON), stamp.Format(time.RFC3339Nano), stamp.Format(time.RFC3339Nano))
	if err != nil {
		return domain.ReleaseGuardMonitor{}, err
	}
	return s.ReleaseGuardMonitor(ctx, tenant, deploymentName, promotedRevisionID)
}

func (s *Store) ReleaseGuardMonitor(ctx context.Context, tenant, deploymentName, promotedRevisionID string) (domain.ReleaseGuardMonitor, error) {
	if tenant == "" {
		tenant = "global"
	}
	var value domain.ReleaseGuardMonitor
	var deadline, created, updated string
	err := s.QueryRowContext(ctx, `SELECT m.id,m.tenant_id,m.deployment_id,m.promoted_revision_id,m.rollback_revision_id,m.status,COALESCE(m.evaluation_id,''),m.reason,m.policy_json::text,m.deadline,m.created_at,m.updated_at FROM release_guard_monitors m JOIN deployments d ON d.id=m.deployment_id WHERE m.tenant_id=? AND d.name=? AND m.promoted_revision_id=?`, tenant, deploymentName, promotedRevisionID).Scan(&value.ID, &value.TenantID, &value.DeploymentID, &value.PromotedRevisionID, &value.RollbackRevisionID, &value.Status, &value.EvaluationID, &value.Reason, &value.PolicyJSON, &deadline, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return value, ErrNotFound
	}
	value.Deadline = parseTime(deadline)
	value.CreatedAt = parseTime(created)
	value.UpdatedAt = parseTime(updated)
	return value, err
}

func (s *Store) EvaluateReleaseGuardMonitor(ctx context.Context, tenant, deploymentName, promotedRevisionID string, window time.Duration) (domain.ReleaseGuardEvaluation, error) {
	monitor, err := s.ReleaseGuardMonitor(ctx, tenant, deploymentName, promotedRevisionID)
	if err != nil {
		return domain.ReleaseGuardEvaluation{}, err
	}
	var policy domain.ReleaseGuardPolicy
	if err = json.Unmarshal([]byte(monitor.PolicyJSON), &policy); err != nil {
		return domain.ReleaseGuardEvaluation{}, fmt.Errorf("decode persisted release monitor policy: %w", err)
	}
	// The monitor owns the observation window. Keep the argument for interface
	// compatibility, but never let a later policy edit alter an in-flight decision.
	window = monitor.Deadline.Sub(monitor.CreatedAt)
	if window < 30*time.Second || window > time.Hour {
		return domain.ReleaseGuardEvaluation{}, errors.New("persisted release monitor has an invalid observation window")
	}
	baseline, err := s.RevisionMetrics(ctx, monitor.RollbackRevisionID, window)
	if err != nil {
		return domain.ReleaseGuardEvaluation{}, err
	}
	candidate, err := s.RevisionMetricsSince(ctx, monitor.PromotedRevisionID, monitor.CreatedAt)
	if err != nil {
		return domain.ReleaseGuardEvaluation{}, err
	}
	benchmarks, err := s.BenchmarksForDeployment(ctx, tenant, deploymentName, 100)
	if err != nil {
		return domain.ReleaseGuardEvaluation{}, err
	}
	if a, c, ok := benchmarkGuardMetrics(baseline.ReadyReplicas, candidate.ReadyReplicas, benchmarks, monitor.RollbackRevisionID, monitor.PromotedRevisionID); ok {
		baseline.Compatible, candidate.Compatible = a.Compatible, c.Compatible
		baseline.CompatibilityEvidence, candidate.CompatibilityEvidence = a.CompatibilityEvidence, c.CompatibilityEvidence
		baseline.SyntheticValidation, candidate.SyntheticValidation = a.SyntheticValidation, c.SyntheticValidation
		baseline.SourcedHourlyCost, candidate.SourcedHourlyCost = a.SourcedHourlyCost, c.SourcedHourlyCost
	}
	if policy.RequireCompatibilityEvidence && candidate.Compatible == nil {
		baseline.CompatibilityEvidence = "comparable benchmark identity unavailable"
		candidate.CompatibilityEvidence = baseline.CompatibilityEvidence
	}
	result := releaseguard.Evaluate(releaseguard.Input{Policy: policy, Active: baseline, Candidate: candidate})
	if result.Decision == "WAIT" && !time.Now().UTC().Before(monitor.Deadline) {
		result.Decision = "REJECT"
		result.Reasons = append(result.Reasons, releaseguard.Reason{Code: "observation_deadline_exceeded", Message: "Post-promotion evidence did not satisfy policy before the persisted deadline"})
	}
	reasons, _ := json.Marshal(result.Reasons)
	metrics, _ := json.Marshal(map[string]domain.RevisionMetrics{"active": baseline, "candidate": candidate})
	policyJSON, _ := json.Marshal(policy)
	id, err := newID()
	if err != nil {
		return domain.ReleaseGuardEvaluation{}, err
	}
	stamp := time.Now().UTC()
	evaluation := domain.ReleaseGuardEvaluation{ID: id, DeploymentID: monitor.DeploymentID, ActiveRevisionID: monitor.RollbackRevisionID, CandidateRevisionID: monitor.PromotedRevisionID, Decision: result.Decision, ReasonCodesJSON: string(reasons), MetricsJSON: string(metrics), PolicyJSON: string(policyJSON), CreatedAt: stamp}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return evaluation, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO release_guard_evaluations(id,deployment_id,active_revision_id,candidate_revision_id,decision,reason_codes_json,metrics_json,policy_json,created_at) VALUES(?,?,?,?,?,?::jsonb,?::jsonb,?::jsonb,?)`, evaluation.ID, evaluation.DeploymentID, evaluation.ActiveRevisionID, evaluation.CandidateRevisionID, evaluation.Decision, evaluation.ReasonCodesJSON, evaluation.MetricsJSON, evaluation.PolicyJSON, stamp.Format(time.RFC3339Nano)); err != nil {
		return evaluation, err
	}
	status := "observing"
	if result.Decision == "ACCEPT" {
		status = "accepted"
	}
	if result.Decision == "REJECT" {
		status = "failed"
	}
	if _, err = tx.ExecContext(ctx, `UPDATE release_guard_monitors SET status=?,evaluation_id=?,reason=?,updated_at=? WHERE id=?`, status, evaluation.ID, string(reasons), stamp.Format(time.RFC3339Nano), monitor.ID); err != nil {
		return evaluation, err
	}
	return evaluation, tx.Commit()
}

func (s *Store) MarkReleaseGuardMonitorRolledBack(ctx context.Context, tenant, deploymentName, promotedRevisionID string) error {
	monitor, err := s.ReleaseGuardMonitor(ctx, tenant, deploymentName, promotedRevisionID)
	if err != nil {
		return err
	}
	_, err = s.ExecContext(ctx, `UPDATE release_guard_monitors SET status='rolled_back',updated_at=? WHERE id=?`, now(), monitor.ID)
	return err
}
