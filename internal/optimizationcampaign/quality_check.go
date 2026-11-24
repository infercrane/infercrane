package optimizationcampaign

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
)

// Failure codes and retry codes of the quality step. The API reads the same
// persisted state to say which one a candidate is in.
const (
	FailureQualityReferenceUnavailable = "quality_reference_unavailable"
	FailureQualityReferenceUnanswered  = "quality_reference_unanswered"
	FailureQualityRejected             = "quality_gate_rejected"
	retryQualityEvidencePending        = "optimization_quality_evidence_pending"
	retryQualityEvalSetPending         = "optimization_quality_eval_set_pending"
	retryQualityCheckFailed            = "optimization_quality_check_failed"
	maxQualityCheckDuration            = 30 * time.Minute
)

var (
	// ErrQualityEvalSetMissing means the campaign names a customer eval set
	// that has not been uploaded. The candidate waits for it.
	ErrQualityEvalSetMissing = errors.New("the campaign's eval set has not been uploaded")
	// ErrQualityReferenceUnavailable means no serving revision can supply
	// reference answers, so the candidate's answers cannot be compared.
	ErrQualityReferenceUnavailable = errors.New("no serving reference revision for the answer check")
	// ErrQualityReferenceUnanswered means the reference gave no answer text
	// to some prompts, so there is nothing to compare the candidate with.
	ErrQualityReferenceUnanswered = qualitycheck.ErrReferenceUnanswered
)

// QualityChecker produces InferCrane-run quality evidence for one candidate.
type QualityChecker interface {
	Check(context.Context, domain.OptimizationCampaign, domain.OptimizationCandidateRun) (domain.QualityEvidence, error)
}

// QualityCheckStore is the persistence boundary of the built-in check.
type QualityCheckStore interface {
	ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error)
	ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error)
	QualityEvalSet(context.Context, string, string) (domain.QualityEvalSet, error)
	QualityEvidenceForDeployment(context.Context, string, string, int) ([]domain.QualityEvidence, error)
	RecordQualityEvidence(context.Context, string, string, domain.QualityEvidence) (domain.QualityEvidence, bool, error)
}

// BuiltinQualityCheck asks the candidate and a reference revision the same
// prompts with greedy decoding, scores agreement, and records the result as
// evidence signed with the control plane's key and labelled as InferCrane-run.
//
// The reference is the campaign control. For an evolve-endpoint campaign it is
// the target's active revision when that revision is directly reachable,
// because Release Guard compares the candidate with the active revision. The
// reference's own evidence records score 1: its answers are the reference.
// Answers are compared in memory and discarded.
type BuiltinQualityCheck struct {
	Store     QualityCheckStore
	Backends  map[string]BenchmarkBackend
	Completer qualitycheck.Completer
	Signer    ed25519.PrivateKey
	Now       func() time.Time
}

type qualityArm struct {
	target     armTarget
	deployment string
}

func (c BuiltinQualityCheck) Check(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) (domain.QualityEvidence, error) {
	if c.Store == nil || c.Completer == nil || len(c.Signer) != ed25519.PrivateKeySize {
		return domain.QualityEvidence{}, errors.New("built-in quality check requires a store, a completer, and a signing key")
	}
	set, err := c.evalSet(ctx, campaign)
	if err != nil {
		return domain.QualityEvidence{}, err
	}
	reference, err := c.reference(ctx, campaign, candidate)
	if err != nil {
		return domain.QualityEvidence{}, err
	}
	existing, err := c.Store.QualityEvidenceForDeployment(ctx, candidate.TenantID, candidate.DeploymentName, 100)
	if err != nil {
		return domain.QualityEvidence{}, fmt.Errorf("list candidate quality evidence: %w", err)
	}
	// A recorded check for the same revision, eval set, and reference is
	// adopted, so a retry after a lost response does not ask again.
	if row := newestEvidence(existing, candidate.RevisionID, func(row domain.QualityEvidence) bool {
		return builtinEvidence(row, set) && row.ReferenceRevisionID == reference.target.revisionID
	}); row != nil {
		return *row, nil
	}
	draft, err := candidateDraft(candidate)
	if err != nil {
		return domain.QualityEvidence{}, err
	}
	resolved, err := c.Store.ResolveForTenant(ctx, candidate.TenantID, candidate.DeploymentName)
	if err != nil {
		return domain.QualityEvidence{}, fmt.Errorf("resolve candidate deployment: %w", err)
	}
	target, _, err := directReplica(ctx, c.Store, c.Backends, candidate, resolved.Deployment, draft)
	if err != nil {
		return domain.QualityEvidence{}, fmt.Errorf("candidate: %w", err)
	}
	deadline := c.now().Add(maxQualityCheckDuration)
	if campaign.ApprovalExpiresAt != nil && campaign.ApprovalExpiresAt.Before(deadline) {
		deadline = *campaign.ApprovalExpiresAt
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Until(deadline))
	defer cancel()
	comparison, err := qualitycheck.Compare(checkCtx, c.Completer, set, reference.target.qualityTarget(), target.qualityTarget(), c.now)
	if err != nil {
		return domain.QualityEvidence{}, err
	}
	if reference.target.revisionID != candidate.RevisionID {
		if err = c.recordReference(ctx, candidate.TenantID, reference.deployment, set, comparison); err != nil {
			return domain.QualityEvidence{}, err
		}
	}
	return c.record(ctx, candidate.TenantID, candidate.DeploymentName, comparison.Payload(candidate.DeploymentName, false), comparison.ReferenceID)
}

