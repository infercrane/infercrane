package controlapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelapiproduct"
	"github.com/infercrane/infercrane/internal/store"
)

type fakeModelAPIProductStore struct {
	products       []modelapiproduct.PublicProjection
	accessByTenant map[string]store.ModelAPIProductAccess
	err            error
	accessTenants  []string
}

func (f *fakeModelAPIProductStore) PublicModelAPIProducts(context.Context, time.Time) ([]modelapiproduct.PublicProjection, error) {
	return append([]modelapiproduct.PublicProjection(nil), f.products...), f.err
}

func (f *fakeModelAPIProductStore) PublicModelAPIProduct(_ context.Context, productID string, _ time.Time) (modelapiproduct.PublicProjection, error) {
	if f.err != nil {
		return modelapiproduct.PublicProjection{}, f.err
	}
	for _, product := range f.products {
		if product.ID == productID {
			return product, nil
		}
	}
	return modelapiproduct.PublicProjection{}, store.ErrNotFound
}

func (f *fakeModelAPIProductStore) ModelAPIProductAccess(_ context.Context, tenant, productID string, _ time.Time) (store.ModelAPIProductAccess, error) {
	f.accessTenants = append(f.accessTenants, tenant)
	if f.err != nil {
		return store.ModelAPIProductAccess{}, f.err
	}
	if access, ok := f.accessByTenant[tenant]; ok && access.Product.ID == productID {
		return access, nil
	}
	for _, product := range f.products {
		if product.ID == productID {
			return store.ModelAPIProductAccess{Product: product}, nil
		}
	}
	return store.ModelAPIProductAccess{}, store.ErrNotFound
}

func TestDurableModelAPICatalogPreservesPublicContractAndRedactsInternals(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	product := callableModelAPIProduct(t, now)
	durable := &fakeModelAPIProductStore{products: []modelapiproduct.PublicProjection{product}}
	handler := (API{Store: &fakeStore{}, APIKey: "secret", ModelAPIProducts: durable}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-catalog?task=coding&limit=1", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	for _, expected := range []string{`"catalog_source":"durable_product_catalog"`, `"display_name":"GLM-5.2"`, `"access":"ready"`, `"callable":true`, `"id":"infercrane-standard"`, `"version":1`} {
		if response.Code != http.StatusOK || !strings.Contains(body, expected) {
			t.Fatalf("status=%d missing=%q body=%s", response.Code, expected, body)
		}
	}
	for _, secret := range []string{"operator_workspace_id", "serving_plan_id", "supply_plan_id", "contract_digest", "rate-glm-1", "supplier_model_id", "credential"} {
		if strings.Contains(body, secret) {
			t.Fatalf("durable catalog leaked private field %q: %s", secret, body)
		}
	}
}

func TestDurableModelAPICatalogHidesExpiredRateAndFailsClosed(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	product := callableModelAPIProduct(t, now.Add(-48*time.Hour))
	// Deliberately retain the stale projection's prior callable bit. The HTTP
	// boundary must independently reject expired rate and evidence windows.
	product.Callable = true
	durable := &fakeModelAPIProductStore{products: []modelapiproduct.PublicProjection{product}}
	handler := (API{Store: &fakeStore{}, APIKey: "secret", ModelAPIProducts: durable}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-catalog", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"callable":false`) || !strings.Contains(body, `"offers":[]`) || strings.Contains(body, `"pricing"`) {
		t.Fatalf("expired durable product was not closed: status=%d body=%s", response.Code, body)
	}
}

