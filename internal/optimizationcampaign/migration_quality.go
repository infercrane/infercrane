package optimizationcampaign

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

// FailureMigrationQualityInconclusive means the paired comparison with the
// closed API could not decide: too few pairs, or an interval that straddles
// the margin. The candidate is not rejected and cannot be switched to.
const FailureMigrationQualityInconclusive = "quality_migration_inconclusive"

// ErrClosedAPICredentialUnreadable means the closed API's sealed credential
// is stored but cannot be opened. The check never asks unauthenticated.
var ErrClosedAPICredentialUnreadable = errors.New("the closed API's stored credential cannot be opened")

// MigrationQualityStore is the persistence of the migration check.
type MigrationQualityStore interface {
	QualityCheckStore
	ExternalBindingTarget(context.Context, string, string, string) (domain.ResolvedEndpoint, domain.BackendBinding, domain.Target, error)
	RecordBindingQualityEvidence(context.Context, string, string, string, domain.BindingQualityEvidence) (domain.BindingQualityEvidence, bool, error)
	BindingQualityEvidence(context.Context, string, string, int) ([]domain.BindingQualityEvidence, error)
	RecordMigrationVerdict(context.Context, string, string, domain.MigrationQualityVerdict) (domain.MigrationQualityVerdict, error)
	MigrationVerdictsForCampaign(context.Context, string, string) ([]domain.MigrationQualityVerdict, error)
}

// ClosedAPICredentials opens the bearer credential sealed for a closed API's
// target. It returns domain.ErrNotFound when the target has none.
type ClosedAPICredentials interface {
	Resolve(context.Context, string, string) (string, error)
	Unreadable(error) bool
}

// MigrationResult is a decided migration check: the signed verdict and the
// candidate's evidence row it rests on.
type MigrationResult struct {
	Verdict  domain.MigrationQualityVerdict
	Evidence domain.QualityEvidence
}

// MigrationChecker decides whether a candidate answers as well as the closed
// API a migration campaign replaces. customer is the customer's signed
// evidence for the candidate revision, or nil.
type MigrationChecker interface {
	CheckMigration(context.Context, domain.OptimizationCampaign, domain.OptimizationCandidateRun, *domain.QualityEvidence) (MigrationResult, error)
}

// MigrationQualityCheck compares a candidate with a closed API on a graded
// suite, or on the customer's own signed evaluations of both, and signs the
// verdict. The paired bootstrap decides; the 19 Nov guards stay in force: a
// failed request on either side stops the check instead of scoring zero, and
// an answer without text scores zero.
type MigrationQualityCheck struct {
	Store       MigrationQualityStore
	Backends    map[string]BenchmarkBackend
	Completer   qualitycheck.Completer
	Credentials ClosedAPICredentials
	Signer      ed25519.PrivateKey
	Now         func() time.Time
}

func (c MigrationQualityCheck) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c MigrationQualityCheck) CheckMigration(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, customer *domain.QualityEvidence) (MigrationResult, error) {
	migration := campaignRequest(campaign).Migration
	if migration == nil || c.Store == nil || len(c.Signer) != ed25519.PrivateKeySize {
		return MigrationResult{}, errors.New("migration quality check requires a migration campaign, a store, and a signing key")
	}
	resolved, binding, target, err := c.Store.ExternalBindingTarget(ctx, campaign.TenantID, migration.Endpoint, migration.Binding)
	if errors.Is(err, domain.ErrNotFound) {
		return MigrationResult{}, fmt.Errorf("%w: the closed API binding %s/%s no longer exists", ErrQualityReferenceUnavailable, migration.Endpoint, migration.Binding)
	}
	if err != nil {
		return MigrationResult{}, fmt.Errorf("resolve the closed API: %w", err)
	}
	if verdict, found, verdictErr := c.recordedVerdict(ctx, campaign, candidate, binding.ID, customer); verdictErr != nil || found {
		return verdict, verdictErr
	}
	if customer != nil || QualityWaitsForCustomer(campaign) {
		return c.customerVerdict(ctx, campaign, candidate, *migration, resolved, binding, customer)
	}
	return c.gradedVerdict(ctx, campaign, candidate, *migration, resolved, binding, target)
}

