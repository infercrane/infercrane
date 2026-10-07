package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/modelapirouting"
	"github.com/infercrane/infercrane/internal/modelapisupply"
	"github.com/infercrane/infercrane/internal/modelapitarget"
	"github.com/infercrane/infercrane/internal/store"
)

type fakeModelAPIOperatorStore struct {
	*fakeStore
	offer                  modelapisupply.Offer
	targetBinding          modelapitarget.Binding
	targetOperator         string
	targetPublish          int
	modelAPIAudit          domain.AuditEvent
	reconciliationOperator string
	reconciliationEvidence modelapirouting.ReconciliationEvidence
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
func (f *fakeModelAPIOperatorStore) PublishModelAPITargetBinding(_ context.Context, operator string, value modelapitarget.Binding) (modelapitarget.Binding, error) {
	f.targetOperator = operator
	f.targetBinding = value
	f.targetPublish++
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
func (f *fakeModelAPIOperatorStore) Audit(_ context.Context, event domain.AuditEvent) error {
	f.modelAPIAudit = event
	return nil
}
func (f *fakeModelAPIOperatorStore) RecordOperatorModelAPIUsageReconciliationEvidence(_ context.Context, operator, actor string, evidence modelapirouting.ReconciliationEvidence) (modelapirouting.ReconciliationEvidence, bool, error) {
	evidence.RecordedBy = actor
	f.reconciliationOperator, f.reconciliationEvidence = operator, evidence
	evidence.Supplier = "deepseek"
	evidence.RecordedAt = time.Now().UTC()
	return evidence, true, nil
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

func TestModelAPIUsageRepairRecordsAuditedEvidenceWithoutDirectMoneyMutation(t *testing.T) {
	operatorStore := &fakeModelAPIOperatorStore{fakeStore: &fakeStore{}}
	handler := (API{Store: operatorStore, APIKey: "secret", ModelAPIOperatorTenantID: "global"}).Handler()
	observed := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	body, err := json.Marshal(map[string]any{
		"customer_tenant_id": "customer-one", "outcome": "no_charge",
		"supplier_request_id": "supplier-request-1", "evidence_reference": "invoice://supplier/line-7",
		"observed_at": observed,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/model-api/usage-reservations/reservation-1/evidence", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if operatorStore.reconciliationOperator != "global" || operatorStore.reconciliationEvidence.ReservationID != "reservation-1" || operatorStore.reconciliationEvidence.TenantID != "customer-one" || operatorStore.reconciliationEvidence.Authority != modelapirouting.EvidenceOperatorVerified || operatorStore.reconciliationEvidence.RecordedBy != "bootstrap" {
		t.Fatalf("operator=%q evidence=%+v", operatorStore.reconciliationOperator, operatorStore.reconciliationEvidence)
	}
	if operatorStore.modelAPIAudit.Action != "model_api_usage.reconciliation_evidence" || operatorStore.modelAPIAudit.TenantID != "customer-one" || operatorStore.modelAPIAudit.ResourceName != "reservation-1" {
		t.Fatalf("audit=%+v", operatorStore.modelAPIAudit)
	}
	if !strings.Contains(response.Body.String(), `"reconciliation":"queued"`) || !strings.Contains(response.Body.String(), `"content_recorded":false`) {
		t.Fatalf("response=%s", response.Body.String())
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

func TestModelAPIOperatorTargetBindingRequiresExactAuthenticatedWorkspace(t *testing.T) {
	operatorStore := &fakeModelAPIOperatorStore{fakeStore: &fakeStore{}}
	handler := (API{Store: operatorStore, APIKey: "secret", ModelAPIOperatorTenantID: "global"}).Handler()
	binding := targetBindingFixture(t, "different-operator")
	body, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/model-api/target-bindings", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnprocessableEntity || operatorStore.targetPublish != 0 {
		t.Fatalf("status=%d publishes=%d body=%s", response.Code, operatorStore.targetPublish, response.Body.String())
	}
}

func TestModelAPIOperatorTargetBindingPublishesAndAudits(t *testing.T) {
	operatorStore := &fakeModelAPIOperatorStore{fakeStore: &fakeStore{}}
	handler := (API{Store: operatorStore, APIKey: "secret", ModelAPIOperatorTenantID: "global"}).Handler()
	binding := targetBindingFixture(t, "global")
	body, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/admin/model-api/target-bindings", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || operatorStore.targetPublish != 1 || operatorStore.targetOperator != "global" || operatorStore.targetBinding.ContractDigest != binding.ContractDigest {
		t.Fatalf("status=%d operator=%q binding=%+v body=%s", response.Code, operatorStore.targetOperator, operatorStore.targetBinding, response.Body.String())
	}
	if operatorStore.modelAPIAudit.TenantID != "global" || operatorStore.modelAPIAudit.Action != "model_api.target_binding.publish" || operatorStore.modelAPIAudit.ResourceType != "model_api_target_binding" || operatorStore.modelAPIAudit.ResourceName != binding.ID {
		t.Fatalf("audit=%+v", operatorStore.modelAPIAudit)
	}
	var envelope struct {
		TargetBinding modelapitarget.Binding `json:"target_binding"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.TargetBinding.ContractDigest != binding.ContractDigest {
		t.Fatalf("response binding=%+v err=%v body=%s", envelope.TargetBinding, err, response.Body.String())
	}
}

func targetBindingFixture(t *testing.T, operator string) modelapitarget.Binding {
	t.Helper()
	created := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	binding, err := modelapitarget.NewBinding(modelapitarget.Draft{
		ID: "binding-one", OperatorTenantID: operator, ProductID: "deepseek-v4-flash", Kind: modelapitarget.KindUpstream,
		OfferID: "offer-one", OfferVersion: 1, Adapter: "openai", SupplierModelID: "deepseek-v4-flash",
		EndpointReference: "supplier/deepseek", EndpointConfigDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Region: "global",
		CreatedAt: created, ValidFrom: created.Add(time.Minute), ValidUntil: created.Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return binding
}
