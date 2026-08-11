// Package integration owns versioned provider/runtime contracts, capability
// declarations, and qualification state. It does not implement providers,
// runtimes, lifecycle policy, or public support policy.
package integration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

const (
	ProviderContractV1 = "infercrane.provider/v1"
	RuntimeContractV1  = "infercrane.runtime/v1"
)

type ComputeMode string

const (
	ElasticMode    ComputeMode = "elastic"
	ServerlessMode ComputeMode = "serverless"
	ExternalMode   ComputeMode = "external"
)

type CapabilityState string

const (
	CapabilitySupported   CapabilityState = "supported"
	CapabilityUnsupported CapabilityState = "unsupported"
	CapabilityUnknown     CapabilityState = "unknown"
)

type QualificationState string

const (
	QualificationRegistered QualificationState = "registered"
	QualificationSimulated  QualificationState = "simulated"
	QualificationLocal      QualificationState = "local-qualified"
	QualificationReal       QualificationState = "real-qualified"
	QualificationDeferred   QualificationState = "deferred"
	QualificationFailed     QualificationState = "failed"
)

type Capability struct {
	Name     string          `json:"name"`
	State    CapabilityState `json:"state"`
	Detail   string          `json:"detail,omitempty"`
	Evidence string          `json:"evidence,omitempty"`
}

// RequestSurvivalContract delegates in-progress request migration to a
// qualified runtime/backend. InferCrane never implements token/KV migration.
type RequestSurvivalContract struct {
	State         CapabilityState    `json:"state"`
	Mechanism     string             `json:"mechanism,omitempty"`
	Evidence      string             `json:"evidence,omitempty"`
	Qualification QualificationState `json:"qualification"`
}

func (c RequestSurvivalContract) Validate() error {
	if c.State == CapabilitySupported && (c.Mechanism == "" || c.Evidence == "" || (c.Qualification != QualificationLocal && c.Qualification != QualificationReal)) {
		return errors.New("request survival support requires a delegated mechanism and qualified evidence")
	}
	if c.State != CapabilitySupported && c.State != CapabilityUnsupported && c.State != CapabilityUnknown {
		return errors.New("invalid request survival state")
	}
	return nil
}

type Qualification struct {
	State       QualificationState `json:"state"`
	Evidence    string             `json:"evidence,omitempty"`
	Environment string             `json:"environment,omitempty"`
	Reason      string             `json:"reason,omitempty"`
}

type ProviderProfile struct {
	Adapter         string          `json:"adapter"`
	Cloud           string          `json:"cloud"`
	ContractVersion string          `json:"contract_version"`
	AdapterVersion  string          `json:"adapter_version"`
	Modes           []ComputeMode   `json:"modes"`
	Capabilities    []Capability    `json:"capabilities"`
	Qualification   []Qualification `json:"qualification"`
}

type RuntimeProfile struct {
	Runtime         string                   `json:"runtime"`
	ContractVersion string                   `json:"contract_version"`
	AdapterVersion  string                   `json:"adapter_version"`
	EngineVersion   string                   `json:"engine_version,omitempty"`
	Protocol        string                   `json:"protocol"`
	Capabilities    []Capability             `json:"capabilities"`
	Qualification   []Qualification          `json:"qualification"`
	DefaultWorkload runtimecontract.Workload `json:"default_workload,omitzero"`
}

// ProtocolCapabilities projects independently qualified protocol behavior
// from the runtime profile. Unknown and unsupported declarations fail closed.
func (p RuntimeProfile) ProtocolCapabilities() runtimecontract.ProtocolCapabilities {
	var out runtimecontract.ProtocolCapabilities
	for _, capability := range p.Capabilities {
		if capability.State != CapabilitySupported {
			continue
		}
		switch capability.Name {
		case "buffered_chat":
			out.ChatCompletions = true
		case "responses":
			out.Responses = true
		case "embeddings":
			out.Embeddings = true
		case "chat_batch":
			out.Batch = true
		case "completions":
			out.Completions = true
		case "streaming_chat", "streaming_responses", "streaming_completions":
			out.Streaming = true
		case "tool_calling":
			out.ToolCalling = true
		}
	}
	return out
}

type RuntimeCompatibility struct {
	Runtime  string             `json:"runtime"`
	Adapter  string             `json:"adapter"`
	Cloud    string             `json:"cloud"`
	Mode     ComputeMode        `json:"mode"`
	State    QualificationState `json:"state"`
	Evidence string             `json:"evidence,omitempty"`
	Reason   string             `json:"reason,omitempty"`
}