// recordReference keeps one InferCrane-run reference row per reference
// revision and eval set, so Release Guard and reports have comparable
// evidence for both sides.
func (c BuiltinQualityCheck) recordReference(ctx context.Context, tenant, deployment string, set qualitycheck.EvalSet, comparison qualitycheck.Comparison) error {
	rows, err := c.Store.QualityEvidenceForDeployment(ctx, tenant, deployment, 100)
	if err != nil {
		return fmt.Errorf("list reference quality evidence: %w", err)
	}
	if newestEvidence(rows, comparison.ReferenceID, func(row domain.QualityEvidence) bool { return builtinEvidence(row, set) }) != nil {
		return nil
	}
	_, err = c.record(ctx, tenant, deployment, comparison.Payload(deployment, true), comparison.ReferenceID)
	return err
}

func (c BuiltinQualityCheck) record(ctx context.Context, tenant, deployment string, payload qualityevidence.Payload, referenceRevisionID string) (domain.QualityEvidence, error) {
	envelope, err := passport.Sign(payload, c.Signer)
	if err != nil {
		return domain.QualityEvidence{}, err
	}
	// Decode verifies the signature and the payload exactly as it does for
	// customer evidence.
	verified, err := qualityevidence.Decode(envelope)
	if err != nil {
		return domain.QualityEvidence{}, err
	}
	row, err := qualityevidence.Record(verified, envelope)
	if err != nil {
		return domain.QualityEvidence{}, err
	}
	row.Source, row.ReferenceRevisionID = domain.QualityEvidenceSourceInferCrane, referenceRevisionID
	row, _, err = c.Store.RecordQualityEvidence(ctx, tenant, deployment, row)
	if err != nil {
		return domain.QualityEvidence{}, fmt.Errorf("record built-in quality evidence: %w", err)
	}
	return row, nil
}

// evalSet returns the customer eval set the campaign names, or the built-in
// default suite.
func (c BuiltinQualityCheck) evalSet(ctx context.Context, campaign domain.OptimizationCampaign) (qualitycheck.EvalSet, error) {
	digest := campaignRequest(campaign).QualityEvalSet
	if digest == "" {
		return qualitycheck.Default(), nil
	}
	stored, err := c.Store.QualityEvalSet(ctx, campaign.TenantID, digest)
	if errors.Is(err, domain.ErrNotFound) {
		return qualitycheck.EvalSet{}, fmt.Errorf("%w: upload %s", ErrQualityEvalSetMissing, digest)
	}
	if err != nil {
		return qualitycheck.EvalSet{}, fmt.Errorf("load eval set: %w", err)
	}
	set, err := qualitycheck.DecodeEvalSet([]byte(stored.Body))
	if err != nil || set.Digest != digest {
		return qualitycheck.EvalSet{}, fmt.Errorf("stored eval set %s does not match its digest", digest)
	}
	return set, nil
}

