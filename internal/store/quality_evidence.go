package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/releaseguard"
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

// qualityEvals pairs the newest candidate evidence for every eval (suite and
// evaluator) with the newest reference evidence from the same suite and
// evaluator versions. Rows arrive newest first, so evals are ordered by the
// candidate's newest evidence. An eval without comparable reference evidence
// keeps a nil Reference and stays unknown.
func qualityEvals(rows []domain.QualityEvidence, referenceRevisionID, candidateRevisionID string) []domain.QualityEvalEvidence {
	type evalKey struct{ suite, evaluator string }
	seen := map[evalKey]bool{}
	evals := make([]domain.QualityEvalEvidence, 0)
	for index := range rows {
		row := &rows[index]
		key := evalKey{row.Suite, row.Evaluator}
		if candidateRevisionID == "" || row.RevisionID != candidateRevisionID || seen[key] {
			continue
		}
		seen[key] = true
		eval := domain.QualityEvalEvidence{Suite: row.Suite, SuiteVersion: row.SuiteVersion, Evaluator: row.Evaluator, EvaluatorVersion: row.EvaluatorVersion, Candidate: qualityEvalSide(row)}
		for referenceIndex := range rows {
			reference := &rows[referenceIndex]
			if referenceRevisionID != "" && reference.RevisionID == referenceRevisionID && reference.Suite == row.Suite && reference.SuiteVersion == row.SuiteVersion && reference.Evaluator == row.Evaluator && reference.EvaluatorVersion == row.EvaluatorVersion {
				side := qualityEvalSide(reference)
				eval.Reference = &side
				break
			}
		}
		evals = append(evals, eval)
	}
	return evals
}

func qualityEvalSide(row *domain.QualityEvidence) domain.QualityEvalSide {
	side := domain.QualityEvalSide{EvidenceID: row.ID, Score: row.Score, Passed: row.Passed, SampleCount: row.SampleCount}
	var distribution struct {
		PairingDigest string    `json:"pairing_digest"`
		Scores        []float64 `json:"scores"`
	}
	if json.Unmarshal([]byte(row.DistributionJSON), &distribution) == nil && distribution.PairingDigest != "" && len(distribution.Scores) == row.SampleCount {
		side.PairingDigest, side.Scores = distribution.PairingDigest, distribution.Scores
	}
	return side
}

// applyQualityEvidence copies the newest candidate eval into the summary
// fields that rollout inspect and stored evaluations display. Release Guard
// gates on every eval, never on this summary.
func applyQualityEvidence(active, candidate *domain.RevisionMetrics, evals []domain.QualityEvalEvidence) {
	if len(evals) == 0 {
		return
	}
	newest := evals[0]
	comparable := newest.Reference != nil
	setQualitySummary(candidate, newest, newest.Candidate, comparable)
	if newest.Reference != nil {
		setQualitySummary(active, newest, *newest.Reference, comparable)
	}
}

func setQualitySummary(metrics *domain.RevisionMetrics, eval domain.QualityEvalEvidence, side domain.QualityEvalSide, comparable bool) {
	score, passed := side.Score, side.Passed
	metrics.QualityScore, metrics.QualityPassed, metrics.QualityComparable = &score, &passed, &comparable
	metrics.QualityEvidenceID, metrics.QualitySuite, metrics.QualitySampleCount = side.EvidenceID, eval.Suite+"@"+eval.SuiteVersion, side.SampleCount
	metrics.QualityPairingDigest, metrics.QualityScores = side.PairingDigest, side.Scores
}

// Quality verdict view states for the quality evidence API.
const (
	QualityVerdictsCompared    = "compared"
	QualityVerdictsNoCandidate = "no_candidate"
	QualityVerdictsNotGated    = "not_gated"
)

// QualityVerdicts classifies the current candidate's evals against the active
// revision under the persisted Release Guard policy, using the same function
// Release Guard uses. It reads persisted state only and records nothing; the
// decision of record stays the stored Release Guard evaluation.
func (s *Store) QualityVerdicts(ctx context.Context, tenant, deploymentName string) (string, *releaseguard.QualityReport, error) {
	if tenant == "" {
		tenant = "global"
	}
	resolved, err := s.ResolveForTenant(ctx, tenant, deploymentName)
	if err != nil {
		return "", nil, err
	}
	policy, err := s.ReleaseGuardPolicy(ctx, tenant, deploymentName)
	if err != nil {
		return "", nil, err
	}
	if !releaseguard.QualityGated(policy) {
		return QualityVerdictsNotGated, nil, nil
	}
	if resolved.Deployment.CandidateRevisionID == "" {
		return QualityVerdictsNoCandidate, nil, nil
	}
	rows, err := s.QualityEvidenceForDeployment(ctx, tenant, deploymentName, 100)
	if err != nil {
		return "", nil, err
	}
	report := releaseguard.CompareQuality(policy, qualityEvals(rows, resolved.Deployment.ActiveRevisionID, resolved.Deployment.CandidateRevisionID))
	if err = s.describeQualitySubjects(ctx, &report, resolved.Deployment.ActiveRevisionID, resolved.Deployment.CandidateRevisionID); err != nil {
		return "", nil, err
	}
	return QualityVerdictsCompared, &report, nil
}

// describeQualitySubjects names the reference and candidate revisions and the
// model each one serves, so verdicts read per model.
func (s *Store) describeQualitySubjects(ctx context.Context, report *releaseguard.QualityReport, referenceRevisionID, candidateRevisionID string) error {
	for _, subject := range []struct {
		target     *releaseguard.QualitySubject
		revisionID string
	}{{&report.Reference, referenceRevisionID}, {&report.Candidate, candidateRevisionID}} {
		model, err := s.revisionModel(ctx, subject.revisionID)
		if err != nil {
			return err
		}
		subject.target.RevisionID, subject.target.Model = subject.revisionID, model
	}
	return nil
}

// revisionModel prefers the resolved immutable model identity and falls back
// to the model the revision spec requested. It returns "" when neither exists.
func (s *Store) revisionModel(ctx context.Context, revisionID string) (string, error) {
	if revisionID == "" {
		return "", nil
	}
	var identity, requested string
	err := s.QueryRowContext(ctx, `SELECT COALESCE(a.model_identity,''),COALESCE(r.spec_json->>'model','') FROM deployment_revisions r LEFT JOIN model_artifacts a ON a.id=r.model_artifact_id WHERE r.id=?`, revisionID).Scan(&identity, &requested)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if identity != "" {
		return identity, nil
	}
	return requested, nil
}
