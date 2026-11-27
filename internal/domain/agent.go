package domain

import "time"

// AgentPrincipalKind is the principal kind of an agent key. The control API
// lets this kind call only the agent routes.
const AgentPrincipalKind = "agent"

// Agent is one InferCrane agent: an outbound-only process beside a customer's
// model server. HardwareJSON and ServersJSON hold the content-free inventory
// it last reported.
type Agent struct {
	ID, TenantID, PrincipalID, Name, State, CreatedBy string
	AgentVersion, ProtocolVersion, OS, Arch           string
	HardwareJSON, ServersJSON                         string
	GatewayServerKey                                  string
	LastSeenAt                                        *time.Time
	CreatedAt, UpdatedAt                              time.Time
	RevokedAt                                         *time.Time
}

// AgentHeartbeat is what one agent sync records about the agent itself.
type AgentHeartbeat struct {
	AgentVersion, ProtocolVersion, OS, Arch string
	HardwareJSON, ServersJSON               string
	GatewayServerKey                        string
	SeenAt                                  time.Time
}

// AgentServer is a local server observed through an agent, with the endpoint
// identities a relayed request record needs.
type AgentServer struct {
	AgentID, Key, TargetID, Address, Runtime, UpstreamModel string
	Health                                                  string
	ReportedAt                                              *time.Time
	EndpointID, EndpointName, LogicalModelID, EnvironmentID string
	BindingID, ServingPlanID, OwnershipMode                 string
	// Rollout marks a server a rollout started on an endpoint the agent
	// already serves: a second binding there, never the endpoint's own
	// server and never the current setup a run measures against.
	Rollout bool
}

// AgentServerRegistration asks to observe one local server.
type AgentServerRegistration struct {
	Key, EndpointName, UpstreamModel, Runtime, Address string
}

// SealedCredential is a credential encrypted with the tenant credential key.
// It is never serialized.
type SealedCredential struct {
	Ciphertext, Nonce []byte
}

// AdoptedCredential is the sealed bearer credential of one adopted server
// that the control plane reaches directly.
type AdoptedCredential struct {
	TenantID, EndpointName, TargetID string
	Sealed                           SealedCredential
}

// AgentDataPlaneBinding is one active-plan binding of an agent-managed
// endpoint, with the identities a request record needs. The agent resolves
// ServerKey to an address it found itself.
type AgentDataPlaneBinding struct {
	EndpointID, EndpointName, LogicalModelID, EnvironmentID string
	ServingPlanID, RoutingPolicy                            string
	BindingID, TargetID, ServerKey, UpstreamModel, Runtime  string
	Health                                                  string
	Priority, Weight                                        int
	// Rollout marks a server a rollout started beside the endpoint's own.
	Rollout bool
}

// AgentCommand is work queued for one agent and its content-free outcome.
type AgentCommand struct {
	ID, TenantID, AgentID, Kind, State, Phase string
	// IdempotencyKey, when set, makes queuing the same work twice return
	// the command queued first.
	IdempotencyKey             string
	SpecJSON, ResultJSON       string
	FailureCode, FailureDetail string
	Attempts                   int
	LeaseExpiresAt             *time.Time
	DeadlineAt                 time.Time
	CreatedBy                  string
	CreatedAt, UpdatedAt       time.Time
	StartedAt, FinishedAt      *time.Time
}

// AgentCommandUpdate is an agent's report on one leased command.
type AgentCommandUpdate struct {
	State, Phase, FailureCode, FailureDetail, ResultJSON string
	Lease                                                time.Duration
}

// AgentRolloutSwitch is where a guarded rollout on an agent's endpoint stands:
// the plan that routes to the rollout's server first and the endpoint's own
// server as its fallback, the plan it replaced, the guard decision that
// allowed the switch, and when.
type AgentRolloutSwitch struct {
	PlanID, PreviousPlanID, EvaluationID string
	// OwnBindingID is the endpoint's own server's binding, the fallback.
	OwnBindingID string
	// SwitchedAt is when the guard allowed the switch: the watch window
	// counts from it.
	SwitchedAt time.Time
}
