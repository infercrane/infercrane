package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

const maxOptimizationEvidenceBytes = 1 << 20

func (s *Store) CreateOptimizationCampaign(ctx context.Context, campaign domain.OptimizationCampaign, candidates []domain.OptimizationCandidateRun) (domain.OptimizationCampaign, bool, error) {
	if err := validateOptimizationCampaign(campaign, candidates); err != nil {
		return campaign, false, err
	}
	requested := campaign
	tx, err := s.beginTx(ctx)
	if err != nil {
		return campaign, false, err
	}
	defer tx.Rollback()
	if campaign.ID == "" {
		campaign.ID, err = newID()
		if err != nil {
			return campaign, false, err
		}
	}
	stamp := now()
	result, err := tx.ExecContext(ctx, `INSERT INTO optimization_campaigns(id,tenant_id,idempotency_key,input_digest,model_identity,objective,source,state,proposal_json,max_candidates,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?::jsonb,?,?,?) ON CONFLICT(tenant_id,idempotency_key) DO NOTHING`, campaign.ID, campaign.TenantID, campaign.IdempotencyKey, campaign.InputDigest, campaign.ModelIdentity, campaign.Objective, campaign.Source, optimizationcampaign.CampaignAwaitingApproval, campaign.ProposalJSON, len(candidates), stamp, stamp)
	if err != nil {
		return campaign, false, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		if err = tx.Rollback(); err != nil {
			return campaign, false, err
		}
		existing, lookupErr := s.OptimizationCampaignByIdempotencyKey(ctx, requested.TenantID, requested.IdempotencyKey)
		if lookupErr != nil {
			return campaign, false, lookupErr
		}
		if existing.InputDigest != requested.InputDigest || existing.ModelIdentity != requested.ModelIdentity || existing.Objective != requested.Objective || existing.Source != requested.Source || !semanticJSONEqual(existing.ProposalJSON, requested.ProposalJSON) || len(existing.Candidates) != len(candidates) {
			return existing, false, domain.ErrConflict
		}
		return existing, false, nil
	}
	for index := range candidates {
		candidate := candidates[index]
		candidate.ID, err = newID()
		if err != nil {
			return campaign, false, err
		}
		candidate.TenantID, candidate.CampaignID = campaign.TenantID, campaign.ID
		if _, err = tx.ExecContext(ctx, `INSERT INTO optimization_candidate_runs(id,tenant_id,campaign_id,proposal_candidate_id,rank,state,evidence_state,deployment_spec_json,predicted_evidence_json,actual_evidence_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?::jsonb,?::jsonb,?,?)`, candidate.ID, candidate.TenantID, candidate.CampaignID, candidate.ProposalCandidateID, candidate.Rank, optimizationcampaign.CandidateProposed, candidate.EvidenceState, candidate.DeploymentSpecJSON, candidate.PredictedEvidenceJSON, candidate.ActualEvidenceJSON, stamp, stamp); err != nil {
			return campaign, false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return campaign, false, err
	}
	created, err := s.OptimizationCampaign(ctx, campaign.TenantID, campaign.ID)
	return created, true, err
}

func validateOptimizationCampaign(campaign domain.OptimizationCampaign, candidates []domain.OptimizationCandidateRun) error {
	if campaign.TenantID == "" || campaign.IdempotencyKey == "" || len(campaign.IdempotencyKey) > 128 || len(campaign.InputDigest) != 64 || campaign.ModelIdentity == "" || campaign.Objective == "" || campaign.Source == "" || !boundedJSON(campaign.ProposalJSON) {
		return errors.New("complete immutable optimization campaign identity and proposal are required")
	}
	if len(candidates) < 1 || len(candidates) > 100 {
		return errors.New("optimization campaign requires between 1 and 100 candidates")
	}
	seenIDs, seenRanks := map[string]struct{}{}, map[int]struct{}{}
	for _, candidate := range candidates {
		if candidate.ProposalCandidateID == "" || candidate.Rank < 1 || candidate.Rank > len(candidates) || !boundedJSON(candidate.DeploymentSpecJSON) || !boundedJSON(candidate.PredictedEvidenceJSON) || !boundedJSON(candidate.ActualEvidenceJSON) {
			return errors.New("complete bounded candidate identity, rank, deployment spec, and evidence are required")
		}
		if candidate.EvidenceState != "unmeasured" && candidate.EvidenceState != "modeled" {
			return errors.New("new candidates must start with unmeasured or modeled evidence")
		}
		if _, duplicate := seenIDs[candidate.ProposalCandidateID]; duplicate {
			return errors.New("candidate IDs must be unique")
		}
		if _, duplicate := seenRanks[candidate.Rank]; duplicate {
			return errors.New("candidate ranks must be unique")
		}
		seenIDs[candidate.ProposalCandidateID], seenRanks[candidate.Rank] = struct{}{}, struct{}{}
	}
	return nil
}

func boundedJSON(value string) bool {
	return len(value) > 0 && len(value) <= maxOptimizationEvidenceBytes && json.Valid([]byte(value))
}

func (s *Store) OptimizationCampaign(ctx context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	return s.optimizationCampaign(ctx, `tenant_id=? AND id=?`, tenant, id)
}

func (s *Store) OptimizationCampaignByIdempotencyKey(ctx context.Context, tenant, key string) (domain.OptimizationCampaign, error) {
	return s.optimizationCampaign(ctx, `tenant_id=? AND idempotency_key=?`, tenant, key)
}

func (s *Store) optimizationCampaign(ctx context.Context, predicate string, values ...any) (domain.OptimizationCampaign, error) {
	var row domain.OptimizationCampaign
	var cost sql.NullFloat64
	var expiry, approvedAt, approvedBy, failure sql.NullString
	var created, updated string
	query := `SELECT id,tenant_id,idempotency_key,input_digest,model_identity,objective,source,state,proposal_json::text,max_candidates,approved_max_cost_usd,approval_expires_at,approved_by,approved_at,cancel_requested,failure_code,created_at,updated_at FROM optimization_campaigns WHERE ` + predicate
	err := s.QueryRowContext(ctx, query, values...).Scan(&row.ID, &row.TenantID, &row.IdempotencyKey, &row.InputDigest, &row.ModelIdentity, &row.Objective, &row.Source, &row.State, &row.ProposalJSON, &row.MaxCandidates, &cost, &expiry, &approvedBy, &approvedAt, &row.CancelRequested, &failure, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return row, domain.ErrNotFound
	}
	if err != nil {
		return row, err
	}
	if cost.Valid {
		row.ApprovedMaxCostUSD = &cost.Float64
	}
	if expiry.Valid {
		value := parseTime(expiry.String)
		row.ApprovalExpiresAt = &value
	}
	if approvedAt.Valid {
		value := parseTime(approvedAt.String)
		row.ApprovedAt = &value
	}
	row.ApprovedBy, row.FailureCode = approvedBy.String, failure.String
	row.CreatedAt, row.UpdatedAt = parseTime(created), parseTime(updated)
	row.Candidates, err = s.optimizationCandidates(ctx, row.TenantID, row.ID)
	return row, err
}

func (s *Store) OptimizationCampaigns(ctx context.Context, tenant string, limit int) ([]domain.OptimizationCampaign, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.QueryContext(ctx, `SELECT id FROM optimization_campaigns WHERE tenant_id=? ORDER BY created_at DESC LIMIT ?`, tenant, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]domain.OptimizationCampaign, 0, len(ids))
	for _, id := range ids {
		row, lookupErr := s.OptimizationCampaign(ctx, tenant, id)
		if lookupErr != nil {
			return nil, lookupErr
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *Store) optimizationCandidates(ctx context.Context, tenant, campaignID string) ([]domain.OptimizationCandidateRun, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,campaign_id,proposal_candidate_id,rank,state,evidence_state,deployment_spec_json::text,predicted_evidence_json::text,actual_evidence_json::text,COALESCE(deployment_name,''),COALESCE(revision_id,''),COALESCE(benchmark_id,''),COALESCE(quality_evidence_id,''),COALESCE(release_guard_evaluation_id,''),COALESCE(optimized_artifact_id,''),COALESCE(failure_code,''),created_at,updated_at FROM optimization_candidate_runs WHERE tenant_id=? AND campaign_id=? ORDER BY rank`, tenant, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.OptimizationCandidateRun
	for rows.Next() {
		var row domain.OptimizationCandidateRun
		var created, updated string
		if err = rows.Scan(&row.ID, &row.TenantID, &row.CampaignID, &row.ProposalCandidateID, &row.Rank, &row.State, &row.EvidenceState, &row.DeploymentSpecJSON, &row.PredictedEvidenceJSON, &row.ActualEvidenceJSON, &row.DeploymentName, &row.RevisionID, &row.BenchmarkID, &row.QualityEvidenceID, &row.ReleaseGuardEvaluationID, &row.OptimizedArtifactID, &row.FailureCode, &created, &updated); err != nil {
			return nil, err
		}
		row.CreatedAt, row.UpdatedAt = parseTime(created), parseTime(updated)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) ApproveOptimizationCampaign(ctx context.Context, tenant, id, actor string, maxCostUSD float64, expiresAt time.Time) (domain.OptimizationCampaign, error) {
	if actor == "" || maxCostUSD <= 0 || maxCostUSD > 1_000_000 || math.IsNaN(maxCostUSD) || math.IsInf(maxCostUSD, 0) || !expiresAt.After(time.Now()) || expiresAt.After(time.Now().Add(24*time.Hour)) {
		return domain.OptimizationCampaign{}, errors.New("approval requires an actor, maximum cost in (0,1000000], and expiry within 24 hours")
	}
	stamp := now()
	result, err := s.ExecContext(ctx, `UPDATE optimization_campaigns SET state=?,approved_max_cost_usd=?,approval_expires_at=?,approved_by=?,approved_at=?,updated_at=? WHERE tenant_id=? AND id=? AND state=? AND cancel_requested=FALSE`, optimizationcampaign.CampaignApproved, maxCostUSD, expiresAt.UTC(), actor, stamp, stamp, tenant, id, optimizationcampaign.CampaignAwaitingApproval)
	if err != nil {
		return domain.OptimizationCampaign{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		row, lookupErr := s.OptimizationCampaign(ctx, tenant, id)
		if lookupErr != nil {
			return row, lookupErr
		}
		// Retrying an approval expressed as a relative duration naturally
		// computes a slightly different absolute expiry. The first persisted
		// boundary remains authoritative; identical actor and cost retries are
		// idempotent and never extend paid-resource authority.
		if (row.State == optimizationcampaign.CampaignApproved || row.State == optimizationcampaign.CampaignRunning) && row.ApprovedBy == actor && row.ApprovedMaxCostUSD != nil && *row.ApprovedMaxCostUSD == maxCostUSD && row.ApprovalExpiresAt != nil && row.ApprovalExpiresAt.After(time.Now()) {
			return row, nil
		}
		return row, domain.ErrConflict
	}
	return s.OptimizationCampaign(ctx, tenant, id)
}

func (s *Store) CancelOptimizationCampaign(ctx context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	stamp := now()
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.OptimizationCampaign{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE optimization_campaigns SET state=?,cancel_requested=TRUE,updated_at=? WHERE tenant_id=? AND id=? AND state NOT IN ('promoted','observed','cleaned')`, optimizationcampaign.CampaignCancelled, stamp, tenant, id)
	if err != nil {
		return domain.OptimizationCampaign{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		var state string
		lookupErr := tx.QueryRowContext(ctx, `SELECT state FROM optimization_campaigns WHERE tenant_id=? AND id=?`, tenant, id).Scan(&state)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return domain.OptimizationCampaign{}, domain.ErrNotFound
		}
		if lookupErr != nil {
			return domain.OptimizationCampaign{}, lookupErr
		}
		if state == optimizationcampaign.CampaignCancelled {
			if err = tx.Commit(); err != nil {
				return domain.OptimizationCampaign{}, err
			}
			return s.OptimizationCampaign(ctx, tenant, id)
		}
		return domain.OptimizationCampaign{}, domain.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE optimization_candidate_runs SET state=?,evidence_state='stale',updated_at=? WHERE tenant_id=? AND campaign_id=? AND state IN ('proposed','provisioning','ready','measuring','validating','ranked','guard_passed')`, optimizationcampaign.CandidateCancelled, stamp, tenant, id); err != nil {
		return domain.OptimizationCampaign{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.OptimizationCampaign{}, err
	}
	return s.OptimizationCampaign(ctx, tenant, id)
}

// TransitionOptimizationCandidate is the fenced worker boundary. The caller
// must supply the state it observed; stale workers cannot overwrite newer
// evidence. Actual evidence is append-by-replacement only for the same legal
// transition and remains distinct from PredictedEvidenceJSON.
func (s *Store) TransitionOptimizationCandidate(ctx context.Context, tenant, campaignID, candidateID, from, to string, updates domain.OptimizationCandidateRun) (domain.OptimizationCandidateRun, error) {
	if err := optimizationcampaign.ValidateCandidateTransition(from, to); err != nil {
		return domain.OptimizationCandidateRun{}, err
	}
	if updates.ActualEvidenceJSON != "" && !boundedJSON(updates.ActualEvidenceJSON) {
		return domain.OptimizationCandidateRun{}, errors.New("actual evidence must be bounded valid JSON")
	}
	evidence, err := optimizationcampaign.EvidenceForState(to)
	if err != nil {
		return domain.OptimizationCandidateRun{}, err
	}
	if to == optimizationcampaign.CandidateReady && (updates.DeploymentName == "" || updates.RevisionID == "") {
		return domain.OptimizationCandidateRun{}, errors.New("ready candidate requires deployment and immutable revision identity")
	}
	if (to == optimizationcampaign.CandidateValidating || to == optimizationcampaign.CandidateRanked) && updates.BenchmarkID == "" {
		return domain.OptimizationCandidateRun{}, errors.New("measured candidate requires benchmark evidence")
	}
	if (to == optimizationcampaign.CandidateGuardPassed || to == optimizationcampaign.CandidatePromoted) && updates.ReleaseGuardEvaluationID == "" {
		return domain.OptimizationCandidateRun{}, errors.New("qualified candidate requires Release Guard evidence")
	}
	currentRows, err := s.optimizationCandidates(ctx, tenant, campaignID)
	if err != nil {
		return domain.OptimizationCandidateRun{}, err
	}
	var current domain.OptimizationCandidateRun
	for _, row := range currentRows {
		if row.ID == candidateID {
			current = row
			break
		}
	}
	if current.ID == "" {
		return domain.OptimizationCandidateRun{}, domain.ErrNotFound
	}
	if current.State != from {
		return current, domain.ErrConflict
	}
	if current.OptimizedArtifactID != "" && updates.OptimizedArtifactID != "" && current.OptimizedArtifactID != updates.OptimizedArtifactID {
		return current, errors.New("optimized artifact identity is immutable once attached to a candidate")
	}
	if current.QualityEvidenceID != "" && updates.QualityEvidenceID != "" && current.QualityEvidenceID != updates.QualityEvidenceID {
		return current, errors.New("quality evidence identity is immutable once attached to a candidate")
	}
	effectiveArtifactID, effectiveQualityID := current.OptimizedArtifactID, current.QualityEvidenceID
	if updates.OptimizedArtifactID != "" {
		effectiveArtifactID = updates.OptimizedArtifactID
	}
	if updates.QualityEvidenceID != "" {
		effectiveQualityID = updates.QualityEvidenceID
	}
	if effectiveArtifactID != "" {
		artifact, _, lookupErr := s.OptimizedArtifact(ctx, tenant, effectiveArtifactID)
		if lookupErr != nil {
			return current, fmt.Errorf("optimized artifact: %w", lookupErr)
		}
		if artifact.State != "ready" {
			return current, errors.New("candidate optimized artifact must have an immutable ready build attestation")
		}
		if (to == optimizationcampaign.CandidateGuardPassed || to == optimizationcampaign.CandidatePromoted) && (effectiveQualityID == "" || artifact.EvidenceState != "qualified" || artifact.QualityEvidenceID != effectiveQualityID) {
			return current, errors.New("optimized artifact candidate requires matching qualified semantic evidence before Release Guard can pass")
		}
	}
	stamp := now()
	cleanup := to == optimizationcampaign.CandidateCleaned
	result, err := s.ExecContext(ctx, `UPDATE optimization_candidate_runs c SET state=?,evidence_state=CASE WHEN c.evidence_state='modeled' AND ?='unmeasured' THEN 'modeled' ELSE ? END,actual_evidence_json=COALESCE(NULLIF(?,'')::jsonb,c.actual_evidence_json),deployment_name=COALESCE(NULLIF(?,''),c.deployment_name),revision_id=COALESCE(NULLIF(?,''),c.revision_id),benchmark_id=COALESCE(NULLIF(?,''),c.benchmark_id),quality_evidence_id=COALESCE(NULLIF(?,''),c.quality_evidence_id),release_guard_evaluation_id=COALESCE(NULLIF(?,''),c.release_guard_evaluation_id),optimized_artifact_id=COALESCE(NULLIF(?,''),c.optimized_artifact_id),failure_code=COALESCE(NULLIF(?,''),c.failure_code),updated_at=? FROM optimization_campaigns campaign WHERE c.tenant_id=? AND c.campaign_id=? AND c.id=? AND c.state=? AND campaign.id=c.campaign_id AND campaign.tenant_id=c.tenant_id AND ((? AND campaign.state IN ('running','ranked','guard_passed','rejected','inconclusive','promoted','observed','cancelled','failed','cleaned')) OR (NOT ? AND campaign.state IN ('approved','running','ranked','guard_passed','rejected','inconclusive','promoted','observed') AND campaign.cancel_requested=FALSE AND campaign.approval_expires_at>NOW()))`, to, evidence, evidence, updates.ActualEvidenceJSON, updates.DeploymentName, updates.RevisionID, updates.BenchmarkID, updates.QualityEvidenceID, updates.ReleaseGuardEvaluationID, updates.OptimizedArtifactID, updates.FailureCode, stamp, tenant, campaignID, candidateID, from, cleanup, cleanup)
	if err != nil {
		return domain.OptimizationCandidateRun{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return domain.OptimizationCandidateRun{}, domain.ErrConflict
	}
	rows, err := s.optimizationCandidates(ctx, tenant, campaignID)
	if err != nil {
		return domain.OptimizationCandidateRun{}, err
	}
	states := make([]string, 0, len(rows))
	for _, row := range rows {
		states = append(states, row.State)
	}
	campaignState, stateErr := optimizationcampaign.AggregateState(states)
	if stateErr != nil {
		return domain.OptimizationCandidateRun{}, stateErr
	}
	if _, err = s.ExecContext(ctx, `UPDATE optimization_campaigns SET state=?,updated_at=? WHERE tenant_id=? AND id=?`, campaignState, stamp, tenant, campaignID); err != nil {
		return domain.OptimizationCandidateRun{}, err
	}
	for _, row := range rows {
		if row.ID == candidateID {
			return row, nil
		}
	}
	return domain.OptimizationCandidateRun{}, fmt.Errorf("%w: candidate", domain.ErrNotFound)
}
