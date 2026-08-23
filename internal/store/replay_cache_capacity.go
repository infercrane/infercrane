package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/replay"
)

func (s *Store) CaptureReplayTrace(ctx context.Context, tenant, deploymentName string, window time.Duration, limit int) (domain.ReplayTrace, error) {
	if window <= 0 || window > 30*24*time.Hour {
		return domain.ReplayTrace{}, errors.New("replay window must be positive and at most 30 days")
	}
	if limit < 1 || limit > 10000 {
		limit = 1000
	}
	resolved, err := s.ResolveForTenant(ctx, tenant, deploymentName)
	if err != nil {
		return domain.ReplayTrace{}, err
	}
	end := time.Now().UTC()
	start := end.Add(-window)
	rows, err := s.QueryContext(ctx, `SELECT started_at,latency_ms,input_tokens,output_tokens,operation_name,streaming,COALESCE(session_id_hash,''),COALESCE(parent_session_id_hash,''),COALESCE(shared_prefix_hash,''),tool_pause_ms FROM request_records WHERE tenant_id=? AND deployment_id=? AND started_at>=? AND started_at<=? ORDER BY started_at DESC LIMIT ?`, tenant, resolved.Deployment.ID, start, end, limit)
	if err != nil {
		return domain.ReplayTrace{}, err
	}
	defer rows.Close()
	var observations []replay.Observation
	for rows.Next() {
		var o replay.Observation
		var started string
		if err = rows.Scan(&started, &o.DurationMS, &o.InputTokens, &o.OutputTokens, &o.Operation, &o.Streaming, &o.SessionIDHash, &o.ParentSessionIDHash, &o.SharedPrefixHash, &o.ToolPauseMS); err != nil {
			return domain.ReplayTrace{}, err
		}
		o.StartedAt = parseTime(started)
		observations = append(observations, o)
	}
	if err = rows.Err(); err != nil {
		return domain.ReplayTrace{}, err
	}
	trace, err := replay.Build(resolved.Deployment.ID, deploymentName, resolved.Deployment.ActiveRevisionID, start, end, observations)
	if err != nil {
		return trace, err
	}
	trace.ID, err = newID()
	if err != nil {
		return trace, err
	}
	stamp := now()
	_, err = s.ExecContext(ctx, `INSERT INTO replay_traces(id,tenant_id,deployment_id,deployment_name,revision_id,schema_version,window_start,window_end,request_count,shape_json,summary_json,shape_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,?::jsonb,?,?)`, trace.ID, tenant, trace.DeploymentID, trace.DeploymentName, null(trace.RevisionID), trace.SchemaVersion, trace.WindowStart, trace.WindowEnd, trace.RequestCount, trace.ShapeJSON, trace.SummaryJSON, trace.ShapeDigest, stamp)
	trace.TenantID = tenant
	trace.CreatedAt = parseTime(stamp)
	return trace, err
}

func (s *Store) ReplayTrace(ctx context.Context, tenant, id string) (domain.ReplayTrace, error) {
	var row domain.ReplayTrace
	var start, end, created string
	err := s.QueryRowContext(ctx, `SELECT id,tenant_id,deployment_id,deployment_name,COALESCE(revision_id,''),schema_version,window_start,window_end,request_count,shape_json::text,summary_json::text,shape_digest,created_at FROM replay_traces WHERE tenant_id=? AND id=?`, tenant, id).Scan(&row.ID, &row.TenantID, &row.DeploymentID, &row.DeploymentName, &row.RevisionID, &row.SchemaVersion, &start, &end, &row.RequestCount, &row.ShapeJSON, &row.SummaryJSON, &row.ShapeDigest, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return row, domain.ErrNotFound
	}
	if err != nil {
		return row, err
	}
	row.WindowStart, row.WindowEnd, row.CreatedAt = parseTime(start), parseTime(end), parseTime(created)
	return row, nil
}