// recordedVerdict adopts a verdict already signed for this candidate
// revision, so a retry after a lost response does not ask again. A verdict
// from InferCrane's graded check is not adopted once customer evidence for
// the revision exists: the customer's evidence wins.
func (c MigrationQualityCheck) recordedVerdict(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, bindingID string, customer *domain.QualityEvidence) (MigrationResult, bool, error) {
	verdicts, err := c.Store.MigrationVerdictsForCampaign(ctx, campaign.TenantID, campaign.ID)
	if err != nil {
		return MigrationResult{}, false, fmt.Errorf("list migration verdicts: %w", err)
	}
	for _, verdict := range verdicts {
		if verdict.CandidateRunID != candidate.ID || verdict.RevisionID != candidate.RevisionID || verdict.BindingID != bindingID {
			continue
		}
		var payload qualityevidence.MigrationVerdict
		if json.Unmarshal([]byte(verdict.PayloadJSON), &payload) != nil {
			continue
		}
		if customer != nil && payload.CandidateEvidenceDigest != customer.PayloadDigest {
			continue
		}
		rows, err := c.Store.QualityEvidenceForDeployment(ctx, candidate.TenantID, candidate.DeploymentName, 100)
		if err != nil {
			return MigrationResult{}, false, fmt.Errorf("list candidate quality evidence: %w", err)
		}
		for _, row := range rows {
			if row.PayloadDigest == payload.CandidateEvidenceDigest {
				return MigrationResult{Verdict: verdict, Evidence: row}, true, nil
			}
		}
	}
	return MigrationResult{}, false, nil
}

func (c MigrationQualityCheck) gradedVerdict(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, migration optimizer.MigrationBaseline, resolved domain.ResolvedEndpoint, binding domain.BackendBinding, target domain.Target) (MigrationResult, error) {
	set, err := (BuiltinQualityCheck{Store: c.Store}).evalSet(ctx, campaign)
	if err != nil {
		return MigrationResult{}, err
	}
	if !set.Graded() {
		return MigrationResult{}, operations.Permanent("optimization_quality_eval_set_not_graded", errors.New("a migration needs a graded eval set with expected answers"))
	}
	if target.AgentID != "" {
		// The control plane never dials a server only an agent reaches.
		return MigrationResult{}, fmt.Errorf("%w: the closed API is reached through an agent", ErrQualityReferenceUnavailable)
	}
	credential := ""
	if c.Credentials != nil {
		credential, err = c.Credentials.Resolve(ctx, campaign.TenantID, target.ID)
		switch {
		case err == nil:
		case errors.Is(err, domain.ErrNotFound):
			credential = ""
		case c.Credentials.Unreadable(err):
			return MigrationResult{}, fmt.Errorf("%w: %w", ErrQualityReferenceUnavailable, ErrClosedAPICredentialUnreadable)
		default:
			return MigrationResult{}, fmt.Errorf("open the closed API credential: %w", err)
		}
	}
	draft, err := candidateDraft(candidate)
	if err != nil {
		return MigrationResult{}, err
	}
	deployment, err := c.Store.ResolveForTenant(ctx, candidate.TenantID, candidate.DeploymentName)
	if err != nil {
		return MigrationResult{}, fmt.Errorf("resolve candidate deployment: %w", err)
	}
	candidateArm, _, err := directReplica(ctx, c.Store, c.Backends, candidate, deployment.Deployment, draft)
	if err != nil {
		return MigrationResult{}, fmt.Errorf("candidate: %w", err)
	}
	deadline := c.now().Add(maxQualityCheckDuration)
	if campaign.ApprovalExpiresAt != nil && campaign.ApprovalExpiresAt.Before(deadline) {
		deadline = *campaign.ApprovalExpiresAt
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Until(deadline))
	defer cancel()
	closedAPI := qualitycheck.Target{Endpoint: target.URL, APIKey: credential, Model: target.UpstreamModel, SubjectID: qualitycheck.BindingSubject(binding.ID), Dialect: qualitycheck.Dialect(migration.Dialect)}
	comparison, err := qualitycheck.CompareGraded(checkCtx, c.Completer, set, closedAPI, candidateArm.qualityTarget(), c.Now)
	if err != nil {
		return MigrationResult{}, err
	}
	referencePayload := comparison.Payload(resolved.Endpoint.Name, true, true)
	candidatePayload := comparison.Payload(candidate.DeploymentName, false, true)
	verdict := qualitycheck.MigrationVerdict(referencePayload, candidatePayload, migration.MaxQualityRegressionPercent)
	candidatePayload.Passed = verdict.Verdict != releaseguard.QualityBlock
	referenceEnvelope, err := c.sign(referencePayload)
	if err != nil {
		return MigrationResult{}, err
	}
	if _, _, err = c.Store.RecordBindingQualityEvidence(ctx, campaign.TenantID, resolved.Endpoint.Name, binding.ID, bindingEvidence(referencePayload, referenceEnvelope, domain.QualityEvidenceSourceInferCrane)); err != nil {
		return MigrationResult{}, fmt.Errorf("record the closed API's quality evidence: %w", err)
	}
	evidence, err := (BuiltinQualityCheck{Store: c.Store, Signer: c.Signer}).record(ctx, candidate.TenantID, candidate.DeploymentName, candidatePayload, qualitycheck.BindingSubject(binding.ID))
	if err != nil {
		return MigrationResult{}, err
	}
	return c.recordVerdict(ctx, campaign, candidate, migration, resolved, binding, referenceEnvelope.Digest, evidence, verdict)
}

