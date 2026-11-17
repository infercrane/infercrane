// Package agentrelay is the control-plane side of the InferCrane agent. It
// issues and revokes agent keys, registers agent-local servers as
// observe-only endpoints, and turns relayed engine scrapes and content-free
// traffic shapes into the same operational measurements and request records
// the control plane produces when it scrapes or routes a server itself. It
// never dials an agent's servers and never branches on engine or provider
// names: an engine is identified only by its metric namespace, as for a
// direct scrape.
package agentrelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/metrics"
)

var (
	// ErrRevoked means the agent's key was revoked; the agent must stop.
	ErrRevoked = errors.New("agent is revoked")
	// ErrUpgradeRequired means this control plane no longer accepts the
	// agent's version or protocol.
	ErrUpgradeRequired = errors.New("agent upgrade required")
	// ErrInvalid wraps a request that failed validation.
	ErrInvalid = errors.New("invalid agent request")
)

// evidenceTTL matches the control plane's own scrape validity: three scrape
// intervals, after which a reading turns stale instead of looking live.
const evidenceTTL = 3 * agentprotocol.ScrapeInterval

// Store is the persistence the relay needs.
type Store interface {
	CreateAgent(context.Context, string, string, string) (domain.Agent, string, error)
	AgentsForTenant(context.Context, string) ([]domain.Agent, error)
	AgentForTenant(context.Context, string, string) (domain.Agent, error)
	AgentForPrincipal(context.Context, string) (domain.Agent, error)
	RevokeAgent(context.Context, string, string) (domain.Agent, error)
	RecordAgentHeartbeat(context.Context, string, string, domain.AgentHeartbeat) error
	AgentServers(context.Context, string, string) ([]domain.AgentServer, error)
	RegisterAgentServer(context.Context, string, string, domain.AgentServerRegistration) (domain.AgentServer, bool, error)
	ReportAgentServerHealth(context.Context, string, string, string, string, time.Time) error
	RecordTargetMeasurements(context.Context, string, string, []domain.OperationalMeasurement) ([]domain.OperationalMeasurement, error)
	RecordRequest(context.Context, domain.InferenceRecord) error
}

// Service implements agent management and the agent protocol.
type Service struct {
	Store Store
	// PollInterval is advised to agents; zero means the protocol default.
	PollInterval time.Duration
	// MinimumAgentVersion refuses older agents with ErrUpgradeRequired when
	// set. RecommendedAgentVersion is only advice.
	MinimumAgentVersion, RecommendedAgentVersion string
	// RefreshEndpoints recompiles endpoint state after a registration so the
	// new endpoint reads "observing" without waiting for the next pass.
	RefreshEndpoints func(context.Context) error
	Now              func() time.Time
}

func (s Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s Service) pollInterval() time.Duration {
	interval := s.PollInterval
	if interval <= 0 {
		interval = agentprotocol.DefaultPollInterval
	}
	if interval < agentprotocol.MinPollInterval {
		interval = agentprotocol.MinPollInterval
	}
	if interval > agentprotocol.MaxPollInterval {
		interval = agentprotocol.MaxPollInterval
	}
	return interval
}

// Create mints an agent key for a tenant. The key is returned once.
func (s Service) Create(ctx context.Context, tenant, name, actor string) (domain.Agent, string, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if !agentprotocol.ValidEndpointName(name) {
		return domain.Agent{}, "", fmt.Errorf("%w: agent name must be 1..64 lowercase letters, digits, dots, underscores, or hyphens", ErrInvalid)
	}
	return s.Store.CreateAgent(ctx, tenant, name, actor)
}

// List returns a tenant's agents.
func (s Service) List(ctx context.Context, tenant string) ([]domain.Agent, error) {
	return s.Store.AgentsForTenant(ctx, tenant)
}

// Get returns one agent and the servers observed through it.
func (s Service) Get(ctx context.Context, tenant, id string) (domain.Agent, []domain.AgentServer, error) {
	agent, err := s.Store.AgentForTenant(ctx, tenant, id)
	if err != nil {
		return domain.Agent{}, nil, err
	}
	servers, err := s.Store.AgentServers(ctx, tenant, agent.ID)
	return agent, servers, err
}

