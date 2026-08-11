package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/passport"
)

func (s *Store) InferencePassportPayload(ctx context.Context, tenant, deploymentName, revisionID string) (passport.Payload, error) {
	resolved, err := s.ResolveForTenant(ctx, tenant, deploymentName)
	if err != nil {
		return passport.Payload{}, err
	}
	if revisionID == "" {
		revisionID = resolved.Deployment.ActiveRevisionID
	}
	revision, err := s.Revision(ctx, tenant, deploymentName, revisionID)
	if err != nil {
		return passport.Payload{}, err
	}
	var spec domain.DeploymentRevisionSpec
	if err = json.Unmarshal([]byte(revision.SpecJSON), &spec); err != nil {
		return passport.Payload{}, err
	}
	payload := passport.Payload{Schema: "infercrane.inference-passport/v1", Deployment: deploymentName, RevisionID: revision.ID, RevisionNumber: revision.Number, RevisionSpec: spec, Benchmarks: []passport.Benchmark{}, MissingEvidence: []string{}, Reproduce: passport.Reproduction{DeploymentSpec: spec, BenchmarkCommands: []string{}}, IssuedAt: time.Now().UTC()}
	if artifact, artifactErr := s.ModelArtifactForRevision(ctx, tenant, revisionID); artifactErr == nil {
		payload.Artifact = &passport.Artifact{ID: artifact.ID, Source: artifact.Source, Repository: artifact.Repository, ImmutableRevision: artifact.ImmutableRevision, ModelIdentity: artifact.ModelIdentity, CacheState: artifact.CacheState, ApproximateSizeBytes: artifact.ApproximateSizeBytes}
	} else if !errors.Is(artifactErr, ErrNotFound) {
		return passport.Payload{}, artifactErr
	}
	benchmarks, err := s.BenchmarksForDeployment(ctx, tenant, deploymentName, 100)
	if err != nil {
		return passport.Payload{}, err
	}
	for _, row := range benchmarks {
		if row.RevisionID != revisionID {
			continue
		}
		payload.Benchmarks = append(payload.Benchmarks, passport.Benchmark{ID: row.ID, Tool: row.Tool, ToolVersion: row.ToolVersion, Runtime: row.Runtime, RuntimeVersion: row.RuntimeVersion, Provider: row.Provider, Region: row.Region, GPU: row.GPU, ComputeMode: row.ComputeMode, Workload: json.RawMessage(row.WorkloadJSON), ReproductionCommand: row.ReproductionCommand, RequestCount: row.RequestCount, Succeeded: row.Succeeded, Failed: row.Failed, TTFTP95MS: row.TTFTP95MS, LatencyP95MS: row.LatencyP95MS, OutputTokenThroughput: row.OutputTokenThroughput, CostMetadata: json.RawMessage(row.CostMetadataJSON), CreatedAt: row.CreatedAt})
		payload.Reproduce.BenchmarkCommands = append(payload.Reproduce.BenchmarkCommands, row.ReproductionCommand)
	}
	payload.ColdStart, err = s.ColdStartStats(ctx, resolved.Deployment.ID, 24*time.Hour)
	if err != nil {
		return passport.Payload{}, err
	}
	var evidence domain.ReleaseGuardEvaluation
	var created string
	err = s.QueryRowContext(ctx, `SELECT id,deployment_id,active_revision_id,candidate_revision_id,decision,reason_codes_json::text,metrics_json::text,policy_json::text,created_at FROM release_guard_evaluations WHERE deployment_id=? AND (active_revision_id=? OR candidate_revision_id=?) ORDER BY created_at DESC LIMIT 1`, resolved.Deployment.ID, revisionID, revisionID).Scan(&evidence.ID, &evidence.DeploymentID, &evidence.ActiveRevisionID, &evidence.CandidateRevisionID, &evidence.Decision, &evidence.ReasonCodesJSON, &evidence.MetricsJSON, &evidence.PolicyJSON, &created)
	if err == nil {
		evidence.CreatedAt = parseTime(created)
		payload.GuardEvaluation = &passport.GuardEvidence{ID: evidence.ID, ActiveRevisionID: evidence.ActiveRevisionID, CandidateRevisionID: evidence.CandidateRevisionID, Decision: evidence.Decision, Reasons: json.RawMessage(evidence.ReasonCodesJSON), Metrics: json.RawMessage(evidence.MetricsJSON), Policy: json.RawMessage(evidence.PolicyJSON), CreatedAt: evidence.CreatedAt}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return passport.Payload{}, err
	}
	return payload, nil
}

func (s *Store) RecordInferencePassport(ctx context.Context, value domain.InferencePassport) (domain.InferencePassport, error) {
	if value.TenantID == "" || value.DeploymentID == "" || value.RevisionID == "" || !json.Valid([]byte(value.PayloadJSON)) || len(value.PayloadJSON) > 4<<20 || value.PayloadDigest == "" || value.Signature == "" || value.PublicKey == "" || value.Algorithm != passport.Algorithm || value.KeyID == "" {
		return domain.InferencePassport{}, errors.New("invalid inference passport")
	}
	if value.ID == "" {
		var err error
		value.ID, err = newID()
		if err != nil {
			return value, err
		}
	}
	stamp := now()
	_, err := s.ExecContext(ctx, `INSERT INTO inference_passports(id,tenant_id,deployment_id,revision_id,payload_json,payload_digest,signature,public_key,algorithm,key_id,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,deployment_id,revision_id,payload_digest,key_id) DO NOTHING`, value.ID, value.TenantID, value.DeploymentID, value.RevisionID, value.PayloadJSON, value.PayloadDigest, value.Signature, value.PublicKey, value.Algorithm, value.KeyID, stamp)
	if err != nil {
		return domain.InferencePassport{}, err
	}
	var created string
	err = s.QueryRowContext(ctx, `SELECT id,created_at FROM inference_passports WHERE tenant_id=? AND deployment_id=? AND revision_id=? AND payload_digest=? AND key_id=?`, value.TenantID, value.DeploymentID, value.RevisionID, value.PayloadDigest, value.KeyID).Scan(&value.ID, &created)
	value.CreatedAt = parseTime(created)
	return value, err
}

func (s *Store) InferencePassports(ctx context.Context, tenant, deploymentName string, limit int) ([]domain.InferencePassport, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.QueryContext(ctx, `SELECT p.id,p.tenant_id,p.deployment_id,p.revision_id,p.payload_json,p.payload_digest,p.signature,p.public_key,p.algorithm,p.key_id,p.created_at FROM inference_passports p JOIN deployments d ON d.id=p.deployment_id WHERE p.tenant_id=? AND d.name=? ORDER BY p.created_at DESC,p.id DESC LIMIT ?`, tenant, deploymentName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []domain.InferencePassport
	for rows.Next() {
		var value domain.InferencePassport
		var created string
		if err = rows.Scan(&value.ID, &value.TenantID, &value.DeploymentID, &value.RevisionID, &value.PayloadJSON, &value.PayloadDigest, &value.Signature, &value.PublicKey, &value.Algorithm, &value.KeyID, &created); err != nil {
			return nil, err
		}
		value.CreatedAt = parseTime(created)
		values = append(values, value)
	}
	return values, rows.Err()
}
