package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

func (s *Store) RecordQualityEvidence(ctx context.Context, tenant, deploymentName string, evidence domain.QualityEvidence) (domain.QualityEvidence, bool, error) {
	if tenant == "" {
		tenant = "global"
	}
	resolved, err := s.ResolveForTenant(ctx, tenant, deploymentName)
	if err != nil {
		return domain.QualityEvidence{}, false, err
	}
	if evidence.RevisionID == "" {
		return domain.QualityEvidence{}, false, errors.New("quality evidence revision is required")
	}
	var revisionID string
	err = s.QueryRowContext(ctx, `SELECT r.id FROM deployment_revisions r JOIN deployments d ON d.id=r.deployment_id WHERE d.tenant_id=? AND d.id=? AND r.id=?`, tenant, resolved.Deployment.ID, evidence.RevisionID).Scan(&revisionID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.QualityEvidence{}, false, fmt.Errorf("%w: quality evidence revision does not belong to deployment", ErrNotFound)
	}
	if err != nil {
		return domain.QualityEvidence{}, false, err
	}
	if evidence.ID == "" {
		evidence.ID, err = newID()
		if err != nil {
			return domain.QualityEvidence{}, false, err
		}
	}
	evidence.TenantID, evidence.DeploymentID = tenant, resolved.Deployment.ID
	if evidence.CreatedAt.IsZero() {
		evidence.CreatedAt = time.Now().UTC()
	}
	result, err := s.ExecContext(ctx, `INSERT INTO revision_quality_evidence(id,tenant_id,deployment_id,revision_id,suite,suite_version,evaluator,evaluator_version,score,passed,sample_count,distribution_json,artifact_digest,payload_digest,signature,public_key,algorithm,key_id,evaluated_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?::jsonb,?,?,?,?,?,?,?,?) ON CONFLICT(tenant_id,payload_digest) DO NOTHING`, evidence.ID, evidence.TenantID, evidence.DeploymentID, evidence.RevisionID, evidence.Suite, evidence.SuiteVersion, evidence.Evaluator, evidence.EvaluatorVersion, evidence.Score, evidence.Passed, evidence.SampleCount, nullJSON(evidence.DistributionJSON), evidence.ArtifactDigest, evidence.PayloadDigest, evidence.Signature, evidence.PublicKey, evidence.Algorithm, evidence.KeyID, evidence.EvaluatedAt.Format(time.RFC3339Nano), evidence.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return domain.QualityEvidence{}, false, err
	}
	affected, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return domain.QualityEvidence{}, false, affectedErr
	}
	created := affected == 1
	if !created {
		var existing domain.QualityEvidence
		existing, err = s.qualityEvidenceByDigest(ctx, tenant, evidence.PayloadDigest)
		if err != nil {
			return domain.QualityEvidence{}, false, err
		}
		if existing.DeploymentID != evidence.DeploymentID || existing.RevisionID != evidence.RevisionID || existing.Signature != evidence.Signature || existing.PublicKey != evidence.PublicKey {
			return domain.QualityEvidence{}, false, fmt.Errorf("%w: quality evidence digest already identifies another immutable record", ErrConflict)
		}
		return existing, false, nil
	}
	return evidence, true, nil
}

func (s *Store) QualityEvidenceForDeployment(ctx context.Context, tenant, deploymentName string, limit int) ([]domain.QualityEvidence, error) {
	if tenant == "" {
		tenant = "global"
	}
	if limit < 1 || limit > 100 {
		limit = 20
	}
	rows, err := s.QueryContext(ctx, `SELECT q.id,q.tenant_id,q.deployment_id,q.revision_id,q.suite,q.suite_version,q.evaluator,q.evaluator_version,q.score,q.passed,q.sample_count,q.distribution_json::text,q.artifact_digest,q.payload_digest,q.signature,q.public_key,q.algorithm,q.key_id,q.evaluated_at,q.created_at FROM revision_quality_evidence q JOIN deployments d ON d.id=q.deployment_id WHERE d.tenant_id=? AND d.name=? ORDER BY q.evaluated_at DESC,q.id DESC LIMIT ?`, tenant, deploymentName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]domain.QualityEvidence, 0)
	for rows.Next() {
		item, scanErr := scanQualityEvidence(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) qualityEvidenceByDigest(ctx context.Context, tenant, digest string) (domain.QualityEvidence, error) {
	row := s.QueryRowContext(ctx, `SELECT id,tenant_id,deployment_id,revision_id,suite,suite_version,evaluator,evaluator_version,score,passed,sample_count,distribution_json::text,artifact_digest,payload_digest,signature,public_key,algorithm,key_id,evaluated_at,created_at FROM revision_quality_evidence WHERE tenant_id=? AND payload_digest=?`, tenant, digest)
	item, err := scanQualityEvidence(row)
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrNotFound
	}
	return item, err
}

