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
// captures per deployment are kept, and captures that ended more than
// gpuprofile.MaxCaptureAge ago are removed.
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
	} else if _, err = tx.ExecContext(ctx, `DELETE FROM gpu_profiles WHERE tenant_id=? AND deployment_id=? AND (ended_at<? OR id NOT IN (SELECT id FROM gpu_profiles WHERE tenant_id=? AND deployment_id=? ORDER BY ended_at DESC,created_at DESC,id LIMIT ?))`, tenant, deploymentID, stamp.Add(-gpuprofile.MaxCaptureAge), tenant, deploymentID, gpuprofile.Retained); err != nil {
		return gpuprofile.Receipt{}, err
	}
	if err = tx.Commit(); err != nil {
		return gpuprofile.Receipt{}, err
	}
	return receipt, nil
}

// EndpointGPUProfile returns the newest capture of the active revision of any
// deployment the endpoint routes to. An older revision's capture describes
// code that no longer serves, and a capture older than
// gpuprofile.MaxCaptureAge has expired, so neither is returned. ErrNotFound means the
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
	err = s.QueryRowContext(ctx, `SELECT p.id,d.name,r.revision_number,p.document,p.created_at FROM gpu_profiles p JOIN deployments d ON d.id=p.deployment_id AND d.tenant_id=p.tenant_id AND d.desired_state<>'deleted' AND d.active_revision_id=p.revision_id JOIN deployment_revisions r ON r.id=p.revision_id WHERE p.tenant_id=? AND p.deployment_id IN (SELECT deployment_id FROM backend_bindings WHERE tenant_id=? AND endpoint_id=? AND deployment_id IS NOT NULL) AND p.ended_at>=? ORDER BY p.ended_at DESC,p.created_at DESC,p.id LIMIT 1`, tenant, tenant, resolved.Endpoint.ID, time.Now().UTC().Add(-gpuprofile.MaxCaptureAge)).Scan(&profile.Capture.ID, &profile.Deployment, &profile.Revision, &document, &profile.RecordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return gpuprofile.Profile{}, gpuprofile.ErrNotRecorded
	}
	if err != nil {
		return gpuprofile.Profile{}, err
	}
	return readStoredProfile(profile, document)
}

// readStoredProfile fills a profile's samples and analysis from its stored
// recording, keeping the identity already read.
func readStoredProfile(identity gpuprofile.Profile, document []byte) (gpuprofile.Profile, error) {
	var recording gpuprofile.Recording
	if err := json.Unmarshal(document, &recording); err != nil {
		return gpuprofile.Profile{}, fmt.Errorf("stored GPU profile %s is unreadable: %w", identity.Capture.ID, err)
	}
	profile := gpuprofile.ProfileFromRecording(recording)
	profile.Endpoint, profile.Deployment, profile.Revision = identity.Endpoint, identity.Deployment, identity.Revision
	profile.Capture.ID = identity.Capture.ID
	profile.RecordedAt = identity.RecordedAt.UTC()
	return profile, nil
}

const endpointProfileScope = `FROM gpu_profiles p JOIN deployments d ON d.id=p.deployment_id AND d.tenant_id=p.tenant_id AND d.desired_state<>'deleted' JOIN deployment_revisions r ON r.id=p.revision_id WHERE p.tenant_id=? AND p.deployment_id IN (SELECT deployment_id FROM backend_bindings WHERE tenant_id=? AND endpoint_id=? AND deployment_id IS NOT NULL) AND p.ended_at>=?`

// EndpointGPUProfiles lists every retained capture of every deployment the
// endpoint routes to, newest first, including captures of revisions that no
// longer serve, so a capture before a rollout can be compared with one
// after it. Active marks captures of a deployment's active revision.
func (s *Store) EndpointGPUProfiles(ctx context.Context, tenant, endpointName string) ([]gpuprofile.Summary, error) {
	if tenant == "" {
		tenant = "global"
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpointName)
	if err != nil {
		return nil, err
	}
	rows, err := s.QueryContext(ctx, `SELECT p.id,d.name,r.revision_number,d.active_revision_id=p.revision_id,p.document->>'schema_version',p.document->'capture',p.document#>'{analysis,verdict}',p.document#>'{analysis,utilization,busy_share}',p.created_at `+endpointProfileScope+` ORDER BY p.ended_at DESC,p.created_at DESC,p.id LIMIT 200`,
		tenant, tenant, resolved.Endpoint.ID, time.Now().UTC().Add(-gpuprofile.MaxCaptureAge))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	summaries := []gpuprofile.Summary{}
	for rows.Next() {
		var summary gpuprofile.Summary
		var capture, verdict, busy []byte
		if err = rows.Scan(&summary.ID, &summary.Deployment, &summary.Revision, &summary.Active, &summary.SchemaVersion, &capture, &verdict, &busy, &summary.RecordedAt); err != nil {
			return nil, err
		}
		var metadata gpuprofile.CaptureMetadata
		if err = json.Unmarshal(capture, &metadata); err != nil {
			return nil, fmt.Errorf("stored GPU profile %s is unreadable: %w", summary.ID, err)
		}
		summary.Tool, summary.ToolVersion, summary.Accelerator, summary.GPUs = metadata.Tool, metadata.ToolVersion, metadata.Accelerator, metadata.GPUs
		summary.StartedAt, summary.EndedAt, summary.RecordedAt = metadata.StartedAt.UTC(), metadata.EndedAt.UTC(), summary.RecordedAt.UTC()
		if len(verdict) > 0 && string(verdict) != "null" {
			var decoded gpuprofile.Verdict
			if err = json.Unmarshal(verdict, &decoded); err != nil {
				return nil, fmt.Errorf("stored GPU profile %s is unreadable: %w", summary.ID, err)
			}
			summary.Verdict = &decoded
		}
		if len(busy) > 0 && string(busy) != "null" {
			var share float64
			if err = json.Unmarshal(busy, &share); err != nil {
				return nil, fmt.Errorf("stored GPU profile %s is unreadable: %w", summary.ID, err)
			}
			summary.BusyShare = &share
		}
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}

// EndpointGPUProfileByID returns one retained capture of a deployment the
// endpoint routes to, whether or not its revision still serves, and whether
// it belongs to the active revision. A capture of another endpoint's
// deployment is gpuprofile.ErrNotRecorded.
func (s *Store) EndpointGPUProfileByID(ctx context.Context, tenant, endpointName, id string) (gpuprofile.Profile, bool, error) {
	if tenant == "" {
		tenant = "global"
	}
	resolved, err := s.ResolveEndpointForTenant(ctx, tenant, endpointName)
	if err != nil {
		return gpuprofile.Profile{}, false, err
	}
	identity := gpuprofile.Profile{Endpoint: resolved.Endpoint.Name}
	var active bool
	var document []byte
	err = s.QueryRowContext(ctx, `SELECT p.id,d.name,r.revision_number,d.active_revision_id=p.revision_id,p.document,p.created_at `+endpointProfileScope+` AND p.id=?`,
		tenant, tenant, resolved.Endpoint.ID, time.Now().UTC().Add(-gpuprofile.MaxCaptureAge), id).Scan(&identity.Capture.ID, &identity.Deployment, &identity.Revision, &active, &document, &identity.RecordedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return gpuprofile.Profile{}, false, gpuprofile.ErrNotRecorded
	}
	if err != nil {
		return gpuprofile.Profile{}, false, err
	}
	profile, err := readStoredProfile(identity, document)
	return profile, active, err
}