// Revoke disables an agent. Its next request is refused.
func (s Service) Revoke(ctx context.Context, tenant, id string) (domain.Agent, error) {
	agent, err := s.Store.AgentForTenant(ctx, tenant, id)
	if err != nil {
		return domain.Agent{}, err
	}
	return s.Store.RevokeAgent(ctx, tenant, agent.ID)
}

// Authenticate resolves the agent behind an authenticated agent principal
// and refuses a revoked one.
func (s Service) Authenticate(ctx context.Context, principal domain.Principal) (domain.Agent, error) {
	if principal.Kind != domain.AgentPrincipalKind {
		return domain.Agent{}, ErrRevoked
	}
	agent, err := s.Store.AgentForPrincipal(ctx, principal.ID)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.Agent{}, ErrRevoked
	}
	if err != nil {
		return domain.Agent{}, err
	}
	if agent.State != "active" || agent.TenantID != principal.TenantID {
		return domain.Agent{}, ErrRevoked
	}
	return agent, nil
}

// Upgrade says whether an agent version may connect.
func (s Service) Upgrade(agentVersion string) agentprotocol.Upgrade {
	upgrade := agentprotocol.Upgrade{Minimum: s.MinimumAgentVersion, Recommended: s.RecommendedAgentVersion}
	if s.MinimumAgentVersion != "" && compareVersions(agentVersion, s.MinimumAgentVersion) < 0 {
		upgrade.Required = true
		upgrade.Detail = fmt.Sprintf("this control plane needs InferCrane agent %s or newer", s.MinimumAgentVersion)
	} else if s.RecommendedAgentVersion != "" && compareVersions(agentVersion, s.RecommendedAgentVersion) < 0 {
		upgrade.Detail = fmt.Sprintf("InferCrane %s is available", s.RecommendedAgentVersion)
	}
	return upgrade
}

