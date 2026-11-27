package optimizationcampaign

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// FailureQualityAnswersNotShownHeld ends a setup that changes answers and
	// could not show them held within a point: unknown, never qualified.
	FailureQualityAnswersNotShownHeld = "quality_answers_not_shown_held"
	retryQualityEvidencePending       = "optimization_quality_evidence_pending"
	retryQualityEvalSetPending        = "optimization_quality_eval_set_pending"
	retryQualityCheckFailed           = "optimization_quality_check_failed"
	maxQualityCheckDuration           = 30 * time.Minute
	// maxTaskQualityCheckDuration bounds a check on a task set: up to 1,000 items with worked
	// answers on both servers, 32 at a time (qualitycheck), about 25 minutes on an L4.
	maxTaskQualityCheckDuration = 45 * time.Minute
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
	// model and revision are what the arm serves, when known: the
	// tokenizer guard compares them (tokenizerPair).
	model, revision string
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
	target, draft, err := c.candidateTarget(ctx, candidate)
	if err != nil {
		return domain.QualityEvidence{}, err
	}
	checkCtx, cancel := c.checkContext(ctx, campaign, set)
	defer cancel()
	// A setup that changes answers (other weights or precision, or a technique
	// that declares it) must show its answers held within a point.
	var options []qualitycheck.CompareOption
	if reference.target.revisionID != candidate.RevisionID && changesAnswers(campaign, candidate, draft) {
		options = append(options, qualitycheck.RequireAnswersHeld())
	}
	// A setup measured again on fresh servers answered the task set on its
	// first servers too, before they were retired (AskFirstPair): the verdict
	// pools both pairs.
	if checkpoint, ok := DecodeMeasurementCheckpoint(candidate.MeasurementCheckpointJSON); ok && checkpoint.Confirmation != nil && checkpoint.Confirmation.FirstAnswers != nil {
		options = append(options, qualitycheck.PoolWith(*checkpoint.Confirmation.FirstAnswers))
	}
	comparison, err := qualitycheck.Compare(checkCtx, c.Completer, set, reference.target.qualityTarget(), target.qualityTarget(), c.now, options...)
	if err != nil {
		return domain.QualityEvidence{}, err
	}
	// A change of numeric precision also gets the candidate's divergence
	// from the reference on forced decoding, read on long answers: the task
	// set's items when the campaign has one, else the embedded long-answer
	// prompts (qualitycheck.DivergencePrompts). It is evidence: a server that
	// does not report log probabilities, or any failure to read it, leaves
	// it unknown and the check stands on its answers.
	if scorer, ok := c.Completer.(qualitycheck.ForcedScorer); ok && reference.target.revisionID != candidate.RevisionID && changesPrecision(campaign, draft) {
		referenceTarget, candidateTarget := reference.target.qualityTarget(), target.qualityTarget()
		referenceTarget.Tokenizer, candidateTarget.Tokenizer = tokenizerPair(campaign, reference, draft)
		if divergence, divergenceErr := qualitycheck.MeasureDivergence(checkCtx, c.Completer, scorer, qualitycheck.DivergencePrompts(set), referenceTarget, candidateTarget); divergenceErr == nil {
			comparison.Divergence = divergence
		}
	}
	if reference.target.revisionID != candidate.RevisionID {
		if err = c.recordReference(ctx, candidate.TenantID, reference.deployment, set, comparison); err != nil {
			return domain.QualityEvidence{}, err
		}
	}
	return c.record(ctx, candidate.TenantID, candidate.DeploymentName, comparison.Payload(candidate.DeploymentName, false), comparison.ReferenceID)
}

// signedEvidence signs an InferCrane-run payload with the control plane's key
// and decodes it exactly as customer evidence is decoded, so both paths store
// verified rows.
func signedEvidence(signer ed25519.PrivateKey, payload qualityevidence.Payload, referenceRevisionID string) (domain.QualityEvidence, error) {
	envelope, err := passport.Sign(payload, signer)
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
	return row, nil
}

