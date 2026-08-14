package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

var integrationNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@+-]{0,255}$`)

// CreateSandboxReference creates only InferCrane-side identity and an
// endpoint-restricted, expiring inference credential. The external sandbox is
// never created, mutated, or deleted by this operation.
func (s *Store) CreateSandboxReference(ctx context.Context, tenant string, reference domain.SandboxReference, ttl time.Duration) (domain.SandboxReference, string, error) {
	if tenant == "" || !integrationNamePattern.MatchString(reference.Provider) || !integrationNamePattern.MatchString(reference.ExternalID) || !integrationNamePattern.MatchString(reference.EndpointName) {
		return domain.SandboxReference{}, "", errors.New("tenant, provider, external ID, and endpoint are required and must use safe identifier characters")
	}
	if ttl < time.Minute || ttl > 24*time.Hour {
		return domain.SandboxReference{}, "", errors.New("sandbox credential TTL must be between 1 minute and 24 hours")
	}
	if reference.ExternalRevision != "" && !integrationNamePattern.MatchString(reference.ExternalRevision) {
		return domain.SandboxReference{}, "", errors.New("external revision contains unsupported characters")
	}
	if reference.MetadataJSON == "" {
		reference.MetadataJSON = "{}"
	}
	var metadata map[string]any
	if len(reference.MetadataJSON) > 16<<10 || json.Unmarshal([]byte(reference.MetadataJSON), &metadata) != nil || metadata == nil {
		return domain.SandboxReference{}, "", errors.New("sandbox metadata must be a JSON object no larger than 16 KiB")
	}
	if prohibited := prohibitedSandboxMetadataKey(metadata); prohibited != "" {
		return domain.SandboxReference{}, "", fmt.Errorf("sandbox metadata must not include %q content", prohibited)
	}

	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.SandboxReference{}, "", err
	}
	defer func() { _ = tx.Rollback() }()
	var endpointID string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM endpoints WHERE tenant_id=? AND name=? AND desired_state<>'deleted' FOR SHARE`, tenant, reference.EndpointName).Scan(&endpointID); errors.Is(err, sql.ErrNoRows) {
		return domain.SandboxReference{}, "", ErrNotFound
	} else if err != nil {
		return domain.SandboxReference{}, "", err
	}
	// Expiry is a credential boundary, not a permanent reservation of the
	// external sandbox identity. Retire expired references in this transaction
	// so the same workload can reconnect without weakening the invariant that
	// at most one live reference exists per external identity.
	rows, err := tx.QueryContext(ctx, `UPDATE sandbox_references SET status='stopped',updated_at=? WHERE tenant_id=? AND provider=? AND external_id=? AND status='referenced' AND expires_at<=NOW() RETURNING principal_id`, now(), tenant, reference.Provider, reference.ExternalID)
	if err != nil {
		return domain.SandboxReference{}, "", err
	}
	expiredPrincipalIDs := make([]string, 0, 1)
	for rows.Next() {
		var principalID string
		if err = rows.Scan(&principalID); err != nil {
			_ = rows.Close()
			return domain.SandboxReference{}, "", err
		}
		expiredPrincipalIDs = append(expiredPrincipalIDs, principalID)
	}
	if err = rows.Close(); err != nil {
		return domain.SandboxReference{}, "", err
	}
	for _, principalID := range expiredPrincipalIDs {
		if _, err = tx.ExecContext(ctx, `UPDATE principals SET disabled=TRUE WHERE tenant_id=? AND id=?`, tenant, principalID); err != nil {
			return domain.SandboxReference{}, "", err
		}
	}

	reference.ID, err = newID()
	if err != nil {
		return domain.SandboxReference{}, "", err
	}
	reference.PrincipalID, err = newID()
	if err != nil {
		return domain.SandboxReference{}, "", err
	}
	token, hash, err := newCredential()
	if err != nil {
		return domain.SandboxReference{}, "", err
	}
	stamp := now()
	created := parseTime(stamp)
	reference.TenantID, reference.Status = tenant, "referenced"
	reference.CreatedAt, reference.UpdatedAt, reference.ExpiresAt = created, created, created.Add(ttl)
	endpointNames, _ := json.Marshal([]string{reference.EndpointName})
	principalName := "sandbox/" + reference.Provider + "/" + reference.ExternalID + "/" + reference.ID
	if _, err = tx.ExecContext(ctx, `INSERT INTO principals(id,tenant_id,name,role,credential_hash,kind,scopes_json,disabled,created_at,expires_at,endpoint_names_json) VALUES(?,?,?,?,?,'inference_token','["read"]'::jsonb,FALSE,?,?,?::jsonb)`, reference.PrincipalID, tenant, principalName, "viewer", hash, stamp, reference.ExpiresAt.Format(time.RFC3339Nano), string(endpointNames)); err != nil {
		if isUniqueViolation(err) {
			return domain.SandboxReference{}, "", fmt.Errorf("%w: sandbox credential identity already exists", ErrConflict)
		}
		return domain.SandboxReference{}, "", err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sandbox_references(id,tenant_id,provider,external_id,external_revision,endpoint_name,principal_id,status,metadata_json,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'referenced',?::jsonb,?,?,?)`, reference.ID, tenant, reference.Provider, reference.ExternalID, reference.ExternalRevision, reference.EndpointName, reference.PrincipalID, reference.MetadataJSON, reference.ExpiresAt.Format(time.RFC3339Nano), stamp, stamp); err != nil {
		if isUniqueViolation(err) {
			return domain.SandboxReference{}, "", fmt.Errorf("%w: sandbox reference already exists", ErrConflict)
		}
		return domain.SandboxReference{}, "", err
	}
	if err = tx.Commit(); err != nil {
		return domain.SandboxReference{}, "", err
	}
	return reference, token, nil
}

func (s *Store) SandboxReferences(ctx context.Context, tenant string) ([]domain.SandboxReference, error) {
	rows, err := s.QueryContext(ctx, `SELECT id,tenant_id,provider,external_id,external_revision,endpoint_name,principal_id,status,metadata_json::text,expires_at,created_at,updated_at FROM sandbox_references WHERE tenant_id=? ORDER BY created_at DESC,id DESC`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.SandboxReference, 0)
	for rows.Next() {
		var row domain.SandboxReference
		var expires, created, updated string
		if err = rows.Scan(&row.ID, &row.TenantID, &row.Provider, &row.ExternalID, &row.ExternalRevision, &row.EndpointName, &row.PrincipalID, &row.Status, &row.MetadataJSON, &expires, &created, &updated); err != nil {
			return nil, err
		}
		row.ExpiresAt, row.CreatedAt, row.UpdatedAt = parseTime(expires), parseTime(created), parseTime(updated)
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) RevokeSandboxReference(ctx context.Context, tenant, id string) error {
	tx, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var principalID string
	if err = tx.QueryRowContext(ctx, `SELECT principal_id FROM sandbox_references WHERE tenant_id=? AND id=? FOR UPDATE`, tenant, id).Scan(&principalID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	stamp := now()
	if _, err = tx.ExecContext(ctx, `UPDATE principals SET disabled=TRUE WHERE tenant_id=? AND id=?`, tenant, principalID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sandbox_references SET status='stopped',updated_at=? WHERE tenant_id=? AND id=?`, stamp, tenant, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RotateSandboxCredential(ctx context.Context, tenant, id string) (string, error) {
	var principalID string
	if err := s.QueryRowContext(ctx, `SELECT principal_id FROM sandbox_references WHERE tenant_id=? AND id=? AND status='referenced' AND expires_at>NOW()`, tenant, id).Scan(&principalID); errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	} else if err != nil {
		return "", err
	}
	return s.RotatePrincipalForTenant(ctx, tenant, principalID)
}

