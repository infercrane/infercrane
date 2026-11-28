package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/agentrelay"
	"github.com/infercrane/infercrane/internal/domain"
)

// agentDataPlaneService serves the agent as the data plane inside a
// customer's cloud, and its command channel.
type agentDataPlaneService interface {
	DataPlane(context.Context, domain.Agent, string) (agentprotocol.DataPlaneSnapshot, error)
	LeaseQuota(context.Context, domain.Agent, agentprotocol.QuotaLeaseRequest) (agentprotocol.QuotaLeaseResponse, error)
	Commands(context.Context, domain.Agent, time.Duration, func(agentprotocol.CommandsResponse) error) error
	UpdateCommand(context.Context, domain.Agent, string, agentprotocol.CommandUpdate) (agentprotocol.CommandUpdateResponse, error)
	CreateCandidateRun(context.Context, string, string, string, agentprotocol.CandidateRunSpec) (domain.Agent, domain.AgentCommand, error)
	CandidateRuns(context.Context, string, string) (domain.Agent, []domain.AgentCommand, error)
	CandidateRun(context.Context, string, string, string) (domain.Agent, domain.AgentCommand, error)
	CancelCandidateRun(context.Context, string, string, string) (domain.Agent, domain.AgentCommand, error)
}

// residencyStore changes and reads endpoint residency.
type residencyStore interface {
	SetEndpointResidency(context.Context, string, string, string) (domain.Endpoint, error)
	EndpointResidency(context.Context, string, string) (string, error)
}

func (a API) dataPlaneAgents(w http.ResponseWriter) (agentDataPlaneService, bool) {
	service, ok := a.Agents.(agentDataPlaneService)
	if !ok || a.Agents == nil {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "the InferCrane agent data plane is not enabled on this control plane")
		return nil, false
	}
	return service, true
}

func writeDataPlaneError(w http.ResponseWriter, err error) {
	if errors.Is(err, agentrelay.ErrUnavailable) {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", err.Error())
		return
	}
	writeAgentError(w, err)
}

// agentDataPlane answers an agent's data-plane pull. The answer carries API
// key digests, so it is never cached.
func (a API) agentDataPlane(w http.ResponseWriter, r *http.Request, agent domain.Agent) {
	service, ok := a.dataPlaneAgents(w)
	if !ok {
		return
	}
	generation := r.URL.Query().Get("generation")
	if generation != "" && !agentprotocol.ValidGeneration(generation) {
		writeError(w, http.StatusBadRequest, "invalid_request", "generation must be a snapshot digest")
		return
	}
	snapshot, err := service.DataPlane(r.Context(), agent, generation)
	if err != nil {
		writeDataPlaneError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snapshot)
}

func (a API) agentQuotaLease(w http.ResponseWriter, r *http.Request, agent domain.Agent) {
	service, ok := a.dataPlaneAgents(w)
	if !ok {
		return
	}
	var request agentprotocol.QuotaLeaseRequest
	if !decodeAgentBody(w, r, 4<<10, &request) {
		return
	}
	response, err := service.LeaseQuota(r.Context(), agent, request)
	if err != nil {
		writeDataPlaneError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// agentCommands is the agent's long poll for work. It holds the request for
// at most agentprotocol.MaxCommandWait and is still an outbound call.
func (a API) agentCommands(w http.ResponseWriter, r *http.Request, agent domain.Agent) {
	service, ok := a.dataPlaneAgents(w)
	if !ok {
		return
	}
	wait := agentprotocol.MaxCommandWait
	if raw := r.URL.Query().Get("wait"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			seconds, numberErr := strconv.Atoi(raw)
			if numberErr != nil {
				writeError(w, http.StatusBadRequest, "invalid_request", "wait must be a duration such as 25s")
				return
			}
			parsed = time.Duration(seconds) * time.Second
		}
		if parsed < 0 || parsed > agentprotocol.MaxCommandWait {
			writeError(w, http.StatusBadRequest, "invalid_request", "wait must be between 0s and 25s")
			return
		}
		wait = parsed
	}
	// An answer that cannot be written hands its commands back at once.
	err := service.Commands(r.Context(), agent, wait, func(response agentprotocol.CommandsResponse) error {
		return writeDeliveredJSON(w, http.StatusOK, response)
	})
	if err != nil {
		writeDataPlaneError(w, err)
	}
}

// writeDeliveredJSON writes and flushes a JSON answer, and returns an error
// when it did not reach the connection. A writer that cannot flush counts as
// delivered: its answer is sent when the handler returns, and nothing shows
// that it failed.
func writeDeliveredJSON(w http.ResponseWriter, status int, value any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		return err
	}
	if err := http.NewResponseController(w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

func (a API) agentCommandUpdate(w http.ResponseWriter, r *http.Request, agent domain.Agent) {
	service, ok := a.dataPlaneAgents(w)
	if !ok {
		return
	}
	var update agentprotocol.CommandUpdate
	if !decodeAgentBody(w, r, 512<<10, &update) {
		return
	}
	response, err := service.UpdateCommand(r.Context(), agent, r.PathValue("id"), update)
	if err != nil {
		writeDataPlaneError(w, err)
		return
	}
	if update.State != agentprotocol.CommandRunning {
		_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: agent.TenantID, Actor: "agent:" + agent.Name, Action: "agent.candidate_run." + update.State, ResourceType: "agent_command", ResourceName: r.PathValue("id"), Outcome: "succeeded"})
	}
	writeJSON(w, http.StatusOK, response)
}

