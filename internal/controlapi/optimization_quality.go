package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

type qualityEvalSetStore interface {
	RecordQualityEvalSet(context.Context, string, string, []byte) (domain.QualityEvalSet, bool, error)
	QualityEvalSet(context.Context, string, string) (domain.QualityEvalSet, error)
	DeleteQualityEvalSet(context.Context, string, string) error
}

type qualityEvidenceLookupStore interface {
	QualityEvidenceByIDs(context.Context, string, []string) (map[string]domain.QualityEvidence, error)
}

// Quality step states for one candidate, as the run page and CLI show them.
const (
	qualityStatePending           = "pending"
	qualityStateChecking          = "checking"
	qualityStateWaitingForEvalSet = "waiting_for_eval_set"
	qualityStateWaitingEvidence   = "waiting_for_evidence"
	qualityStatePassed            = "passed"
	qualityStateWatch             = "watch"
	qualityStateBlocked           = "blocked"
	qualityStateInconclusive      = "inconclusive"
	qualityStateNotRun            = "not_run"
)

// candidateQualityView is the quality step of one candidate, derived from
// persisted state only: the candidate, the campaign's quality choice, the
// evidence row it selected, and whether the named eval set is uploaded.
type candidateQualityView struct {
	State string `json:"state"`
	// Check is builtin or customer_evidence, as the campaign asked.
	Check string `json:"check"`
	// Source names who produced the deciding evidence: infercrane or customer.
	Source              string   `json:"source,omitempty"`
	Role                string   `json:"role,omitempty"`
	Suite               string   `json:"suite,omitempty"`
	SuiteVersion        string   `json:"suite_version,omitempty"`
	EvalSet             string   `json:"eval_set,omitempty"`
	PromptCount         int      `json:"prompt_count,omitempty"`
	Score               *float64 `json:"score,omitempty"`
	RegressionPercent   *float64 `json:"regression_percent,omitempty"`
	WatchLimitPercent   *float64 `json:"watch_limit_percent,omitempty"`
	BlockLimitPercent   *float64 `json:"block_limit_percent,omitempty"`
	ReferenceRevisionID string   `json:"reference_revision_id,omitempty"`
	EvidenceID          string   `json:"evidence_id,omitempty"`
	// ToolCalls is the built-in check's score over cases that declare tools.
	ToolCalls *qualityevidence.ToolCallScores `json:"tool_calls,omitempty"`
	Reason    string                          `json:"reason"`
	Command   string                          `json:"command,omitempty"`
}

// campaignQualityInputs is what candidateQuality needs beyond the campaign.
// A nil evidence map means rows were not loaded, so verdict details are
// omitted rather than guessed.
type campaignQualityInputs struct {
	evidence        map[string]domain.QualityEvidence
	evalSetUploaded *bool
}

// campaignQualityInputs reads, in one query, the evidence every listed
// candidate selected, and checks each named eval set once. A failed read
// leaves those details out of the response instead of failing it.
func (a API) campaignQualityInputs(ctx context.Context, tenant string, campaigns ...domain.OptimizationCampaign) map[string]campaignQualityInputs {
	out := make(map[string]campaignQualityInputs, len(campaigns))
	var evidence map[string]domain.QualityEvidence
	if store, ok := a.Store.(qualityEvidenceLookupStore); ok {
		ids := make([]string, 0)
		for _, campaign := range campaigns {
			for _, candidate := range campaign.Candidates {
				ids = append(ids, candidate.QualityEvidenceID)
			}
		}
		if rows, err := store.QualityEvidenceByIDs(ctx, tenant, ids); err == nil {
			evidence = rows
		}
	}
	uploaded := map[string]*bool{}
	for _, campaign := range campaigns {
		inputs := campaignQualityInputs{evidence: evidence}
		digest := optimizationcampaign.QualityEvalSetDigest(campaign)
		if store, ok := a.Store.(qualityEvalSetStore); ok && digest != "" && campaignValidating(campaign) {
			if _, known := uploaded[digest]; !known {
				_, err := store.QualityEvalSet(ctx, tenant, digest)
				if err == nil || errors.Is(err, domain.ErrNotFound) {
					present := err == nil
					uploaded[digest] = &present
				} else {
					uploaded[digest] = nil
				}
			}
			inputs.evalSetUploaded = uploaded[digest]
		}
		out[campaign.ID] = inputs
	}
	return out
}

func campaignValidating(campaign domain.OptimizationCampaign) bool {
	for _, candidate := range campaign.Candidates {
		if candidate.State == optimizationcampaign.CandidateValidating {
			return true
		}
	}
	return false
}

