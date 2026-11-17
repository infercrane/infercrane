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
