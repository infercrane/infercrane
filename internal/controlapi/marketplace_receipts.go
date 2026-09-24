package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/openrouterprovider"
)

type marketplaceReceiptStore interface {
	RecordMarketplaceRequestReceipt(context.Context, openrouterprovider.RequestReceipt) error
	MarketplaceRequestCosts(context.Context, string, []string) ([]openrouterprovider.RequestCost, error)
}

func (a API) marketplaceReceiptOperator(w http.ResponseWriter, r *http.Request) (marketplaceReceiptStore, bool) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if a.ModelAPIOperatorTenantID == "" {
		writeError(w, http.StatusServiceUnavailable, "marketplace_accounting_unconfigured", "marketplace accounting is not configured")
		return nil, false
	}
	if actor.TenantID != a.ModelAPIOperatorTenantID {
		writeError(w, http.StatusForbidden, "forbidden", "only the configured platform operator workspace may access marketplace accounting")
		return nil, false
	}
	ledger, ok := a.Store.(marketplaceReceiptStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "marketplace receipt persistence is unavailable")
		return nil, false
	}
	return ledger, true
}

func (a API) recordMarketplaceReceipt(w http.ResponseWriter, r *http.Request) {
	ledger, ok := a.marketplaceReceiptOperator(w, r)
	if !ok {
		return
	}
	var input struct {
		Receipt openrouterprovider.RequestReceipt `json:"receipt"`
	}
	if !decodeMarketplaceBody(w, r, &input) {
		return
	}
	if err := input.Receipt.Validate(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	if err := ledger.RecordMarketplaceRequestReceipt(r.Context(), input.Receipt); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			writeError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal", "marketplace receipt could not be recorded")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{"recorded": true})
}

func (a API) marketplaceBillingRequests(w http.ResponseWriter, r *http.Request) {
	ledger, ok := a.marketplaceReceiptOperator(w, r)
	if !ok {
		return
	}
	var input struct {
		Channel    string   `json:"channel"`
		RequestIDs []string `json:"request_ids"`
	}
	if !decodeMarketplaceBody(w, r, &input) {
		return
	}
	requests, err := ledger.MarketplaceRequestCosts(r.Context(), input.Channel, input.RequestIDs)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
		return
	}
	var responseRequests any = requests
	if len(requests) == 0 {
		responseRequests = nil
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"requests": responseRequests})
}

func decodeMarketplaceBody(w http.ResponseWriter, r *http.Request, destination any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return false
	}
	return true
}
