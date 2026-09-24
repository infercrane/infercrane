package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/continualoptimizer"
	"github.com/infercrane/infercrane/internal/domain"
)

const maxContinualOptimizationJSON = 1 << 20

func (s *Store) SetContinualOptimizationPolicy(ctx context.Context, tenant, deployment string, policy continualoptimizer.Policy) (domain.ContinualOptimizationPolicyRecord, error) {
	if err := continualoptimizer.ValidatePolicy(policy); err != nil {
		return domain.ContinualOptimizationPolicyRecord{}, err
	}
	encoded, err := json.Marshal(policy)
	if err != nil || len(encoded) > maxContinualOptimizationJSON {
		return domain.ContinualOptimizationPolicyRecord{}, errors.New("continual optimization policy exceeds its storage boundary")
	}
	resolved, err := s.ResolveForTenant(ctx, tenant, deployment)
	if err != nil {
		return domain.ContinualOptimizationPolicyRecord{}, err
	}
	stamp := now()
	_, err = s.ExecContext(ctx, `INSERT INTO continual_optimization_policies(tenant_id,deployment_id,policy_json,generation,created_at,updated_at) VALUES(?,?,?::jsonb,1,?,?) ON CONFLICT(deployment_id) DO UPDATE SET policy_json=EXCLUDED.policy_json,generation=continual_optimization_policies.generation+1,updated_at=EXCLUDED.updated_at WHERE continual_optimization_policies.tenant_id=EXCLUDED.tenant_id`, tenant, resolved.Deployment.ID, string(encoded), stamp, stamp)
	if err != nil {
		return domain.ContinualOptimizationPolicyRecord{}, err
	}
	return s.ContinualOptimizationPolicy(ctx, tenant, deployment)
}

func (s *Store) ContinualOptimizationPolicy(ctx context.Context, tenant, deployment string) (domain.ContinualOptimizationPolicyRecord, error) {
	var row domain.ContinualOptimizationPolicyRecord
	var created, updated time.Time
	err := s.QueryRowContext(ctx, `SELECT p.tenant_id,p.deployment_id,d.name,p.policy_json::text,p.generation,p.created_at,p.updated_at FROM continual_optimization_policies p JOIN deployments d ON d.id=p.deployment_id AND d.tenant_id=p.tenant_id WHERE p.tenant_id=? AND d.name=?`, tenant, deployment).Scan(&row.TenantID, &row.DeploymentID, &row.DeploymentName, &row.PolicyJSON, &row.Generation, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return row, domain.ErrNotFound
	}
	row.CreatedAt, row.UpdatedAt = created.UTC(), updated.UTC()
	return row, err
}

func (s *Store) ContinualOptimizationState(ctx context.Context, tenant, deployment string) (domain.ContinualOptimizationState, error) {
	var row domain.ContinualOptimizationState
	var baseline, digest, latest sql.NullString
	var last sql.NullTime
	var created, updated time.Time
	err := s.QueryRowContext(ctx, `SELECT st.tenant_id,st.deployment_id,d.name,st.baseline_json::text,st.baseline_digest,st.last_experiment_at,st.latest_decision_id,st.generation,st.created_at,st.updated_at FROM continual_optimization_states st JOIN deployments d ON d.id=st.deployment_id AND d.tenant_id=st.tenant_id WHERE st.tenant_id=? AND d.name=?`, tenant, deployment).Scan(&row.TenantID, &row.DeploymentID, &row.DeploymentName, &baseline, &digest, &last, &latest, &row.Generation, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return row, domain.ErrNotFound
	}
	if err != nil {
		return row, err
	}
	row.BaselineJSON, row.BaselineDigest, row.LatestDecisionID = baseline.String, digest.String, latest.String
	if last.Valid {
		value := last.Time.UTC()
		row.LastExperimentAt = &value
	}
	row.CreatedAt, row.UpdatedAt = created.UTC(), updated.UTC()
	return row, nil
}