func (s *Store) RecordArtifactCacheObservation(ctx context.Context, tenant string, row domain.ArtifactCacheObservation) (domain.ArtifactCacheObservation, error) {
	if tenant == "" || row.ModelArtifactID == "" || row.Provider == "" || row.Location == "" || row.Source == "" || row.ObservedAt.IsZero() || !row.ExpiresAt.After(row.ObservedAt) || row.ExpiresAt.Sub(row.ObservedAt) > 24*time.Hour || !json.Valid([]byte(row.EvidenceJSON)) {
		return row, errors.New("complete bounded cache observation is required")
	}
	switch row.State {
	case "present", "prefetching", "missing", "unknown":
	default:
		return row, errors.New("invalid cache state")
	}
	var err error
	row.ID, err = newID()
	if err != nil {
		return row, err
	}
	stamp := now()
	result, err := s.ExecContext(ctx, `INSERT INTO artifact_cache_observations(id,tenant_id,model_artifact_id,provider,region,location,state,source,evidence_json,observed_at,expires_at,created_at) SELECT ?,?,?,?,?,?,?,?,?::jsonb,?,?,? WHERE EXISTS(SELECT 1 FROM model_artifacts WHERE tenant_id=? AND id=?)`, row.ID, tenant, row.ModelArtifactID, row.Provider, row.Region, row.Location, row.State, row.Source, row.EvidenceJSON, row.ObservedAt, row.ExpiresAt, stamp, tenant, row.ModelArtifactID)
	if err != nil {
		return row, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return row, domain.ErrNotFound
	}
	row.TenantID = tenant
	row.CreatedAt = parseTime(stamp)
	return row, nil
}

func (s *Store) RequestArtifactPrefetch(ctx context.Context, tenant string, row domain.ArtifactPrefetch) (domain.ArtifactPrefetch, bool, error) {
	if tenant == "" || row.ModelArtifactID == "" || row.Provider == "" || row.Location == "" || row.IdempotencyKey == "" {
		return row, false, errors.New("artifact, provider, location, and idempotency key are required")
	}
	requested := row
	var idErr error
	row.ID, idErr = newID()
	if idErr != nil {
		return row, false, idErr
	}
	row.Status = "requested"
	stamp := now()
	result, err := s.ExecContext(ctx, `INSERT INTO artifact_prefetches(id,tenant_id,model_artifact_id,provider,region,location,status,idempotency_key,created_at,updated_at) SELECT ?,?,?,?,?,?,?,?,?,? WHERE EXISTS(SELECT 1 FROM model_artifacts WHERE tenant_id=? AND id=?) ON CONFLICT(tenant_id,idempotency_key) DO NOTHING`, row.ID, tenant, row.ModelArtifactID, row.Provider, row.Region, row.Location, row.Status, row.IdempotencyKey, stamp, stamp, tenant, row.ModelArtifactID)
	if err != nil {
		return row, false, err
	}
	count, _ := result.RowsAffected()
	if count == 0 {
		var created, updated string
		err = s.QueryRowContext(ctx, `SELECT id,model_artifact_id,provider,region,location,status,idempotency_key,COALESCE(provider_operation_id,''),COALESCE(error_code,''),created_at,updated_at FROM artifact_prefetches WHERE tenant_id=? AND idempotency_key=?`, tenant, row.IdempotencyKey).Scan(&row.ID, &row.ModelArtifactID, &row.Provider, &row.Region, &row.Location, &row.Status, &row.IdempotencyKey, &row.ProviderOperationID, &row.ErrorCode, &created, &updated)
		row.TenantID = tenant
		row.CreatedAt, row.UpdatedAt = parseTime(created), parseTime(updated)
		if errors.Is(err, sql.ErrNoRows) {
			return row, false, domain.ErrNotFound
		}
		if err == nil && (row.ModelArtifactID != requested.ModelArtifactID || row.Provider != requested.Provider || row.Region != requested.Region || row.Location != requested.Location) {
			return row, false, domain.ErrConflict
		}
		return row, false, err
	}
	row.TenantID = tenant
	row.CreatedAt, row.UpdatedAt = parseTime(stamp), parseTime(stamp)
	return row, true, nil
}

