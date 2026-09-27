package controlapi

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/store"
)

const selfServiceModelAPIMaxRequestMicrousd int64 = 2_000_000

func (a API) durableModelAPIModels(w http.ResponseWriter, r *http.Request, offset, limit int) {
	now := time.Now().UTC()
	products, err := a.ModelAPIProducts.PublicModelAPIProducts(r.Context(), now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "model_api_catalog_unavailable", "Model API catalog could not be read")
		return
	}
	products = filterModelAPIProducts(products, r, now)
	sort.SliceStable(products, func(i, j int) bool { return products[i].DisplayName < products[j].DisplayName })
	total := len(products)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	data := make([]map[string]any, 0, end-offset)
	for _, product := range products[offset:end] {
		data = append(data, modelAPIProductResponse(product, now))
	}
	var next *int
	if end < total {
		value := end
		next = &value
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data, "total": total, "next_offset": next,
		"catalog_policy": "catalog-only products are discoverable; only a current qualified publication and an active exact entitlement authorize traffic",
		"catalog_source": "durable_product_catalog",
	})
}

func (a API) durableModelAPIModel(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	product, err := a.ModelAPIProducts.PublicModelAPIProduct(r.Context(), r.PathValue("id"), now)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Model API catalog entry was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "model_api_catalog_unavailable", "Model API catalog entry could not be read")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	access, err := a.ModelAPIProducts.ModelAPIProductAccess(r.Context(), actor.TenantID, product.ID, now)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Model API catalog entry was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "model_api_access_unavailable", "Model API access could not be verified")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model":          modelAPIProductResponse(product, now),
		"access":         modelAPIProductAccessResponse(access, now),
		"catalog_source": "durable_product_catalog",
	})
}

func (a API) activateModelAPIProduct(w http.ResponseWriter, r *http.Request) {
	if a.ModelAPIProducts == nil || a.ModelAPIOperatorTenantID == "" {
		writeError(w, http.StatusServiceUnavailable, "model_api_activation_unavailable", "Model API activation is not configured")
		return
	}
	activation, ok := a.ModelAPIProducts.(modelAPIActivationStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "Model API activation persistence is unavailable")
		return
	}

	now := time.Now().UTC()
	product, err := a.ModelAPIProducts.PublicModelAPIProduct(r.Context(), r.PathValue("id"), now)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Model API catalog entry was not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "model_api_catalog_unavailable", "Model API catalog entry could not be read")
		return
	}
	if !modelAPIProductCallableAt(product, now) {
		writeError(w, http.StatusConflict, "model_unavailable", "This model does not currently have a qualified callable route")
		return
	}
	publication, err := activation.ModelAPIOperatorPublication(r.Context(), a.ModelAPIOperatorTenantID, product.ID)
	if err != nil {
		writeError(w, http.StatusConflict, "model_unavailable", "This model does not currently have an activatable route")
		return
	}
	if publication.RetailRate == nil || !publication.Qualification.CurrentAt(now) || !publication.RetailRate.CurrentAt(now) {
		writeError(w, http.StatusConflict, "model_unavailable", "This model's qualification or rate is no longer current")
		return
	}

	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if actor.TenantID == a.ModelAPIOperatorTenantID {
		writeError(w, http.StatusConflict, "operator_workspace", "The platform operator workspace cannot activate customer Model API access")
		return
	}
	id := modelAPIEntitlementID(actor.TenantID, product.ID)
	createdAt := now
	if existing, existingErr := activation.ModelAPIProductEntitlement(r.Context(), actor.TenantID, product.ID); existingErr == nil {
		id, createdAt = existing.ID, existing.CreatedAt.UTC()
	} else if !errors.Is(existingErr, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "model_api_access_unavailable", "Existing Model API access could not be verified")
		return
	}
	maxRequest := selfServiceModelAPIMaxRequestMicrousd
	entitlement := modelapiproduct.ProductEntitlement{
		SchemaVersion: modelapiproduct.EntitlementSchemaVersion,
		ID:            id, CustomerWorkspaceID: actor.TenantID, ProductID: product.ID,
		OperatorWorkspaceID: a.ModelAPIOperatorTenantID, ServingPlanID: publication.ServingPlanID,
		RetailRateID: publication.RetailRate.ID, RetailRateVersion: publication.RetailRate.Version,
		State: modelapiproduct.EntitlementActive, Limits: modelapiproduct.CustomerLimits{MaxRequestMicrousd: &maxRequest},
		ValidFrom: now.Add(-time.Minute), CreatedAt: createdAt, UpdatedAt: now,
	}
	stored, err := activation.SaveModelAPIProductEntitlement(r.Context(), actor.TenantID, entitlement)
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "model_activation_conflict", "Model access changed while activation was in progress; refresh and retry")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "model_activation_failed", "Model access could not be activated")
		return
	}
	projection, err := stored.CustomerProjection()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "model_api_access_unavailable", "Activated Model API access could not be projected")
		return
	}
	a.auditModelAPIMutation(r, actor, "model_api.entitlement.activate", "model_api_product_entitlement", stored.ID)
	writeJSON(w, http.StatusOK, map[string]any{"access": map[string]any{"authorized": true, "entitlement": projection}})
}

