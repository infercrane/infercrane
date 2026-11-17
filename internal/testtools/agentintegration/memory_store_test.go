package agentintegration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/controlapi"
	"github.com/infercrane/infercrane/internal/domain"
)

// memoryStore implements the relay store, the control API's audit write, and
// a credential snapshot that drops revoked keys at once.
type memoryStore struct {
	controlapi.Store // nil: only Audit is reached by the agent routes

	mu           sync.Mutex
	agents       map[string]*domain.Agent
	keys         map[string]string // credential hash -> agent ID
	servers      map[string]*domain.AgentServer
	measurements map[string][]domain.OperationalMeasurement
	requests     []domain.InferenceRecord
	health       map[string]string
	endpoints    map[string]string // endpoint name -> agent/key
	audits       []string
	next         int
}

func newMemoryStore() *memoryStore {
	return &memoryStore{agents: map[string]*domain.Agent{}, keys: map[string]string{}, servers: map[string]*domain.AgentServer{}, measurements: map[string][]domain.OperationalMeasurement{}, health: map[string]string{}, endpoints: map[string]string{}}
}

func (m *memoryStore) id(prefix string) string {
	m.next++
	return fmt.Sprintf("%s-%d", prefix, m.next)
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (m *memoryStore) Audit(_ context.Context, event domain.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audits = append(m.audits, event.Action)
	return nil
}

// AuthenticatePrincipal resolves the operator key and live agent keys.
func (m *memoryStore) AuthenticatePrincipal(_ context.Context, token string) (domain.Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if token == operatorKey {
		return domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "operator", Role: string(authz.Operator), Scopes: authz.DefaultScopeNames(authz.Operator)}, nil
	}
	agentID, ok := m.keys[hash(token)]
	if !ok {
		return domain.Principal{}, domain.ErrNotFound
	}
	agent := m.agents[agentID]
	if agent.State != "active" {
		return domain.Principal{}, domain.ErrNotFound
	}
	return domain.Principal{ID: agent.PrincipalID, TenantID: agent.TenantID, Name: "agent:" + agent.Name, Role: string(authz.Viewer), Kind: domain.AgentPrincipalKind}, nil
}

func (m *memoryStore) CreateAgent(_ context.Context, tenant, name, createdBy string) (domain.Agent, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, agent := range m.agents {
		if agent.TenantID == tenant && agent.Name == name && agent.State == "active" {
			return domain.Agent{}, "", domain.ErrConflict
		}
	}
	agent := &domain.Agent{ID: m.id("agent"), TenantID: tenant, PrincipalID: m.id("principal"), Name: name, State: "active", CreatedBy: createdBy, HardwareJSON: "{}", ServersJSON: "[]", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	key := "ic_" + hash(agent.ID)
	m.agents[agent.ID] = agent
	m.keys[hash(key)] = agent.ID
	return *agent, key, nil
}

func (m *memoryStore) AgentsForTenant(_ context.Context, tenant string) ([]domain.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Agent{}
	for _, agent := range m.agents {
		if agent.TenantID == tenant {
			out = append(out, *agent)
		}
	}
	return out, nil
}

func (m *memoryStore) AgentForTenant(_ context.Context, tenant, id string) (domain.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, agent := range m.agents {
		if agent.TenantID == tenant && (agent.ID == id || agent.Name == id) {
			return *agent, nil
		}
	}
	return domain.Agent{}, domain.ErrNotFound
}

func (m *memoryStore) AgentForPrincipal(_ context.Context, principalID string) (domain.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, agent := range m.agents {
		if agent.PrincipalID == principalID {
			return *agent, nil
		}
	}
	return domain.Agent{}, domain.ErrNotFound
}

func (m *memoryStore) RevokeAgent(_ context.Context, tenant, id string) (domain.Agent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[id]
	if !ok || agent.TenantID != tenant {
		return domain.Agent{}, domain.ErrNotFound
	}
	now := time.Now().UTC()
	agent.State, agent.RevokedAt = "revoked", &now
	for key, server := range m.servers {
		if server.AgentID == id {
			m.health[key] = "unhealthy"
		}
	}
	return *agent, nil
}

func (m *memoryStore) RecordAgentHeartbeat(_ context.Context, tenant, agentID string, heartbeat domain.AgentHeartbeat) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	agent, ok := m.agents[agentID]
	if !ok || agent.TenantID != tenant || agent.State != "active" {
		return domain.ErrNotFound
	}
	seen := heartbeat.SeenAt
	agent.AgentVersion, agent.ProtocolVersion, agent.OS, agent.Arch = heartbeat.AgentVersion, heartbeat.ProtocolVersion, heartbeat.OS, heartbeat.Arch
	agent.HardwareJSON, agent.ServersJSON, agent.GatewayServerKey, agent.LastSeenAt = heartbeat.HardwareJSON, heartbeat.ServersJSON, heartbeat.GatewayServerKey, &seen
	return nil
}