// ElasticProvider is the mutation boundary for one durable replica intent.
// Implementations must use ExternalKey for replay-safe ensure/adoption.
type ElasticProvider interface {
	Handle(string) provision.ProviderHandle
	EnsureReplica(context.Context, provision.ReplicaSpec) (provision.ProviderHandle, error)
	ObserveReplica(context.Context, provision.ProviderHandle, int) (provision.Observation, error)
	DeleteReplica(context.Context, provision.ProviderHandle) error
}

// ServerlessProvider manages a provider-native endpoint. Worker scheduling is
// intentionally absent: it remains owned by the provider.
type ServerlessProvider interface {
	EnsureEndpoint(context.Context, provision.ServerlessEndpointSpec) (provision.ServerlessEndpoint, error)
	ListEndpoints(context.Context) ([]provision.ServerlessEndpoint, error)
	DeleteEndpoint(context.Context, string) error
	EndpointURL(string) string
}

// RuntimeInspector is the minimum health/model-identity contract needed by
// reconciliation. Protocol, streaming, cancellation, drain, and telemetry are
// separately declared and qualified capabilities.
type RuntimeInspector interface {
	Inspect(context.Context, string) (bool, map[string]struct{})
}

// RuntimeBackend binds executable runtime behavior to the exact profile that
// declares and qualifies it. This prevents an implementation from executing
// under another adapter's capability claims.
type RuntimeBackend struct {
	Profile   RuntimeProfile
	Inspector RuntimeInspector
}

// RuntimeBackends is immutable after construction. Lifecycle code selects a
// backend by declared runtime identity and never branches on engine names.
type RuntimeBackends struct {
	byRuntime map[string]RuntimeBackend
}

func NewRuntimeBackends(backends ...RuntimeBackend) (RuntimeBackends, error) {
	registry := RuntimeBackends{byRuntime: make(map[string]RuntimeBackend, len(backends))}
	for _, backend := range backends {
		backend.Profile = normalizeRuntime(backend.Profile)
		if err := backend.Profile.Validate(); err != nil {
			return RuntimeBackends{}, fmt.Errorf("validate runtime backend: %w", err)
		}
		if backend.Inspector == nil {
			return RuntimeBackends{}, fmt.Errorf("runtime backend %q requires an inspector", backend.Profile.Runtime)
		}
		if _, duplicate := registry.byRuntime[backend.Profile.Runtime]; duplicate {
			return RuntimeBackends{}, fmt.Errorf("runtime backend %q is already registered", backend.Profile.Runtime)
		}
		registry.byRuntime[backend.Profile.Runtime] = backend
	}
	return registry, nil
}

func (r RuntimeBackends) ForRuntime(name string) (RuntimeBackend, error) {
	backend, ok := r.byRuntime[name]
	if !ok {
		return RuntimeBackend{}, fmt.Errorf("no runtime backend is registered for runtime %q", name)
	}
	return backend, nil
}

type Registry struct {
	providers     map[string]ProviderProfile
	runtimes      map[string]RuntimeProfile
	compatibility []RuntimeCompatibility
}

type Snapshot struct {
	ProviderContract string                 `json:"provider_contract"`
	RuntimeContract  string                 `json:"runtime_contract"`
	Providers        []ProviderProfile      `json:"providers"`
	Runtimes         []RuntimeProfile       `json:"runtimes"`
	Compatibility    []RuntimeCompatibility `json:"compatibility"`
}

func (r *Registry) SetCompatibility(entries ...RuntimeCompatibility) error {
	seen := map[string]struct{}{}
	for _, entry := range entries {
		if _, err := r.Runtime(entry.Runtime); err != nil {
			return err
		}
		profile, err := r.Provider(entry.Adapter)
		if err != nil {
			return fmt.Errorf("runtime compatibility provider: %w", err)
		}
		if entry.Cloud == "" || profile.Cloud != entry.Cloud || !containsMode(profile.Modes, entry.Mode) {
			return errors.New("runtime compatibility requires an adapter with matching cloud and mode")
		}
		switch entry.State {
		case QualificationRegistered, QualificationSimulated, QualificationLocal, QualificationReal, QualificationDeferred, QualificationFailed:
		default:
			return fmt.Errorf("runtime compatibility has invalid state %q", entry.State)
		}
		if (entry.State == QualificationLocal || entry.State == QualificationReal || entry.State == QualificationSimulated) && entry.Evidence == "" {
			return errors.New("qualified runtime compatibility requires evidence")
		}
		key := entry.Runtime + "\x00" + entry.Adapter + "\x00" + string(entry.Mode)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate runtime compatibility for %s/%s/%s", entry.Runtime, entry.Cloud, entry.Mode)
		}
		seen[key] = struct{}{}
	}
	r.compatibility = append([]RuntimeCompatibility(nil), entries...)
	return nil
}