func modelAPIEntitlementID(tenant, product string) string {
	digest := sha256.Sum256([]byte(tenant + "\x00" + product))
	return "entitlement-" + hex.EncodeToString(digest[:16])
}

func filterModelAPIProducts(products []modelapiproduct.PublicProjection, r *http.Request, now time.Time) []modelapiproduct.PublicProjection {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	task := r.URL.Query().Get("task")
	capability := r.URL.Query().Get("capability")
	publisher := r.URL.Query().Get("publisher")
	access := r.URL.Query().Get("access")
	filtered := make([]modelapiproduct.PublicProjection, 0, len(products))
	for _, product := range products {
		capabilities := modelAPIProductCapabilities(product)
		if task != "" && !modelAPIStringContains(product.Tasks, task) ||
			capability != "" && !modelAPIStringContains(capabilities, capability) ||
			publisher != "" && modelAPIPublisherSlug(product.Publisher) != publisher ||
			access != "" && modelAPIProductAccess(product, now) != access {
			continue
		}
		if query != "" {
			haystack := strings.ToLower(strings.Join(append([]string{product.ID, product.DisplayName, product.Publisher, product.Description}, product.Tasks...), " "))
			if !strings.Contains(haystack, query) {
				continue
			}
		}
		filtered = append(filtered, product)
	}
	return filtered
}

func modelAPIProductResponse(product modelapiproduct.PublicProjection, now time.Time) map[string]any {
	capabilities := modelAPIProductCapabilities(product)
	callable := modelAPIProductCallableAt(product, now)
	offers := make([]map[string]any, 0, 1)
	if rate := product.RetailRate; rate != nil && rate.CachedInputMicrousdPerMillion == nil && rate.CurrentAt(now) {
		pricing := map[string]any{
			"currency": rate.Currency, "input_microusd_per_million": rate.InputMicrousdPerMillion,
			"output_microusd_per_million": rate.OutputMicrousdPerMillion,
			"provenance":                  modelapiproduct.CustomerRetailRateProvenance, "observed_at": rate.PublishedAt,
			"valid_until": rate.ValidUntil, "version": rate.Version,
		}
		availability := "unknown"
		if callable {
			availability = "available"
		} else if product.Availability == modelapiproduct.AvailabilityUnavailable {
			availability = "unavailable"
		}
		offers = append(offers, map[string]any{
			"id": "infercrane-standard", "protocol": product.Protocol,
			"access": modelAPIProductAccess(product, now), "availability": availability,
			"capabilities": capabilities, "pricing": pricing,
		})
	}
	qualification := modelAPIProductQualification(product, now)
	response := map[string]any{
		"schema_version": product.SchemaVersion, "id": product.ID,
		"display_name": product.DisplayName, "publisher": product.Publisher,
		"publisher_slug": modelAPIPublisherSlug(product.Publisher), "family": product.DisplayName,
		"description": product.Description, "tasks": product.Tasks, "capabilities": capabilities,
		"input_modalities": product.InputModalities, "output_modalities": product.OutputModalities,
		"context_window_tokens": product.ContextWindowTokens, "access": modelAPIProductAccess(product, now),
		"qualification": qualification, "qualification_note": modelAPIProductQualificationNote(product, now),
		"availability": product.Availability, "self_host_eligibility": product.SelfHostEligibility,
		"callable": callable, "offers": offers,
	}
	if product.EvidenceValidUntil != nil && product.EvidenceValidUntil.After(now) {
		response["evidence_valid_until"] = product.EvidenceValidUntil
	}
	return response
}

