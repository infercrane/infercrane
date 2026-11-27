package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/apibaseline"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapicatalog"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/replay"
	"github.com/infercrane/infercrane/internal/store"
)

// migrationProofStore is what the migration Proof routes need: endpoint- and
// binding-scoped capture, a closed API's rate card and quality evidence, the
// signed verdicts, and the staged switch.
type migrationProofStore interface {
	ResolveEndpointForTenant(context.Context, string, string) (domain.ResolvedEndpoint, error)
	ExternalBindingTarget(context.Context, string, string, string) (domain.ResolvedEndpoint, domain.BackendBinding, domain.Target, error)
	CaptureEndpointReplayTrace(context.Context, string, string, string, time.Duration, int) (domain.ReplayTrace, error)
	EndpointReplayTraces(context.Context, string, string, int) ([]domain.ReplayTrace, error)
	ReplayTraceByDigest(context.Context, string, string) (domain.ReplayTrace, error)
	RecordBindingRateCard(context.Context, string, string, string, apibaseline.RateCard) (apibaseline.RateCard, error)
	BindingRateCard(context.Context, string, string) (*apibaseline.RateCard, error)
	RecordBindingQualityEvidence(context.Context, string, string, string, domain.BindingQualityEvidence) (domain.BindingQualityEvidence, bool, error)
	BindingQualityEvidence(context.Context, string, string, int) ([]domain.BindingQualityEvidence, error)
	MigrationVerdictsForCampaign(context.Context, string, string) ([]domain.MigrationQualityVerdict, error)
	StageMigration(context.Context, string, string, string, string, int, time.Duration) (store.MigrationStage, error)
	QualityEvalSet(context.Context, string, string) (domain.QualityEvalSet, error)
	RecordRequest(context.Context, domain.InferenceRecord) error
}

func (a API) migrationProof(w http.ResponseWriter) (migrationProofStore, bool) {
	proofStore, ok := a.Store.(migrationProofStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "migration Proof storage is not configured")
	}
	return proofStore, ok
}

func reservedEvaluator(name string) bool {
	name = strings.TrimSpace(name)
	return strings.EqualFold(name, qualitycheck.Evaluator) || strings.EqualFold(name, qualitycheck.GradedEvaluator) || strings.EqualFold(name, qualitycheck.TaskEvaluator)
}

