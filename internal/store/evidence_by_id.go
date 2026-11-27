package store

import (
	"context"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
)

// maxEvidenceIDs bounds one by-ID evidence read.
const maxEvidenceIDs = 1000

// The by-ID evidence readers below serve records that a finished workflow,
// such as an optimization campaign, already recorded by ID. They filter by
// tenant only, never by deployment name or state: a campaign deletes its
// candidate deployments at cleanup, and their evidence must stay readable
// under its own ID. Unknown IDs and other tenants' rows are absent from the
// returned map, so the caller keeps the dependent field unknown.

// evidenceIDs drops empty and repeated IDs and enforces maxEvidenceIDs.
func evidenceIDs(ids []string, kind string) ([]string, error) {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; id == "" || duplicate {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) > maxEvidenceIDs {
		return nil, fmt.Errorf("at most %d %s IDs can be read at once", maxEvidenceIDs, kind)
	}
	return unique, nil
}

// BenchmarksByIDs returns the tenant's benchmark results with the given IDs,
// keyed by ID, whether or not their deployment still exists.
func (s *Store) BenchmarksByIDs(ctx context.Context, tenant string, ids []string) (map[string]domain.BenchmarkResult, error) {
	if tenant == "" {
		tenant = "global"
	}
	unique, err := evidenceIDs(ids, "benchmark")
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.BenchmarkResult, len(unique))
	if len(unique) == 0 {
		return out, nil
	}
	rows, err := s.QueryContext(ctx, `SELECT `+benchmarkResultColumns+` FROM benchmark_results WHERE tenant_id=? AND id=ANY(?)`, tenant, unique)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		row, scanErr := scanBenchmarkResult(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out[row.ID] = row
	}
	return out, rows.Err()
}

// ReleaseGuardEvaluationsByIDs returns the tenant's Release Guard evaluations
// with the given IDs, keyed by ID, whether or not their deployment still
// exists. Evaluations carry no tenant column; the deployment row that each one
// references, deleted or not, holds the tenant.
func (s *Store) ReleaseGuardEvaluationsByIDs(ctx context.Context, tenant string, ids []string) (map[string]domain.ReleaseGuardEvaluation, error) {
	if tenant == "" {
		tenant = "global"
	}
	unique, err := evidenceIDs(ids, "Release Guard evaluation")
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.ReleaseGuardEvaluation, len(unique))
	if len(unique) == 0 {
		return out, nil
	}
	rows, err := s.QueryContext(ctx, `SELECT e.id,e.deployment_id,e.active_revision_id,e.candidate_revision_id,e.decision,e.reason_codes_json::text,e.metrics_json::text,e.policy_json::text,e.created_at FROM release_guard_evaluations e JOIN deployments d ON d.id=e.deployment_id WHERE d.tenant_id=? AND e.id=ANY(?)`, tenant, unique)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var value domain.ReleaseGuardEvaluation
		var created string
		if err = rows.Scan(&value.ID, &value.DeploymentID, &value.ActiveRevisionID, &value.CandidateRevisionID, &value.Decision, &value.ReasonCodesJSON, &value.MetricsJSON, &value.PolicyJSON, &created); err != nil {
			return nil, err
		}
		value.CreatedAt = parseTime(created)
		out[value.ID] = value
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// A setup an agent measured has its decision in its own table, with the
	// setups' agent identities in place of revisions and no deployment.
	agentRows, err := s.QueryContext(ctx, `SELECT id,active_setup_id,candidate_setup_id,decision,reason_codes_json::text,metrics_json::text,policy_json::text,created_at FROM agent_release_guard_evaluations WHERE tenant_id=? AND id=ANY(?)`, tenant, unique)
	if err != nil {
		return nil, err
	}
	defer agentRows.Close()
	for agentRows.Next() {
		var value domain.ReleaseGuardEvaluation
		var created string
		if err = agentRows.Scan(&value.ID, &value.ActiveRevisionID, &value.CandidateRevisionID, &value.Decision, &value.ReasonCodesJSON, &value.MetricsJSON, &value.PolicyJSON, &created); err != nil {
			return nil, err
		}
		value.CreatedAt = parseTime(created)
		out[value.ID] = value
	}
	return out, agentRows.Err()
}

// LabEvaluationsByIDs returns the tenant's Lab evaluations with the given
// IDs, keyed by ID. A campaign ranking is one: it keeps each candidate's
// ranking decision after cleanup rewrites the candidate states.
func (s *Store) LabEvaluationsByIDs(ctx context.Context, tenant string, ids []string) (map[string]domain.LabEvaluation, error) {
	if tenant == "" {
		tenant = "global"
	}
	unique, err := evidenceIDs(ids, "Lab evaluation")
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.LabEvaluation, len(unique))
	if len(unique) == 0 {
		return out, nil
	}
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,model_identity,algorithm_version,input_json::text,results_json::text,input_digest,created_at FROM lab_evaluations WHERE tenant_id=? AND id=ANY(?)`, tenant, unique)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var value domain.LabEvaluation
		var created string
		if err = rows.Scan(&value.ID, &value.TenantID, &value.ModelIdentity, &value.AlgorithmVersion, &value.InputJSON, &value.ResultsJSON, &value.InputDigest, &created); err != nil {
			return nil, err
		}
		value.CreatedAt = parseTime(created)
		out[value.ID] = value
	}
	return out, rows.Err()
}
