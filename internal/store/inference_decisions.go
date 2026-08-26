package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/infercrane/infercrane/internal/decision"
	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) SetSLOPolicy(ctx context.Context, tenant, deploymentName string, policy domain.SLOPolicy) (domain.SLOPolicy, error) {
	if err := (decision.SLOPolicy{MaxTTFTP95MS: policy.MaxTTFTP95MS, MaxLatencyP95MS: policy.MaxLatencyP95MS, MaxErrorRate: policy.MaxErrorRate, MinOutputTokensSecond: policy.MinOutputTokensSecond, MaxHourlyCost: policy.MaxHourlyCost}).Validate(); err != nil {
		return domain.SLOPolicy{}, err
	}
	resolved, err := s.ResolveForTenant(ctx, tenant, deploymentName)
	if err != nil {
		return domain.SLOPolicy{}, err
	}
	stamp := now()
	_, err = s.ExecContext(ctx, `INSERT INTO deployment_slo_policies(deployment_id,tenant_id,max_ttft_p95_ms,max_latency_p95_ms,max_error_rate,min_output_tokens_second,max_hourly_cost,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(deployment_id) DO UPDATE SET max_ttft_p95_ms=EXCLUDED.max_ttft_p95_ms,max_latency_p95_ms=EXCLUDED.max_latency_p95_ms,max_error_rate=EXCLUDED.max_error_rate,min_output_tokens_second=EXCLUDED.min_output_tokens_second,max_hourly_cost=EXCLUDED.max_hourly_cost,updated_at=EXCLUDED.updated_at WHERE deployment_slo_policies.tenant_id=EXCLUDED.tenant_id`, resolved.Deployment.ID, tenant, policy.MaxTTFTP95MS, policy.MaxLatencyP95MS, policy.MaxErrorRate, policy.MinOutputTokensSecond, policy.MaxHourlyCost, stamp, stamp)
	if err != nil {
		return domain.SLOPolicy{}, err
	}
	return s.SLOPolicy(ctx, tenant, deploymentName)
}

func (s *Store) SLOPolicy(ctx context.Context, tenant, deploymentName string) (domain.SLOPolicy, error) {
	var out domain.SLOPolicy
	var created, updated string
	err := s.QueryRowContext(ctx, `SELECT p.deployment_id,p.max_ttft_p95_ms,p.max_latency_p95_ms,p.max_error_rate,p.min_output_tokens_second,p.max_hourly_cost,p.created_at,p.updated_at FROM deployment_slo_policies p JOIN deployments d ON d.id=p.deployment_id AND d.tenant_id=p.tenant_id WHERE p.tenant_id=? AND d.name=?`, tenant, deploymentName).Scan(&out.DeploymentID, &out.MaxTTFTP95MS, &out.MaxLatencyP95MS, &out.MaxErrorRate, &out.MinOutputTokensSecond, &out.MaxHourlyCost, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SLOPolicy{}, ErrNotFound
	}
	if err != nil {
		return domain.SLOPolicy{}, err
	}
	out.CreatedAt, out.UpdatedAt = parseTime(created), parseTime(updated)
	return out, nil
}

