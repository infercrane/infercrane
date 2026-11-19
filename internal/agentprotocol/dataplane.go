package agentprotocol

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

// The agent as the data plane inside a customer's cloud. The agent pulls
// what it needs to serve an endpoint's traffic itself (routes, a snapshot of
// hashed API keys, admission policies, and request-quota leases) over the
// same outbound HTTPS channel it syncs on. Requests are routed only to
// servers this agent owns, prompts and completions stay where the agent
// runs, and only content-free request shapes are sent back.

const (
	// OwnershipAgentManaged marks an agent-owned server whose traffic the
	// agent serves. The control plane never routes to it.
	OwnershipAgentManaged = "agent-managed"
	// ComputeModeDataPlane marks request records the agent's data plane
	// served; ComputeModeGateway marks the optional pass-through gateway.
	ComputeModeDataPlane = "agent-data-plane"
	ComputeModeGateway   = "agent-gateway"

	// ResidencyCustomerCloud means every binding of an endpoint is a server an
	// agent owns, so request content never reaches the control plane.
	ResidencyCustomerCloud = "customer-cloud"
	ResidencyAny           = "any"

	// DataPlaneValidity is how long the agent may keep serving on a snapshot
	// it could not refresh. After that it refuses requests, because a revoked
	// API key could otherwise keep working.
	DataPlaneValidity    = 15 * time.Minute
	MaxDataPlaneValidity = 24 * time.Hour

	MaxDataPlaneRoutes      = MaxServers
	MaxDataPlaneBindings    = 16
	MaxDataPlaneCredentials = 10000
	// MaxQuotaLease bounds one request-quota lease.
	MaxQuotaLease = 1000
	// MaxDataPlaneBodyBytes bounds one snapshot response.
	MaxDataPlaneBodyBytes = 16 << 20
)