// customerVerdict compares the customer's signed evidence for the candidate
// revision with the customer's signed evidence for the closed API, from the
// same suite and evaluator.
func (c MigrationQualityCheck) customerVerdict(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, migration optimizer.MigrationBaseline, resolved domain.ResolvedEndpoint, binding domain.BackendBinding, customer *domain.QualityEvidence) (MigrationResult, error) {
	if customer == nil {
		return MigrationResult{}, ErrMigrationEvidencePending
	}
	candidatePayload, err := payloadFromEvidence(*customer)
	if err != nil {
		return MigrationResult{}, err
	}
	rows, err := c.Store.BindingQualityEvidence(ctx, campaign.TenantID, binding.ID, 100)
	if err != nil {
		return MigrationResult{}, fmt.Errorf("list the closed API's quality evidence: %w", err)
	}
	for _, row := range rows {
		if row.Source != domain.QualityEvidenceSourceCustomer || row.Suite != candidatePayload.Suite || row.SuiteVersion != candidatePayload.SuiteVersion || row.Evaluator != candidatePayload.Evaluator || row.EvaluatorVersion != candidatePayload.EvaluatorVersion {
			continue
		}
		envelope := passport.Envelope{PayloadJSON: row.PayloadJSON, Digest: row.PayloadDigest, Signature: row.Signature, PublicKey: row.PublicKey, Algorithm: row.Algorithm, KeyID: row.KeyID}
		referencePayload, decodeErr := qualityevidence.Decode(envelope)
		if decodeErr != nil {
			continue
		}
		verdict := qualitycheck.MigrationVerdict(referencePayload, candidatePayload, migration.MaxQualityRegressionPercent)
		return c.recordVerdict(ctx, campaign, candidate, migration, resolved, binding, row.PayloadDigest, *customer, verdict)
	}
	return MigrationResult{}, ErrMigrationEvidencePending
}

// ErrMigrationEvidencePending means the customer evidence a migration
// verdict needs, for the candidate or for the closed API, is not attached yet.
var ErrMigrationEvidencePending = errors.New("attach signed evaluations of the candidate revision and of the closed API (binding:<id>) from the same suite")