func modelAPIProductAccessResponse(access store.ModelAPIProductAccess, now time.Time) map[string]any {
	authorized := access.Authorized && modelAPIProductCallableAt(access.Product, now) && modelAPIEntitlementActiveAt(access.Entitlement, access.Product, now)
	response := map[string]any{"authorized": authorized}
	if access.Entitlement != nil {
		response["entitlement"] = access.Entitlement
	}
	return response
}

func modelAPIEntitlementActiveAt(entitlement *modelapiproduct.CustomerEntitlementProjection, product modelapiproduct.PublicProjection, now time.Time) bool {
	if entitlement == nil || entitlement.State != modelapiproduct.EntitlementActive || now.Before(entitlement.ValidFrom.UTC()) {
		return false
	}
	if entitlement.ValidUntil != nil && !now.Before(entitlement.ValidUntil.UTC()) {
		return false
	}
	return entitlement.Limits.SupportedForAdmission() && product.RetailRate != nil && entitlement.RetailRateVersion == product.RetailRate.Version
}

func modelAPIProductCallableAt(product modelapiproduct.PublicProjection, now time.Time) bool {
	return product.Callable && product.HasCurrentCallableCapabilityAt(now) && product.RetailRate != nil && product.RetailRate.CachedInputMicrousdPerMillion == nil && product.RetailRate.CurrentAt(now) &&
		product.Qualification == modelapiproduct.QualificationQualified && product.EvidenceValidUntil != nil && product.EvidenceValidUntil.After(now)
}

func modelAPIProductAccess(product modelapiproduct.PublicProjection, now time.Time) string {
	if modelAPIProductCallableAt(product, now) {
		return "ready"
	}
	if product.Availability == modelapiproduct.AvailabilityPrivatePreview || product.Availability == modelapiproduct.AvailabilityUnavailable {
		return "request-access"
	}
	return "managed-preview"
}

func modelAPIProductQualification(product modelapiproduct.PublicProjection, now time.Time) string {
	if modelAPIProductCallableAt(product, now) {
		return "measured"
	}
	return "cataloged"
}

func modelAPIProductQualificationNote(product modelapiproduct.PublicProjection, now time.Time) string {
	if modelAPIProductCallableAt(product, now) {
		return "Current route qualification and an InferCrane retail rate are attached to this product."
	}
	if product.Qualification == modelapiproduct.QualificationStale || product.EvidenceValidUntil != nil && !product.EvidenceValidUntil.After(now) {
		return "The prior route qualification is stale; traffic admission and performance claims remain disabled."
	}
	return "Model identity is cataloged; managed availability, rate, performance, and production qualification are not attached."
}

func modelAPIProductCapabilities(product modelapiproduct.PublicProjection) []string {
	capabilities := make([]string, 0, len(product.Capabilities))
	for _, capability := range product.Capabilities {
		capabilities = append(capabilities, capability.Name)
	}
	return capabilities
}

func modelAPIStringContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func modelAPIPublisherSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var slug strings.Builder
	separator := false
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			if separator && slug.Len() > 0 {
				slug.WriteByte('-')
			}
			slug.WriteRune(char)
			separator = false
		} else {
			separator = true
		}
	}
	return strings.Trim(slug.String(), "-")
}