// RecordContinualOptimizationDecision is idempotent by exact normalized input
// digest. Baselines advance only from customer-observed evidence. Public priors
// can therefore seed screening without ever replacing production truth.
func (s *Store) RecordContinualOptimizationDecision(ctx context.Context, tenant, deployment string, current continualoptimizer.Workload, decision continualoptimizer.Decision) (domain.ContinualOptimizationDecisionRecord, bool, error) {
	if decision.SchemaVersion != continualoptimizer.SchemaVersion || decision.AlgorithmVersion == "" || len(decision.InputDigest) != 64 || decision.DecisionDigest == "" || decision.WorkloadAuthority != current.Source || decision.Action == "" || current.Digest == "" {
		return domain.ContinualOptimizationDecisionRecord{}, false, errors.New("continual optimization decision identity is incomplete")
	}
	decisionJSON, err := json.Marshal(decision)
	if err != nil || len(decisionJSON) > maxContinualOptimizationJSON {
		return domain.ContinualOptimizationDecisionRecord{}, false, errors.New("continual optimization decision exceeds its storage boundary")
	}
	baselineJSON, err := json.Marshal(current)
	if err != nil || len(baselineJSON) > maxContinualOptimizationJSON {
		return domain.ContinualOptimizationDecisionRecord{}, false, errors.New("continual optimization workload exceeds its storage boundary")
	}
	resolved, err := s.ResolveForTenant(ctx, tenant, deployment)
	if err != nil {
		return domain.ContinualOptimizationDecisionRecord{}, false, err
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ContinualOptimizationDecisionRecord{}, false, err
	}
	defer tx.Rollback()
	id, err := newID()
	if err != nil {
		return domain.ContinualOptimizationDecisionRecord{}, false, err
	}
	stamp := now()
	result, err := tx.ExecContext(ctx, `INSERT INTO continual_optimization_decisions(id,tenant_id,deployment_id,input_digest,workload_source,workload_digest,action,promotion_eligible,decision_json,created_at) VALUES(?,?,?,?,?,?,?,?,?::jsonb,?) ON CONFLICT(tenant_id,deployment_id,input_digest) DO NOTHING`, id, tenant, resolved.Deployment.ID, decision.InputDigest, current.Source, current.Digest, decision.Action, decision.PromotionEligible, string(decisionJSON), stamp)
	if err != nil {
		return domain.ContinualOptimizationDecisionRecord{}, false, err
	}
	created, _ := result.RowsAffected()
	if created == 0 {
		if err = tx.Rollback(); err != nil {
			return domain.ContinualOptimizationDecisionRecord{}, false, err
		}
		existing, lookupErr := s.continualOptimizationDecisionByInput(ctx, tenant, resolved.Deployment.ID, decision.InputDigest)
		if lookupErr != nil {
			return existing, false, lookupErr
		}
		if !semanticJSONEqual(existing.DecisionJSON, string(decisionJSON)) {
			return existing, false, domain.ErrConflict
		}
		return existing, false, nil
	}
	baselineValue, baselineDigest := any(nil), any(nil)
	if current.Source == continualoptimizer.SourceCustomerObserved && decision.BaselineReplacement {
		baselineValue, baselineDigest = string(baselineJSON), current.Digest
	}
	lastExperiment := any(nil)
	if decision.Action == continualoptimizer.ActionStartExperiment {
		lastExperiment = decision.EvaluatedAt.UTC()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO continual_optimization_states(tenant_id,deployment_id,baseline_json,baseline_digest,last_experiment_at,latest_decision_id,generation,created_at,updated_at) VALUES(?,?,?::jsonb,?,?,?,1,?,?) ON CONFLICT(deployment_id) DO UPDATE SET baseline_json=COALESCE(EXCLUDED.baseline_json,continual_optimization_states.baseline_json),baseline_digest=COALESCE(EXCLUDED.baseline_digest,continual_optimization_states.baseline_digest),last_experiment_at=COALESCE(EXCLUDED.last_experiment_at,continual_optimization_states.last_experiment_at),latest_decision_id=EXCLUDED.latest_decision_id,generation=continual_optimization_states.generation+1,updated_at=EXCLUDED.updated_at WHERE continual_optimization_states.tenant_id=EXCLUDED.tenant_id`, tenant, resolved.Deployment.ID, baselineValue, baselineDigest, lastExperiment, id, stamp, stamp)
	if err != nil {
		return domain.ContinualOptimizationDecisionRecord{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ContinualOptimizationDecisionRecord{}, false, err
	}
	row, err := s.continualOptimizationDecisionByInput(ctx, tenant, resolved.Deployment.ID, decision.InputDigest)
	return row, true, err
}

func (s *Store) ContinualOptimizationDecisions(ctx context.Context, tenant, deployment string, limit int) ([]domain.ContinualOptimizationDecisionRecord, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	resolved, err := s.ResolveForTenant(ctx, tenant, deployment)
	if err != nil {
		return nil, err
	}
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,deployment_id,input_digest,workload_source,workload_digest,action,promotion_eligible,decision_json::text,created_at FROM continual_optimization_decisions WHERE tenant_id=? AND deployment_id=? ORDER BY created_at DESC LIMIT ?`, tenant, resolved.Deployment.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []domain.ContinualOptimizationDecisionRecord{}
	for rows.Next() {
		var row domain.ContinualOptimizationDecisionRecord
		if err = rows.Scan(&row.ID, &row.TenantID, &row.DeploymentID, &row.InputDigest, &row.WorkloadSource, &row.WorkloadDigest, &row.Action, &row.PromotionEligible, &row.DecisionJSON, &row.CreatedAt); err != nil {
			return nil, err
		}
		row.DeploymentName = deployment
		row.CreatedAt = row.CreatedAt.UTC()
		result = append(result, row)
	}
	return result, rows.Err()
}

func (s *Store) continualOptimizationDecisionByInput(ctx context.Context, tenant, deploymentID, inputDigest string) (domain.ContinualOptimizationDecisionRecord, error) {
	var row domain.ContinualOptimizationDecisionRecord
	err := s.QueryRowContext(ctx, `SELECT c.id,c.tenant_id,c.deployment_id,d.name,c.input_digest,c.workload_source,c.workload_digest,c.action,c.promotion_eligible,c.decision_json::text,c.created_at FROM continual_optimization_decisions c JOIN deployments d ON d.id=c.deployment_id AND d.tenant_id=c.tenant_id WHERE c.tenant_id=? AND c.deployment_id=? AND c.input_digest=?`, tenant, deploymentID, strings.ToLower(inputDigest)).Scan(&row.ID, &row.TenantID, &row.DeploymentID, &row.DeploymentName, &row.InputDigest, &row.WorkloadSource, &row.WorkloadDigest, &row.Action, &row.PromotionEligible, &row.DecisionJSON, &row.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return row, domain.ErrNotFound
	}
	row.CreatedAt = row.CreatedAt.UTC()
	return row, err
}