func (a API) captureEndpointReplay(w http.ResponseWriter, r *http.Request) {
	proofStore, ok := a.migrationProof(w)
	if !ok {
		return
	}
	var request struct {
		Binding       string `json:"binding,omitempty"`
		WindowSeconds int    `json:"window_seconds"`
		MaxRequests   int    `json:"max_requests"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.WindowSeconds == 0 {
		request.WindowSeconds = 86400
	}
	if request.MaxRequests == 0 {
		request.MaxRequests = 1000
	}
	if request.WindowSeconds < 1 || request.WindowSeconds > 30*86400 || request.MaxRequests < 1 || request.MaxRequests > 10000 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "window_seconds must be 1..2592000 and max_requests 1..10000")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	trace, err := proofStore.CaptureEndpointReplayTrace(r.Context(), actor.TenantID, r.PathValue("name"), strings.TrimSpace(request.Binding), time.Duration(request.WindowSeconds)*time.Second, request.MaxRequests)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "endpoint or binding was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "replay trace could not be captured")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint_replay.capture", ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"replay": replayTraceResponse(trace)})
}

func (a API) endpointReplays(w http.ResponseWriter, r *http.Request) {
	proofStore, ok := a.migrationProof(w)
	if !ok {
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeError(w, http.StatusBadRequest, "invalid_query", "limit must be between 1 and 100")
			return
		}
		limit = parsed
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	traces, err := proofStore.EndpointReplayTraces(r.Context(), actor.TenantID, r.PathValue("name"), limit)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "endpoint was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "replay traces could not be read")
		return
	}
	data := make([]map[string]any, 0, len(traces))
	for _, trace := range traces {
		data = append(data, replayTraceResponse(trace))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "content_recorded": false})
}

func (a API) importEndpointTrafficObservations(w http.ResponseWriter, r *http.Request) {
	proofStore, ok := a.migrationProof(w)
	if !ok {
		return
	}
	request, ok := decodeTrafficImport(w, r)
	if !ok {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	resolved, err := proofStore.ResolveEndpointForTenant(r.Context(), actor.TenantID, r.PathValue("name"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "endpoint was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "endpoint could not be resolved")
		return
	}
	scope := domain.InferenceRecord{EndpointID: resolved.Endpoint.ID, LogicalModelID: resolved.Endpoint.LogicalModelID, EnvironmentID: resolved.Endpoint.EnvironmentID}
	scopeID := "endpoint:" + resolved.Endpoint.ID
	if name := strings.TrimSpace(request.Binding); name != "" {
		var binding domain.BackendBinding
		for _, candidate := range resolved.Bindings {
			if candidate.Name == name || candidate.ID == name {
				binding = candidate
			}
		}
		if binding.ID == "" {
			writeError(w, http.StatusNotFound, "not_found", "binding was not found on this endpoint")
			return
		}
		scope.BindingID, scope.TargetID, scope.DeploymentID = binding.ID, binding.TargetID, binding.DeploymentID
		scopeID = "binding:" + binding.ID
	}
	a.recordTrafficObservations(w, r, proofStore, request, "endpoint", scope, scopeID)
}

// decodeTrafficImport decodes and checks the import envelope shared by
// deployment and endpoint imports.
func decodeTrafficImport(w http.ResponseWriter, r *http.Request) (trafficObservationImport, bool) {
	var request trafficObservationImport
	if !decodeMutationBody(w, r, &request) {
		return request, false
	}
	if request.SchemaVersion != "infercrane.traffic-observations/v1" {
		writeError(w, http.StatusUnprocessableEntity, "unsupported_schema", "schema_version must be infercrane.traffic-observations/v1")
		return request, false
	}
	allowedSources := map[string]bool{"opentelemetry": true, "datadog": true, "baseten": true, "fireworks": true, "openai": true, "anthropic": true, "custom": true}
	if !allowedSources[request.Source] || len(request.Observations) < 1 || len(request.Observations) > 1000 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "source must be opentelemetry, datadog, baseten, fireworks, openai, anthropic, or custom and observations must contain 1..1000 rows")
		return request, false
	}
	return request, true
}

type rateCardRequest struct {
	Authority                     string    `json:"authority"`
	Currency                      string    `json:"currency"`
	InputMicrousdPerMillion       *int64    `json:"input_microusd_per_million"`
	CachedInputMicrousdPerMillion *int64    `json:"cached_input_microusd_per_million,omitempty"`
	OutputMicrousdPerMillion      *int64    `json:"output_microusd_per_million"`
	Source                        string    `json:"source,omitempty"`
	ValidUntil                    time.Time `json:"valid_until"`
}

func rateCardResponse(card apibaseline.RateCard) map[string]any {
	response := map[string]any{"id": card.ID, "binding_id": card.BindingID, "authority": card.Authority, "currency": card.Pricing.Currency, "input_microusd_per_million": card.Pricing.InputMicrousdPerMillion, "output_microusd_per_million": card.Pricing.OutputMicrousdPerMillion, "source": card.Source, "valid_until": card.ValidUntil, "created_at": card.CreatedAt, "expired": !card.ValidUntil.After(time.Now())}
	if card.Pricing.CachedInputMicrousdPerMillion != nil {
		response["cached_input_microusd_per_million"] = card.Pricing.CachedInputMicrousdPerMillion
	}
	return response
}

func (a API) setBindingRateCard(w http.ResponseWriter, r *http.Request) {
	proofStore, ok := a.migrationProof(w)
	if !ok {
		return
	}
	var request rateCardRequest
	if !decodeMutationBody(w, r, &request) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	card := apibaseline.RateCard{Authority: strings.TrimSpace(request.Authority), Pricing: modelapicatalog.Pricing{Currency: strings.ToUpper(strings.TrimSpace(request.Currency)), InputMicrousdPerMillion: request.InputMicrousdPerMillion, CachedInputMicrousdPerMillion: request.CachedInputMicrousdPerMillion, OutputMicrousdPerMillion: request.OutputMicrousdPerMillion}, Source: strings.TrimSpace(request.Source), ValidUntil: request.ValidUntil.UTC(), RecordedBy: actor.Name}
	card, err := proofStore.RecordBindingRateCard(r.Context(), actor.TenantID, r.PathValue("name"), r.PathValue("binding"), card)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "endpoint or external binding was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_rate_card", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "binding_rate_card.record", ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"rate_card": rateCardResponse(card)})
}

func (a API) bindingRateCard(w http.ResponseWriter, r *http.Request) {
	proofStore, ok := a.migrationProof(w)
	if !ok {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	_, binding, _, err := proofStore.ExternalBindingTarget(r.Context(), actor.TenantID, r.PathValue("name"), r.PathValue("binding"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "endpoint or external binding was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "rate card could not be read")
		return
	}
	card, err := proofStore.BindingRateCard(r.Context(), actor.TenantID, binding.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "rate card could not be read")
		return
	}
	if card == nil {
		// No rate card means the closed API's cost is unknown, never zero.
		writeJSON(w, http.StatusOK, map[string]any{"rate_card": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rate_card": rateCardResponse(*card)})
}

func bindingQualityResponse(item domain.BindingQualityEvidence) map[string]any {
	return map[string]any{"id": item.ID, "endpoint_id": item.EndpointID, "binding_id": item.BindingID, "source": item.Source, "suite": item.Suite, "suite_version": item.SuiteVersion, "evaluator": item.Evaluator, "evaluator_version": item.EvaluatorVersion, "score": item.Score, "sample_count": item.SampleCount, "pairing_digest": item.PairingDigest, "payload_digest": item.PayloadDigest, "key_id": item.KeyID, "evaluated_at": item.EvaluatedAt, "created_at": item.CreatedAt}
}

// recordBindingQualityEvidence attaches a customer evaluator's signed result
// for the closed API behind an external binding. The payload's deployment is
// the endpoint name and its revision_id is binding:<binding id>, so it can
// never be mistaken for evidence about an owned revision.
func (a API) recordBindingQualityEvidence(w http.ResponseWriter, r *http.Request) {
	proofStore, ok := a.migrationProof(w)
	if !ok {
		return
	}
	var envelope passport.Envelope
	if !decodeMutationBody(w, r, &envelope) {
		return
	}
	payload, err := qualityevidence.Decode(envelope)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_quality_evidence", err.Error())
		return
	}
	if reservedEvaluator(payload.Evaluator) {
		writeError(w, http.StatusUnprocessableEntity, "reserved_quality_evaluator", "the evaluator names "+qualitycheck.Evaluator+", "+qualitycheck.GradedEvaluator+" and "+qualitycheck.TaskEvaluator+" are reserved for InferCrane's own checks")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	_, binding, _, err := proofStore.ExternalBindingTarget(r.Context(), actor.TenantID, r.PathValue("name"), r.PathValue("binding"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "endpoint or external binding was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "binding could not be resolved")
		return
	}
	if payload.Deployment != r.PathValue("name") || payload.RevisionID != qualitycheck.BindingSubject(binding.ID) {
		writeError(w, http.StatusUnprocessableEntity, "subject_mismatch", "closed API evidence must name the endpoint as deployment and binding:"+binding.ID+" as revision_id")
		return
	}
	pairing := ""
	if payload.Distribution != nil {
		pairing = payload.Distribution.PairingDigest
	}
	item, created, err := proofStore.RecordBindingQualityEvidence(r.Context(), actor.TenantID, r.PathValue("name"), binding.ID, domain.BindingQualityEvidence{Source: domain.QualityEvidenceSourceCustomer, Suite: payload.Suite, SuiteVersion: payload.SuiteVersion, Evaluator: payload.Evaluator, EvaluatorVersion: payload.EvaluatorVersion, Score: payload.Score, SampleCount: payload.SampleCount, PairingDigest: pairing, PayloadJSON: envelope.PayloadJSON, PayloadDigest: envelope.Digest, Signature: envelope.Signature, PublicKey: envelope.PublicKey, Algorithm: envelope.Algorithm, KeyID: envelope.KeyID, EvaluatedAt: payload.EvaluatedAt})
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "idempotency_conflict", "this signed evidence is already recorded for another binding")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "quality evidence could not be persisted")
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "binding_quality_evidence.record", ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: "succeeded"})
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{"evidence": bindingQualityResponse(item)})
}

func (a API) bindingQualityEvidence(w http.ResponseWriter, r *http.Request) {
	proofStore, ok := a.migrationProof(w)
	if !ok {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	_, binding, _, err := proofStore.ExternalBindingTarget(r.Context(), actor.TenantID, r.PathValue("name"), r.PathValue("binding"))
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "endpoint or external binding was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "binding could not be resolved")
		return
	}
	rows, err := proofStore.BindingQualityEvidence(r.Context(), actor.TenantID, binding.ID, 20)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "quality evidence could not be read")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, bindingQualityResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func migrationVerdictResponse(item domain.MigrationQualityVerdict) map[string]any {
	response := map[string]any{"id": item.ID, "campaign_id": item.CampaignID, "candidate_id": item.CandidateRunID, "endpoint_id": item.EndpointID, "binding_id": item.BindingID, "deployment_id": item.DeploymentID, "revision_id": item.RevisionID, "verdict": item.Verdict, "code": item.Code, "margin_percent": item.MarginPercent, "payload_digest": item.PayloadDigest, "key_id": item.KeyID, "evaluated_at": item.EvaluatedAt, "created_at": item.CreatedAt}
	var payload qualityevidence.MigrationVerdict
	if json.Unmarshal([]byte(item.PayloadJSON), &payload) == nil {
		response["reference_score"], response["candidate_score"], response["bootstrap"] = payload.ReferenceScore, payload.CandidateScore, payload.Bootstrap
		response["suite"], response["evaluator"] = payload.Suite, payload.Evaluator
	}
	return response
}

func (a API) campaignMigrationVerdicts(w http.ResponseWriter, r *http.Request) {
	proofStore, ok := a.migrationProof(w)
	if !ok {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	rows, err := proofStore.MigrationVerdictsForCampaign(r.Context(), actor.TenantID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "migration verdicts could not be read")
		return
	}
	data := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data = append(data, migrationVerdictResponse(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

// stageEndpointMigration takes one step of the staged switch from a closed
// API to an owned deployment: canary (at most 5% to the owned deployment,
// admitted on a signed PASS verdict), primary (the owned deployment first and
// the closed API as fallback, admitted on per-binding Release Guard metrics
// from the canary), or rollback (back to the closed API alone).
func (a API) stageEndpointMigration(w http.ResponseWriter, r *http.Request) {
	proofStore, ok := a.migrationProof(w)
	if !ok {
		return
	}
	var request struct {
		Step              string `json:"step"`
		VerdictID         string `json:"verdict_id,omitempty"`
		CanaryBasisPoints int    `json:"canary_basis_points,omitempty"`
		WindowSeconds     int    `json:"window_seconds,omitempty"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.WindowSeconds == 0 {
		request.WindowSeconds = 3600
	}
	if request.WindowSeconds < 60 || request.WindowSeconds > 30*86400 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "window_seconds must be 60..2592000")
		return
	}
	if request.Step == store.MigrationStepCanary && request.VerdictID == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "a canary needs the PASS migration verdict it rests on")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	stage, err := proofStore.StageMigration(r.Context(), actor.TenantID, r.PathValue("name"), request.Step, strings.TrimSpace(request.VerdictID), request.CanaryBasisPoints, time.Duration(request.WindowSeconds)*time.Second)
	if writeEndpointMutationError(w, err) {
		return
	}
	routing := "unchanged"
	if stage.Activated {
		routing = a.refreshEndpoints(r.Context())
	}
	outcome := "staged"
	if stage.Activated {
		outcome = "activated"
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint_migration." + stage.Step, ResourceType: "endpoint", ResourceName: r.PathValue("name"), Outcome: outcome})
	status := http.StatusOK
	if !stage.Activated {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"migration": map[string]any{"step": stage.Step, "activated": stage.Activated, "routing": routing, "plan": servingPlanResponse(stage.Plan), "evaluation": endpointGuardEvaluationResponse(stage.Evaluation)}})
}

