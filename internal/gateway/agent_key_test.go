package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/routes"
)

func TestAgentKeysCannotInvokeInference(t *testing.T) {
	directory := routes.New()
	directory.Put(routes.Snapshot{TenantID: "team", DeploymentID: "d", Alias: "coder", UpstreamModel: "coder", RouterURL: "http://router.invalid"})
	principal := domain.Principal{ID: "agent-principal", TenantID: "team", Role: "viewer", Kind: domain.AgentPrincipalKind}
	handler := (&Gateway{Routes: directory, Authenticator: fakeAuthenticator{principal: principal}}).Handler()
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/models", nil),
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coder","messages":[]}`)),
	} {
		request.Header.Set("Authorization", "Bearer ic_agent")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "agent keys cannot invoke inference") {
			t.Fatalf("%s %s: %d %s", request.Method, request.URL.Path, response.Code, response.Body.String())
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coder","messages":[]}`))
	response := httptest.NewRecorder()
	(&Gateway{Routes: directory}).ServeChatAs(response, request, principal)
	if response.Code != http.StatusForbidden {
		t.Fatalf("ServeChatAs accepted an agent principal: %d %s", response.Code, response.Body.String())
	}
}