func containsMode(modes []ComputeMode, mode ComputeMode) bool {
	for _, candidate := range modes {
		if candidate == mode {
			return true
		}
	}
	return false
}

func NewRegistry() *Registry {
	return &Registry{providers: map[string]ProviderProfile{}, runtimes: map[string]RuntimeProfile{}}
}

func (r *Registry) RegisterProvider(profile ProviderProfile) error {
	if r == nil {
		return errors.New("integration registry is nil")
	}
	profile = normalizeProvider(profile)
	if err := profile.Validate(); err != nil {
		return err
	}
	if _, exists := r.providers[profile.Adapter]; exists {
		return fmt.Errorf("provider adapter %q is already registered", profile.Adapter)
	}
	r.providers[profile.Adapter] = profile
	return nil
}

func (r *Registry) RegisterRuntime(profile RuntimeProfile) error {
	if r == nil {
		return errors.New("integration registry is nil")
	}
	profile = normalizeRuntime(profile)
	if err := profile.Validate(); err != nil {
		return err
	}
	if _, exists := r.runtimes[profile.Runtime]; exists {
		return fmt.Errorf("runtime adapter %q is already registered", profile.Runtime)
	}
	r.runtimes[profile.Runtime] = profile
	return nil
}

func (r *Registry) Provider(adapter string) (ProviderProfile, error) {
	if r == nil {
		return ProviderProfile{}, errors.New("integration registry is nil")
	}
	profile, ok := r.providers[adapter]
	if !ok {
		return ProviderProfile{}, fmt.Errorf("provider adapter %q is not registered", adapter)
	}
	return profile, nil
}

func (r *Registry) Runtime(name string) (RuntimeProfile, error) {
	if r == nil {
		return RuntimeProfile{}, errors.New("integration registry is nil")
	}
	profile, ok := r.runtimes[name]
	if !ok {
		return RuntimeProfile{}, fmt.Errorf("runtime adapter %q is not registered", name)
	}
	return profile, nil
}

func (r *Registry) Snapshot() Snapshot {
	out := Snapshot{ProviderContract: ProviderContractV1, RuntimeContract: RuntimeContractV1}
	if r == nil {
		return out
	}
	for _, profile := range r.providers {
		out.Providers = append(out.Providers, profile)
	}
	for _, profile := range r.runtimes {
		out.Runtimes = append(out.Runtimes, profile)
	}
	out.Compatibility = append([]RuntimeCompatibility(nil), r.compatibility...)
	sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].Adapter < out.Providers[j].Adapter })
	sort.Slice(out.Runtimes, func(i, j int) bool { return out.Runtimes[i].Runtime < out.Runtimes[j].Runtime })
	sort.Slice(out.Compatibility, func(i, j int) bool {
		a, b := out.Compatibility[i], out.Compatibility[j]
		if a.Runtime != b.Runtime {
			return a.Runtime < b.Runtime
		}
		if a.Cloud != b.Cloud {
			return a.Cloud < b.Cloud
		}
		return a.Mode < b.Mode
	})
	return out
}

func (p ProviderProfile) Validate() error {
	if p.Adapter == "" || p.Cloud == "" || p.AdapterVersion == "" {
		return errors.New("provider adapter, cloud, and adapter version are required")
	}
	if p.ContractVersion != ProviderContractV1 {
		return fmt.Errorf("provider %q uses unsupported contract %q", p.Adapter, p.ContractVersion)
	}
	if len(p.Modes) == 0 {
		return fmt.Errorf("provider %q must declare at least one compute mode", p.Adapter)
	}
	seen := map[ComputeMode]struct{}{}
	for _, mode := range p.Modes {
		if mode != ElasticMode && mode != ServerlessMode && mode != ExternalMode {
			return fmt.Errorf("provider %q declares invalid compute mode %q", p.Adapter, mode)
		}
		if _, duplicate := seen[mode]; duplicate {
			return fmt.Errorf("provider %q declares compute mode %q more than once", p.Adapter, mode)
		}
		seen[mode] = struct{}{}
	}
	return validateEvidence(p.Adapter, p.Capabilities, p.Qualification)
}

