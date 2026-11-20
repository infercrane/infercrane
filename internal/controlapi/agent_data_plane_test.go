package controlapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/agentrelay"
	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

type fakeResidencyStore struct {
	*fakeStore
	residency map[string]string
	conflict  bool
}

func (f *fakeResidencyStore) SetEndpointResidency(_ context.Context, tenant, name, residency string) (domain.Endpoint, error) {
	if f.conflict {
		return domain.Endpoint{}, domain.ErrResidency
	}
	f.residency[name] = residency
	return domain.Endpoint{ID: "endpoint-1", TenantID: tenant, Name: name, Residency: residency}, nil
}

func (f *fakeResidencyStore) EndpointResidency(_ context.Context, _, name string) (string, error) {
	if value, ok := f.residency[name]; ok {
		return value, nil
	}
	return "", domain.ErrNotFound
}

// activeAgentStore resolves every agent key to one active agent.
type activeAgentStore struct{ agentrelay.Store }

func (activeAgentStore) AgentForPrincipal(context.Context, string) (domain.Agent, error) {
	return domain.Agent{ID: "agent-1", TenantID: "tenant-a", Name: "cluster", State: "active"}, nil
}

func TestAgentDataPlaneRoutesAreConfinedToTheirCallers(t *testing.T) {
	agentStore := &fakeStore{principal: domain.Principal{ID: "agent-principal", TenantID: "tenant-a", Name: "agent:cluster", Role: string(authz.Viewer), Kind: domain.AgentPrincipalKind}}
	relay := &agentrelay.Service{Store: activeAgentStore{}}
	agentHandler := (API{Store: agentStore, Authenticator: agentStore, Agents: relay}).Handler()
	// An agent key cannot queue work for itself or read candidate runs.
	for _, request := range [][2]string{{http.MethodPost, "/api/v1/agents/agent-1/candidate-runs"}, {http.MethodGet, "/api/v1/agents/agent-1/candidate-runs"}, {http.MethodPut, "/api/v1/endpoints/support-bot/residency"}} {
		if response := serve(agentHandler, request[0], request[1], "ic_agent", `{}`); response.Code != http.StatusForbidden {
			t.Fatalf("agent key on %s %s: %d", request[0], request[1], response.Code)
		}
	}
	operator := &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "operator", Role: string(authz.Operator), Scopes: authz.DefaultScopeNames(authz.Operator)}}
	operatorHandler := (API{Store: operator, Authenticator: operator, Agents: relay}).Handler()
	// A human key cannot pull a data plane snapshot or take commands.
	for _, request := range [][2]string{{http.MethodGet, "/api/v1/agent/data-plane"}, {http.MethodGet, "/api/v1/agent/commands?wait=0s"}, {http.MethodPost, "/api/v1/agent/quota-leases"}, {http.MethodPost, "/api/v1/agent/commands/command-0001"}} {
		if response := serve(operatorHandler, request[0], request[1], "operator", `{}`); response.Code != http.StatusUnauthorized {
			t.Fatalf("human key on %s %s: %d", request[0], request[1], response.Code)
		}
	}
	// The fake agents service predates the data plane: the routes say so.
	if response := serve((API{Store: agentStore, Authenticator: agentStore, Agents: &fakeAgents{}}).Handler(), http.MethodGet, "/api/v1/agent/data-plane", "ic_agent", ""); response.Code != http.StatusNotImplemented {
		t.Fatalf("data plane without support: %d", response.Code)
	}
	if response := serve(agentHandler, http.MethodGet, "/api/v1/agent/data-plane?generation=v1", "ic_agent", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid generation: %d", response.Code)
	}
	if response := serve(agentHandler, http.MethodGet, "/api/v1/agent/commands?wait=5m", "ic_agent", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("unbounded wait: %d", response.Code)
	}
}

func TestEndpointResidencyIsSetExplicitlyAndRefusesHostedContent(t *testing.T) {
	store := &fakeResidencyStore{fakeStore: &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "operator", Role: string(authz.Operator), Scopes: authz.DefaultScopeNames(authz.Operator)}}, residency: map[string]string{}}
	handler := (API{Store: store, Authenticator: store}).Handler()
	response := serve(handler, http.MethodPut, "/api/v1/endpoints/support-bot/residency", "operator", `{"residency":"customer-cloud"}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"residency":"customer-cloud"`) || store.residency["support-bot"] != "customer-cloud" {
		t.Fatalf("set residency: %d %s", response.Code, response.Body.String())
	}
	if response = serve(handler, http.MethodPut, "/api/v1/endpoints/support-bot/residency", "operator", `{"residency":"eu"}`); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown residency: %d", response.Code)
	}
	store.conflict = true
	if response = serve(handler, http.MethodPut, "/api/v1/endpoints/support-bot/residency", "operator", `{"residency":"customer-cloud"}`); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "residency_conflict") {
		t.Fatalf("residency conflict: %d %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/endpoints/support-bot/async", nil)
	recorder := httptest.NewRecorder()
	if (API{Store: store}).refuseCustomerCloudContent(recorder, request, "tenant-a", "support-bot") == false || recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "residency_customer_cloud") {
		t.Fatalf("content for a customer-cloud endpoint was accepted: %d %s", recorder.Code, recorder.Body.String())
	}
	if (API{Store: store}).refuseCustomerCloudContent(httptest.NewRecorder(), request, "tenant-a", "hosted-endpoint") {
		t.Fatal("an endpoint without residency was refused")
	}
}
