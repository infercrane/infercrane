package agentintegration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/infercrane/infercrane/internal/admission"
	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

// Keys an application uses to call the agent's data plane. Their digests are
// all the control plane holds and all the agent receives.
const (
	appKey        = "ic_app-key-for-customer-cloud-3b9d"
	otherScopeKey = "ic_key-for-another-endpoint-77ae"
)

// dataPlaneState is what the memory store keeps for the data plane and the
// command channel.
type dataPlaneState struct {
	residency map[string]string // endpoint -> residency
	leases    int
	commands  map[string]*domain.AgentCommand
}

func (m *memoryStore) dp() *dataPlaneState {
	if m.dataPlane == nil {
		m.dataPlane = &dataPlaneState{residency: map[string]string{}, commands: map[string]*domain.AgentCommand{}}
	}
	return m.dataPlane
}

// makeAgentManaged is what PUT /api/v1/adoptions/endpoints/{name}/ownership
// does in PostgreSQL: the binding becomes agent-managed and the endpoint's
// residency customer-cloud.
func (m *memoryStore) makeAgentManaged(endpoint string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	server := m.servers[m.endpoints[endpoint]]
	server.OwnershipMode = "agent-managed"
	m.dp().residency[endpoint] = "customer-cloud"
}

func (m *memoryStore) AgentDataPlaneBindings(_ context.Context, tenant, agentID string) ([]domain.AgentDataPlaneBinding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rollouts := map[string]bool{}
	for _, server := range m.servers {
		if server.Rollout {
			rollouts[server.EndpointName] = true
		}
	}
	out := []domain.AgentDataPlaneBinding{}
	for slot, server := range m.servers {
		if server.AgentID != agentID || server.OwnershipMode != "agent-managed" || m.dp().residency[server.EndpointName] != "customer-cloud" {
			continue
		}
		// An endpoint with a rollout server routes to it first, its own
		// server as the fallback, as a rollout's switched plan does.
		policy, priority := "manual", 0
		if rollouts[server.EndpointName] {
			policy, priority = "primary-fallback", 1
			if server.Rollout {
				priority = 0
			}
		}
		out = append(out, domain.AgentDataPlaneBinding{
			EndpointID: server.EndpointID, EndpointName: server.EndpointName, LogicalModelID: server.LogicalModelID, EnvironmentID: server.EnvironmentID,
			ServingPlanID: server.ServingPlanID, RoutingPolicy: policy, BindingID: server.BindingID, TargetID: server.TargetID,
			ServerKey: server.Key, UpstreamModel: server.UpstreamModel, Runtime: server.Runtime, Health: m.health[slot], Priority: priority, Weight: 100, Rollout: server.Rollout,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].EndpointName != out[j].EndpointName {
			return out[i].EndpointName < out[j].EndpointName
		}
		return out[i].Priority < out[j].Priority
	})
	_ = tenant
	return out, nil
}

// makeRollout turns the server registered as fromEndpoint into a rollout
// server of endpoint, as a rollout's registration and switch leave it.
func (m *memoryStore) makeRollout(fromEndpoint, endpoint string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var target *domain.AgentServer
	for _, server := range m.servers {
		if server.EndpointName == endpoint {
			target = server
		}
	}
	for _, server := range m.servers {
		if server.EndpointName == fromEndpoint && target != nil {
			server.EndpointName, server.EndpointID, server.LogicalModelID, server.EnvironmentID, server.ServingPlanID = endpoint, target.EndpointID, target.LogicalModelID, target.EnvironmentID, target.ServingPlanID
			server.OwnershipMode, server.Rollout = "agent-managed", true
		}
	}
}

func (m *memoryStore) AgentDataPlaneCredentials(_ context.Context, tenant string) ([]domain.CredentialRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	records := []domain.CredentialRecord{
		{Hash: hash(appKey), Principal: domain.Principal{ID: "principal-app", TenantID: tenant, Name: "app", Role: string(authz.Viewer), Kind: "inference_token", Scopes: authz.DefaultScopeNames(authz.Viewer)}},
		{Hash: hash(otherScopeKey), Principal: domain.Principal{ID: "principal-other", TenantID: tenant, Name: "other", Role: string(authz.Viewer), Kind: "inference_token", Scopes: authz.DefaultScopeNames(authz.Viewer), EndpointNames: []string{"some-other-endpoint"}}},
	}
	for keyHash, agentID := range m.keys {
		// The store never returns agent keys; the relay must also drop them
		// if a store did.
		records = append(records, domain.CredentialRecord{Hash: keyHash, Principal: domain.Principal{ID: m.agents[agentID].PrincipalID, TenantID: tenant, Role: string(authz.Viewer), Kind: domain.AgentPrincipalKind}})
	}
	return records, nil
}

