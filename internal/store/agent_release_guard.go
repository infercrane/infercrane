package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/infercrane/infercrane/internal/domain"
)

// RecordAgentReleaseGuardEvaluation stores Release Guard's decision on a
// setup an agent measured, once per candidate and measurement: a retried
// guard step adopts the row it recorded. A different decision on the same
// measurement is a conflict.
func (s *Store) RecordAgentReleaseGuardEvaluation(ctx context.Context, value domain.AgentReleaseGuardEvaluation) (domain.AgentReleaseGuardEvaluation, error) {
	if value.TenantID == "" || value.AgentID == "" || value.CampaignID == "" || value.CandidateID == "" || value.BenchmarkID == "" ||
		!agentSetupIdentity.MatchString(value.ActiveSetupID) || !agentSetupIdentity.MatchString(value.CandidateSetupID) ||
		!json.Valid([]byte(value.ReasonCodesJSON)) || !json.Valid([]byte(value.MetricsJSON)) || !json.Valid([]byte(value.PolicyJSON)) {
		return value, errors.New("complete agent Release Guard evidence is required")
	}
	switch value.Decision {
	case "PASS", "REJECT", "INCONCLUSIVE":
	default:
		return value, errors.New("agent Release Guard decision must be PASS, REJECT, or INCONCLUSIVE")
	}
	id, err := newID()
	if err != nil {
		return value, err
	}
	if _, err = s.ExecContext(ctx, `INSERT INTO agent_release_guard_evaluations(id,tenant_id,agent_id,campaign_id,candidate_id,active_setup_id,candidate_setup_id,benchmark_id,decision,reason_codes_json,metrics_json,policy_json,created_at) SELECT ?,c.tenant_id,?,c.campaign_id,c.id,?,?,?,?,?::jsonb,?::jsonb,?::jsonb,? FROM optimization_candidate_runs c JOIN optimization_campaigns campaign ON campaign.id=c.campaign_id AND campaign.tenant_id=c.tenant_id WHERE c.tenant_id=? AND c.campaign_id=? AND c.id=? AND campaign.agent_id=? ON CONFLICT(tenant_id,candidate_id,benchmark_id) DO NOTHING`,
		id, value.AgentID, value.ActiveSetupID, value.CandidateSetupID, value.BenchmarkID, value.Decision, value.ReasonCodesJSON, value.MetricsJSON, value.PolicyJSON, now(), value.TenantID, value.CampaignID, value.CandidateID, value.AgentID); err != nil {
		return value, err
	}
	var stored domain.AgentReleaseGuardEvaluation
	var created string
	err = s.QueryRowContext(ctx, `SELECT id,tenant_id,agent_id,campaign_id,candidate_id,active_setup_id,candidate_setup_id,benchmark_id,decision,reason_codes_json::text,metrics_json::text,policy_json::text,created_at FROM agent_release_guard_evaluations WHERE tenant_id=? AND candidate_id=? AND benchmark_id=?`, value.TenantID, value.CandidateID, value.BenchmarkID).
		Scan(&stored.ID, &stored.TenantID, &stored.AgentID, &stored.CampaignID, &stored.CandidateID, &stored.ActiveSetupID, &stored.CandidateSetupID, &stored.BenchmarkID, &stored.Decision, &stored.ReasonCodesJSON, &stored.MetricsJSON, &stored.PolicyJSON, &created)
	if errors.Is(err, sql.ErrNoRows) {
		// Nothing was inserted and nothing matches: the candidate is not this
		// agent's run's.
		return value, ErrNotFound
	}
	if err != nil {
		return value, err
	}
	stored.CreatedAt = parseTime(created)
	if stored.CampaignID != value.CampaignID || stored.AgentID != value.AgentID || stored.Decision != value.Decision || stored.CandidateSetupID != value.CandidateSetupID {
		return stored, ErrConflict
	}
	return stored, nil
}
