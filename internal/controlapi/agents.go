package controlapi

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/agentrelay"
	"github.com/infercrane/infercrane/internal/domain"
)

// agentService manages InferCrane agents and serves the agent protocol.
type agentService interface {
	Create(context.Context, string, string, string) (domain.Agent, string, error)
	List(context.Context, string) ([]domain.Agent, error)
	Get(context.Context, string, string) (domain.Agent, []domain.AgentServer, error)
	Revoke(context.Context, string, string) (domain.Agent, error)
	Authenticate(context.Context, domain.Principal) (domain.Agent, error)
	Register(context.Context, domain.Agent, agentprotocol.RegisterServerRequest) (agentprotocol.RegisterServerResponse, error)
	Sync(context.Context, domain.Agent, agentprotocol.SyncRequest) (agentprotocol.SyncResponse, error)
}

type agentKey struct{}

// agentAuth admits only agent keys, and only while their agent is active.
// Every call re-checks the agent row, so a revoked agent is refused on its
// next request even before the credential snapshot refreshes.
func (a API) agentAuth(next func(http.ResponseWriter, *http.Request, domain.Agent)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if a.Agents == nil {
			writeError(w, http.StatusNotImplemented, "capability_unavailable", "the InferCrane agent is not enabled on this control plane")
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		var principal domain.Principal
		if a.Authenticator != nil && token != "" && token != r.Header.Get("Authorization") {
			if resolved, err := a.Authenticator.AuthenticatePrincipal(r.Context(), token); err == nil {
				principal = resolved
			}
		}
		if principal.ID == "" || principal.Kind != domain.AgentPrincipalKind {
			writeError(w, http.StatusUnauthorized, "agent_unauthenticated", "an active InferCrane agent key is required")
			return
		}
		agent, err := a.Agents.Authenticate(r.Context(), principal)
		if errors.Is(err, agentrelay.ErrRevoked) {
			writeError(w, http.StatusUnauthorized, "agent_revoked", "this agent key was revoked; the agent must stop")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "the agent could not be resolved")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), agentKey{}, agent)), agent)
	}
}

func agentResponse(item domain.Agent) map[string]any {
	var hardware, servers any
	_ = json.Unmarshal([]byte(item.HardwareJSON), &hardware)
	_ = json.Unmarshal([]byte(item.ServersJSON), &servers)
	connection := "never_connected"
	if item.State != "active" {
		connection = "revoked"
	} else if item.LastSeenAt != nil {
		connection = "connected"
		if time.Since(*item.LastSeenAt) > 3*agentprotocol.MaxPollInterval {
			connection = "silent"
		}
	}
	return map[string]any{
		"id": item.ID, "name": item.Name, "state": item.State, "connection": connection,
		"created_by": item.CreatedBy, "agent_version": nullableText(item.AgentVersion), "protocol": nullableText(item.ProtocolVersion),
		"os": nullableText(item.OS), "arch": nullableText(item.Arch), "hardware": hardware, "servers": servers,
		"gateway_server_key": nullableText(item.GatewayServerKey), "last_seen_at": item.LastSeenAt,
		"created_at": item.CreatedAt, "updated_at": item.UpdatedAt, "revoked_at": item.RevokedAt,
	}
}

func agentServerResponse(item domain.AgentServer, gatewayKey string) map[string]any {
	trafficSource, trafficDetail := "none", "No traffic is visible: requests go straight to the server. Route them through the agent's local gateway to record their shape."
	if gatewayKey != "" && gatewayKey == item.Key {
		trafficSource, trafficDetail = "agent-gateway", "Requests sent through the agent's local gateway are recorded as content-free shapes."
	}
	return map[string]any{
		"key": item.Key, "endpoint": item.EndpointName, "target_id": item.TargetID, "address": item.Address,
		"engine": item.Runtime, "model": item.UpstreamModel, "health": item.Health, "reported_at": item.ReportedAt,
		"ownership": item.OwnershipMode, "traffic_source": trafficSource, "traffic_detail": trafficDetail,
	}
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func writeAgentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, agentrelay.ErrRevoked):
		writeError(w, http.StatusUnauthorized, "agent_revoked", "this agent key was revoked; the agent must stop")
	case errors.Is(err, agentrelay.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "agent was not found")
	case errors.Is(err, domain.ErrConflict):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", "the agent request could not be completed")
	}
}