func (s *Store) DeleteSLOPolicy(ctx context.Context, tenant, deploymentName string) error {
	result, err := s.ExecContext(ctx, `DELETE FROM deployment_slo_policies p USING deployments d WHERE p.deployment_id=d.id AND p.tenant_id=? AND d.tenant_id=? AND d.name=?`, tenant, tenant, deploymentName)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RecordInferenceRecommendation(ctx context.Context, row domain.InferenceRecommendation) (domain.InferenceRecommendation, error) {
	if row.TenantID == "" || row.DeploymentID == "" || row.AlgorithmVersion == "" || row.Reason == "" || row.InputSnapshotJSON == "" {
		return domain.InferenceRecommendation{}, errors.New("recommendation identity, algorithm, reason, and input snapshot are required")
	}
	if len(row.InputSnapshotJSON) > 4<<20 || len(row.CandidatesJSON) > 2<<20 || len(row.MissingJSON) > 64<<10 || len(row.Reason) > 4096 || !json.Valid([]byte(row.InputSnapshotJSON)) || !json.Valid([]byte(row.CandidatesJSON)) || !json.Valid([]byte(row.MissingJSON)) {
		return domain.InferenceRecommendation{}, errors.New("recommendation evidence is invalid or exceeds bounded storage limits")
	}
	if row.ID == "" {
		var err error
		row.ID, err = newID()
		if err != nil {
			return domain.InferenceRecommendation{}, err
		}
	}
	if row.InputDigest == "" {
		sum := sha256.Sum256([]byte(row.InputSnapshotJSON))
		row.InputDigest = hex.EncodeToString(sum[:])
	}
	stamp := now()
	_, err := s.ExecContext(ctx, `INSERT INTO inference_recommendations(id,tenant_id,deployment_id,status,algorithm_version,selected_evidence_id,reason,missing_json,candidates_json,input_snapshot_json,input_digest,created_at) VALUES(?,?,?,?,?,?,?,?::jsonb,?::jsonb,?::jsonb,?,?)`, row.ID, row.TenantID, row.DeploymentID, row.Status, row.AlgorithmVersion, null(row.SelectedEvidenceID), row.Reason, nullJSON(row.MissingJSON), nullJSON(row.CandidatesJSON), row.InputSnapshotJSON, row.InputDigest, stamp)
	if err != nil {
		return domain.InferenceRecommendation{}, err
	}
	row.CreatedAt = parseTime(stamp)
	return row, nil
}

func (s *Store) InferenceRecommendations(ctx context.Context, tenant, deploymentName string, limit int) ([]domain.InferenceRecommendation, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.QueryContext(ctx, `SELECT r.id,r.tenant_id,r.deployment_id,r.status,r.algorithm_version,COALESCE(r.selected_evidence_id,''),r.reason,r.missing_json::text,r.candidates_json::text,r.input_snapshot_json::text,r.input_digest,r.created_at FROM inference_recommendations r JOIN deployments d ON d.id=r.deployment_id WHERE r.tenant_id=? AND d.tenant_id=? AND d.name=? ORDER BY r.created_at DESC,r.id DESC LIMIT ?`, tenant, tenant, deploymentName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.InferenceRecommendation
	for rows.Next() {
		var row domain.InferenceRecommendation
		var created string
		if err = rows.Scan(&row.ID, &row.TenantID, &row.DeploymentID, &row.Status, &row.AlgorithmVersion, &row.SelectedEvidenceID, &row.Reason, &row.MissingJSON, &row.CandidatesJSON, &row.InputSnapshotJSON, &row.InputDigest, &created); err != nil {
			return nil, err
		}
		row.CreatedAt = parseTime(created)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) RecordCapacityEvidence(ctx context.Context, row domain.CapacityEvidence) (domain.CapacityEvidence, error) {
	if row.GPUCount == 0 {
		row.GPUCount = 1
	}
	if row.TenantID == "" || row.Provider == "" || row.Runtime == "" || row.ComputeMode == "" || row.GPUCount < 1 || row.GPUCount > 1024 || row.Source == "" || row.ObservedAt.IsZero() || !row.ExpiresAt.After(row.ObservedAt) || row.ExpiresAt.Sub(row.ObservedAt) > 24*time.Hour {
		return domain.CapacityEvidence{}, errors.New("complete bounded capacity evidence is required")
	}
	if len(row.Source) > 256 || len(row.EvidenceJSON) > 64<<10 || !json.Valid([]byte(row.EvidenceJSON)) {
		return domain.CapacityEvidence{}, errors.New("capacity provenance is invalid or exceeds bounded storage limits")
	}
	switch row.State {
	case "available", "constrained", "unavailable", "unknown":
	default:
		return domain.CapacityEvidence{}, errors.New("invalid capacity evidence state")
	}
	if row.ID == "" {
		var err error
		row.ID, err = newID()
		if err != nil {
			return domain.CapacityEvidence{}, err
		}
	}
	stamp := now()
	_, err := s.ExecContext(ctx, `INSERT INTO capacity_evidence(id,tenant_id,provider,runtime,compute_mode,region,gpu,gpu_count,state,source,evidence_json,observed_at,expires_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?::jsonb,?,?,?)`, row.ID, row.TenantID, row.Provider, row.Runtime, row.ComputeMode, row.Region, row.GPU, row.GPUCount, row.State, row.Source, nullJSON(row.EvidenceJSON), row.ObservedAt.UTC().Format(time.RFC3339Nano), row.ExpiresAt.UTC().Format(time.RFC3339Nano), stamp)
	if err != nil {
		return domain.CapacityEvidence{}, err
	}
	row.CreatedAt = parseTime(stamp)
	return row, nil
}

func (s *Store) LatestCapacityEvidence(ctx context.Context, tenant, provider, runtime, mode, region, gpu string, gpuCount int) (domain.CapacityEvidence, error) {
	if gpuCount == 0 {
		gpuCount = 1
	}
	var row domain.CapacityEvidence
	var observed, expires, created string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,provider,runtime,compute_mode,region,gpu,gpu_count,state,source,evidence_json::text,observed_at,expires_at,created_at FROM capacity_evidence WHERE tenant_id=? AND provider=? AND runtime=? AND compute_mode=? AND region=? AND gpu=? AND gpu_count=? ORDER BY observed_at DESC,id DESC LIMIT 1`, tenant, provider, runtime, mode, region, gpu, gpuCount).Scan(&row.ID, &row.TenantID, &row.Provider, &row.Runtime, &row.ComputeMode, &row.Region, &row.GPU, &row.GPUCount, &row.State, &row.Source, &row.EvidenceJSON, &observed, &expires, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CapacityEvidence{}, ErrNotFound
	}
	if err != nil {
		return domain.CapacityEvidence{}, err
	}
	row.ObservedAt, row.ExpiresAt, row.CreatedAt = parseTime(observed), parseTime(expires), parseTime(created)
	if !row.ExpiresAt.After(time.Now().UTC()) {
		row.State = "unknown"
	}
	return row, nil
}
