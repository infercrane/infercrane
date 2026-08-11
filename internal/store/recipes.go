package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) CreateModelRecipe(ctx context.Context, tenant string, value domain.ModelRecipe) (domain.ModelRecipe, error) {
	if tenant == "" {
		tenant = "global"
	}
	if value.Name == "" || value.Version == "" || len(value.Digest) != 64 || !json.Valid([]byte(value.PayloadJSON)) || !json.Valid([]byte(value.ProvenanceJSON)) || len(value.PayloadJSON) > 1<<20 || len(value.ProvenanceJSON) > 256<<10 {
		return domain.ModelRecipe{}, errors.New("complete bounded recipe identity, payload, provenance, and digest are required")
	}
	if value.ID == "" {
		var err error
		value.ID, err = newID()
		if err != nil {
			return domain.ModelRecipe{}, err
		}
	}
	stamp := now()
	_, err := s.ExecContext(ctx, `INSERT INTO model_recipes(id,tenant_id,name,version,digest,payload_json,provenance_json,created_at) VALUES(?,?,?,?,?,?::jsonb,?::jsonb,?) ON CONFLICT(tenant_id,name,version) DO NOTHING`, value.ID, tenant, value.Name, value.Version, value.Digest, value.PayloadJSON, value.ProvenanceJSON, stamp)
	if err != nil {
		return domain.ModelRecipe{}, err
	}
	row, err := s.ModelRecipe(ctx, tenant, value.Name, value.Version)
	if err != nil {
		return domain.ModelRecipe{}, err
	}
	if row.Digest != value.Digest {
		return domain.ModelRecipe{}, domain.ErrConflict
	}
	return row, nil
}

func (s *Store) ModelRecipe(ctx context.Context, tenant, name, version string) (domain.ModelRecipe, error) {
	var out domain.ModelRecipe
	var created string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,name,version,digest,payload_json::text,provenance_json::text,created_at FROM model_recipes WHERE tenant_id=? AND name=? AND version=?`, tenant, name, version).Scan(&out.ID, &out.TenantID, &out.Name, &out.Version, &out.Digest, &out.PayloadJSON, &out.ProvenanceJSON, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return out, domain.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.CreatedAt = parseTime(created)
	return out, nil
}

func (s *Store) ModelRecipes(ctx context.Context, tenant, query string, limit int) ([]domain.ModelRecipe, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	query = "%" + strings.ToLower(strings.TrimSpace(query)) + "%"
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,name,version,digest,payload_json::text,provenance_json::text,created_at FROM model_recipes WHERE tenant_id=? AND LOWER(name) LIKE ? ORDER BY created_at DESC LIMIT ?`, tenant, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ModelRecipe
	for rows.Next() {
		var row domain.ModelRecipe
		var created string
		if err = rows.Scan(&row.ID, &row.TenantID, &row.Name, &row.Version, &row.Digest, &row.PayloadJSON, &row.ProvenanceJSON, &created); err != nil {
			return nil, err
		}
		row.CreatedAt = parseTime(created)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) BenchmarksForModel(ctx context.Context, tenant, modelIdentity string, limit int) ([]domain.BenchmarkResult, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,deployment_id,deployment_name,revision_id,COALESCE(model_artifact_id,''),model_identity,runtime,runtime_version,runtime_config_json::text,provider,region,gpu,gpu_count,compute_mode,tool,tool_version,workload_json::text,reproduction_command,request_count,succeeded,failed,duration_seconds,request_throughput,output_token_throughput,ttft_p50_ms,ttft_p95_ms,tpot_p50_ms,tpot_p95_ms,latency_p50_ms,latency_p95_ms,goodput,gpu_utilization,cost_metadata_json::text,created_at FROM benchmark_results WHERE tenant_id=? AND model_identity=? ORDER BY created_at DESC LIMIT ?`, tenant, modelIdentity, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.BenchmarkResult
	for rows.Next() {
		var row domain.BenchmarkResult
		var created string
		if err = rows.Scan(&row.ID, &row.TenantID, &row.DeploymentID, &row.DeploymentName, &row.RevisionID, &row.ModelArtifactID, &row.ModelIdentity, &row.Runtime, &row.RuntimeVersion, &row.RuntimeConfigJSON, &row.Provider, &row.Region, &row.GPU, &row.GPUCount, &row.ComputeMode, &row.Tool, &row.ToolVersion, &row.WorkloadJSON, &row.ReproductionCommand, &row.RequestCount, &row.Succeeded, &row.Failed, &row.DurationSeconds, &row.RequestThroughput, &row.OutputTokenThroughput, &row.TTFTP50MS, &row.TTFTP95MS, &row.TPOTP50MS, &row.TPOTP95MS, &row.LatencyP50MS, &row.LatencyP95MS, &row.Goodput, &row.GPUUtilization, &row.CostMetadataJSON, &created); err != nil {
			return nil, err
		}
		row.CreatedAt = parseTime(created)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) RecordLabEvaluation(ctx context.Context, tenant string, value domain.LabEvaluation) (domain.LabEvaluation, error) {
	if tenant == "" {
		tenant = "global"
	}
	if value.ModelIdentity == "" || value.AlgorithmVersion == "" || len(value.InputDigest) != 64 || !json.Valid([]byte(value.InputJSON)) || !json.Valid([]byte(value.ResultsJSON)) {
		return domain.LabEvaluation{}, errors.New("complete lab evaluation evidence is required")
	}
	if value.ID == "" {
		var err error
		value.ID, err = newID()
		if err != nil {
			return value, err
		}
	}
	stamp := now()
	_, err := s.ExecContext(ctx, `INSERT INTO lab_evaluations(id,tenant_id,model_identity,algorithm_version,input_json,results_json,input_digest,created_at) VALUES(?,?,?,?,?::jsonb,?::jsonb,?,?)`, value.ID, tenant, value.ModelIdentity, value.AlgorithmVersion, value.InputJSON, value.ResultsJSON, value.InputDigest, stamp)
	value.TenantID = tenant
	value.CreatedAt = parseTime(stamp)
	return value, err
}