func (a API) createAgent(w http.ResponseWriter, r *http.Request) {
	if a.Agents == nil {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "the InferCrane agent is not enabled on this control plane")
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	agent, key, err := a.Agents.Create(r.Context(), actor.TenantID, request.Name, actor.Name)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "agent.create", ResourceType: "agent", ResourceName: agent.Name, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{
		"agent": agentResponse(agent), "key": key,
		"run_command": "INFERCRANE_AGENT_KEY=<key> infercrane agent run --url " + strings.TrimRight(a.GatewayURL, "/"),
	})
}

func (a API) listAgents(w http.ResponseWriter, r *http.Request) {
	if a.Agents == nil {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "the InferCrane agent is not enabled on this control plane")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	items, err := a.Agents.List(r.Context(), actor.TenantID)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(items))
	for _, item := range items {
		data = append(data, agentResponse(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (a API) getAgent(w http.ResponseWriter, r *http.Request) {
	if a.Agents == nil {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "the InferCrane agent is not enabled on this control plane")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	agent, servers, err := a.Agents.Get(r.Context(), actor.TenantID, r.PathValue("id"))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(servers))
	for _, server := range servers {
		data = append(data, agentServerResponse(server, agent.GatewayServerKey))
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": agentResponse(agent), "observed_servers": data})
}

func (a API) revokeAgent(w http.ResponseWriter, r *http.Request) {
	if a.Agents == nil {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "the InferCrane agent is not enabled on this control plane")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	agent, err := a.Agents.Revoke(r.Context(), actor.TenantID, r.PathValue("id"))
	if err != nil {
		writeAgentError(w, err)
		return
	}
	if a.CredentialRefresh != nil {
		_ = a.CredentialRefresh(context.WithoutCancel(r.Context()))
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "agent.revoke", ResourceType: "agent", ResourceName: agent.Name, Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"agent": agentResponse(agent)})
}

// decodeAgentBody reads one bounded, optionally gzip-encoded JSON object.
// Unknown fields are ignored so a newer agent can add fields within one
// protocol version.
func decodeAgentBody(w http.ResponseWriter, r *http.Request, limit int64, destination any) bool {
	var reader io.Reader = http.MaxBytesReader(w, r.Body, limit)
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		decompressed, err := gzip.NewReader(reader)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "request body is not valid gzip")
			return false
		}
		defer decompressed.Close()
		reader = &limitedReader{reader: decompressed, remaining: limit}
	default:
		writeError(w, http.StatusUnsupportedMediaType, "invalid_request", "request body must be JSON, optionally gzip-encoded")
		return false
	}
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body is invalid: "+err.Error())
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body must contain one JSON object")
		return false
	}
	return true
}

// limitedReader fails once more than remaining bytes are read, so a small
// compressed body cannot expand without bound.
type limitedReader struct {
	reader    io.Reader
	remaining int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining <= 0 {
		return 0, errors.New("decompressed request body is too large")
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.reader.Read(p)
	l.remaining -= int64(n)
	return n, err
}

func (a API) agentSync(w http.ResponseWriter, r *http.Request, agent domain.Agent) {
	var request agentprotocol.SyncRequest
	if !decodeAgentBody(w, r, agentprotocol.MaxSyncBodyBytes, &request) {
		return
	}
	if request.Protocol != agentprotocol.Version {
		writeJSON(w, http.StatusUpgradeRequired, map[string]any{"error": map[string]any{"code": "upgrade_required", "category": "compatibility", "message": "this control plane speaks " + agentprotocol.Version, "retryable": false, "remediation": "Install the InferCrane CLI version this control plane advertises, then run infercrane agent run again."}})
		return
	}
	response, err := a.Agents.Sync(r.Context(), agent, request)
	if errors.Is(err, agentrelay.ErrUpgradeRequired) {
		writeJSON(w, http.StatusUpgradeRequired, map[string]any{"error": map[string]any{"code": "upgrade_required", "category": "compatibility", "message": response.Upgrade.Detail, "retryable": false, "remediation": "Install a newer InferCrane CLI, then run infercrane agent run again."}, "upgrade": response.Upgrade})
		return
	}
	if err != nil {
		writeAgentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (a API) agentRegisterServer(w http.ResponseWriter, r *http.Request, agent domain.Agent) {
	var request agentprotocol.RegisterServerRequest
	if !decodeAgentBody(w, r, 64<<10, &request) {
		return
	}
	response, err := a.Agents.Register(r.Context(), agent, request)
	if err != nil {
		writeAgentError(w, err)
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: agent.TenantID, Actor: "agent:" + agent.Name, Action: "agent.server.register", ResourceType: "endpoint", ResourceName: response.Server.Endpoint, Outcome: "succeeded"})
	status := http.StatusOK
	if response.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, response)
}
