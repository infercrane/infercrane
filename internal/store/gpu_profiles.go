package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/gpuprofile"
)

// RecordGPUProfile stores one capture against the deployment's active
// revision, resolved in the same transaction so a capture cannot attach to a
// later rollout. A retry of the same capture returns the original receipt; a
// retry that changes it is a conflict. Only the newest gpuprofile.Retained
// captures per deployment are kept.
func (s *Store) RecordGPUProfile(ctx context.Context, tenant, deploymentName string, recording gpuprofile.Recording) (gpuprofile.Receipt, error) {
	if tenant == "" {
		tenant = "global"
	}
	recording = gpuprofile.Normalize(recording)
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	if err := gpuprofile.Validate(recording, stamp); err != nil {
		return gpuprofile.Receipt{}, err
	}
	document, err := json.Marshal(recording)
	if err != nil {
		return gpuprofile.Receipt{}, err
	}
	digest := sha256.Sum256(document)
	documentSHA := hex.EncodeToString(digest[:])
	tx, err := s.beginTx(ctx)
	if err != nil {
		return gpuprofile.Receipt{}, err
	}
	defer tx.Rollback()
	receipt := gpuprofile.Receipt{StartedAt: recording.Capture.StartedAt, EndedAt: recording.Capture.EndedAt, Frames: len(recording.Frames), Stacks: len(recording.Stacks)}
	var deploymentID, revisionID string
	err = tx.QueryRowContext(ctx, `SELECT d.id,d.name,d.active_revision_id,r.revision_number FROM deployments d JOIN deployment_revisions r ON r.id=d.active_revision_id WHERE d.tenant_id=? AND (d.id=? OR d.name=?) AND d.desired_state<>'deleted' FOR SHARE OF d`, tenant, deploymentName, deploymentName).Scan(&deploymentID, &receipt.Deployment, &revisionID, &receipt.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return gpuprofile.Receipt{}, ErrNotFound
	}
	if err != nil {
		return gpuprofile.Receipt{}, err
	}
	identity := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s", tenant, deploymentID, revisionID, recording.Capture.Tool, recording.Capture.StartedAt.Format(time.RFC3339Nano), recording.Capture.EndedAt.Format(time.RFC3339Nano))))
	receipt.ID = "gpc-" + hex.EncodeToString(identity[:16])
	inserted, err := tx.ExecContext(ctx, `INSERT INTO gpu_profiles(id,tenant_id,deployment_id,revision_id,tool,evidence_class,started_at,ended_at,document,document_sha256,created_at) VALUES(?,?,?,?,?,?,?,?,?::jsonb,?,?) ON CONFLICT(tenant_id,deployment_id,revision_id,tool,started_at,ended_at) DO NOTHING`, receipt.ID, tenant, deploymentID, revisionID, recording.Capture.Tool, recording.Capture.EvidenceClass, recording.Capture.StartedAt, recording.Capture.EndedAt, string(document), documentSHA, stamp)
	if err != nil {
		return gpuprofile.Receipt{}, err
	}
	count, err := inserted.RowsAffected()
	if err != nil {
		return gpuprofile.Receipt{}, err
	}
	receipt.RecordedAt = stamp
	if count == 0 {
		var existingSHA string
		if err = tx.QueryRowContext(ctx, `SELECT document_sha256,created_at FROM gpu_profiles WHERE id=? AND tenant_id=?`, receipt.ID, tenant).Scan(&existingSHA, &receipt.RecordedAt); err != nil {
			return gpuprofile.Receipt{}, err
		}
		if existingSHA != documentSHA {
			return gpuprofile.Receipt{}, fmt.Errorf("%w: a retry of this GPU profile capture changed its samples", ErrConflict)
		}
		receipt.RecordedAt = receipt.RecordedAt.UTC()
	} else if _, err = tx.ExecContext(ctx, `DELETE FROM gpu_profiles WHERE tenant_id=? AND deployment_id=? AND id NOT IN (SELECT id FROM gpu_profiles WHERE tenant_id=? AND deployment_id=? ORDER BY ended_at DESC,created_at DESC,id LIMIT ?)`, tenant, deploymentID, tenant, deploymentID, gpuprofile.Retained); err != nil {
		return gpuprofile.Receipt{}, err
	}
	if err = tx.Commit(); err != nil {
		return gpuprofile.Receipt{}, err
	}
	return receipt, nil
}

// EndpointGPUProfile returns the newest capture of the active revision of any
// deployment the endpoint routes to. An older revision's capture describes
// code that no longer serves, so it is not returned. ErrNotFound means the
// endpoint does not exist; gpuprofile.ErrNotRecorded means it has no capture.
func (s *Store) EndpointGPUProfile(ctx context.Context, tenant, endpointName string) (gpuprofile.Profile, error) {
	if tenant == "" {
		tenant = "global"
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpointName)
	if err != nil {
		return gpuprofile.Profile{}, err
	}
	profile := gpuprofile.Profile{Endpoint: resolved.Endpoint.Name}
	var document []byte
	err = s.QueryRowContext(ctx, `SELECT p.id,d.name,r.revision_number,p.document,p.created_at FROM gpu_profiles p JOIN deployments d ON d.id=p.deployment_id AND d.tenant_id=p.tenant_id AND d.desired_state<>'deleted' AND d.active_revision_id=p.revision_id JOIN deployment_revisions r ON r.id=p.revision_id WHERE p.tenant_id=? AND p.deployment_id IN (SELECT deployment_id FROM backend_bindings WHERE tenant_id=? AND endpoint_id=? AND deployment_id IS NOT NULL) ORDER BY p.ended_at DESC,p.created_at DESC,p.id LIMIT 1`, tenant, tenant, resolved.Endpoint.ID).Scan(&profile.Capture.ID, &profile.Deployment, &profile.Revision, &document, &profile.RecordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return gpuprofile.Profile{}, gpuprofile.ErrNotRecorded
	}
	if err != nil {
		return gpuprofile.Profile{}, err
	}
	var recording gpuprofile.Recording
	if err = json.Unmarshal(document, &recording); err != nil {
		return gpuprofile.Profile{}, fmt.Errorf("stored GPU profile %s is unreadable: %w", profile.Capture.ID, err)
	}
	recording = gpuprofile.Normalize(recording)
	profile.SchemaVersion = recording.SchemaVersion
	profile.Capture.CaptureMetadata = recording.Capture
	profile.Timeline = recording.Timeline
	profile.Frames = recording.Frames
	profile.Stacks = recording.Stacks
	profile.UnavailableLayers = recording.UnavailableLayers
	profile.RecordedAt = profile.RecordedAt.UTC()
	return profile, nil
}