func (c MigrationQualityCheck) recordVerdict(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, migration optimizer.MigrationBaseline, resolved domain.ResolvedEndpoint, binding domain.BackendBinding, referenceDigest string, evidence domain.QualityEvidence, eval releaseguard.QualityEval) (MigrationResult, error) {
	payload := qualityevidence.MigrationVerdict{
		Schema: qualityevidence.MigrationVerdictSchema, CampaignID: campaign.ID, Endpoint: resolved.Endpoint.Name, BindingID: binding.ID,
		CandidateDeployment: candidate.DeploymentName, CandidateRevisionID: candidate.RevisionID,
		ReferenceEvidenceDigest: referenceDigest, CandidateEvidenceDigest: evidence.PayloadDigest,
		Suite: evidence.Suite, SuiteVersion: evidence.SuiteVersion, Evaluator: evidence.Evaluator, EvaluatorVersion: evidence.EvaluatorVersion,
		MarginPercent: migration.MaxQualityRegressionPercent, Verdict: eval.Verdict, Code: eval.Code,
		ReferenceScore: eval.ReferenceScore, CandidateScore: eval.CandidateScore, Bootstrap: eval.Bootstrap,
		EvaluatedAt: c.now().Truncate(time.Second),
	}
	envelope, err := passport.Sign(payload, c.Signer)
	if err != nil {
		return MigrationResult{}, err
	}
	if _, err = qualityevidence.DecodeMigrationVerdict(envelope); err != nil {
		return MigrationResult{}, err
	}
	stored, err := c.Store.RecordMigrationVerdict(ctx, campaign.TenantID, candidate.DeploymentName, domain.MigrationQualityVerdict{
		CampaignID: campaign.ID, CandidateRunID: candidate.ID, EndpointID: resolved.Endpoint.ID, BindingID: binding.ID, RevisionID: candidate.RevisionID,
		Verdict: eval.Verdict, Code: eval.Code, MarginPercent: migration.MaxQualityRegressionPercent,
		PayloadJSON: envelope.PayloadJSON, PayloadDigest: envelope.Digest, Signature: envelope.Signature, PublicKey: envelope.PublicKey, Algorithm: envelope.Algorithm, KeyID: envelope.KeyID, EvaluatedAt: payload.EvaluatedAt,
	})
	if err != nil {
		return MigrationResult{}, fmt.Errorf("record migration verdict: %w", err)
	}
	return MigrationResult{Verdict: stored, Evidence: evidence}, nil
}

func (c MigrationQualityCheck) sign(payload qualityevidence.Payload) (passport.Envelope, error) {
	envelope, err := passport.Sign(payload, c.Signer)
	if err != nil {
		return passport.Envelope{}, err
	}
	if _, err = qualityevidence.Decode(envelope); err != nil {
		return passport.Envelope{}, err
	}
	return envelope, nil
}

func bindingEvidence(payload qualityevidence.Payload, envelope passport.Envelope, source string) domain.BindingQualityEvidence {
	pairing := ""
	if payload.Distribution != nil {
		pairing = payload.Distribution.PairingDigest
	}
	return domain.BindingQualityEvidence{Source: source, Suite: payload.Suite, SuiteVersion: payload.SuiteVersion, Evaluator: payload.Evaluator, EvaluatorVersion: payload.EvaluatorVersion, Score: payload.Score, SampleCount: payload.SampleCount, PairingDigest: pairing, PayloadJSON: envelope.PayloadJSON, PayloadDigest: envelope.Digest, Signature: envelope.Signature, PublicKey: envelope.PublicKey, Algorithm: envelope.Algorithm, KeyID: envelope.KeyID, EvaluatedAt: payload.EvaluatedAt}
}

// payloadFromEvidence rebuilds the comparable fields of a stored revision
// evidence row, including its paired scores.
func payloadFromEvidence(row domain.QualityEvidence) (qualityevidence.Payload, error) {
	payload := qualityevidence.Payload{Schema: qualityevidence.Schema, RevisionID: row.RevisionID, Suite: row.Suite, SuiteVersion: row.SuiteVersion, Evaluator: row.Evaluator, EvaluatorVersion: row.EvaluatorVersion, Score: row.Score, Passed: row.Passed, SampleCount: row.SampleCount, ArtifactDigest: row.ArtifactDigest, EvaluatedAt: row.EvaluatedAt}
	if row.DistributionJSON != "" && row.DistributionJSON != "{}" {
		var distribution qualityevidence.Distribution
		if err := json.Unmarshal([]byte(row.DistributionJSON), &distribution); err != nil {
			return payload, fmt.Errorf("decode the candidate's paired scores: %w", err)
		}
		payload.Distribution = &distribution
	}
	return payload, nil
}