// candidateTarget is the candidate's own server, which a check asks, and its
// draft.
func (c BuiltinQualityCheck) candidateTarget(ctx context.Context, candidate domain.OptimizationCandidateRun) (armTarget, optimizer.DeploymentDraft, error) {
	draft, err := candidateDraft(candidate)
	if err != nil {
		return armTarget{}, draft, err
	}
	resolved, err := c.Store.ResolveForTenant(ctx, candidate.TenantID, candidate.DeploymentName)
	if err != nil {
		return armTarget{}, draft, fmt.Errorf("resolve candidate deployment: %w", err)
	}
	target, _, err := directReplica(ctx, c.Store, c.Backends, candidate, resolved.Deployment, draft)
	if err != nil {
		return armTarget{}, draft, fmt.Errorf("candidate: %w", err)
	}
	return target, draft, nil
}

// checkContext bounds one check: a task set may take longer than prompts,
// and nothing runs past the approval.
func (c BuiltinQualityCheck) checkContext(ctx context.Context, campaign domain.OptimizationCampaign, set qualitycheck.EvalSet) (context.Context, context.CancelFunc) {
	limit := maxQualityCheckDuration
	if set.Task() {
		limit = maxTaskQualityCheckDuration
	}
	deadline := c.now().Add(limit)
	if campaign.ApprovalExpiresAt != nil && campaign.ApprovalExpiresAt.Before(deadline) {
		deadline = *campaign.ApprovalExpiresAt
	}
	return context.WithTimeout(ctx, time.Until(deadline))
}

// AskFirstPair asks a setup's first servers, and the reference, the
// campaign's task set before a confirmation retires them, so the verdict can
// pool their answers with the fresh servers' (rule 3). Answers on one server
// each rarely show a change of answers held within a point; two pairs halve
// the servers' disagreement on the same items. It asks only a setup that
// changes answers, on a task set; for any other it returns false. Nothing is
// recorded: the verdict is the check after the fresh window (Check).
func (c BuiltinQualityCheck) AskFirstPair(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) (qualitycheck.ServerPair, bool, error) {
	if c.Store == nil || c.Completer == nil {
		return qualitycheck.ServerPair{}, false, nil
	}
	set, err := c.evalSet(ctx, campaign)
	if err != nil || !set.Task() {
		return qualitycheck.ServerPair{}, false, err
	}
	reference, err := c.reference(ctx, campaign, candidate)
	if err != nil {
		return qualitycheck.ServerPair{}, false, err
	}
	draft, err := candidateDraft(candidate)
	if err != nil {
		return qualitycheck.ServerPair{}, false, err
	}
	if reference.target.revisionID == candidate.RevisionID || !changesAnswers(campaign, candidate, draft) {
		return qualitycheck.ServerPair{}, false, nil
	}
	target, _, err := c.candidateTarget(ctx, candidate)
	if err != nil {
		return qualitycheck.ServerPair{}, false, err
	}
	checkCtx, cancel := c.checkContext(ctx, campaign, set)
	defer cancel()
	comparison, err := qualitycheck.Compare(checkCtx, c.Completer, set, reference.target.qualityTarget(), target.qualityTarget(), c.now, qualitycheck.RequireAnswersHeld())
	if err != nil {
		return qualitycheck.ServerPair{}, false, err
	}
	pair, ok := comparison.Pair()
	return pair, ok, nil
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
	row, err := signedEvidence(c.Signer, payload, referenceRevisionID)
	if err != nil {
		return domain.QualityEvidence{}, err
	}
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
				// The active revision's commit is the campaign control's when
				// the control serves the endpoint's model; otherwise unknown.
				arm := qualityArm{target: target, deployment: campaign.TargetDeployment, model: resolved.Deployment.Model}
				if control, found := CampaignControl(campaign); found {
					if draft, draftErr := candidateDraft(control); draftErr == nil && draft.Model.ID == resolved.Deployment.Model {
						arm.revision = draft.Model.Revision
					}
				}
				return arm, nil
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
	return qualityArm{target: target, deployment: control.DeploymentName, model: draft.Model.ID, revision: draft.Model.Revision}, nil
}