func (s *Store) AttachTrainingArtifactHandoff(ctx context.Context, tenant, deploymentName string, handoff domain.TrainingArtifactHandoff, artifact domain.ModelArtifact) (domain.TrainingArtifactHandoff, domain.ModelArtifact, error) {
	if tenant == "" || deploymentName == "" || handoff.RevisionID == "" || handoff.Provider == "" || handoff.ExternalRunID == "" || handoff.ArtifactDigest == "" || handoff.PayloadDigest == "" || handoff.Signature == "" || handoff.PublicKey == "" || handoff.Algorithm == "" || handoff.KeyID == "" {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, errors.New("complete signed training handoff identity is required")
	}
	if artifact.Source != "training-handoff" || artifact.Repository == "" || artifact.ImmutableRevision == "" || artifact.ModelIdentity == "" {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, errors.New("complete immutable training model artifact identity is required")
	}
	if !json.Valid([]byte(artifact.RuntimeCompatibilityJSON)) {
		artifact.RuntimeCompatibilityJSON = "{}"
	}

	tx, err := s.beginTx(ctx)
	if err != nil {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = tx.QueryRowContext(ctx, `SELECT d.id FROM deployments d JOIN deployment_revisions r ON r.deployment_id=d.id WHERE d.tenant_id=? AND d.name=? AND r.id=? FOR UPDATE`, tenant, deploymentName, handoff.RevisionID).Scan(&handoff.DeploymentID); errors.Is(err, sql.ErrNoRows) {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, ErrNotFound
	} else if err != nil {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, err
	}
	var existingArtifact sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT model_artifact_id FROM deployment_revisions WHERE id=? FOR UPDATE`, handoff.RevisionID).Scan(&existingArtifact); err != nil {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, err
	}
	if existingArtifact.Valid {
		var created string
		if err = tx.QueryRowContext(ctx, `SELECT id,tenant_id,deployment_id,revision_id,model_artifact_id,provider,external_run_id,repository,immutable_revision,artifact_digest,base_model_identity,method,framework,framework_version,dataset_fingerprint,payload_digest,signature,public_key,algorithm,key_id,created_at FROM training_artifact_handoffs WHERE tenant_id=? AND revision_id=? AND payload_digest=?`, tenant, handoff.RevisionID, handoff.PayloadDigest).Scan(&handoff.ID, &handoff.TenantID, &handoff.DeploymentID, &handoff.RevisionID, &handoff.ModelArtifactID, &handoff.Provider, &handoff.ExternalRunID, &handoff.Repository, &handoff.ImmutableRevision, &handoff.ArtifactDigest, &handoff.BaseModelIdentity, &handoff.Method, &handoff.Framework, &handoff.FrameworkVersion, &handoff.DatasetFingerprint, &handoff.PayloadDigest, &handoff.Signature, &handoff.PublicKey, &handoff.Algorithm, &handoff.KeyID, &created); err == nil {
			handoff.CreatedAt = parseTime(created)
			artifact, err = modelArtifactQuery(ctx, tx, tenant, existingArtifact.String)
			return handoff, artifact, err
		}
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, fmt.Errorf("%w: revision already has a different immutable model artifact", ErrConflict)
	}

	artifact.ID, err = newID()
	if err != nil {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, err
	}
	handoff.ID, err = newID()
	if err != nil {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, err
	}
	stamp := now()
	artifact.TenantID, artifact.ResolvedAt = tenant, parseTime(stamp)
	artifact.RequestedRevision = artifact.ImmutableRevision
	artifact.CacheState = "unknown"
	if _, err = tx.ExecContext(ctx, `INSERT INTO model_artifacts(id,tenant_id,source,repository,requested_revision,immutable_revision,model_identity,approximate_size_bytes,cache_state,runtime_compatibility_json,resolved_at) VALUES(?,?,?,?,?,?,?,?,?,?::jsonb,?) ON CONFLICT(tenant_id,source,repository,immutable_revision) DO NOTHING`, artifact.ID, tenant, artifact.Source, artifact.Repository, artifact.RequestedRevision, artifact.ImmutableRevision, artifact.ModelIdentity, artifact.ApproximateSizeBytes, artifact.CacheState, artifact.RuntimeCompatibilityJSON, stamp); err != nil {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT id FROM model_artifacts WHERE tenant_id=? AND source=? AND repository=? AND immutable_revision=?`, tenant, artifact.Source, artifact.Repository, artifact.ImmutableRevision).Scan(&artifact.ID); err != nil {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE deployment_revisions SET model_artifact_id=? WHERE id=? AND model_artifact_id IS NULL`, artifact.ID, handoff.RevisionID); err != nil {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, err
	}
	handoff.TenantID, handoff.ModelArtifactID, handoff.CreatedAt = tenant, artifact.ID, parseTime(stamp)
	if _, err = tx.ExecContext(ctx, `INSERT INTO training_artifact_handoffs(id,tenant_id,deployment_id,revision_id,model_artifact_id,provider,external_run_id,repository,immutable_revision,artifact_digest,base_model_identity,method,framework,framework_version,dataset_fingerprint,payload_digest,signature,public_key,algorithm,key_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, handoff.ID, tenant, handoff.DeploymentID, handoff.RevisionID, artifact.ID, handoff.Provider, handoff.ExternalRunID, handoff.Repository, handoff.ImmutableRevision, handoff.ArtifactDigest, handoff.BaseModelIdentity, handoff.Method, handoff.Framework, handoff.FrameworkVersion, handoff.DatasetFingerprint, handoff.PayloadDigest, handoff.Signature, handoff.PublicKey, handoff.Algorithm, handoff.KeyID, stamp); err != nil {
		if isUniqueViolation(err) {
			return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, fmt.Errorf("%w: revision already has a training handoff", ErrConflict)
		}
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.TrainingArtifactHandoff{}, domain.ModelArtifact{}, err
	}
	return handoff, artifact, nil
}

func prohibitedSandboxMetadataKey(value any) string {
	prohibited := map[string]struct{}{
		"command": {}, "commands": {}, "file": {}, "files": {}, "prompt": {}, "prompts": {},
		"output": {}, "outputs": {}, "credential": {}, "credentials": {}, "secret": {}, "secrets": {},
	}
	var inspect func(any) string
	inspect = func(candidate any) string {
		switch typed := candidate.(type) {
		case map[string]any:
			for key, nested := range typed {
				if _, found := prohibited[strings.ToLower(key)]; found {
					return key
				}
				if found := inspect(nested); found != "" {
					return found
				}
			}
		case []any:
			for _, nested := range typed {
				if found := inspect(nested); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return inspect(value)
}

func (s *Store) TrainingArtifactHandoffs(ctx context.Context, tenant, deploymentName string) ([]domain.TrainingArtifactHandoff, error) {
	rows, err := s.QueryContext(ctx, `SELECT h.id,h.tenant_id,h.deployment_id,h.revision_id,h.model_artifact_id,h.provider,h.external_run_id,h.repository,h.immutable_revision,h.artifact_digest,h.base_model_identity,h.method,h.framework,h.framework_version,h.dataset_fingerprint,h.payload_digest,h.signature,h.public_key,h.algorithm,h.key_id,h.created_at FROM training_artifact_handoffs h JOIN deployments d ON d.id=h.deployment_id WHERE h.tenant_id=? AND d.name=? ORDER BY h.created_at DESC,h.id DESC`, tenant, deploymentName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.TrainingArtifactHandoff, 0)
	for rows.Next() {
		var row domain.TrainingArtifactHandoff
		var created string
		if err = rows.Scan(&row.ID, &row.TenantID, &row.DeploymentID, &row.RevisionID, &row.ModelArtifactID, &row.Provider, &row.ExternalRunID, &row.Repository, &row.ImmutableRevision, &row.ArtifactDigest, &row.BaseModelIdentity, &row.Method, &row.Framework, &row.FrameworkVersion, &row.DatasetFingerprint, &row.PayloadDigest, &row.Signature, &row.PublicKey, &row.Algorithm, &row.KeyID, &created); err != nil {
			return nil, err
		}
		row.CreatedAt = parseTime(created)
		out = append(out, row)
	}
	return out, rows.Err()
}
