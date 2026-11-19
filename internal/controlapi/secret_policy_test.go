package controlapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secrets"
)

// A workspace on a shared control plane must not be able to name the control
// plane's own environment, which holds the operator's keys.
func TestSecretAPIRefusesEnvironmentReferencesFromOtherWorkspaces(t *testing.T) {
	policy := secrets.Environment{Tenants: secrets.NewTenantPolicy(secrets.BootstrapTenant)}
	store := &fakeStore{principal: domain.Principal{ID: "signed-up-admin", TenantID: "signed-up", Name: "admin", Role: "admin", Scopes: []string{"manage_secrets"}}}
	body := `{"name":"stolen","resolver":"env","reference":"INFERCRANE_API_KEY"}`

	request := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer workspace")
	response := httptest.NewRecorder()
	(API{Store: store, Authenticator: store, Secrets: policy}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "env_secret_reference_not_allowed") {
		t.Fatalf("workspace reference to the control plane's environment was not refused: %d %s", response.Code, response.Body.String())
	}

	// The operator's own key belongs to the bootstrap workspace and keeps working.
	request = httptest.NewRequest(http.MethodPost, "/api/v1/secrets", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	(API{Store: &fakeStore{}, APIKey: "secret", Secrets: policy}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("operator reference refused: %d %s", response.Code, response.Body.String())
	}
}