func TestDurableModelAPIAccessIsTenantScopedAndRequiresExactActiveEntitlement(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	product := callableModelAPIProduct(t, now)
	validUntil := now.Add(time.Hour)
	maxRequest := int64(100_000)
	entitlement := &modelapiproduct.CustomerEntitlementProjection{
		SchemaVersion: modelapiproduct.CustomerEntitlementSchemaVersion,
		ID:            "entitlement-1", ProductID: product.ID, RetailRateVersion: product.RetailRate.Version,
		State: modelapiproduct.EntitlementActive, Limits: modelapiproduct.CustomerLimits{MaxRequestMicrousd: &maxRequest}, ValidFrom: now.Add(-time.Hour), ValidUntil: &validUntil,
	}
	durable := &fakeModelAPIProductStore{
		products: []modelapiproduct.PublicProjection{product},
		accessByTenant: map[string]store.ModelAPIProductAccess{
			"tenant-a": {Product: product, Entitlement: entitlement, Authorized: true},
		},
	}
	principalStore := &fakeStore{principal: domain.Principal{ID: "reader", TenantID: "tenant-a", Name: "reader", Role: string(authz.Viewer), Scopes: []string{"read"}}}
	handler := (API{Store: principalStore, Authenticator: principalStore, ModelAPIProducts: durable}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-catalog/glm-5.2", nil)
	request.Header.Set("Authorization", "Bearer tenant-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"authorized":true`) || len(durable.accessTenants) != 1 || durable.accessTenants[0] != "tenant-a" {
		t.Fatalf("tenant access status=%d tenants=%v body=%s", response.Code, durable.accessTenants, body)
	}
	for _, secret := range []string{"operator_workspace_id", "serving_plan_id", "supply_plan_id", "retail_rate_id", "customer_workspace_id"} {
		if strings.Contains(body, secret) {
			t.Fatalf("tenant access leaked private field %q: %s", secret, body)
		}
	}

	wrongVersion := *entitlement
	wrongVersion.RetailRateVersion++
	durable.accessByTenant["tenant-a"] = store.ModelAPIProductAccess{Product: product, Entitlement: &wrongVersion, Authorized: true}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"authorized":false`) {
		t.Fatalf("mismatched entitlement authorized: status=%d body=%s", response.Code, response.Body.String())
	}

	unsupported := *entitlement
	rpm := int64(60)
	unsupported.Limits.RequestsPerMinute = &rpm
	durable.accessByTenant["tenant-a"] = store.ModelAPIProductAccess{Product: product, Entitlement: &unsupported, Authorized: true}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"authorized":false`) {
		t.Fatalf("unenforced entitlement limit authorized: status=%d body=%s", response.Code, response.Body.String())
	}

	expired := *entitlement
	past := now.Add(-time.Minute)
	expired.ValidUntil = &past
	durable.accessByTenant["tenant-a"] = store.ModelAPIProductAccess{Product: product, Entitlement: &expired, Authorized: true}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"authorized":false`) {
		t.Fatalf("expired entitlement authorized: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDurableModelAPICatalogSuppressesUnsupportedCachedInputPricing(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	product := callableModelAPIProduct(t, now)
	cached := int64(100_000)
	product.RetailRate.CachedInputMicrousdPerMillion = &cached
	// Simulate a stale or independently produced projection. The HTTP boundary
	// must not trust its previous callable bit or expose unsupported pricing.
	product.Callable = true
	durable := &fakeModelAPIProductStore{products: []modelapiproduct.PublicProjection{product}}
	handler := (API{Store: &fakeStore{}, APIKey: "secret", ModelAPIProducts: durable}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-catalog", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"callable":false`) || !strings.Contains(response.Body.String(), `"offers":[]`) || strings.Contains(response.Body.String(), "cached_input_microusd_per_million") {
		t.Fatalf("unsupported cached-input price was published: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDurableModelAPIAccessDoesNotCrossTenants(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	product := callableModelAPIProduct(t, now)
	durable := &fakeModelAPIProductStore{products: []modelapiproduct.PublicProjection{product}, accessByTenant: map[string]store.ModelAPIProductAccess{}}
	principalStore := &fakeStore{principal: domain.Principal{ID: "reader-b", TenantID: "tenant-b", Name: "reader", Role: string(authz.Viewer), Scopes: []string{"read"}}}
	handler := (API{Store: principalStore, Authenticator: principalStore, ModelAPIProducts: durable}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-catalog/glm-5.2", nil)
	request.Header.Set("Authorization", "Bearer tenant-b-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"authorized":false`) || len(durable.accessTenants) != 1 || durable.accessTenants[0] != "tenant-b" {
		t.Fatalf("tenant isolation status=%d tenants=%v body=%s", response.Code, durable.accessTenants, response.Body.String())
	}
}

func TestModelAPIAccessCompatibilityFallbackNeverAuthorizes(t *testing.T) {
	handler := (API{Store: &fakeStore{}, APIKey: "secret"}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-catalog/qwen3-8b", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"catalog_source":"compatibility_fallback"`) || !strings.Contains(response.Body.String(), `"authorized":false`) {
		t.Fatalf("fallback access status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestDurableModelAPICatalogDoesNotFallBackOnReadFailure(t *testing.T) {
	durable := &fakeModelAPIProductStore{err: errors.New("database offline")}
	handler := (API{Store: &fakeStore{}, APIKey: "secret", ModelAPIProducts: durable}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-catalog", nil)
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"model_api_catalog_unavailable"`) || strings.Contains(response.Body.String(), "qwen3-8b") {
		t.Fatalf("durable failure did not fail closed: status=%d body=%s", response.Code, response.Body.String())
	}
}

func callableModelAPIProduct(t *testing.T, now time.Time) modelapiproduct.PublicProjection {
	t.Helper()
	var product modelapiproduct.Product
	for _, candidate := range modelapiproduct.DefaultCatalog() {
		if candidate.ID == "glm-5.2" {
			product = candidate
			break
		}
	}
	product.Availability = modelapiproduct.AvailabilityAvailable
	for index := range product.Capabilities {
		if product.Capabilities[index].Name == "chat-completions" || product.Capabilities[index].Name == "streaming" {
			evidenceUntil := now.Add(24 * time.Hour)
			name := product.Capabilities[index].Name
			product.Capabilities[index] = modelapiproduct.CapabilityClaim{Name: name, State: modelapiproduct.ClaimQualified, EvidenceID: name + "-evidence-private", EvidenceUntil: &evidenceUntil}
		}
	}
	rate, err := modelapiproduct.NewRetailRate(modelapiproduct.RetailRateDraft{
		ID: "rate-glm-1", ProductID: product.ID, Version: 1,
		InputMicrousdPerMillion: 1_260_000, OutputMicrousdPerMillion: 3_960_000,
		ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(24 * time.Hour),
		PublishedAt: now.Add(-2 * time.Hour), PublicProvenance: "InferCrane rate card",
	})
	if err != nil {
		t.Fatal(err)
	}
	evidenceUntil := now.Add(24 * time.Hour)
	publication := modelapiproduct.OperatorPublication{
		SchemaVersion: modelapiproduct.OperatorProjectionSchemaVersion, ProductID: product.ID,
		OperatorWorkspaceID: "operator-private", ServingPlanID: "serving-private", SupplyPlanID: "supply-private",
		Qualification: modelapiproduct.RouteQualification{State: modelapiproduct.QualificationQualified, EvidenceID: "evidence-private", EvidenceUntil: &evidenceUntil},
		RetailRate:    &rate, UpdatedAt: now,
	}
	projection, err := modelapiproduct.PublicProjectionAt(product, &publication, now)
	if err != nil {
		t.Fatal(err)
	}
	return projection
}