func candidateQuality(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, inputs campaignQualityInputs) candidateQualityView {
	view := candidateQualityView{Check: "builtin", EvalSet: optimizationcampaign.QualityEvalSetDigest(campaign)}
	if optimizationcampaign.QualityWaitsForCustomer(campaign) {
		view.Check = "customer_evidence"
	}
	if view.Check == "builtin" && view.EvalSet == "" {
		set := qualitycheck.Default()
		view.Suite, view.SuiteVersion, view.PromptCount = set.Name, set.SuiteVersion(), len(set.Prompts)
	}
	if candidate.QualityEvidenceID != "" {
		if row, loaded := inputs.evidence[candidate.QualityEvidenceID]; loaded {
			return evidenceQuality(view, candidate, row)
		}
		// The deciding evidence was not read for this response, so only the
		// candidate's own record says how the step ended.
		view.EvidenceID = candidate.QualityEvidenceID
		if candidate.FailureCode == optimizationcampaign.FailureQualityRejected {
			view.State, view.Reason = qualityStateBlocked, "The answer check blocked this setup."
		} else {
			view.State, view.Reason = qualityStatePassed, "The answer check passed."
		}
		return view
	}
	switch {
	case candidate.FailureCode == optimizationcampaign.FailureQualityReferenceUnavailable:
		view.State = qualityStateInconclusive
		view.Reason = "No serving reference to compare answers with, so this setup's answers could not be checked."
	case candidate.FailureCode == optimizationcampaign.FailureQualityReferenceUnanswered:
		view.State = qualityStateInconclusive
		view.Reason = "The reference gave no answer text to some prompts within the token limit, so this setup's answers could not be checked. Reasoning does not count as an answer. Use an eval set with a larger max_tokens."
	case candidate.FailureCode == optimizationcampaign.FailureQualityNothingChecked:
		view.State = qualityStateInconclusive
		view.Reason = "Every case in the eval set declares tools and the reference refused requests with tools, so nothing could be checked. Add cases without tools, or start the reference with tool calling on."
	case candidate.State == optimizationcampaign.CandidateValidating && view.Check == "customer_evidence":
		view.State = qualityStateWaitingEvidence
		view.Reason = "Waiting for your signed evaluation of this setup."
		view.Command = customerEvidenceCommand(candidate)
	case candidate.State == optimizationcampaign.CandidateValidating && inputs.evalSetUploaded != nil && !*inputs.evalSetUploaded:
		view.State = qualityStateWaitingForEvalSet
		view.Reason = "Waiting for your eval set " + view.EvalSet + ". Upload the file whose digest matches it."
		view.Command = "infercrane evaluation set upload ./eval-set.json"
	case candidate.State == optimizationcampaign.CandidateValidating:
		view.State = qualityStateChecking
		view.Reason = "Asking this setup and the reference the same prompts and comparing the answers."
		if view.PromptCount > 0 {
			view.Reason = fmt.Sprintf("Asking this setup and the reference the same %d prompts and comparing the answers.", view.PromptCount)
		}
	case qualityStepAhead(candidate.State):
		view.State, view.Reason = qualityStatePending, "Runs after this setup is measured."
	default:
		view.State, view.Reason = qualityStateNotRun, "The answer check did not run for this setup."
	}
	return view
}

func evidenceQuality(view candidateQualityView, candidate domain.OptimizationCandidateRun, row domain.QualityEvidence) candidateQualityView {
	score := row.Score
	view.EvidenceID, view.Source, view.Score = row.ID, row.Source, &score
	view.Suite, view.SuiteVersion, view.PromptCount = row.Suite, row.SuiteVersion, row.SampleCount
	if view.Source == "" {
		view.Source = domain.QualityEvidenceSourceCustomer
	}
	if view.Source == domain.QualityEvidenceSourceCustomer {
		view.Role = "candidate"
		if row.Passed {
			view.State, view.Reason = qualityStatePassed, fmt.Sprintf("Your signed evaluation %s passed with score %.2f.", row.Suite, row.Score)
		} else {
			view.State, view.Reason = qualityStateBlocked, fmt.Sprintf("Your signed evaluation %s failed with score %.2f.", row.Suite, row.Score)
		}
		return view
	}
	view.ReferenceRevisionID = row.ReferenceRevisionID
	var distribution qualityevidence.Distribution
	if json.Unmarshal([]byte(row.DistributionJSON), &distribution) == nil {
		view.ToolCalls = distribution.ToolCalls
	}
	watch, block := qualitycheck.WatchRegressionPercent, qualitycheck.BlockRegressionPercent
	view.WatchLimitPercent, view.BlockLimitPercent = &watch, &block
	if row.ReferenceRevisionID != "" && row.ReferenceRevisionID == candidate.RevisionID {
		view.Role, view.State = "reference", qualityStatePassed
		view.Reason = fmt.Sprintf("This setup's answers to the %d prompts are the reference the other setups are compared with.", row.SampleCount)
		return view
	}
	view.Role = "candidate"
	eval := qualitycheck.VerdictForScore(row.Score)
	if eval.RegressionPercent != nil {
		drop := math.Round(*eval.RegressionPercent*10) / 10
		view.RegressionPercent = &drop
	}
	matched := fmt.Sprintf("Answers matched the reference %.0f%% across %d prompts", row.Score*100, row.SampleCount)
	if calls := view.ToolCalls; calls != nil && calls.Cases > 0 {
		matched += fmt.Sprintf("; tool calls matched %.0f%% across %d", calls.Score*100, calls.Cases)
	}
	if calls := view.ToolCalls; calls != nil && calls.NotChecked > 0 {
		// No score is given for these: unknown is not a match.
		matched += fmt.Sprintf("; %d tool cases not checked because the reference refused requests with tools", calls.NotChecked)
	}
	switch {
	case view.ToolCalls != nil && view.ToolCalls.Broken > 0:
		view.State = qualityStateBlocked
		view.Reason = fmt.Sprintf("%s. This setup broke %d of %d tool calls (%s). A broken tool call blocks a setup.", matched, view.ToolCalls.Broken, view.ToolCalls.Cases, toolCallFailureWords(view.ToolCalls.Candidate))
	case !row.Passed || eval.Verdict == releaseguard.QualityBlock:
		view.State, view.Reason = qualityStateBlocked, fmt.Sprintf("%s. More than %.0f%% different blocks a setup.", matched, block)
	case eval.Verdict == releaseguard.QualityWatch:
		view.State, view.Reason = qualityStateWatch, fmt.Sprintf("%s, inside the %.0f%% to %.0f%% watch band. Review the answers before rolling out.", matched, watch, block)
	default:
		view.State, view.Reason = qualityStatePassed, matched+"."
	}
	return view
}