var (
	credentialHashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
	identifierPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	generationPattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// DataPlaneSnapshot is everything an agent needs to serve its agent-managed
// endpoints without asking the control plane per request. It holds no
// request content and no secret: API keys appear only as SHA-256 digests,
// exactly as the control plane stores them.
type DataPlaneSnapshot struct {
	Generation string    `json:"generation"`
	Unchanged  bool      `json:"unchanged,omitempty"`
	ValidUntil time.Time `json:"valid_until"`
	TenantID   string    `json:"tenant_id"`
	// The fields below are omitted when Unchanged is true.
	Routes       []DataPlaneRoute      `json:"routes,omitempty"`
	Credentials  []DataPlaneCredential `json:"credentials,omitempty"`
	Admission    []DataPlaneAdmission  `json:"admission,omitempty"`
	RequestQuota *DataPlaneQuota       `json:"request_quota,omitempty"`
}

// DataPlaneRoute is one agent-managed endpoint and the servers that serve it.
type DataPlaneRoute struct {
	Endpoint       string             `json:"endpoint"`
	EndpointID     string             `json:"endpoint_id"`
	LogicalModelID string             `json:"logical_model_id"`
	EnvironmentID  string             `json:"environment_id"`
	ServingPlanID  string             `json:"serving_plan_id"`
	RoutingPolicy  string             `json:"routing_policy"`
	Bindings       []DataPlaneBinding `json:"bindings"`
}

// DataPlaneBinding names a server by the agent's own server key. The agent
// resolves the key to an address it found itself; the control plane never
// tells the agent where to send traffic.
type DataPlaneBinding struct {
	BindingID     string                               `json:"binding_id"`
	TargetID      string                               `json:"target_id"`
	ServerKey     string                               `json:"server_key"`
	UpstreamModel string                               `json:"upstream_model"`
	Runtime       string                               `json:"runtime"`
	Weight        int                                  `json:"weight"`
	Capabilities  runtimecontract.ProtocolCapabilities `json:"capabilities"`
}

// DataPlaneCredential is one API key that may call the agent's endpoints,
// as the SHA-256 digest of the key.
type DataPlaneCredential struct {
	Hash          string     `json:"hash"`
	PrincipalID   string     `json:"principal_id"`
	Role          string     `json:"role"`
	Scopes        []string   `json:"scopes"`
	EndpointNames []string   `json:"endpoint_names"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

// DataPlaneAdmission is one endpoint's admission policy, enforced by the
// agent exactly as the hosted gateway enforces it.
type DataPlaneAdmission struct {
	Endpoint          string   `json:"endpoint"`
	MaxConcurrency    int      `json:"max_concurrency"`
	MaxQueueDepth     int      `json:"max_queue_depth"`
	QueueTimeoutMS    int      `json:"queue_timeout_ms"`
	RequestTimeoutMS  int      `json:"request_timeout_ms"`
	MaxRequestBytes   int      `json:"max_request_bytes"`
	MaxOutputTokens   int      `json:"max_output_tokens"`
	AllowedPriorities []string `json:"allowed_priorities"`
	RetryBudget       int      `json:"retry_budget"`
	Enabled           bool     `json:"enabled"`
}

// DataPlaneQuota is the workspace's hard request rate. The agent spends it
// only through leases (QuotaLeaseRequest), so several agents and the hosted
// gateway share one limit.
type DataPlaneQuota struct {
	MaxRequestsPerMinute int `json:"max_requests_per_minute"`
}

// QuotaLeaseRequest asks for part of the current minute's request quota.
type QuotaLeaseRequest struct {
	Window    time.Time `json:"window"`
	Requested int       `json:"requested"`
}

// QuotaLeaseResponse grants zero or more requests for one minute window.
// Unlimited means the workspace has no request quota.
type QuotaLeaseResponse struct {
	Window    time.Time `json:"window"`
	Granted   int       `json:"granted"`
	Unlimited bool      `json:"unlimited,omitempty"`
}

// ValidGeneration reports whether a generation is a snapshot digest.
func ValidGeneration(value string) bool { return generationPattern.MatchString(value) }

// Validate bounds a snapshot before the agent serves with it, so a broken
// control plane cannot make the agent route somewhere unexpected.
func (s DataPlaneSnapshot) Validate(now time.Time) error {
	if !ValidGeneration(s.Generation) {
		return errors.New("data plane generation is invalid")
	}
	if s.ValidUntil.IsZero() || s.ValidUntil.Before(now) || s.ValidUntil.After(now.Add(MaxDataPlaneValidity)) {
		return errors.New("data plane valid_until is outside the accepted window")
	}
	if !identifierPattern.MatchString(s.TenantID) {
		return errors.New("data plane tenant_id is invalid")
	}
	if s.Unchanged {
		return nil
	}
	if len(s.Routes) > MaxDataPlaneRoutes || len(s.Credentials) > MaxDataPlaneCredentials || len(s.Admission) > MaxDataPlaneRoutes {
		return errors.New("data plane snapshot is too large")
	}
	seen := map[string]bool{}
	for _, route := range s.Routes {
		if !ValidEndpointName(route.Endpoint) || seen[route.Endpoint] {
			return fmt.Errorf("data plane route %q is invalid or repeated", route.Endpoint)
		}
		seen[route.Endpoint] = true
		for _, value := range []string{route.EndpointID, route.LogicalModelID, route.EnvironmentID, route.ServingPlanID} {
			if !identifierPattern.MatchString(value) {
				return fmt.Errorf("data plane route %s has an invalid identity", route.Endpoint)
			}
		}
		switch route.RoutingPolicy {
		case "manual", "weighted", "primary-fallback":
		default:
			return fmt.Errorf("data plane route %s has an unknown routing policy", route.Endpoint)
		}
		if len(route.Bindings) == 0 || len(route.Bindings) > MaxDataPlaneBindings {
			return fmt.Errorf("data plane route %s needs 1..%d bindings", route.Endpoint, MaxDataPlaneBindings)
		}
		for _, binding := range route.Bindings {
			if !identifierPattern.MatchString(binding.BindingID) || !identifierPattern.MatchString(binding.TargetID) || !ValidServerKey(binding.ServerKey) {
				return fmt.Errorf("data plane route %s has an invalid binding", route.Endpoint)
			}
			if err := boundedText("upstream_model", binding.UpstreamModel, true); err != nil {
				return err
			}
			if err := boundedText("runtime", binding.Runtime, true); err != nil {
				return err
			}
			if binding.Weight < 0 || binding.Weight > 100 {
				return fmt.Errorf("data plane route %s has a weight outside 0..100", route.Endpoint)
			}
		}
	}
	for _, credential := range s.Credentials {
		if !credentialHashPattern.MatchString(credential.Hash) || !identifierPattern.MatchString(credential.PrincipalID) {
			return errors.New("data plane credential is invalid")
		}
		if err := boundedText("role", credential.Role, true); err != nil {
			return err
		}
		if len(credential.Scopes) > 64 || len(credential.EndpointNames) > 256 {
			return errors.New("data plane credential is too large")
		}
	}
	for _, policy := range s.Admission {
		if !ValidEndpointName(policy.Endpoint) || policy.MaxConcurrency < 1 || policy.MaxQueueDepth < 0 || policy.QueueTimeoutMS < 1 || policy.RequestTimeoutMS < 0 || policy.MaxRequestBytes < 1 || policy.MaxOutputTokens < 1 || policy.RetryBudget < 0 || policy.RetryBudget > 3 || len(policy.AllowedPriorities) > 8 {
			return fmt.Errorf("data plane admission policy for %q is invalid", policy.Endpoint)
		}
	}
	if s.RequestQuota != nil && s.RequestQuota.MaxRequestsPerMinute < 0 {
		return errors.New("data plane request quota is invalid")
	}
	return nil
}

// Validate bounds a quota lease request.
func (r QuotaLeaseRequest) Validate(now time.Time) error {
	if r.Requested < 1 || r.Requested > MaxQuotaLease {
		return fmt.Errorf("requested must be between 1 and %d", MaxQuotaLease)
	}
	window := r.Window.UTC()
	if !window.Equal(window.Truncate(time.Minute)) {
		return errors.New("window must be a UTC minute")
	}
	current := now.UTC().Truncate(time.Minute)
	if !window.Equal(current) && !window.Equal(current.Add(-time.Minute)) && !window.Equal(current.Add(time.Minute)) {
		return errors.New("window must be the current minute")
	}
	return nil
}