func (s *Store) UpdateArtifactPrefetch(ctx context.Context, tenant, id, status, providerOperationID, errorCode string) (domain.ArtifactPrefetch, error) {
	if tenant == "" || id == "" {
		return domain.ArtifactPrefetch{}, errors.New("tenant and prefetch ID are required")
	}
	switch status {
	case "requested", "running", "succeeded", "failed", "cancelled":
	default:
		return domain.ArtifactPrefetch{}, errors.New("invalid prefetch status")
	}
	stamp := now()
	result, err := s.ExecContext(ctx, `UPDATE artifact_prefetches SET status=?,provider_operation_id=NULLIF(?,''),error_code=NULLIF(?,''),updated_at=? WHERE tenant_id=? AND id=?`, status, providerOperationID, errorCode, stamp, tenant, id)
	if err != nil {
		return domain.ArtifactPrefetch{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return domain.ArtifactPrefetch{}, domain.ErrNotFound
	}
	var row domain.ArtifactPrefetch
	var created, updated string
	err = s.QueryRowContext(ctx, `SELECT id,tenant_id,model_artifact_id,provider,region,location,status,idempotency_key,COALESCE(provider_operation_id,''),COALESCE(error_code,''),created_at,updated_at FROM artifact_prefetches WHERE tenant_id=? AND id=?`, tenant, id).Scan(&row.ID, &row.TenantID, &row.ModelArtifactID, &row.Provider, &row.Region, &row.Location, &row.Status, &row.IdempotencyKey, &row.ProviderOperationID, &row.ErrorCode, &created, &updated)
	row.CreatedAt, row.UpdatedAt = parseTime(created), parseTime(updated)
	return row, err
}

func (s *Store) ArtifactCacheState(ctx context.Context, tenant, artifactID string) ([]domain.ArtifactCacheObservation, []domain.ArtifactPrefetch, error) {
	if tenant == "" || artifactID == "" {
		return nil, nil, errors.New("tenant and artifact are required")
	}
	var exists bool
	if err := s.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM model_artifacts WHERE tenant_id=? AND id=?)`, tenant, artifactID).Scan(&exists); err != nil {
		return nil, nil, err
	}
	if !exists {
		return nil, nil, domain.ErrNotFound
	}
	observationRows, err := s.QueryContext(ctx, `SELECT id,tenant_id,model_artifact_id,provider,region,location,state,source,evidence_json::text,observed_at,expires_at,created_at FROM artifact_cache_observations WHERE tenant_id=? AND model_artifact_id=? ORDER BY observed_at DESC,id DESC LIMIT 100`, tenant, artifactID)
	if err != nil {
		return nil, nil, err
	}
	observations := make([]domain.ArtifactCacheObservation, 0)
	for observationRows.Next() {
		var row domain.ArtifactCacheObservation
		var observedAt, expiresAt, createdAt string
		if err = observationRows.Scan(&row.ID, &row.TenantID, &row.ModelArtifactID, &row.Provider, &row.Region, &row.Location, &row.State, &row.Source, &row.EvidenceJSON, &observedAt, &expiresAt, &createdAt); err != nil {
			_ = observationRows.Close()
			return nil, nil, err
		}
		row.ObservedAt, row.ExpiresAt, row.CreatedAt = parseTime(observedAt), parseTime(expiresAt), parseTime(createdAt)
		observations = append(observations, row)
	}
	if err = observationRows.Close(); err != nil {
		return nil, nil, err
	}
	prefetchRows, err := s.QueryContext(ctx, `SELECT id,tenant_id,model_artifact_id,provider,region,location,status,idempotency_key,COALESCE(provider_operation_id,''),COALESCE(error_code,''),created_at,updated_at FROM artifact_prefetches WHERE tenant_id=? AND model_artifact_id=? ORDER BY created_at DESC,id DESC LIMIT 100`, tenant, artifactID)
	if err != nil {
		return nil, nil, err
	}
	defer prefetchRows.Close()
	prefetches := make([]domain.ArtifactPrefetch, 0)
	for prefetchRows.Next() {
		var row domain.ArtifactPrefetch
		var createdAt, updatedAt string
		if err = prefetchRows.Scan(&row.ID, &row.TenantID, &row.ModelArtifactID, &row.Provider, &row.Region, &row.Location, &row.Status, &row.IdempotencyKey, &row.ProviderOperationID, &row.ErrorCode, &createdAt, &updatedAt); err != nil {
			return nil, nil, err
		}
		row.CreatedAt, row.UpdatedAt = parseTime(createdAt), parseTime(updatedAt)
		prefetches = append(prefetches, row)
	}
	return observations, prefetches, prefetchRows.Err()
}

func (s *Store) RecordCapacityOperation(ctx context.Context, row domain.CapacityOperation) (domain.CapacityOperation, error) {
	if row.TenantID == "" || row.Provider == "" || row.Runtime == "" || row.ComputeMode == "" || row.Operation == "" || row.ResourceKey == "" || row.StartedAt.IsZero() {
		return row, errors.New("complete capacity operation is required")
	}
	// Production callers omit completion so both ends of a multi-host duration
	// stay in the PostgreSQL clock domain. Historical/test ingestion may supply
	// an explicit completion timestamp.
	if row.CompletedAt.IsZero() {
		if err := s.QueryRowContext(ctx, `SELECT NOW()`).Scan(&row.CompletedAt); err != nil {
			return row, fmt.Errorf("read capacity completion clock: %w", err)
		}
	}
	if row.CompletedAt.Before(row.StartedAt) {
		return row, errors.New("capacity completion precedes its durable start")
	}
	switch row.Outcome {
	case "succeeded", "capacity_unavailable", "runtime_failed", "provider_failed", "pending":
	default:
		return row, errors.New("invalid capacity outcome")
	}
	row.DurationSeconds = row.CompletedAt.Sub(row.StartedAt).Seconds()
	var err error
	row.ID, err = newID()
	if err != nil {
		return row, err
	}
	stamp := now()
	_, err = s.ExecContext(ctx, `INSERT INTO capacity_operations(id,tenant_id,provider,runtime,compute_mode,region,gpu,operation,resource_key,outcome,error_code,started_at,completed_at,duration_seconds,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, row.ID, row.TenantID, row.Provider, row.Runtime, row.ComputeMode, row.Region, row.GPU, row.Operation, row.ResourceKey, row.Outcome, null(row.ErrorCode), row.StartedAt, row.CompletedAt, row.DurationSeconds, stamp)
	row.CreatedAt = parseTime(stamp)
	return row, err
}