// tokenizerPair is what each arm reads text with, for the divergence's
// tokenizer guard: one model at one revision on both reads the same files;
// two models are compared by the tokenizer digests the proposal recorded
// (input.model_tokenizers). Anything not recorded is empty, so the
// divergence is unknown, never a pass or a failure.
func tokenizerPair(campaign domain.OptimizationCampaign, reference qualityArm, candidate optimizer.DeploymentDraft) (string, string) {
	if reference.model == "" || candidate.Model.ID == "" {
		return "", ""
	}
	// The same model and revision as both drafts state it (a revision left
	// to the catalog is resolved alike for both setups of one campaign).
	if reference.model == candidate.Model.ID && reference.revision == candidate.Model.Revision {
		same := "model:" + optimizer.TokenizerKey(reference.model, reference.revision)
		return same, same
	}
	if reference.revision == "" || candidate.Model.Revision == "" {
		return "", ""
	}
	var proposal struct {
		Input struct {
			ModelTokenizers map[string]string `json:"model_tokenizers"`
		} `json:"input"`
	}
	if json.Unmarshal([]byte(campaign.ProposalJSON), &proposal) != nil {
		return "", ""
	}
	recorded := proposal.Input.ModelTokenizers
	return recorded[optimizer.TokenizerKey(reference.model, reference.revision)], recorded[optimizer.TokenizerKey(candidate.Model.ID, candidate.Model.Revision)]
}

// changesAnswers reports whether a candidate gives other answers than the
// campaign's control by design: it serves other numbers (changesPrecision) or
// a technique whose manifest declares it changes answers made it.
func changesAnswers(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft) bool {
	if changesPrecision(campaign, draft) {
		return true
	}
	var proposal optimizer.Proposal
	if json.Unmarshal([]byte(campaign.ProposalJSON), &proposal) != nil {
		return false
	}
	for _, proposed := range proposal.Candidates {
		if proposed.ID == candidate.ProposalCandidateID && proposed.Technique != nil {
			manifest, known := optimizer.TechniqueManifest(proposed.Technique.ID)
			return known && manifest.ChangesAnswers
		}
	}
	return false
}

// precisionFlags are the engine flags that set numeric precision.
var precisionFlags = []string{"--quantization", "--kv-cache-dtype", "--dtype"}

// changesPrecision reports whether the candidate serves other numbers than
// the campaign's control: another checkpoint, or another value of a flag that
// sets weight, KV cache, or activation precision. Without a readable control
// it says no, so the divergence is not measured.
func changesPrecision(campaign domain.OptimizationCampaign, candidate optimizer.DeploymentDraft) bool {
	control, found := CampaignControl(campaign)
	if !found {
		return false
	}
	reference, err := candidateDraft(control)
	if err != nil {
		return false
	}
	if reference.Model.ID != candidate.Model.ID || reference.Model.Revision != candidate.Model.Revision {
		return true
	}
	value := func(args []string, flag string) string {
		for index, arg := range args {
			name, setting, inline := strings.Cut(arg, "=")
			if name != flag {
				continue
			}
			if !inline && index+1 < len(args) {
				setting = args[index+1]
			}
			return strings.ToLower(setting)
		}
		return ""
	}
	for _, flag := range precisionFlags {
		if value(reference.Runtime.Args, flag) != value(candidate.Runtime.Args, flag) {
			return true
		}
	}
	return false
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
	evaluator, version := qualitycheck.Evaluator, qualitycheck.EvaluatorVersion
	if set.Task() {
		evaluator, version = qualitycheck.TaskEvaluator, qualitycheck.TaskEvaluatorVersion
	}
	return row.Source == domain.QualityEvidenceSourceInferCrane && row.Evaluator == evaluator && row.EvaluatorVersion == version && row.Suite == set.Name && row.SuiteVersion == set.SuiteVersion()
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
