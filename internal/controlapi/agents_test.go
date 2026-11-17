package controlapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/agentrelay"
	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

type fakeAgents struct {
	revoked bool
	synced  int
}

func (f *fakeAgents) Create(_ context.Context, tenant, name, actor string) (domain.Agent, string, error) {
	return domain.Agent{ID: "agent-1", TenantID: tenant, Name: name, State: "active", CreatedBy: actor, HardwareJSON: "{}", ServersJSON: "[]"}, "ic_new_agent_key", nil
}
func (f *fakeAgents) List(context.Context, string) ([]domain.Agent, error) { return nil, nil }
func (f *fakeAgents) Get(context.Context, string, string) (domain.Agent, []domain.AgentServer, error) {
	return domain.Agent{}, nil, domain.ErrNotFound
}
func (f *fakeAgents) Revoke(context.Context, string, string) (domain.Agent, error) {
	return domain.Agent{}, domain.ErrNotFound
}
func (f *fakeAgents) Authenticate(_ context.Context, principal domain.Principal) (domain.Agent, error) {
	if f.revoked {
		return domain.Agent{}, agentrelay.ErrRevoked
	}
	return domain.Agent{ID: "agent-1", TenantID: principal.TenantID, Name: "laptop", State: "active"}, nil
}
func (f *fakeAgents) Register(context.Context, domain.Agent, agentprotocol.RegisterServerRequest) (agentprotocol.RegisterServerResponse, error) {
	return agentprotocol.RegisterServerResponse{Created: true}, nil
}
func (f *fakeAgents) Sync(_ context.Context, agent domain.Agent, request agentprotocol.SyncRequest) (agentprotocol.SyncResponse, error) {
	f.synced++
	return agentprotocol.SyncResponse{AgentID: agent.ID, Servers: []agentprotocol.RegisteredServer{}, Rejected: []agentprotocol.Rejection{}, Commands: []agentprotocol.Command{}}, nil
}

func gzipped(t *testing.T, value []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, _ = writer.Write(value)
	_ = writer.Close()
	return buffer.Bytes()
}

func TestAgentKeysReachOnlyTheAgentAPI(t *testing.T) {
	agentStore := &fakeStore{principal: domain.Principal{ID: "agent-principal", TenantID: "tenant-a", Name: "agent:laptop", Role: string(authz.Viewer), Kind: domain.AgentPrincipalKind}}
	agents := &fakeAgents{}
	handler := (API{Store: agentStore, Authenticator: agentStore, Agents: agents}).Handler()
	for _, path := range []string{"/api/v1/endpoints", "/api/v1/agents", "/api/v1/deployments"} {
		if response := serve(handler, http.MethodGet, path, "ic_agent", ""); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "agent API") {
			t.Fatalf("%s: %d %s", path, response.Code, response.Body.String())
		}
	}
	body, _ := json.Marshal(agentprotocol.SyncRequest{Protocol: agentprotocol.Version, AgentVersion: "1.0.0", OS: "linux", Arch: "amd64"})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent/sync", bytes.NewReader(gzipped(t, body)))
	request.Header.Set("Authorization", "Bearer ic_agent")
	request.Header.Set("Content-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || agents.synced != 1 {
		t.Fatalf("gzip sync: %d %s", response.Code, response.Body.String())
	}
	// A small compressed body that inflates past the limit is refused.
	bomb := gzipped(t, append([]byte(`{"protocol":"`), bytes.Repeat([]byte("a"), agentprotocol.MaxSyncBodyBytes+10)...))
	request = httptest.NewRequest(http.MethodPost, "/api/v1/agent/sync", bytes.NewReader(bomb))
	request.Header.Set("Authorization", "Bearer ic_agent")
	request.Header.Set("Content-Encoding", "gzip")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || agents.synced != 1 {
		t.Fatalf("gzip bomb: %d %s", response.Code, response.Body.String())
	}
	old, _ := json.Marshal(agentprotocol.SyncRequest{Protocol: "infercrane.agent/v0"})
	if response = serve(handler, http.MethodPost, "/api/v1/agent/sync", "ic_agent", string(old)); response.Code != http.StatusUpgradeRequired {
		t.Fatalf("old protocol: %d", response.Code)
	}
	agents.revoked = true
	if response = serve(handler, http.MethodPost, "/api/v1/agent/sync", "ic_agent", string(body)); response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), "agent_revoked") {
		t.Fatalf("revoked: %d %s", response.Code, response.Body.String())
	}

	operator := &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "operator", Role: string(authz.Operator), Scopes: authz.DefaultScopeNames(authz.Operator)}}
	operatorHandler := (API{Store: operator, Authenticator: operator, Agents: &fakeAgents{}, GatewayURL: "https://control.example.test"}).Handler()
	if response = serve(operatorHandler, http.MethodPost, "/api/v1/agent/sync", "operator", string(body)); response.Code != http.StatusUnauthorized {
		t.Fatalf("a human key reached the agent API: %d", response.Code)
	}
	response = serve(operatorHandler, http.MethodPost, "/api/v1/agents", "operator", `{"name":"laptop"}`)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), "ic_new_agent_key") || !strings.Contains(response.Body.String(), "https://control.example.test") {
		t.Fatalf("create: %d %s", response.Code, response.Body.String())
	}
	viewer := &fakeStore{principal: domain.Principal{ID: "viewer", TenantID: "tenant-a", Name: "viewer", Role: string(authz.Viewer), Scopes: []string{"read"}}}
	if response = serve((API{Store: viewer, Authenticator: viewer, Agents: &fakeAgents{}}).Handler(), http.MethodPost, "/api/v1/agents", "viewer", `{"name":"laptop"}`); response.Code != http.StatusForbidden {
		t.Fatalf("viewer created an agent: %d", response.Code)
	}
	if response = serve((API{Store: operator, Authenticator: operator}).Handler(), http.MethodGet, "/api/v1/agents", "operator", ""); response.Code != http.StatusNotImplemented {
		t.Fatalf("agents without a service: %d", response.Code)
	}
}