func (m *memoryStore) AdmissionPoliciesForTenant(_ context.Context, tenant string) ([]admission.Policy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []admission.Policy{}
	for endpoint, residency := range m.dp().residency {
		if residency == "customer-cloud" {
			out = append(out, admission.Policy{Key: tenant + "\x00" + endpoint, MaxConcurrency: 4, MaxQueueDepth: 8, QueueTimeout: 2 * time.Second, RequestTimeout: 30 * time.Second, MaxRequestBytes: 1 << 20, MaxOutputTokens: 4096, AllowedPriorities: map[string]struct{}{"normal": {}, "high": {}}, Enabled: true})
		}
	}
	return out, nil
}

func (m *memoryStore) RequestQuotaLimit(context.Context, string) (int, bool, error) {
	if m.noQuota {
		return 0, false, nil
	}
	return 600, true, nil
}

func (m *memoryStore) ReserveRequestQuota(_ context.Context, _ string, _ time.Time, requested int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dp().leases++
	return requested, nil
}

func (m *memoryStore) quotaLeases() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dp().leases
}

func (m *memoryStore) EnqueueAgentCommand(_ context.Context, command domain.AgentCommand) (domain.AgentCommand, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.dp().commands {
		if command.IdempotencyKey != "" && existing.IdempotencyKey == command.IdempotencyKey && existing.TenantID == command.TenantID {
			return *existing, nil
		}
	}
	command.ID = fmt.Sprintf("command-%08d", len(m.dp().commands)+1)
	command.State, command.CreatedAt, command.UpdatedAt = "queued", time.Now().UTC(), time.Now().UTC()
	m.dp().commands[command.ID] = &command
	return command, nil
}

func (m *memoryStore) AgentCommand(_ context.Context, tenant, agentID, id string) (domain.AgentCommand, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	command, ok := m.dp().commands[id]
	if !ok || command.TenantID != tenant || command.AgentID != agentID {
		return domain.AgentCommand{}, domain.ErrNotFound
	}
	return *command, nil
}

func (m *memoryStore) AgentCommands(_ context.Context, tenant, agentID string, _ int) ([]domain.AgentCommand, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.AgentCommand{}
	for _, command := range m.dp().commands {
		if command.TenantID == tenant && command.AgentID == agentID {
			out = append(out, *command)
		}
	}
	return out, nil
}

func (m *memoryStore) LeaseAgentCommands(_ context.Context, tenant, agentID string, at time.Time, lease time.Duration, limit int) ([]domain.AgentCommand, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.AgentCommand{}
	for _, command := range m.dp().commands {
		if command.TenantID != tenant || command.AgentID != agentID || command.State != "queued" || len(out) >= limit {
			continue
		}
		until := at.Add(lease)
		command.State, command.Attempts, command.LeaseExpiresAt, command.Phase = "running", command.Attempts+1, &until, "leased"
		out = append(out, *command)
	}
	return out, nil
}

func (m *memoryStore) UpdateAgentCommand(_ context.Context, tenant, agentID, id string, update domain.AgentCommandUpdate) (domain.AgentCommand, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	command, ok := m.dp().commands[id]
	if !ok || command.TenantID != tenant || command.AgentID != agentID {
		return domain.AgentCommand{}, domain.ErrNotFound
	}
	if command.State != "running" {
		return *command, nil
	}
	command.Phase = update.Phase
	switch update.State {
	case "running":
	case "succeeded", "failed":
		now := time.Now().UTC()
		command.State, command.ResultJSON, command.FailureCode, command.FailureDetail, command.FinishedAt = update.State, update.ResultJSON, update.FailureCode, update.FailureDetail, &now
	default:
		return domain.AgentCommand{}, errors.New("invalid state")
	}
	return *command, nil
}

func (m *memoryStore) CancelAgentCommand(_ context.Context, tenant, agentID, id string) (domain.AgentCommand, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	command, ok := m.dp().commands[id]
	if !ok || command.TenantID != tenant || command.AgentID != agentID {
		return domain.AgentCommand{}, domain.ErrNotFound
	}
	if command.State == "queued" || command.State == "running" {
		now := time.Now().UTC()
		command.State, command.FinishedAt = "cancelled", &now
	}
	return *command, nil
}

func (m *memoryStore) command(id string) domain.AgentCommand {
	m.mu.Lock()
	defer m.mu.Unlock()
	if command, ok := m.dp().commands[id]; ok {
		return *command
	}
	return domain.AgentCommand{}
}

// agentTenant is the workspace an agent belongs to.
func (m *memoryStore) agentTenant(agentID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if agent, ok := m.agents[agentID]; ok {
		return agent.TenantID
	}
	for _, agent := range m.agents {
		if agent.ID == agentID {
			return agent.TenantID
		}
	}
	return ""
}