func scanQualityEvidence(row rowScanner) (domain.QualityEvidence, error) {
	var item domain.QualityEvidence
	var evaluatedAt, createdAt string
	err := row.Scan(&item.ID, &item.TenantID, &item.DeploymentID, &item.RevisionID, &item.Suite, &item.SuiteVersion, &item.Evaluator, &item.EvaluatorVersion, &item.Score, &item.Passed, &item.SampleCount, &item.DistributionJSON, &item.ArtifactDigest, &item.PayloadDigest, &item.Signature, &item.PublicKey, &item.Algorithm, &item.KeyID, &evaluatedAt, &createdAt)
	item.EvaluatedAt, item.CreatedAt = parseTime(evaluatedAt), parseTime(createdAt)
	return item, err
}

func applyQualityEvidence(active, candidate *domain.RevisionMetrics, rows []domain.QualityEvidence, activeRevisionID, candidateRevisionID string) {
	var candidateEvidence *domain.QualityEvidence
	for index := range rows {
		if rows[index].RevisionID == candidateRevisionID {
			candidateEvidence = &rows[index]
			break
		}
	}
	if candidateEvidence == nil {
		return
	}
	for index := range rows {
		row := &rows[index]
		if row.RevisionID == activeRevisionID && row.Suite == candidateEvidence.Suite && row.SuiteVersion == candidateEvidence.SuiteVersion && row.Evaluator == candidateEvidence.Evaluator && row.EvaluatorVersion == candidateEvidence.EvaluatorVersion {
			comparable := true
			active.QualityScore, active.QualityPassed, active.QualityComparable = &row.Score, &row.Passed, &comparable
			active.QualityEvidenceID, active.QualitySuite = row.ID, row.Suite+"@"+row.SuiteVersion
			candidate.QualityScore, candidate.QualityPassed, candidate.QualityComparable = &candidateEvidence.Score, &candidateEvidence.Passed, &comparable
			candidate.QualityEvidenceID, candidate.QualitySuite = candidateEvidence.ID, candidateEvidence.Suite+"@"+candidateEvidence.SuiteVersion
			applyQualityDistribution(active, row)
			applyQualityDistribution(candidate, candidateEvidence)
			return
		}
	}
	comparable := false
	candidate.QualityScore, candidate.QualityPassed, candidate.QualityComparable = &candidateEvidence.Score, &candidateEvidence.Passed, &comparable
	candidate.QualityEvidenceID, candidate.QualitySuite = candidateEvidence.ID, candidateEvidence.Suite+"@"+candidateEvidence.SuiteVersion
	applyQualityDistribution(candidate, candidateEvidence)
}

func applyQualityDistribution(metrics *domain.RevisionMetrics, evidence *domain.QualityEvidence) {
	metrics.QualitySampleCount = evidence.SampleCount
	var distribution struct {
		PairingDigest string    `json:"pairing_digest"`
		Scores        []float64 `json:"scores"`
	}
	if json.Unmarshal([]byte(evidence.DistributionJSON), &distribution) == nil && distribution.PairingDigest != "" && len(distribution.Scores) == evidence.SampleCount {
		metrics.QualityPairingDigest = distribution.PairingDigest
		metrics.QualityScores = distribution.Scores
	}
}
