package agentrelay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/admission"
	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

// ErrUnavailable means this control plane's store cannot serve the agent
// data plane or command channel.
var ErrUnavailable = errors.New("the agent data plane is not available on this control plane")

// DataPlaneStore is the persistence the agent data plane needs.
type DataPlaneStore interface {
	AgentDataPlaneBindings(context.Context, string, string) ([]domain.AgentDataPlaneBinding, error)
	AgentDataPlaneCredentials(context.Context, string) ([]domain.CredentialRecord, error)
	AdmissionPoliciesForTenant(context.Context, string) ([]admission.Policy, error)
	RequestQuotaLimit(context.Context, string) (int, bool, error)
	ReserveRequestQuota(context.Context, string, time.Time, int) (int, error)
}

func (s *Service) dataPlaneStore() (DataPlaneStore, bool) {
	store, ok := s.Store.(DataPlaneStore)
	return store, ok
}

func (s *Service) validity() time.Duration {
	if s.DataPlaneValidity > 0 && s.DataPlaneValidity <= agentprotocol.MaxDataPlaneValidity {
		return s.DataPlaneValidity
	}
	return agentprotocol.DataPlaneValidity
}

// capabilities resolves what an engine's OpenAI surface supports. An engine
// the runtime registry does not know is an OpenAI-compatible server the
// agent found: chat, text completions, embeddings, and streaming.
func (s *Service) capabilities(runtime string) runtimecontract.ProtocolCapabilities {
	if s.Capabilities != nil {
		if capabilities := s.Capabilities(runtime); capabilities != (runtimecontract.ProtocolCapabilities{}) {
			return capabilities
		}
	}
	return runtimecontract.ProtocolCapabilities{ChatCompletions: true, Completions: true, Embeddings: true, Streaming: true}
}

