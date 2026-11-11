package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/controlapi"
)

// compose.yaml and a default self-hosted install leave
// INFERCRANE_CREDENTIAL_ENCRYPTION_KEY unset. The control API must then serve
// the documented disabled BYOC state instead of calling a nil service.
func TestControlAPIWithoutCredentialKeyServesDisabledComputeConnections(t *testing.T) {
	connections, err := newComputeConnectionService(config.Config{}, nil)
	if err != nil {
		t.Fatalf("newComputeConnectionService() error = %v", err)
	}
	if connections != nil {
		t.Fatalf("newComputeConnectionService() = %#v without a credential key, want nil", connections)
	}
	api := controlapi.API{APIKey: "secret"}
	bindOptionalControlServices(&api, nil, connections)
	if api.ComputeConnections != nil {
		t.Fatalf("ComputeConnections = %T(nil) stored in an interface, want an untyped nil field", api.ComputeConnections)
	}
	if api.AsyncInference != nil {
		t.Fatalf("AsyncInference = %T(nil) stored in an interface, want an untyped nil field", api.AsyncInference)
	}

	handler := api.Handler()
	for _, tc := range []struct {
		method, path, body string
		status             int
		want               string
	}{
		{http.MethodGet, "/api/v1/compute/connections", "", http.StatusOK, `{"data":[]}`},
		{http.MethodPost, "/api/v1/compute/connections", `{"name":"runpod","provider":"runpod","credential":"unused"}`, http.StatusServiceUnavailable, `"code":"capability_unavailable"`},
		{http.MethodDelete, "/api/v1/compute/connections/connection-1", "", http.StatusNotFound, `"code":"not_found"`},
	} {
		request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != tc.status || !strings.Contains(response.Body.String(), tc.want) {
			t.Fatalf("%s %s = %d %s, want %d containing %s", tc.method, tc.path, response.Code, response.Body.String(), tc.status, tc.want)
		}
	}
}

func TestComputeConnectionServiceBindsOnlyWithValidCredentialKey(t *testing.T) {
	if _, err := newComputeConnectionService(config.Config{CredentialEncryptionKey: "too-short"}, nil); err == nil {
		t.Fatal("newComputeConnectionService() accepted a credential key shorter than 32 bytes")
	}
	connections, err := newComputeConnectionService(config.Config{CredentialEncryptionKey: strings.Repeat("k", 32)}, nil)
	if err != nil || connections == nil {
		t.Fatalf("newComputeConnectionService() = %v, %v; want a configured service", connections, err)
	}
	api := controlapi.API{}
	bindOptionalControlServices(&api, nil, connections)
	if api.ComputeConnections == nil {
		t.Fatal("ComputeConnections is nil after binding a configured service")
	}
}
