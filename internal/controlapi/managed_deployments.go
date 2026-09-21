package controlapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/managedbilling"
	"github.com/infercrane/infercrane/internal/provision"
)

type managedDeploymentQuoteRequest struct {
	Provider       string `json:"provider"`
	Region         string `json:"region,omitempty"`
	GPU            string `json:"gpu"`
	GPUCount       int    `json:"gpu_count"`
	ComputeMode    string `json:"compute_mode"`
	RuntimeSeconds int    `json:"runtime_seconds"`
}

// managedDeploymentPublicQuote is the customer contract. Supplier cost,
// margin policy, and procurement provenance deliberately remain server-side.
type managedDeploymentPublicQuote struct {
	State                   string    `json:"state"`
	Provider                string    `json:"provider"`
	Region                  string    `json:"region,omitempty"`
	GPU                     string    `json:"gpu"`
	GPUCount                int       `json:"gpu_count"`
	Currency                string    `json:"currency"`
	RetailHourlyMicrousd    int64     `json:"retail_hourly_microusd"`
	ReservedMicrousd        int64     `json:"reserved_microusd"`
	RuntimeLimitSeconds     int       `json:"runtime_limit_seconds"`
	CleanupAllowanceSeconds int       `json:"cleanup_allowance_seconds"`
	ObservedAt              time.Time `json:"observed_at"`
	ValidUntil              time.Time `json:"valid_until"`
}

func publicManagedDeploymentQuote(quote managedbilling.DeploymentQuote) managedDeploymentPublicQuote {
	return managedDeploymentPublicQuote{
		State: quote.State, Provider: quote.Provider, Region: quote.Region, GPU: quote.GPU,
		GPUCount: quote.GPUCount, Currency: quote.Currency,
		RetailHourlyMicrousd: quote.RetailHourlyMicrousd, ReservedMicrousd: quote.ReservedMicrousd,
		RuntimeLimitSeconds: quote.RuntimeLimitSeconds, CleanupAllowanceSeconds: quote.CleanupAllowanceSeconds,
		ObservedAt: quote.ObservedAt, ValidUntil: quote.ValidUntil,
	}
}

func (a API) managedDeploymentQuote(w http.ResponseWriter, r *http.Request) {
	var request managedDeploymentQuoteRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must be one strict JSON object")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain exactly one JSON object")
		return
	}
	quote, err := a.quoteManagedDeployment(request, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "managed_quote_unavailable", err.Error())
		return
	}
	store, ok := a.Store.(managedBillingStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "managed billing storage is not configured")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	wallet, err := store.ManagedWallet(r.Context(), actor.TenantID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "managed wallet could not be read")
		return
	}
	shortfall := quote.ReservedMicrousd - wallet.AvailableMicrousd
	if shortfall < 0 {
		shortfall = 0
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"quote": publicManagedDeploymentQuote(quote), "wallet": wallet, "affordable": shortfall == 0, "shortfall_microusd": shortfall,
		"funding_mode": "prepaid", "reservation_is_charge": false,
		"disclosure": "The displayed amount is a hold. Metered runtime is settled after provider cleanup and unused credit is released.",
	})
}

func (a API) quoteManagedDeployment(request managedDeploymentQuoteRequest, current time.Time) (managedbilling.DeploymentQuote, error) {
	policy := a.ManagedDeployments.Normalize()
	if !policy.Enabled {
		return managedbilling.DeploymentQuote{}, errors.New("InferCrane Cloud is not enabled on this control plane")
	}
	request.Provider = strings.ToLower(strings.TrimSpace(request.Provider))
	request.Region, request.GPU = strings.TrimSpace(request.Region), strings.TrimSpace(request.GPU)
	if request.GPUCount == 0 {
		request.GPUCount = 1
	}
	if request.Provider != policy.Provider || request.ComputeMode != "elastic" || request.GPUCount != 1 {
		return managedbilling.DeploymentQuote{}, errors.New("InferCrane Cloud currently supports one elastic RunPod GPU")
	}
	catalog := a.catalogLaunchQuote(provision.LaunchProbeRequest{Provider: request.Provider, Region: request.Region, GPU: request.GPU, GPUCount: request.GPUCount}, current)
	if catalog.State != "current" || catalog.HourlyUSD == nil || catalog.Currency != "USD" || catalog.Source == "" || catalog.ObservedAt.IsZero() || catalog.ValidUntil.IsZero() {
		return managedbilling.DeploymentQuote{}, errors.New("a current exact provider price is required before prepaid credit can be held")
	}
	return managedbilling.QuoteDeployment(policy, request.Provider, catalog.Region, request.GPU, catalog.Source, request.GPUCount, *catalog.HourlyUSD, time.Duration(request.RuntimeSeconds)*time.Second, catalog.ObservedAt, catalog.ValidUntil)
}