func (p RuntimeProfile) Validate() error {
	if p.Runtime == "" || p.AdapterVersion == "" || p.Protocol == "" {
		return errors.New("runtime, adapter version, and protocol are required")
	}
	if p.ContractVersion != RuntimeContractV1 {
		return fmt.Errorf("runtime %q uses unsupported contract %q", p.Runtime, p.ContractVersion)
	}
	if !p.DefaultWorkload.Empty() {
		if err := p.DefaultWorkload.Validate(); err != nil {
			return fmt.Errorf("runtime %q default workload: %w", p.Runtime, err)
		}
	}
	return validateEvidence(p.Runtime, p.Capabilities, p.Qualification)
}

func validateEvidence(name string, capabilities []Capability, qualifications []Qualification) error {
	seen := map[string]struct{}{}
	for _, capability := range capabilities {
		if capability.Name == "" {
			return fmt.Errorf("integration %q has an unnamed capability", name)
		}
		if _, duplicate := seen[capability.Name]; duplicate {
			return fmt.Errorf("integration %q declares capability %q more than once", name, capability.Name)
		}
		seen[capability.Name] = struct{}{}
		if capability.State != CapabilitySupported && capability.State != CapabilityUnsupported && capability.State != CapabilityUnknown {
			return fmt.Errorf("integration %q capability %q has invalid state %q", name, capability.Name, capability.State)
		}
		if capability.State == CapabilitySupported && capability.Evidence == "" {
			return fmt.Errorf("integration %q capability %q cannot be supported without evidence", name, capability.Name)
		}
	}
	for _, qualification := range qualifications {
		switch qualification.State {
		case QualificationRegistered, QualificationSimulated, QualificationLocal, QualificationReal, QualificationDeferred, QualificationFailed:
		default:
			return fmt.Errorf("integration %q has invalid qualification state %q", name, qualification.State)
		}
		if qualification.State == QualificationReal && qualification.Evidence == "" {
			return fmt.Errorf("integration %q cannot be real-qualified without evidence", name)
		}
		if qualification.State == QualificationDeferred && qualification.Reason == "" {
			return fmt.Errorf("integration %q deferred qualification requires a reason", name)
		}
	}
	return nil
}

func normalizeProvider(profile ProviderProfile) ProviderProfile {
	profile.Adapter = strings.TrimSpace(profile.Adapter)
	profile.Cloud = strings.TrimSpace(profile.Cloud)
	profile.ContractVersion = strings.TrimSpace(profile.ContractVersion)
	profile.AdapterVersion = strings.TrimSpace(profile.AdapterVersion)
	profile.Modes = append([]ComputeMode(nil), profile.Modes...)
	profile.Capabilities = append([]Capability(nil), profile.Capabilities...)
	profile.Qualification = append([]Qualification(nil), profile.Qualification...)
	sort.Slice(profile.Modes, func(i, j int) bool { return profile.Modes[i] < profile.Modes[j] })
	sort.Slice(profile.Capabilities, func(i, j int) bool { return profile.Capabilities[i].Name < profile.Capabilities[j].Name })
	return profile
}

func normalizeRuntime(profile RuntimeProfile) RuntimeProfile {
	profile.Runtime = strings.TrimSpace(profile.Runtime)
	profile.ContractVersion = strings.TrimSpace(profile.ContractVersion)
	profile.AdapterVersion = strings.TrimSpace(profile.AdapterVersion)
	profile.EngineVersion = strings.TrimSpace(profile.EngineVersion)
	profile.Protocol = strings.TrimSpace(profile.Protocol)
	profile.Capabilities = append([]Capability(nil), profile.Capabilities...)
	profile.Qualification = append([]Qualification(nil), profile.Qualification...)
	profile.DefaultWorkload.Command = append([]string(nil), profile.DefaultWorkload.Command...)
	sort.Slice(profile.Capabilities, func(i, j int) bool { return profile.Capabilities[i].Name < profile.Capabilities[j].Name })
	return profile
}

func HasMode(profile ProviderProfile, mode ComputeMode) bool {
	for _, declared := range profile.Modes {
		if declared == mode {
			return true
		}
	}
	return false
}