// Register observes one agent-local server as an observe-only endpoint.
func (s Service) Register(ctx context.Context, agent domain.Agent, request agentprotocol.RegisterServerRequest) (agentprotocol.RegisterServerResponse, error) {
	if err := request.Validate(); err != nil {
		return agentprotocol.RegisterServerResponse{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	server, created, err := s.Store.RegisterAgentServer(ctx, agent.TenantID, agent.ID, domain.AgentServerRegistration{
		Key: request.ServerKey, EndpointName: request.Endpoint, UpstreamModel: request.Model, Runtime: request.Engine, Address: request.Address,
	})
	if err != nil {
		return agentprotocol.RegisterServerResponse{}, err
	}
	if created && s.RefreshEndpoints != nil {
		_ = s.RefreshEndpoints(ctx)
	}
	return agentprotocol.RegisterServerResponse{Server: registered(server, agent.GatewayServerKey), Created: created}, nil
}

// Sync applies one agent sync: heartbeat and inventory, server health,
// relayed engine scrapes, and traffic shapes. One bad relay is rejected
// without failing the others.
func (s Service) Sync(ctx context.Context, agent domain.Agent, request agentprotocol.SyncRequest) (agentprotocol.SyncResponse, error) {
	now := s.now()
	if err := request.Validate(now); err != nil {
		return agentprotocol.SyncResponse{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	upgrade := s.Upgrade(request.AgentVersion)
	if upgrade.Required {
		return agentprotocol.SyncResponse{Upgrade: upgrade}, ErrUpgradeRequired
	}
	hardware, _ := json.Marshal(request.Hardware)
	servers, _ := json.Marshal(request.Servers)
	gatewayKey := ""
	if request.Gateway != nil {
		gatewayKey = request.Gateway.ServerKey
	}
	if err := s.Store.RecordAgentHeartbeat(ctx, agent.TenantID, agent.ID, domain.AgentHeartbeat{
		AgentVersion: request.AgentVersion, ProtocolVersion: request.Protocol, OS: request.OS, Arch: request.Arch,
		HardwareJSON: string(hardware), ServersJSON: string(servers), GatewayServerKey: gatewayKey, SeenAt: now,
	}); errors.Is(err, domain.ErrNotFound) {
		return agentprotocol.SyncResponse{}, ErrRevoked
	} else if err != nil {
		return agentprotocol.SyncResponse{}, err
	}
	known, err := s.Store.AgentServers(ctx, agent.TenantID, agent.ID)
	if err != nil {
		return agentprotocol.SyncResponse{}, err
	}
	byKey := make(map[string]domain.AgentServer, len(known))
	for _, server := range known {
		byKey[server.Key] = server
	}
	response := agentprotocol.SyncResponse{
		AgentID: agent.ID, AgentName: agent.Name, PollIntervalSeconds: int(s.pollInterval() / time.Second),
		Servers: []agentprotocol.RegisteredServer{}, Rejected: []agentprotocol.Rejection{}, Upgrade: upgrade, Commands: []agentprotocol.Command{},
	}
	for _, reported := range request.Servers {
		server, ok := byKey[reported.Key]
		if !ok {
			continue
		}
		if err = s.Store.ReportAgentServerHealth(ctx, agent.TenantID, agent.ID, reported.Key, reported.Health, now); err != nil {
			return agentprotocol.SyncResponse{}, err
		}
		server.Health = reported.Health
		byKey[reported.Key] = server
	}
	for _, relay := range request.Metrics {
		server, ok := byKey[relay.ServerKey]
		if !ok {
			response.Rejected = append(response.Rejected, agentprotocol.Rejection{ServerKey: relay.ServerKey, Kind: "metrics", Reason: "server is not registered with this agent"})
			continue
		}
		count, relayErr := s.recordMetrics(ctx, agent.TenantID, server, relay, now)
		if relayErr != nil {
			response.Rejected = append(response.Rejected, agentprotocol.Rejection{ServerKey: relay.ServerKey, Kind: "metrics", Reason: relayErr.Error()})
			continue
		}
		response.Accepted.Measurements += count
	}
	skew := now.Sub(request.SentAt.UTC())
	for _, shape := range request.Traffic {
		server, ok := byKey[shape.ServerKey]
		if !ok {
			response.Rejected = append(response.Rejected, agentprotocol.Rejection{ServerKey: shape.ServerKey, Kind: "traffic", Reason: "server is not registered with this agent"})
			continue
		}
		record, shapeErr := requestRecord(agent, server, shape, skew, now)
		if shapeErr != nil {
			response.Rejected = append(response.Rejected, agentprotocol.Rejection{ServerKey: shape.ServerKey, Kind: "traffic", Reason: shapeErr.Error()})
			continue
		}
		if err = s.Store.RecordRequest(ctx, record); err != nil {
			return agentprotocol.SyncResponse{}, fmt.Errorf("record relayed request: %w", err)
		}
		response.Accepted.Requests++
	}
	for _, server := range known {
		response.Servers = append(response.Servers, registered(byKey[server.Key], gatewayKey))
	}
	return response, nil
}

// recordMetrics derives measurements from a relayed pair of scrapes with the
// same parser and window rules as a direct scrape, keyed by the target.
func (s Service) recordMetrics(ctx context.Context, tenant string, server domain.AgentServer, relay agentprotocol.MetricRelay, now time.Time) (int, error) {
	current, err := metrics.ParseEngine(relay.Current.Text)
	if err != nil {
		return 0, fmt.Errorf("engine metrics could not be read: %v", err)
	}
	var baseline *metrics.EngineSnapshot
	if relay.Baseline != nil && relay.Baseline.AgeMS > relay.Current.AgeMS {
		previous, parseErr := metrics.ParseEngine(relay.Baseline.Text)
		if parseErr == nil && metrics.SameEngineProcess(&previous, current) {
			baseline = &previous
		}
	}
	observedAt := now.Add(-time.Duration(relay.Current.AgeMS) * time.Millisecond)
	if now.Sub(observedAt) >= evidenceTTL {
		return 0, errors.New("the scrape is older than its validity window")
	}
	rows, err := metrics.EngineMeasurements(baseline, current, metrics.EngineMeasurementOptions{ReplicaID: server.TargetID, ObservedAt: observedAt, TTL: evidenceTTL})
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if _, err = s.Store.RecordTargetMeasurements(ctx, tenant, server.TargetID, rows); err != nil {
		return 0, fmt.Errorf("engine metrics could not be stored: %v", err)
	}
	return len(rows), nil
}

func requestRecord(agent domain.Agent, server domain.AgentServer, shape agentprotocol.TrafficShape, skew time.Duration, now time.Time) (domain.InferenceRecord, error) {
	started := shape.StartedAt.UTC().Add(skew)
	if started.Before(now.Add(-30*24*time.Hour)) || started.After(now.Add(5*time.Minute)) {
		return domain.InferenceRecord{}, errors.New("started_at is outside the accepted window")
	}
	digest := sha256.Sum256([]byte(agent.TenantID + "\x00" + agent.ID + "\x00" + shape.ID))
	var generation *float64
	if shape.Streaming && shape.TTFTMS != nil && shape.LatencyMS >= *shape.TTFTMS {
		value := shape.LatencyMS - *shape.TTFTMS
		generation = &value
	}
	return domain.InferenceRecord{
		RequestID: "agent-" + hex.EncodeToString(digest[:]), TenantID: agent.TenantID,
		TargetID: server.TargetID, LogicalModelID: server.LogicalModelID, EnvironmentID: server.EnvironmentID,
		EndpointID: server.EndpointID, ServingPlanID: server.ServingPlanID, BindingID: server.BindingID,
		Provider: "external", Runtime: server.Runtime, ComputeMode: "agent-gateway",
		OperationName: shape.Operation, RequestModel: shape.RequestModel,
		SemanticConventionSchema: "https://opentelemetry.io/schemas/gen-ai/1.42.0",
		StartedAt:                started, StatusCode: shape.StatusCode, LatencyMS: shape.LatencyMS, TTFTMS: shape.TTFTMS, GenerationMS: generation,
		InputTokens: shape.InputTokens, OutputTokens: shape.OutputTokens, Streaming: shape.Streaming,
		ErrorType: shape.ErrorType, SharedPrefixHash: shape.SharedPrefixHash, SessionIDHash: shape.SessionIDHash,
	}, nil
}

func registered(server domain.AgentServer, gatewayKey string) agentprotocol.RegisteredServer {
	out := agentprotocol.RegisteredServer{
		Key: server.Key, Endpoint: server.EndpointName, TargetID: server.TargetID, Model: server.UpstreamModel,
		Ownership: server.OwnershipMode, Health: server.Health,
		TrafficSource: "none",
		TrafficDetail: "No traffic is visible: requests go straight to the server. Route them through the agent's local gateway (infercrane agent run --gateway) to record their shape.",
	}
	if gatewayKey != "" && gatewayKey == server.Key {
		out.TrafficSource = "agent-gateway"
		out.TrafficDetail = "Requests sent through the agent's local gateway are recorded as content-free shapes. Requests sent straight to the server are not."
	}
	return out
}

// compareVersions orders dotted numeric versions, ignoring a leading "v" and
// any pre-release suffix. Unparseable versions compare as oldest.
func compareVersions(left, right string) int {
	parse := func(value string) ([]int, bool) {
		value = strings.TrimPrefix(strings.TrimSpace(value), "v")
		if index := strings.IndexAny(value, "-+ "); index >= 0 {
			value = value[:index]
		}
		parts := strings.Split(value, ".")
		out := make([]int, 0, len(parts))
		for _, part := range parts {
			number, err := strconv.Atoi(part)
			if err != nil {
				return nil, false
			}
			out = append(out, number)
		}
		return out, len(out) > 0
	}
	a, okA := parse(left)
	b, okB := parse(right)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for index := 0; index < len(a) || index < len(b); index++ {
		var x, y int
		if index < len(a) {
			x = a[index]
		}
		if index < len(b) {
			y = b[index]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}