func (a API) createCandidateRun(w http.ResponseWriter, r *http.Request) {
	service, ok := a.dataPlaneAgents(w)
	if !ok {
		return
	}
	var spec agentprotocol.CandidateRunSpec
	if !decodeMutationBody(w, r, &spec) {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	agent, command, err := service.CreateCandidateRun(r.Context(), actor.TenantID, r.PathValue("id"), actor.Name, spec)
	if err != nil {
		writeDataPlaneError(w, err)
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "agent.candidate_run.create", ResourceType: "agent_command", ResourceName: command.ID, Outcome: "succeeded"})
	writeJSON(w, http.StatusCreated, map[string]any{"candidate_run": agentrelay.CandidateRunView(agent, command)})
}

func (a API) listCandidateRuns(w http.ResponseWriter, r *http.Request) {
	service, ok := a.dataPlaneAgents(w)
	if !ok {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	agent, commands, err := service.CandidateRuns(r.Context(), actor.TenantID, r.PathValue("id"))
	if err != nil {
		writeDataPlaneError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(commands))
	for _, command := range commands {
		data = append(data, agentrelay.CandidateRunView(agent, command))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}

func (a API) getCandidateRun(w http.ResponseWriter, r *http.Request) {
	service, ok := a.dataPlaneAgents(w)
	if !ok {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	agent, command, err := service.CandidateRun(r.Context(), actor.TenantID, r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		writeDataPlaneError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidate_run": agentrelay.CandidateRunView(agent, command)})
}

func (a API) cancelCandidateRun(w http.ResponseWriter, r *http.Request) {
	service, ok := a.dataPlaneAgents(w)
	if !ok {
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	agent, command, err := service.CancelCandidateRun(r.Context(), actor.TenantID, r.PathValue("id"), r.PathValue("run"))
	if err != nil {
		writeDataPlaneError(w, err)
		return
	}
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "agent.candidate_run.cancel", ResourceType: "agent_command", ResourceName: command.ID, Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"candidate_run": agentrelay.CandidateRunView(agent, command)})
}

// setEndpointResidency changes where an endpoint's request content may go.
func (a API) setEndpointResidency(w http.ResponseWriter, r *http.Request) {
	store, ok := a.Store.(residencyStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "endpoint residency is not supported by this store")
		return
	}
	var request struct {
		Residency string `json:"residency"`
	}
	if !decodeMutationBody(w, r, &request) {
		return
	}
	if request.Residency != agentprotocol.ResidencyAny && request.Residency != agentprotocol.ResidencyCustomerCloud {
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "residency must be any or customer-cloud")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	item, err := store.SetEndpointResidency(r.Context(), actor.TenantID, r.PathValue("name"), request.Residency)
	if errors.Is(err, domain.ErrResidency) {
		writeError(w, http.StatusConflict, "residency_conflict", err.Error())
		return
	}
	if writeEndpointMutationError(w, err) {
		return
	}
	refresh := a.refreshEndpoints(r.Context())
	_ = a.Store.Audit(context.WithoutCancel(r.Context()), domain.AuditEvent{TenantID: actor.TenantID, Actor: actor.Name, Action: "endpoint.residency." + request.Residency, ResourceType: "endpoint", ResourceName: item.Name, Outcome: "succeeded"})
	writeJSON(w, http.StatusOK, map[string]any{"endpoint": endpointResponse(item), "route_refresh": refresh})
}

// refuseCustomerCloudContent answers true (and writes a 403) when request
// content for endpoint must not enter this control plane.
func (a API) refuseCustomerCloudContent(w http.ResponseWriter, r *http.Request, tenant, endpoint string) bool {
	store, ok := a.Store.(residencyStore)
	if !ok || strings.TrimSpace(endpoint) == "" {
		return false
	}
	residency, err := store.EndpointResidency(r.Context(), tenant, endpoint)
	if err != nil || residency != agentprotocol.ResidencyCustomerCloud {
		return false
	}
	writeError(w, http.StatusForbidden, "residency_customer_cloud", "endpoint "+endpoint+" is served inside your cloud by an InferCrane agent, so this control plane does not accept its prompts; send the request to the agent's gateway in your cluster")
	return true
}
