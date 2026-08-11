package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) RecordFinOpsReport(ctx context.Context, row domain.FinOpsReport) (domain.FinOpsReport, error) {
	if row.TenantID == "" || row.DeploymentID == "" || row.DeploymentName == "" || !row.WindowEnd.After(row.WindowStart) || !json.Valid([]byte(row.SummaryJSON)) || !json.Valid([]byte(row.EvidenceJSON)) || len(row.InputDigest) != 64 {
		return row, errors.New("complete evidence-backed FinOps report is required")
	}
	if row.Status != "measured" && row.Status != "partial" && row.Status != "unavailable" {
		return row, errors.New("invalid FinOps status")
	}
	var err error
	row.ID, err = newID()
	if err != nil {
		return row, err
	}
	stamp := now()
	_, err = s.ExecContext(ctx, `INSERT INTO finops_reports(id,tenant_id,deployment_id,deployment_name,window_start,window_end,currency,status,known_cost,estimated_avoidable_cost,summary_json,evidence_json,input_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?::jsonb,?::jsonb,?,?)`, row.ID, row.TenantID, row.DeploymentID, row.DeploymentName, row.WindowStart, row.WindowEnd, row.Currency, row.Status, row.KnownCost, row.EstimatedAvoidableCost, row.SummaryJSON, row.EvidenceJSON, row.InputDigest, stamp)
	row.CreatedAt = parseTime(stamp)
	return row, err
}

func (s *Store) FinOpsReports(ctx context.Context, tenant, name string, limit int) ([]domain.FinOpsReport, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.QueryContext(ctx, `SELECT f.id,f.tenant_id,f.deployment_id,f.deployment_name,f.window_start,f.window_end,f.currency,f.status,f.known_cost,f.estimated_avoidable_cost,f.summary_json::text,f.evidence_json::text,f.input_digest,f.created_at FROM finops_reports f JOIN deployments d ON d.id=f.deployment_id WHERE f.tenant_id=? AND d.name=? ORDER BY f.created_at DESC LIMIT ?`, tenant, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.FinOpsReport
	for rows.Next() {
		var r domain.FinOpsReport
		var a, b, c string
		if err = rows.Scan(&r.ID, &r.TenantID, &r.DeploymentID, &r.DeploymentName, &a, &b, &r.Currency, &r.Status, &r.KnownCost, &r.EstimatedAvoidableCost, &r.SummaryJSON, &r.EvidenceJSON, &r.InputDigest, &c); err != nil {
			return nil, err
		}
		r.WindowStart, r.WindowEnd, r.CreatedAt = parseTime(a), parseTime(b), parseTime(c)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CreateAutopilotPlan(ctx context.Context, row domain.AutopilotPlan) (domain.AutopilotPlan, bool, error) {
	if row.TenantID == "" || row.DeploymentID == "" || row.RecommendationID == "" || row.Objective == "" || !json.Valid([]byte(row.CandidateJSON)) || !json.Valid([]byte(row.EvidenceJSON)) || len(row.InputDigest) != 64 {
		return row, false, errors.New("complete advisory plan is required")
	}
	requested := row
	var err error
	row.ID, err = newID()
	if err != nil {
		return row, false, err
	}
	row.Status = "advisory"
	stamp := now()
	result, err := s.ExecContext(ctx, `INSERT INTO autopilot_plans(id,tenant_id,deployment_id,deployment_name,recommendation_id,status,objective,candidate_json,evidence_json,input_digest,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?::jsonb,?::jsonb,?,?,?) ON CONFLICT(tenant_id,recommendation_id,objective) DO NOTHING`, row.ID, row.TenantID, row.DeploymentID, row.DeploymentName, row.RecommendationID, row.Status, row.Objective, row.CandidateJSON, row.EvidenceJSON, row.InputDigest, stamp, stamp)
	if err != nil {
		return row, false, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		row, err = s.AutopilotPlan(ctx, row.TenantID, "", row.RecommendationID, row.Objective)
		if err == nil && (row.DeploymentID != requested.DeploymentID || row.CandidateJSON != requested.CandidateJSON || row.InputDigest != requested.InputDigest) {
			return row, false, domain.ErrConflict
		}
		return row, false, err
	}
	row.CreatedAt, row.UpdatedAt = parseTime(stamp), parseTime(stamp)
	return row, true, nil
}

func (s *Store) AutopilotPlan(ctx context.Context, tenant, id, recommendation, objective string) (domain.AutopilotPlan, error) {
	var r domain.AutopilotPlan
	var approved sql.NullString
	var approvedAt sql.NullString
	var created, updated string
	query := `SELECT id,tenant_id,deployment_id,deployment_name,recommendation_id,status,objective,candidate_json::text,evidence_json::text,input_digest,approved_by,approved_at,created_at,updated_at FROM autopilot_plans WHERE tenant_id=? AND ((?<>'' AND id=?) OR (?='' AND recommendation_id=? AND objective=?))`
	err := s.QueryRowContext(ctx, query, tenant, id, id, id, recommendation, objective).Scan(&r.ID, &r.TenantID, &r.DeploymentID, &r.DeploymentName, &r.RecommendationID, &r.Status, &r.Objective, &r.CandidateJSON, &r.EvidenceJSON, &r.InputDigest, &approved, &approvedAt, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return r, domain.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.ApprovedBy = approved.String
	if approvedAt.Valid {
		v := parseTime(approvedAt.String)
		r.ApprovedAt = &v
	}
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(updated)
	return r, nil
}

func (s *Store) ApproveAutopilotPlan(ctx context.Context, tenant, id, actor string) (domain.AutopilotPlan, error) {
	if actor == "" {
		return domain.AutopilotPlan{}, errors.New("approver is required")
	}
	stamp := now()
	result, err := s.ExecContext(ctx, `UPDATE autopilot_plans SET status='approved',approved_by=?,approved_at=?,updated_at=? WHERE tenant_id=? AND id=? AND status='advisory'`, actor, stamp, stamp, tenant, id)
	if err != nil {
		return domain.AutopilotPlan{}, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		row, lookup := s.AutopilotPlan(ctx, tenant, id, "", "")
		if lookup == nil && row.Status == "approved" {
			return row, nil
		}
		return row, domain.ErrConflict
	}
	return s.AutopilotPlan(ctx, tenant, id, "", "")
}