func (s *Store) CapacityIntelligence(ctx context.Context, tenant string, window time.Duration) ([]domain.CapacitySummary, error) {
	if window <= 0 || window > 365*24*time.Hour {
		return nil, errors.New("capacity window must be positive and at most one year")
	}
	var end time.Time
	if err := s.QueryRowContext(ctx, `SELECT NOW()`).Scan(&end); err != nil {
		return nil, fmt.Errorf("read capacity window clock: %w", err)
	}
	end = end.UTC()
	start := end.Add(-window)
	rows, err := s.QueryContext(ctx, `WITH latest AS (
		SELECT DISTINCT ON (provider,runtime,compute_mode,region,gpu,operation,resource_key)
			provider,runtime,compute_mode,region,gpu,operation,resource_key,outcome,duration_seconds,completed_at,created_at,id
		FROM capacity_operations
		WHERE tenant_id=? AND completed_at>=? AND completed_at<=?
		ORDER BY provider,runtime,compute_mode,region,gpu,operation,resource_key,completed_at DESC,created_at DESC,id DESC
	)
	SELECT provider,runtime,compute_mode,region,gpu,
		COUNT(*),
		COUNT(*) FILTER(WHERE outcome='succeeded'),
		COUNT(*) FILTER(WHERE outcome='pending'),
		COUNT(*) FILTER(WHERE outcome='capacity_unavailable'),
		COUNT(*) FILTER(WHERE outcome='runtime_failed'),
		COUNT(*) FILTER(WHERE outcome='provider_failed'),
		COALESCE(COUNT(*) FILTER(WHERE outcome='succeeded')::double precision / NULLIF(COUNT(*) FILTER(WHERE outcome<>'pending'),0),0),
		CASE WHEN COUNT(*) FILTER(WHERE outcome='succeeded') >= 3 THEN percentile_cont(0.5) WITHIN GROUP(ORDER BY duration_seconds) FILTER(WHERE outcome='succeeded') END,
		CASE WHEN COUNT(*) FILTER(WHERE outcome='succeeded') >= 20 THEN percentile_cont(0.95) WITHIN GROUP(ORDER BY duration_seconds) FILTER(WHERE outcome='succeeded') END
	FROM latest
	GROUP BY provider,runtime,compute_mode,region,gpu
	ORDER BY provider,runtime,compute_mode,region,gpu`, tenant, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CapacitySummary
	for rows.Next() {
		var row domain.CapacitySummary
		var p50, p95 sql.NullFloat64
		if err = rows.Scan(&row.Provider, &row.Runtime, &row.ComputeMode, &row.Region, &row.GPU, &row.Attempts, &row.Succeeded, &row.Pending, &row.CapacityFailures, &row.RuntimeFailures, &row.ProviderFailures, &row.SuccessRate, &p50, &p95); err != nil {
			return nil, err
		}
		if p50.Valid {
			row.DurationP50Seconds = &p50.Float64
		}
		if p95.Valid {
			row.DurationP95Seconds = &p95.Float64
		}
		row.WindowStart, row.WindowEnd = start, end
		out = append(out, row)
	}
	return out, rows.Err()
}
