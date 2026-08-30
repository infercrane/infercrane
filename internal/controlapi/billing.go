package controlapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/managedbilling"
)

func (a API) managedWallet(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(managedBillingStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "managed billing is not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	wallet, err := store.ManagedWallet(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "managed wallet could not be read")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": wallet, "funding_mode": "prepaid", "funding_available": a.BillingCheckout != nil, "funding_provider": configuredManagedFundingProvider(a.BillingCheckout), "checkout_amounts_microusd": managedbilling.CheckoutAmounts()})
}

func (a API) createManagedCheckoutSession(w http.ResponseWriter, r *http.Request) {
	if a.BillingCheckout == nil {
		writeError(w, http.StatusNotImplemented, "funding_unavailable", "prepaid funding is not configured")
		return
	}
	var request struct {
		AmountMicrousd int64 `json:"amount_microusd"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if !managedbilling.ValidateCheckoutAmount(request.AmountMicrousd) {
		writeError(w, http.StatusBadRequest, "invalid_amount", "amount must be one of the advertised prepaid top-up amounts")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	session, err := a.BillingCheckout.CreateCheckoutSession(r.Context(), actor.TenantID, request.AmountMicrousd)
	if err != nil {
		writeError(w, http.StatusBadGateway, "checkout_unavailable", "payment checkout could not be created")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"checkout": session, "balance_changed": false, "credit_authority": "verified_provider_webhook"})
}

func (a API) managedStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if a.BillingCheckout == nil {
		writeError(w, http.StatusServiceUnavailable, "funding_unavailable", "payment webhook is not configured")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "payment webhook body is invalid")
		return
	}
	payment, err := a.BillingCheckout.ParseWebhook(body, r.Header.Get("Stripe-Signature"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_signature", "payment webhook could not be verified")
		return
	}
	store, ok := a.Store.(managedBillingStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "managed billing storage is not configured")
		return
	}
	result, err := store.ProcessManagedPaymentEvent(r.Context(), payment)
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "payment_conflict", "verified payment event conflicts with recorded funding intent")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "payment_processing_failed", "verified payment event could not be persisted")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"received": true, "status": result.Status, "credit_applied": result.CreditApplied})
}

func configuredManagedFundingProvider(provider any) string {
	if provider == nil {
		return ""
	}
	return "stripe"
}

func (a API) managedWalletLedger(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(managedBillingStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "managed billing is not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := store.ManagedWalletLedger(r.Context(), actor.TenantID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "managed billing ledger could not be read")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": entries, "content_recorded": false})
}

func (a API) creditManagedWallet(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(managedBillingStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "managed billing is not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if actor.ID != "bootstrap" {
		writeError(w, http.StatusForbidden, "forbidden", "only the bootstrap administrator can post externally collected prepaid funds")
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" || len(key) > 128 {
		writeError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key is required and must not exceed 128 characters")
		return
	}
	var request struct {
		TenantID       string `json:"tenant_id"`
		AmountMicrousd int64  `json:"amount_microusd"`
		Description    string `json:"description"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.TenantID) == "" || request.AmountMicrousd < 1 || strings.TrimSpace(request.Description) == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "tenant_id, a positive amount_microusd, and description are required")
		return
	}
	digest := sha256.Sum256([]byte(request.TenantID + "\x00" + key))
	creditID := "credit_" + hex.EncodeToString(digest[:16])
	wallet, err := store.CreditManagedWallet(r.Context(), request.TenantID, creditID, request.Description, request.AmountMicrousd)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "tenant was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "credit_rejected", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: request.TenantID, Actor: actor.Name, Action: "managed_wallet.credit", ResourceType: "tenant", ResourceName: request.TenantID, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"data": wallet, "credit_id": creditID, "payment_collected_by_infercrane": false})
}

func (a API) managedUsageReservations(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(managedBillingStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "managed billing is not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if actor.ID != "bootstrap" {
		writeError(w, http.StatusForbidden, "forbidden", "only the bootstrap administrator can inspect cross-tenant billing reservations")
		return
	}
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if tenant == "" {
		writeError(w, http.StatusBadRequest, "tenant_required", "tenant_id is required")
		return
	}
	rows, err := store.ManagedUsageReservations(r.Context(), tenant, state, limit)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "reservation_query_rejected", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows, "content_recorded": false})
}

func (a API) settleManagedUsage(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(managedBillingStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "managed billing is not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if actor.ID != "bootstrap" {
		writeError(w, http.StatusForbidden, "forbidden", "only the bootstrap administrator can reconcile managed usage")
		return
	}
	var request struct {
		TenantID     string `json:"tenant_id"`
		InputTokens  *int   `json:"input_tokens"`
		OutputTokens *int   `json:"output_tokens"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	request.TenantID = strings.TrimSpace(request.TenantID)
	reservationID := strings.TrimSpace(r.PathValue("id"))
	if request.TenantID == "" || reservationID == "" || request.InputTokens == nil || request.OutputTokens == nil || *request.InputTokens < 0 || *request.OutputTokens < 0 {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "tenant_id and non-negative input_tokens and output_tokens are required")
		return
	}
	row, err := store.SettleManagedUsage(r.Context(), request.TenantID, reservationID, domain.ManagedUsageSettlement{InputTokens: request.InputTokens, OutputTokens: request.OutputTokens})
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "managed usage reservation was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "settlement_rejected", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: request.TenantID, Actor: actor.Name, Action: "managed_usage.settle", ResourceType: "managed_usage_reservation", ResourceName: reservationID, Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"data": row, "content_recorded": false})
}

func (a API) releaseManagedUsage(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(managedBillingStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "managed billing is not supported by this store")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if actor.ID != "bootstrap" {
		writeError(w, http.StatusForbidden, "forbidden", "only the bootstrap administrator can release managed usage reservations")
		return
	}
	var request struct {
		TenantID string `json:"tenant_id"`
		Reason   string `json:"reason"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	request.TenantID, request.Reason = strings.TrimSpace(request.TenantID), strings.TrimSpace(request.Reason)
	reservationID := strings.TrimSpace(r.PathValue("id"))
	if request.TenantID == "" || request.Reason == "" || reservationID == "" {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "tenant_id and reason are required")
		return
	}
	if err := store.ReleaseManagedUsage(r.Context(), request.TenantID, reservationID, request.Reason); errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "managed usage reservation was not found")
		return
	} else if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "release_rejected", err.Error())
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: request.TenantID, Actor: actor.Name, Action: "managed_usage.release", ResourceType: "managed_usage_reservation", ResourceName: reservationID, Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"released": true, "reservation_id": reservationID})
}
