package controlapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/secretcipher"
	"github.com/infercrane/infercrane/internal/secrets"
)

func sealedSecretsForTest(t *testing.T) secrets.Sealed {
	t.Helper()
	cipher, err := secretcipher.New(strings.Repeat("s", 32))
	if err != nil {
		t.Fatal(err)
	}
	return secrets.Sealed{Cipher: &cipher, KeyReference: "environment:TEST_KEY"}
}

func postSecret(t *testing.T, api API, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/secrets", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+bearer)
	response := httptest.NewRecorder()
	api.Handler().ServeHTTP(response, request)
	return response
}

// A workspace on a shared control plane keeps its own token sealed. The value
// is stored only as ciphertext and never comes back in a response.
func TestSealedSecretIsStoredAsCiphertextAndNeverReturned(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "workspace-admin", TenantID: "acme", Name: "admin", Role: "admin", Scopes: []string{"manage_secrets"}}}
	api := API{Store: store, Authenticator: store, SealedSecrets: sealedSecretsForTest(t)}
	response := postSecret(t, api, "workspace", `{"name":"hf-token","resolver":"sealed","value":"hf_secretvalue123"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("sealed secret refused: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "hf_secretvalue123") || strings.Contains(strings.ToLower(response.Body.String()), "ciphertext") {
		t.Fatalf("response exposes the sealed value: %s", response.Body.String())
	}
	if store.sealedSecret == nil || store.sealedSecret.TenantID != "acme" || strings.Contains(string(store.sealedSecret.Sealed.Ciphertext), "hf_secretvalue123") {
		t.Fatalf("store did not receive only ciphertext: %#v", store.sealedSecret)
	}
	opened, err := api.SealedSecrets.Resolve(t.Context(), *store.sealedSecret)
	if err != nil || opened != "hf_secretvalue123" {
		t.Fatalf("stored ciphertext does not open to the value: %q %v", opened, err)
	}
}

func TestSealedSecretCreateRefusals(t *testing.T) {
	store := &fakeStore{principal: domain.Principal{ID: "workspace-admin", TenantID: "acme", Name: "admin", Role: "admin", Scopes: []string{"manage_secrets"}}}
	cases := []struct {
		name, body, code string
		api              API
		status           int
	}{
		{name: "no key", api: API{Store: store, Authenticator: store}, body: `{"name":"hf","resolver":"sealed","value":"hf_value"}`, status: http.StatusNotImplemented, code: "sealed_secrets_unavailable"},
		{name: "reference with sealed", api: API{Store: store, Authenticator: store, SealedSecrets: sealedSecretsForTest(t)}, body: `{"name":"hf","resolver":"sealed","reference":"HF","value":"hf_value"}`, status: http.StatusUnprocessableEntity, code: "validation_failed"},
		{name: "multi-line value", api: API{Store: store, Authenticator: store, SealedSecrets: sealedSecretsForTest(t)}, body: `{"name":"hf","resolver":"sealed","value":"a\nb"}`, status: http.StatusUnprocessableEntity, code: "validation_failed"},
		{name: "value with env", api: API{Store: store, Authenticator: store, Secrets: secrets.Environment{Tenants: secrets.NewTenantPolicy("*")}}, body: `{"name":"hf","resolver":"env","reference":"HF_TOKEN","value":"x"}`, status: http.StatusUnprocessableEntity, code: "validation_failed"},
		{name: "bad name", api: API{Store: store, Authenticator: store, SealedSecrets: sealedSecretsForTest(t)}, body: `{"name":"../x","resolver":"sealed","value":"hf_value"}`, status: http.StatusUnprocessableEntity, code: "validation_failed"},
		{name: "kubernetes without operator compute", api: API{Store: store, Authenticator: store}, body: `{"name":"hf","resolver":"kubernetes","reference":"hf-token/token"}`, status: http.StatusForbidden, code: "worker_secret_reference_not_allowed"},
		{name: "aws without operator compute", api: API{Store: store, Authenticator: store}, body: `{"name":"hf","resolver":"aws-secrets-manager","reference":"arn:aws:secretsmanager:eu-central-1:123456789012:secret:hf-AbCdEf"}`, status: http.StatusForbidden, code: "worker_secret_reference_not_allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := postSecret(t, tc.api, "workspace", tc.body)
			if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.code) {
				t.Fatalf("got %d %s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), "hf_value") {
				t.Fatalf("refusal echoes the value: %s", response.Body.String())
			}
		})
	}
	granted := API{Store: store, Authenticator: store, OperatorComputeTenants: []string{"acme"}}
	if response := postSecret(t, granted, "workspace", `{"name":"hf","resolver":"kubernetes","reference":"hf-token/token"}`); response.Code != http.StatusCreated {
		t.Fatalf("granted workspace could not name its cluster secret: %d %s", response.Code, response.Body.String())
	}
}
