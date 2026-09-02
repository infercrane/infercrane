package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/modelapisupply"
	"github.com/infercrane/infercrane/internal/store"
)

type fakeModelAPIOperatorStore struct {
	*fakeStore
	offer modelapisupply.Offer
}

func (f *fakeModelAPIOperatorStore) SaveManagedModelAPIProduct(_ context.Context, value modelapiproduct.Product) (modelapiproduct.Product, error) {
	return value, nil
}
func (f *fakeModelAPIOperatorStore) PublishModelAPIRetailRate(_ context.Context, value modelapiproduct.RetailRate) (modelapiproduct.RetailRate, error) {
	return value, nil
}
func (f *fakeModelAPIOperatorStore) PublishModelAPISupplierOffer(_ context.Context, _ string, value modelapisupply.Offer) (modelapisupply.Offer, error) {
	f.offer = value
	return value, nil
}
func (f *fakeModelAPIOperatorStore) PublishModelAPISupplyQualification(_ context.Context, _ string, _ string, _ int64, value modelapisupply.QualificationEvidence) (modelapisupply.QualificationEvidence, error) {
	return value, nil
}
func (f *fakeModelAPIOperatorStore) CompileAndPublishModelAPISupplyPlan(_ context.Context, _ store.SupplyPlanDraft) (modelapisupply.Plan, error) {
	return modelapisupply.Plan{Status: modelapisupply.StatusReady}, nil
}
func (f *fakeModelAPIOperatorStore) SaveModelAPIOperatorPublication(_ context.Context, _ string, value modelapiproduct.OperatorPublication) (modelapiproduct.OperatorPublication, error) {
	return value, nil
}
func (f *fakeModelAPIOperatorStore) SaveModelAPIProductEntitlement(_ context.Context, _ string, value modelapiproduct.ProductEntitlement) (modelapiproduct.ProductEntitlement, error) {
	return value, nil
}

func TestModelAPIOperatorMutationRequiresConfiguredPlatformWorkspace(t *testing.T) {
	store := &fakeModelAPIOperatorStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret", ModelAPIOperatorTenantID: "platform-operator"}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/model-api/offers", bytes.NewReader([]byte(`{}`)))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestModelAPIOperatorOfferPinsAuthenticatedWorkspace(t *testing.T) {
	operatorStore := &fakeModelAPIOperatorStore{fakeStore: &fakeStore{}}
	handler := (API{Store: operatorStore, APIKey: "secret", ModelAPIOperatorTenantID: "global"}).Handler()
	offer := modelapisupply.Offer{ID: "offer-one", Version: 1, ProductID: "deepseek-v4-flash", Supplier: "deepseek", Adapter: "openai", SupplierModelID: "deepseek-v4-flash", Protocol: "openai", TupleKey: "sha256:test", Region: "global", CredentialReference: "secret-ref", State: "active", Capabilities: []string{"chat-completions", "streaming"}, Access: "ready", Availability: "available", Health: "healthy"}
	body, err := json.Marshal(offer)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/model-api/offers", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || operatorStore.offer.OperatorTenantID != "global" {
		t.Fatalf("status=%d offer=%+v body=%s", response.Code, operatorStore.offer, response.Body.String())
	}
}
