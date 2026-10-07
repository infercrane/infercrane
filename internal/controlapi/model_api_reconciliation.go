package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapirouting"
)

type modelAPIReconciliationStore interface {
	RecordOperatorModelAPIUsageReconciliationEvidence(context.Context, string, string, modelapirouting.ReconciliationEvidence) (modelapirouting.ReconciliationEvidence, bool, error)
}

// recordModelAPIUsageReconciliationEvidence is an operator break-glass path
// for supplier outcomes that cannot be recovered through a qualified adapter.
// It records immutable evidence only; the restart-safe reconciler remains the
// sole component that settles or releases customer money.
func (a API) recordModelAPIUsageReconciliationEvidence(w http.ResponseWriter, r *http.Request) {
	operatorStore, actor, ok := a.modelAPIOperator(w, r)
	if !ok {
		return
	}
	store, ok := operatorStore.(modelAPIReconciliationStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "Model API usage reconciliation storage is unavailable")
		return
	}
	var request struct {
		CustomerTenantID  string    `json:"customer_tenant_id"`
		Outcome           string    `json:"outcome"`
		SupplierRequestID string    `json:"supplier_request_id"`
		EvidenceReference string    `json:"evidence_reference"`
		InputTokens       *int      `json:"input_tokens"`
		CachedInputTokens *int      `json:"cached_input_tokens"`
		OutputTokens      *int      `json:"output_tokens"`
		ObservedAt        time.Time `json:"observed_at"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	request.CustomerTenantID = strings.TrimSpace(request.CustomerTenantID)
	request.Outcome = strings.TrimSpace(request.Outcome)
	request.SupplierRequestID = strings.TrimSpace(request.SupplierRequestID)
	request.EvidenceReference = strings.TrimSpace(request.EvidenceReference)
	evidence := modelapirouting.ReconciliationEvidence{
		ReservationID: strings.TrimSpace(r.PathValue("id")), TenantID: request.CustomerTenantID,
		SupplierRequestID: request.SupplierRequestID, Outcome: request.Outcome,
		Authority: modelapirouting.EvidenceOperatorVerified, Reference: request.EvidenceReference,
		InputTokens: request.InputTokens, CachedInputTokens: request.CachedInputTokens, OutputTokens: request.OutputTokens,
		ObservedAt: request.ObservedAt,
	}
	if evidence.ReservationID == "" || evidence.TenantID == "" || evidence.Reference == "" || evidence.ObservedAt.IsZero() {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "customer_tenant_id, evidence_reference, observed_at, and reservation id are required")
		return
	}
	if evidence.ObservedAt.After(time.Now().UTC().Add(5 * time.Minute)) {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "observed_at cannot be in the future")
		return
	}
	if evidence.Outcome != modelapirouting.ReconciliationUsage && evidence.Outcome != modelapirouting.ReconciliationNoCharge {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "outcome must be usage or no_charge")
		return
	}
	if evidence.Outcome == modelapirouting.ReconciliationUsage && (evidence.InputTokens == nil || evidence.OutputTokens == nil) {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "usage evidence requires input_tokens and output_tokens")
		return
	}
	if evidence.Outcome == modelapirouting.ReconciliationNoCharge && (evidence.InputTokens != nil || evidence.CachedInputTokens != nil || evidence.OutputTokens != nil) {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "no_charge evidence cannot include token counts")
		return
	}
	item, created, err := store.RecordOperatorModelAPIUsageReconciliationEvidence(r.Context(), actor.TenantID, actor.ID, evidence)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "ambiguous Model API reservation was not found for the configured operator")
		return
	}
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "evidence_conflict", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "evidence_rejected", err.Error())
		return
	}
	payload, _ := json.Marshal(map[string]any{"outcome": item.Outcome, "authority": item.Authority, "evidence_reference": item.Reference, "created": created})
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: item.TenantID, Actor: actor.Name, Action: "model_api_usage.reconciliation_evidence", ResourceType: "model_api_usage_reservation", ResourceName: item.ReservationID, Outcome: "accepted", Payload: string(payload)})
	writeJSON(w, http.StatusAccepted, map[string]any{"data": item, "created": created, "reconciliation": "queued", "content_recorded": false})
}