// validMigrationCampaign checks a migration campaign's baseline before it is
// stored: the closed API is a traffic-managed external binding, the workload
// fingerprint names a trace captured from that endpoint, and the built-in
// check has a graded eval set.
func (a API) validMigrationCampaign(w http.ResponseWriter, r *http.Request, tenant string, input optimizer.Request) bool {
	proofStore, ok := a.migrationProof(w)
	if !ok {
		return false
	}
	migration := input.Migration
	resolved, binding, _, err := proofStore.ExternalBindingTarget(r.Context(), tenant, migration.Endpoint, migration.Binding)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusUnprocessableEntity, "migration_baseline_not_found", "the migration endpoint has no external binding by that name")
		return false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "migration baseline could not be resolved")
		return false
	}
	if binding.OwnershipMode != "traffic-managed" {
		writeError(w, http.StatusUnprocessableEntity, "migration_baseline_observe_only", "the closed API must route through InferCrane (traffic-managed) to be measured and switched")
		return false
	}
	digest := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(input.WorkloadFingerprint)), "sha256:")
	trace, err := proofStore.ReplayTraceByDigest(r.Context(), tenant, digest)
	if err != nil || trace.EndpointID != resolved.Endpoint.ID || (trace.BindingID != "" && trace.BindingID != binding.ID) {
		writeError(w, http.StatusUnprocessableEntity, "migration_workload_not_captured", "workload_fingerprint must name a trace captured from the migration endpoint's traffic")
		return false
	}
	if volume, volumeErr := replay.TraceTokenVolume(trace); volumeErr != nil || volume.Requests == 0 {
		writeError(w, http.StatusUnprocessableEntity, "migration_workload_empty", "the captured trace holds no generation requests to replay")
		return false
	}
	if input.QualityCheck != optimizer.QualityCheckCustomer {
		stored, setErr := proofStore.QualityEvalSet(r.Context(), tenant, input.QualityEvalSet)
		if errors.Is(setErr, domain.ErrNotFound) {
			writeError(w, http.StatusUnprocessableEntity, "quality_eval_set_not_found", "upload the graded eval set before creating the migration")
			return false
		}
		if setErr != nil {
			writeError(w, http.StatusInternalServerError, "internal", "quality eval set could not be read")
			return false
		}
		set, decodeErr := qualitycheck.DecodeEvalSet([]byte(stored.Body))
		if decodeErr != nil || !set.Graded() {
			writeError(w, http.StatusUnprocessableEntity, "quality_eval_set_not_graded", "a migration needs a graded eval set: grading exact_match, token_f1, or answer_line with expected answers")
			return false
		}
		if len(set.Prompts) < qualitycheck.MigrationBootstrapMinSamples {
			writeError(w, http.StatusUnprocessableEntity, "quality_eval_set_too_small", "a migration verdict needs at least 30 graded prompts")
			return false
		}
	}
	return true
}
