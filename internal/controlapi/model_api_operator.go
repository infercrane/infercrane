package controlapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/modelapisupply"
	"github.com/infercrane/infercrane/internal/modelapitarget"
	"github.com/infercrane/infercrane/internal/store"
)

type modelAPIOperatorStore interface {
	SaveManagedModelAPIProduct(context.Context, modelapiproduct.Product) (modelapiproduct.Product, error)
	PublishModelAPIRetailRate(context.Context, modelapiproduct.RetailRate) (modelapiproduct.RetailRate, error)
	PublishModelAPISupplierOffer(context.Context, string, modelapisupply.Offer) (modelapisupply.Offer, error)
	PublishModelAPISupplyQualification(context.Context, string, string, int64, modelapisupply.QualificationEvidence) (modelapisupply.QualificationEvidence, error)
	PublishModelAPITargetBinding(context.Context, string, modelapitarget.Binding) (modelapitarget.Binding, error)
	CompileAndPublishModelAPISupplyPlan(context.Context, store.SupplyPlanDraft) (modelapisupply.Plan, error)
	SaveModelAPIOperatorPublication(context.Context, string, modelapiproduct.OperatorPublication) (modelapiproduct.OperatorPublication, error)
	SaveModelAPIProductEntitlement(context.Context, string, modelapiproduct.ProductEntitlement) (modelapiproduct.ProductEntitlement, error)
}

func (a API) modelAPIOperator(w http.ResponseWriter, r *http.Request) (modelAPIOperatorStore, domain.Principal, bool) {
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	if a.ModelAPIOperatorTenantID == "" {
		writeError(w, http.StatusServiceUnavailable, "model_api_operator_unconfigured", "shared Model API publication is not configured")
		return nil, domain.Principal{}, false
	}
	if actor.TenantID != a.ModelAPIOperatorTenantID {
		writeError(w, http.StatusForbidden, "forbidden", "only the configured platform operator workspace may publish shared Model API routes")
		return nil, domain.Principal{}, false
	}
	operator, ok := a.Store.(modelAPIOperatorStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "Model API operator persistence is unavailable")
		return nil, domain.Principal{}, false
	}
	return operator, actor, true
}

func (a API) publishModelAPIProduct(w http.ResponseWriter, r *http.Request) {
	operator, actor, ok := a.modelAPIOperator(w, r)
	if !ok {
		return
	}
	var product modelapiproduct.Product
	if !decodeMutationBody(w, r, &product) {
		return
	}
	stored, err := operator.SaveManagedModelAPIProduct(r.Context(), product)
	if modelAPIOperatorError(w, err) {
		return
	}
	a.auditModelAPIMutation(r, actor, "model_api.product.publish", "model_api_product", stored.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"product": stored})
}

func (a API) publishModelAPIRetailRate(w http.ResponseWriter, r *http.Request) {
	operator, actor, ok := a.modelAPIOperator(w, r)
	if !ok {
		return
	}
	var rate modelapiproduct.RetailRate
	if !decodeMutationBody(w, r, &rate) {
		return
	}
	stored, err := operator.PublishModelAPIRetailRate(r.Context(), rate)
	if modelAPIOperatorError(w, err) {
		return
	}
	a.auditModelAPIMutation(r, actor, "model_api.rate.publish", "model_api_retail_rate", stored.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"rate": stored})
}

func (a API) publishModelAPISupplierOffer(w http.ResponseWriter, r *http.Request) {
	operator, actor, ok := a.modelAPIOperator(w, r)
	if !ok {
		return
	}
	var offer modelapisupply.Offer
	if !decodeMutationBody(w, r, &offer) {
		return
	}
	if offer.OperatorTenantID != "" && offer.OperatorTenantID != actor.TenantID {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "supplier offer operator tenant does not match the authenticated platform operator")
		return
	}
	offer.OperatorTenantID = actor.TenantID
	stored, err := operator.PublishModelAPISupplierOffer(r.Context(), actor.TenantID, offer)
	if modelAPIOperatorError(w, err) {
		return
	}
	a.auditModelAPIMutation(r, actor, "model_api.offer.publish", "model_api_supplier_offer", stored.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"offer": stored})
}

