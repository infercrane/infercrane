package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) AttachModelArtifact(ctx context.Context, tenant, revisionID string, artifact domain.ModelArtifact) (domain.ModelArtifact, error) {
	if tenant == "" {
		tenant = "global"
	}
	if artifact.Source != "huggingface" || artifact.Repository == "" || artifact.ImmutableRevision == "" || artifact.ModelIdentity == "" {
		return domain.ModelArtifact{}, errors.New("complete Hugging Face artifact identity is required")
	}
	if !json.Valid([]byte(artifact.RuntimeCompatibilityJSON)) {
		artifact.RuntimeCompatibilityJSON = "{}"
	}
	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.ModelArtifact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var existingArtifact sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT r.model_artifact_id FROM deployment_revisions r JOIN deployments d ON d.id=r.deployment_id WHERE r.id=? AND d.tenant_id=? FOR UPDATE`, revisionID, tenant).Scan(&existingArtifact); errors.Is(err, sql.ErrNoRows) {
		return domain.ModelArtifact{}, ErrNotFound
	} else if err != nil {
		return domain.ModelArtifact{}, err
	}
	if existingArtifact.Valid {
		return modelArtifactQuery(ctx, tx, tenant, existingArtifact.String)
	}
	artifact.ID, err = newID()
	if err != nil {
		return domain.ModelArtifact{}, err
	}
	stamp := now()
	artifact.TenantID, artifact.ResolvedAt = tenant, parseTime(stamp)
	if artifact.RequestedRevision == "" {
		artifact.RequestedRevision = "main"
	}
	if artifact.CacheState == "" {
		artifact.CacheState = "unknown"
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO model_artifacts(id,tenant_id,source,repository,requested_revision,immutable_revision,model_identity,approximate_size_bytes,cache_state,runtime_compatibility_json,resolved_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,?) ON CONFLICT(tenant_id,source,repository,immutable_revision) DO NOTHING`, artifact.ID, tenant, artifact.Source, artifact.Repository, artifact.RequestedRevision, artifact.ImmutableRevision, artifact.ModelIdentity, artifact.ApproximateSizeBytes, artifact.CacheState, artifact.RuntimeCompatibilityJSON, stamp); err != nil {
		return domain.ModelArtifact{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT id FROM model_artifacts WHERE tenant_id=? AND source=? AND repository=? AND immutable_revision=?`, tenant, artifact.Source, artifact.Repository, artifact.ImmutableRevision).Scan(&artifact.ID); err != nil {
		return domain.ModelArtifact{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET model_artifact_id=? WHERE id=? AND model_artifact_id IS NULL`, artifact.ID, revisionID); err != nil {
		return domain.ModelArtifact{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.ModelArtifact{}, err
	}
	return artifact, nil
}

func (s *Store) ModelArtifactForRevision(ctx context.Context, tenant, revisionID string) (domain.ModelArtifact, error) {
	if tenant == "" {
		tenant = "global"
	}
	var artifactID sql.NullString
	err := s.QueryRowContext(ctx, `SELECT r.model_artifact_id FROM deployment_revisions r JOIN deployments d ON d.id=r.deployment_id WHERE r.id=? AND d.tenant_id=?`, revisionID, tenant).Scan(&artifactID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !artifactID.Valid) {
		return domain.ModelArtifact{}, ErrNotFound
	}
	if err != nil {
		return domain.ModelArtifact{}, err
	}
	return modelArtifactQuery(ctx, s, tenant, artifactID.String)
}

func (s *Store) ModelArtifactForTenantByID(ctx context.Context, tenant, id string) (domain.ModelArtifact, error) {
	if tenant == "" || id == "" {
		return domain.ModelArtifact{}, errors.New("tenant and artifact ID are required")
	}
	return modelArtifactQuery(ctx, s, tenant, id)
}

type artifactQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func modelArtifactQuery(ctx context.Context, query artifactQuerier, tenant, id string) (domain.ModelArtifact, error) {
	var out domain.ModelArtifact
	var size sql.NullInt64
	var stamp string
	err := query.QueryRowContext(ctx, `SELECT id,tenant_id,source,repository,requested_revision,immutable_revision,model_identity,approximate_size_bytes,cache_state,runtime_compatibility_json::text,resolved_at FROM model_artifacts WHERE tenant_id=? AND id=?`, tenant, id).Scan(&out.ID, &out.TenantID, &out.Source, &out.Repository, &out.RequestedRevision, &out.ImmutableRevision, &out.ModelIdentity, &size, &out.CacheState, &out.RuntimeCompatibilityJSON, &stamp)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrNotFound
	}
	if size.Valid {
		out.ApproximateSizeBytes = &size.Int64
	}
	out.ResolvedAt = parseTime(stamp)
	return out, err
}