// toolCallFailureWords names the kinds of failed calls with their case
// counts, for example "missing call 1, arguments that break the schema 2".
func toolCallFailureWords(failures qualityevidence.ToolCallFailures) string {
	var parts []string
	for _, kind := range []struct {
		count int
		words string
	}{
		{failures.MissingCall, "missing call"}, {failures.WrongFunction, "wrong or undeclared function"},
		{failures.MalformedArguments, "arguments that are not JSON"}, {failures.SchemaInvalid, "arguments that break the schema"},
		{failures.ExtraCalls, "extra call"}, {failures.UnexpectedCall, "call where the reference made none"},
		{failures.ToolsRefused, "refused tools the reference accepted"},
	} {
		if kind.count > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", kind.words, kind.count))
		}
	}
	return strings.Join(parts, ", ")
}

func qualityStepAhead(state string) bool {
	switch state {
	case optimizationcampaign.CandidateProposed, optimizationcampaign.CandidateProvisioning, optimizationcampaign.CandidateReady, optimizationcampaign.CandidateMeasuring:
		return true
	}
	return false
}

// customerEvidenceCommand is the exact command that signs and attaches an
// evaluator result for this candidate's revision.
func customerEvidenceCommand(candidate domain.OptimizationCandidateRun) string {
	deployment, revision := candidate.DeploymentName, candidate.RevisionID
	if deployment == "" {
		deployment = "DEPLOYMENT"
	}
	if revision == "" {
		revision = "REVISION_ID"
	}
	return "infercrane evaluation ingest " + deployment + " " + revision + " --result ./evaluator-result.json --key ./quality-evidence.key --file ./quality-evidence.json --attach"
}

func qualityEvalSetResponse(set domain.QualityEvalSet) map[string]any {
	return map[string]any{"digest": set.Digest, "name": set.Name, "version": set.Version, "prompt_count": set.PromptCount, "created_at": set.CreatedAt}
}

func (a API) uploadQualityEvalSet(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(qualityEvalSetStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "eval set storage is not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, qualitycheck.MaxEvalSetBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "eval_set_too_large", fmt.Sprintf("eval set must be at most %d KiB", qualitycheck.MaxEvalSetBytes>>10))
		return
	}
	if _, err = qualitycheck.DecodeEvalSet(body); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_eval_set", err.Error())
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	set, created, err := store.RecordQualityEvalSet(r.Context(), actor.TenantID, actor.Name, body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "eval set could not be stored")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "quality_eval_set.upload", ResourceType: "quality_eval_set", ResourceName: set.Digest, Outcome: "succeeded"})
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"eval_set": qualityEvalSetResponse(set), "created": created})
}

func (a API) qualityEvalSet(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(qualityEvalSetStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "eval set storage is not configured")
		return
	}
	digest := strings.TrimSpace(r.PathValue("digest"))
	if !qualitycheck.ValidDigest(digest) {
		writeError(w, http.StatusBadRequest, "invalid_digest", "eval set digest must be sha256:<64 lowercase hex characters>")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	set, err := store.QualityEvalSet(r.Context(), actor.TenantID, digest)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "eval set was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "eval set could not be loaded")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"eval_set": qualityEvalSetResponse(set)})
}

func (a API) deleteQualityEvalSet(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(qualityEvalSetStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "eval set storage is not configured")
		return
	}
	digest := strings.TrimSpace(r.PathValue("digest"))
	if !qualitycheck.ValidDigest(digest) {
		writeError(w, http.StatusBadRequest, "invalid_digest", "eval set digest must be sha256:<64 lowercase hex characters>")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	err := store.DeleteQualityEvalSet(r.Context(), actor.TenantID, digest)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "eval set was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "eval set could not be deleted")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "quality_eval_set.delete", ResourceType: "quality_eval_set", ResourceName: digest, Outcome: "succeeded"})
	w.WriteHeader(http.StatusNoContent)
}