func (a API) publishModelAPISupplyQualification(w http.ResponseWriter, r *http.Request) {
	operator, actor, ok := a.modelAPIOperator(w, r)
	if !ok {
		return
	}
	var request struct {
		OfferID      string                               `json:"offer_id"`
		OfferVersion int64                                `json:"offer_version"`
		Evidence     modelapisupply.QualificationEvidence `json:"evidence"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	stored, err := operator.PublishModelAPISupplyQualification(r.Context(), actor.TenantID, request.OfferID, request.OfferVersion, request.Evidence)
	if modelAPIOperatorError(w, err) {
		return
	}
	a.auditModelAPIMutation(r, actor, "model_api.qualification.publish", "model_api_supply_qualification", stored.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"qualification": stored})
}

func (a API) publishModelAPITargetBinding(w http.ResponseWriter, r *http.Request) {
	operator, actor, ok := a.modelAPIOperator(w, r)
	if !ok {
		return
	}
	var binding modelapitarget.Binding
	if !decodeMutationBody(w, r, &binding) {
		return
	}
	// OperatorTenantID participates in the immutable contract digest, so the
	// server must reject a mismatch rather than rewriting it after decoding.
	if binding.OperatorTenantID != actor.TenantID {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "target binding operator tenant does not match the authenticated platform operator")
		return
	}
	stored, err := operator.PublishModelAPITargetBinding(r.Context(), actor.TenantID, binding)
	if modelAPIOperatorError(w, err) {
		return
	}
	a.auditModelAPIMutation(r, actor, "model_api.target_binding.publish", "model_api_target_binding", stored.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"target_binding": stored})
}

func (a API) compileModelAPISupplyPlan(w http.ResponseWriter, r *http.Request) {
	operator, actor, ok := a.modelAPIOperator(w, r)
	if !ok {
		return
	}
	var draft store.SupplyPlanDraft
	if !decodeMutationBody(w, r, &draft) {
		return
	}
	if draft.OperatorTenantID != "" && draft.OperatorTenantID != actor.TenantID {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "supply plan operator tenant does not match the authenticated platform operator")
		return
	}
	draft.OperatorTenantID = actor.TenantID
	plan, err := operator.CompileAndPublishModelAPISupplyPlan(r.Context(), draft)
	if modelAPIOperatorError(w, err) {
		return
	}
	a.auditModelAPIMutation(r, actor, "model_api.plan.compile", "model_api_supply_plan", draft.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"plan": plan})
}

func (a API) publishModelAPIOperatorRoute(w http.ResponseWriter, r *http.Request) {
	operator, actor, ok := a.modelAPIOperator(w, r)
	if !ok {
		return
	}
	var publication modelapiproduct.OperatorPublication
	if !decodeMutationBody(w, r, &publication) {
		return
	}
	if publication.OperatorWorkspaceID != "" && publication.OperatorWorkspaceID != actor.TenantID {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "publication workspace does not match the authenticated platform operator")
		return
	}
	publication.OperatorWorkspaceID = actor.TenantID
	stored, err := operator.SaveModelAPIOperatorPublication(r.Context(), actor.TenantID, publication)
	if modelAPIOperatorError(w, err) {
		return
	}
	a.auditModelAPIMutation(r, actor, "model_api.route.publish", "model_api_operator_publication", stored.ProductID)
	writeJSON(w, http.StatusCreated, map[string]any{"publication": stored})
}

func (a API) publishModelAPIEntitlement(w http.ResponseWriter, r *http.Request) {
	operator, actor, ok := a.modelAPIOperator(w, r)
	if !ok {
		return
	}
	var entitlement modelapiproduct.ProductEntitlement
	if !decodeMutationBody(w, r, &entitlement) {
		return
	}
	if entitlement.OperatorWorkspaceID != "" && entitlement.OperatorWorkspaceID != actor.TenantID {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "entitlement operator workspace does not match the authenticated platform operator")
		return
	}
	entitlement.OperatorWorkspaceID = actor.TenantID
	stored, err := operator.SaveModelAPIProductEntitlement(r.Context(), entitlement.CustomerWorkspaceID, entitlement)
	if modelAPIOperatorError(w, err) {
		return
	}
	a.auditModelAPIMutation(r, actor, "model_api.entitlement.publish", "model_api_product_entitlement", stored.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"entitlement": stored})
}

func modelAPIOperatorError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusUnprocessableEntity, "dependency_not_found", "a referenced immutable Model API contract was not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "immutable_contract_conflict", err.Error())
	default:
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
	}
	return true
}

func (a API) auditModelAPIMutation(r *http.Request, actor domain.Principal, action, resourceType, resourceName string) {
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{
		TenantID: actor.TenantID, Actor: actor.Name, Action: action,
		ResourceType: resourceType, ResourceName: resourceName, Outcome: "succeeded", CreatedAt: time.Now().UTC(),
	})
}