type fakeAdoptionStore struct {
	*fakeStore
	sealed *domain.SealedCredential
}

func (f *fakeAdoptionStore) AdoptEndpoint(ctx context.Context, tenant, name, model, upstream, url, source, ownership, runtime string) (domain.ResolvedEndpoint, domain.AdoptedWorkload, error) {
	return f.AdoptEndpointWithCredential(ctx, tenant, name, model, upstream, url, source, ownership, runtime, nil)
}
func (f *fakeAdoptionStore) AdoptEndpointWithCredential(_ context.Context, tenant, name, _, _, _, source, ownership, _ string, sealed *domain.SealedCredential) (domain.ResolvedEndpoint, domain.AdoptedWorkload, error) {
	f.sealed = sealed
	return domain.ResolvedEndpoint{Endpoint: domain.Endpoint{Name: name, TenantID: tenant}}, domain.AdoptedWorkload{ID: "adoption-1", OwnershipMode: ownership, Source: source}, nil
}
func (f *fakeAdoptionStore) PromoteAdoptionOwnership(context.Context, string, string, string) (domain.AdoptedWorkload, error) {
	return domain.AdoptedWorkload{}, nil
}

type fakeSealer struct{ tenant, endpoint, token string }

func (f *fakeSealer) Seal(tenant, endpoint, token string) (domain.SealedCredential, error) {
	f.tenant, f.endpoint, f.token = tenant, endpoint, token
	return domain.SealedCredential{Ciphertext: []byte("sealed"), Nonce: []byte("nonce")}, nil
}

func TestConnectCredentialIsWriteOnlyAndUsedForDiscovery(t *testing.T) {
	const token = "server-token-4f1c"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"Qwen/Qwen3-8B"}]}`))
	}))
	defer server.Close()
	store := &fakeAdoptionStore{fakeStore: &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "operator", Role: string(authz.Operator), Scopes: authz.DefaultScopeNames(authz.Operator)}}}
	body := `{"name":"private-qwen","logical_model":"private-qwen","url":"` + server.URL + `/v1","source":"openai-compatible","ownership_mode":"observe-only","runtime":"openai-compatible","discover":true,"connector":"auto","upstream_credential":"` + token + `"}`

	response := serve((API{Store: store, Authenticator: store}).Handler(), http.MethodPost, "/api/v1/adoptions/endpoints", "operator", body)
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "upstream_credential_unavailable") || !strings.Contains(response.Body.String(), "infercrane agent run") {
		t.Fatalf("without a credential key: %d %s", response.Code, response.Body.String())
	}
	sealer := &fakeSealer{}
	handler := (API{Store: store, Authenticator: store, UpstreamCredentials: sealer}).Handler()
	response = serve(handler, http.MethodPost, "/api/v1/adoptions/endpoints", "operator", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("adopt: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), token) || store.sealed == nil || string(store.sealed.Ciphertext) != "sealed" || sealer.token != token || sealer.tenant != "tenant-a" || sealer.endpoint != "private-qwen" {
		t.Fatalf("credential handling: body=%s sealed=%v sealer=%+v", response.Body.String(), store.sealed, sealer)
	}
	for _, invalid := range []string{
		strings.Replace(body, `"discover":true`, `"discover":false`, 1),
		strings.Replace(body, token, "two words", 1),
		strings.Replace(body, `"connector":"auto"`, `"connector":"auto","provider_connection":"saved"`, 1),
	} {
		if response = serve(handler, http.MethodPost, "/api/v1/adoptions/endpoints", "operator", invalid); response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid credential request accepted: %d %s", response.Code, response.Body.String())
		}
	}
}