// DataPlane builds what one agent needs to serve its agent-managed endpoints:
// routes by server key, the digests of the workspace's API keys that may call
// those endpoints, their admission policies, and the request quota. When the
// agent already holds the current generation only a fresh validity window is
// returned.
func (s *Service) DataPlane(ctx context.Context, agent domain.Agent, generation string) (agentprotocol.DataPlaneSnapshot, error) {
	store, ok := s.dataPlaneStore()
	if !ok {
		return agentprotocol.DataPlaneSnapshot{}, ErrUnavailable
	}
	bindings, err := store.AgentDataPlaneBindings(ctx, agent.TenantID, agent.ID)
	if err != nil {
		return agentprotocol.DataPlaneSnapshot{}, err
	}
	snapshot := agentprotocol.DataPlaneSnapshot{TenantID: agent.TenantID, Routes: []agentprotocol.DataPlaneRoute{}, Credentials: []agentprotocol.DataPlaneCredential{}, Admission: []agentprotocol.DataPlaneAdmission{}}
	served := map[string]bool{}
	byEndpoint := map[string]int{}
	for _, binding := range bindings {
		index, seen := byEndpoint[binding.EndpointName]
		if !seen {
			if len(snapshot.Routes) >= agentprotocol.MaxDataPlaneRoutes {
				continue
			}
			index = len(snapshot.Routes)
			byEndpoint[binding.EndpointName] = index
			served[binding.EndpointName] = true
			snapshot.Routes = append(snapshot.Routes, agentprotocol.DataPlaneRoute{
				Endpoint: binding.EndpointName, EndpointID: binding.EndpointID, LogicalModelID: binding.LogicalModelID,
				EnvironmentID: binding.EnvironmentID, ServingPlanID: binding.ServingPlanID, RoutingPolicy: binding.RoutingPolicy,
			})
		}
		route := &snapshot.Routes[index]
		if len(route.Bindings) >= agentprotocol.MaxDataPlaneBindings {
			continue
		}
		route.Bindings = append(route.Bindings, agentprotocol.DataPlaneBinding{
			BindingID: binding.BindingID, TargetID: binding.TargetID, ServerKey: binding.ServerKey,
			UpstreamModel: binding.UpstreamModel, Runtime: binding.Runtime, Weight: binding.Weight,
			Capabilities: s.capabilities(binding.Runtime),
			// An agent out of contact serves only the endpoint's own
			// servers; it drops the bindings marked here.
			Rollout: binding.Rollout,
		})
	}
	if len(served) > 0 {
		records, credentialErr := store.AgentDataPlaneCredentials(ctx, agent.TenantID)
		if credentialErr != nil {
			return agentprotocol.DataPlaneSnapshot{}, credentialErr
		}
		if len(records) > agentprotocol.MaxDataPlaneCredentials {
			return agentprotocol.DataPlaneSnapshot{}, fmt.Errorf("the workspace has more than %d API keys; the agent data plane cannot carry them all", agentprotocol.MaxDataPlaneCredentials)
		}
		for _, record := range records {
			principal := record.Principal
			if principal.Kind == domain.AgentPrincipalKind || principal.Disabled || principal.TenantID != agent.TenantID || !authz.AllowedScoped(authz.Role(principal.Role), principal.Scopes, authz.Read) || !mayCallAny(principal.EndpointNames, served) {
				continue
			}
			snapshot.Credentials = append(snapshot.Credentials, agentprotocol.DataPlaneCredential{
				Hash: record.Hash, PrincipalID: principal.ID, Role: principal.Role, Scopes: nonNil(principal.Scopes),
				EndpointNames: nonNil(principal.EndpointNames), ExpiresAt: principal.ExpiresAt,
			})
		}
		policies, policyErr := store.AdmissionPoliciesForTenant(ctx, agent.TenantID)
		if policyErr != nil {
			return agentprotocol.DataPlaneSnapshot{}, policyErr
		}
		for _, policy := range policies {
			endpoint := strings.TrimPrefix(policy.Key, agent.TenantID+"\x00")
			if !served[endpoint] || endpoint == policy.Key {
				continue
			}
			priorities := make([]string, 0, len(policy.AllowedPriorities))
			for priority := range policy.AllowedPriorities {
				priorities = append(priorities, priority)
			}
			sort.Strings(priorities)
			snapshot.Admission = append(snapshot.Admission, agentprotocol.DataPlaneAdmission{
				Endpoint: endpoint, MaxConcurrency: policy.MaxConcurrency, MaxQueueDepth: policy.MaxQueueDepth,
				QueueTimeoutMS: int(policy.QueueTimeout / time.Millisecond), RequestTimeoutMS: int(policy.RequestTimeout / time.Millisecond),
				MaxRequestBytes: policy.MaxRequestBytes, MaxOutputTokens: policy.MaxOutputTokens, AllowedPriorities: priorities,
				RetryBudget: policy.RetryBudget, Enabled: policy.Enabled,
			})
		}
		sort.Slice(snapshot.Admission, func(i, j int) bool { return snapshot.Admission[i].Endpoint < snapshot.Admission[j].Endpoint })
		limit, limited, quotaErr := store.RequestQuotaLimit(ctx, agent.TenantID)
		if quotaErr != nil {
			return agentprotocol.DataPlaneSnapshot{}, quotaErr
		}
		if limited {
			snapshot.RequestQuota = &agentprotocol.DataPlaneQuota{MaxRequestsPerMinute: limit}
		}
	}
	encoded, err := json.Marshal(struct {
		Tenant      string                              `json:"tenant"`
		Routes      []agentprotocol.DataPlaneRoute      `json:"routes"`
		Credentials []agentprotocol.DataPlaneCredential `json:"credentials"`
		Admission   []agentprotocol.DataPlaneAdmission  `json:"admission"`
		Quota       *agentprotocol.DataPlaneQuota       `json:"quota"`
	}{snapshot.TenantID, snapshot.Routes, snapshot.Credentials, snapshot.Admission, snapshot.RequestQuota})
	if err != nil {
		return agentprotocol.DataPlaneSnapshot{}, err
	}
	digest := sha256.Sum256(encoded)
	snapshot.Generation = "sha256:" + hex.EncodeToString(digest[:])
	snapshot.ValidUntil = s.now().Add(s.validity())
	if generation == snapshot.Generation {
		return agentprotocol.DataPlaneSnapshot{Generation: snapshot.Generation, Unchanged: true, ValidUntil: snapshot.ValidUntil, TenantID: snapshot.TenantID}, nil
	}
	return snapshot, nil
}

func mayCallAny(restrictions []string, served map[string]bool) bool {
	if len(restrictions) == 0 {
		return true
	}
	for _, name := range restrictions {
		if served[name] {
			return true
		}
	}
	return false
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// LeaseQuota grants part of the current minute's workspace request quota to
// one agent, from the same durable allocator the hosted gateway uses.
func (s *Service) LeaseQuota(ctx context.Context, agent domain.Agent, request agentprotocol.QuotaLeaseRequest) (agentprotocol.QuotaLeaseResponse, error) {
	store, ok := s.dataPlaneStore()
	if !ok {
		return agentprotocol.QuotaLeaseResponse{}, ErrUnavailable
	}
	if err := request.Validate(s.now()); err != nil {
		return agentprotocol.QuotaLeaseResponse{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	window := request.Window.UTC()
	granted, err := store.ReserveRequestQuota(ctx, agent.TenantID, window, request.Requested)
	if errors.Is(err, domain.ErrNotFound) {
		return agentprotocol.QuotaLeaseResponse{Window: window, Unlimited: true}, nil
	}
	if err != nil {
		return agentprotocol.QuotaLeaseResponse{}, err
	}
	return agentprotocol.QuotaLeaseResponse{Window: window, Granted: granted}, nil
}