func (m *memoryStore) AgentServers(_ context.Context, _ string, agentID string) ([]domain.AgentServer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.AgentServer{}
	for key, server := range m.servers {
		if server.AgentID == agentID {
			copy := *server
			copy.Health = m.health[key]
			out = append(out, copy)
		}
	}
	return out, nil
}

func (m *memoryStore) RegisterAgentServer(_ context.Context, _ string, agentID string, registration domain.AgentServerRegistration) (domain.AgentServer, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	slot := agentID + "/" + registration.Key
	if existing, ok := m.servers[slot]; ok {
		return *existing, false, nil
	}
	if _, taken := m.endpoints[registration.EndpointName]; taken {
		return domain.AgentServer{}, false, domain.ErrConflict
	}
	server := &domain.AgentServer{AgentID: agentID, Key: registration.Key, TargetID: m.id("target"), Address: registration.Address, Runtime: registration.Runtime, UpstreamModel: registration.UpstreamModel, Health: "starting", EndpointID: m.id("endpoint"), EndpointName: registration.EndpointName, LogicalModelID: m.id("model"), EnvironmentID: "production", BindingID: m.id("binding"), ServingPlanID: m.id("plan"), OwnershipMode: "observe-only"}
	m.servers[slot] = server
	m.health[slot] = "starting"
	m.endpoints[registration.EndpointName] = slot
	return *server, true, nil
}

func (m *memoryStore) ReportAgentServerHealth(_ context.Context, _ string, agentID, key, health string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.health[agentID+"/"+key] = health
	return nil
}

func (m *memoryStore) RecordTargetMeasurements(_ context.Context, _ string, targetID string, rows []domain.OperationalMeasurement) ([]domain.OperationalMeasurement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.measurements[targetID] = append(m.measurements[targetID], rows...)
	return rows, nil
}

func (m *memoryStore) RecordRequest(_ context.Context, record domain.InferenceRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.requests {
		if existing.RequestID == record.RequestID {
			return nil
		}
	}
	m.requests = append(m.requests, record)
	return nil
}

func (m *memoryStore) serverByEndpoint(name string) domain.AgentServer {
	m.mu.Lock()
	defer m.mu.Unlock()
	found, ok := m.servers[m.endpoints[name]]
	if !ok {
		return domain.AgentServer{}
	}
	server := *found
	server.Health = m.health[m.endpoints[name]]
	return server
}

func (m *memoryStore) measurementsFor(targetID string) []domain.OperationalMeasurement {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.OperationalMeasurement(nil), m.measurements[targetID]...)
}

func (m *memoryStore) requestRecords() []domain.InferenceRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.InferenceRecord(nil), m.requests...)
}

func (m *memoryStore) deleteEndpoint(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	slot := m.endpoints[name]
	delete(m.servers, slot)
	delete(m.endpoints, name)
}