func (c BuiltinQualityCheck) reference(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) (qualityArm, error) {
	if campaign.Intent == IntentEvolveEndpoint && campaign.TargetDeployment != "" {
		resolved, err := c.Store.ResolveForTenant(ctx, campaign.TenantID, campaign.TargetDeployment)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return qualityArm{}, fmt.Errorf("resolve target deployment: %w", err)
		}
		if err == nil && resolved.Deployment.ActiveRevisionID != "" && resolved.Deployment.ActiveRevisionID != candidate.RevisionID {
			active := domain.OptimizationCandidateRun{ID: candidate.ID, TenantID: campaign.TenantID, RevisionID: resolved.Deployment.ActiveRevisionID}
			target, _, targetErr := directReplica(ctx, c.Store, c.Backends, active, resolved.Deployment, activeDraft(candidate, resolved.Deployment.Model))
			if targetErr == nil {
				return qualityArm{target: target, deployment: campaign.TargetDeployment}, nil
			}
			if !errors.Is(targetErr, errNoDirectEndpoint) {
				return qualityArm{}, targetErr
			}
		}
	}
	control, found := CampaignControl(campaign)
	if !found {
		return qualityArm{}, fmt.Errorf("%w: the campaign has no control", ErrQualityReferenceUnavailable)
	}
	if control.ID == candidate.ID {
		control = candidate
	} else if control.DeploymentName == "" || control.RevisionID == "" || !liveControlState(control.State) {
		return qualityArm{}, fmt.Errorf("%w: the control is not serving (state %s)", ErrQualityReferenceUnavailable, control.State)
	}
	draft, err := candidateDraft(control)
	if err != nil {
		return qualityArm{}, fmt.Errorf("%w: the control spec is invalid", ErrQualityReferenceUnavailable)
	}
	resolved, err := c.Store.ResolveForTenant(ctx, control.TenantID, control.DeploymentName)
	if errors.Is(err, domain.ErrNotFound) {
		return qualityArm{}, fmt.Errorf("%w: the control deployment no longer exists", ErrQualityReferenceUnavailable)
	}
	if err != nil {
		return qualityArm{}, fmt.Errorf("resolve control deployment: %w", err)
	}
	target, _, err := directReplica(ctx, c.Store, c.Backends, control, resolved.Deployment, draft)
	if err != nil {
		return qualityArm{}, fmt.Errorf("control: %w", err)
	}
	return qualityArm{target: target, deployment: control.DeploymentName}, nil
}

// activeDraft names the model the active revision serves. Replicas of one
// deployment serve the same alias, so the candidate's model ID is used when
// the deployment does not record one.
func activeDraft(candidate domain.OptimizationCandidateRun, model string) optimizer.DeploymentDraft {
	draft, _ := candidateDraft(candidate)
	if model != "" {
		draft.Model.ID = model
	}
	return draft
}

func (c BuiltinQualityCheck) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (t armTarget) qualityTarget() qualitycheck.Target {
	return qualitycheck.Target{Endpoint: t.endpoint, APIKey: t.apiKey, Model: t.model, SubjectID: t.revisionID}
}

func builtinEvidence(row domain.QualityEvidence, set qualitycheck.EvalSet) bool {
	return row.Source == domain.QualityEvidenceSourceInferCrane && row.Evaluator == qualitycheck.Evaluator && row.EvaluatorVersion == qualitycheck.EvaluatorVersion && row.Suite == set.Name && row.SuiteVersion == set.SuiteVersion()
}

// newestEvidence returns the newest row for revision that matches keep.
func newestEvidence(rows []domain.QualityEvidence, revision string, keep func(domain.QualityEvidence) bool) *domain.QualityEvidence {
	var selected *domain.QualityEvidence
	for index := range rows {
		row := &rows[index]
		if row.RevisionID != revision || !keep(*row) || selected != nil && !row.EvaluatedAt.After(selected.EvaluatedAt) {
			continue
		}
		selected = row
	}
	return selected
}

// QualityWaitsForCustomer reports whether the campaign asked for customer
// evidence instead of the built-in check.
func QualityWaitsForCustomer(campaign domain.OptimizationCampaign) bool {
	return campaignRequest(campaign).QualityCheck == optimizer.QualityCheckCustomer
}

// QualityEvalSetDigest returns the customer eval set the campaign names, or ""
// for the built-in default suite.
func QualityEvalSetDigest(campaign domain.OptimizationCampaign) string {
	return campaignRequest(campaign).QualityEvalSet
}
