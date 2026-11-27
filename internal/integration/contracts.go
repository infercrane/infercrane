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

	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/provision"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/support"
)

const (
	ProviderContractV1 = "infercrane.provider/v1"
	RuntimeContractV1  = "infercrane.runtime/v1"
	CompositionV1      = "infercrane.composition/v1"

	CompositionGateway         = "gateway"
	CompositionSandbox         = "sandbox"
	CompositionTraining        = "training"
	CompositionArtifactBuilder = "artifact-builder"
	CompositionCache           = "cache"
	CompositionOrchestrator    = "orchestrator"
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
	// Images are the runtime's pinned builds per accelerator platform. A
	// built-in runtime serves the image for the GPU's platform and refuses a
	// GPU whose platform it has no image for.
	Images []support.RuntimeImage `json:"images,omitempty"`
}

// WorkloadFor is the contract a replica of one engine release on this GPU
// serves: the default workload with the image pinned for that release and the
// GPU's accelerator platform. An empty version is the profile's current
// engine release. A runtime without per-platform images serves its default
// workload on every GPU. A release without an image for the platform is
// refused: serving another release's image would record a version the
// replica does not run.
func (p RuntimeProfile) WorkloadFor(gpu, version string) (runtimecontract.Workload, error) {
	if len(p.Images) == 0 {
		return p.DefaultWorkload, nil
	}
	platform := provideridentity.AcceleratorPlatform(gpu)
	if version = support.NormalizeRuntimeVersion(version); version == "" {
		version = p.EngineVersion
	}
	var others []string
	for _, image := range p.Images {
		if image.Platform != platform {
			continue
		}
		if version == "" || image.EngineVersion == version {
			workload := p.DefaultWorkload
			workload.Command = append([]string(nil), workload.Command...)
			workload.Image = image.Image
			return workload, nil
		}
		others = append(others, image.EngineVersion)
	}
	if len(others) > 0 {
		return runtimecontract.Workload{}, fmt.Errorf("runtime %q %s has no pinned image for %s (platform %s); pinned there: %s", p.Runtime, version, gpu, platform, strings.Join(others, ", "))
	}
	return runtimecontract.Workload{}, fmt.Errorf("runtime %q has no pinned image for %s (platform %s)", p.Runtime, gpu, platform)
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
	// Accelerators, RuntimeVersion, and Image narrow an entry to the exact
	// tuple one run qualified. Empty fields cover every value. A narrowed
	// entry sits beside the adapter's general one and is chosen only where
	// it applies.
	Accelerators   []string `json:"accelerators,omitempty"`
	RuntimeVersion string   `json:"runtime_version,omitempty"`
	Image          string   `json:"image,omitempty"`
}

// Applies reports whether the entry covers a GPU and engine version. GPU
// names compare without vendor prefix, case, or separators, so NVIDIA-A10G
// and a10g are the same GPU; NVIDIA-A10G-SHARED, a time-sliced label, is not.
func (c RuntimeCompatibility) Applies(gpu, runtimeVersion string) bool {
	if c.RuntimeVersion != "" && strings.TrimPrefix(strings.TrimSpace(runtimeVersion), "v") != c.RuntimeVersion {
		return false
	}
	if len(c.Accelerators) == 0 {
		return true
	}
	want := provideridentity.NormalizedAccelerator(gpu)
	if want == "" {
		return false
	}
	for _, accelerator := range c.Accelerators {
		if provideridentity.NormalizedAccelerator(accelerator) == want {
			return true
		}
	}
	return false
}

func (c RuntimeCompatibility) narrowed() bool {
	return len(c.Accelerators) > 0 || c.RuntimeVersion != "" || c.Image != ""
}

func (c RuntimeCompatibility) scopeKey() string {
	accelerators := make([]string, 0, len(c.Accelerators))
	for _, accelerator := range c.Accelerators {
		accelerators = append(accelerators, provideridentity.NormalizedAccelerator(accelerator))
	}
	sort.Strings(accelerators)
	return strings.Join(accelerators, ",") + "\x00" + c.RuntimeVersion + "\x00" + c.Image
}

// QualificationRank orders states by the evidence behind them: real, then
// local, then simulated, then every other state.
func QualificationRank(state QualificationState) int {
	switch state {
	case QualificationReal:
		return 0
	case QualificationLocal:
		return 1
	case QualificationSimulated:
		return 2
	default:
		return 3
	}
}

// Strongest keeps one entry for each runtime, adapter, and mode: the best
// qualified, and on a tie the narrowed one. The entries must already apply
// to the request. Order follows the first entry of each group.
func Strongest(entries []RuntimeCompatibility) []RuntimeCompatibility {
	best := map[string]int{}
	var out []RuntimeCompatibility
	for _, entry := range entries {
		key := entry.Runtime + "\x00" + entry.Adapter + "\x00" + string(entry.Mode)
		index, seen := best[key]
		if !seen {
			best[key] = len(out)
			out = append(out, entry)
			continue
		}
		current := out[index]
		if rank, held := QualificationRank(entry.State), QualificationRank(current.State); rank < held || rank == held && entry.narrowed() && !current.narrowed() {
			out[index] = entry
		}
	}
	return out
}

// SelectCompatibility returns the strongest entry for a runtime on a cloud
// in a mode that applies to the GPU and engine version.
func SelectCompatibility(entries []RuntimeCompatibility, runtime, cloud string, mode ComputeMode, gpu, runtimeVersion string) (RuntimeCompatibility, bool) {
	var applicable []RuntimeCompatibility
	for _, entry := range entries {
		if entry.Runtime == runtime && entry.Cloud == cloud && entry.Mode == mode && entry.Applies(gpu, runtimeVersion) {
			applicable = append(applicable, entry)
		}
	}
	if len(applicable) == 0 {
		return RuntimeCompatibility{}, false
	}
	sort.SliceStable(applicable, func(i, j int) bool {
		left, right := QualificationRank(applicable[i].State), QualificationRank(applicable[j].State)
		if left != right {
			return left < right
		}
		return applicable[i].narrowed() && !applicable[j].narrowed()
	})
	return applicable[0], true
}

// CompositionProfile describes a replaceable external system that composes
// with InferCrane without becoming a provider, runtime, or lifecycle owner.
type CompositionProfile struct {
	Adapter         string          `json:"adapter"`
	Kind            string          `json:"kind"`
	ContractVersion string          `json:"contract_version"`
	Ownership       string          `json:"ownership"`
	Capabilities    []Capability    `json:"capabilities"`
	Qualification   []Qualification `json:"qualification"`
}

func (p CompositionProfile) Validate() error {
	if p.Adapter == "" || p.ContractVersion != CompositionV1 {
		return errors.New("composition adapter and current contract version are required")
	}
	switch p.Kind {
	case CompositionGateway, CompositionSandbox, CompositionTraining, CompositionArtifactBuilder, CompositionCache, CompositionOrchestrator:
	default:
		return errors.New("composition kind must be gateway, sandbox, training, artifact-builder, cache, or orchestrator")
	}
	if p.Ownership == "" || len(p.Capabilities) == 0 || len(p.Qualification) == 0 {
		return errors.New("composition ownership, capabilities, and qualification are required")
	}
	for _, capability := range p.Capabilities {
		if capability.Name == "" || (capability.State != CapabilitySupported && capability.State != CapabilityUnsupported && capability.State != CapabilityUnknown) {
			return errors.New("composition capability is invalid")
		}
		if capability.State == CapabilitySupported && capability.Evidence == "" {
			return fmt.Errorf("composition capability %q is supported without evidence", capability.Name)
		}
	}
	for _, qualification := range p.Qualification {
		switch qualification.State {
		case QualificationRegistered, QualificationSimulated, QualificationLocal, QualificationReal, QualificationDeferred, QualificationFailed:
		default:
			return fmt.Errorf("composition qualification has invalid state %q", qualification.State)
		}
		if (qualification.State == QualificationSimulated || qualification.State == QualificationLocal || qualification.State == QualificationReal) && qualification.Evidence == "" {
			return errors.New("qualified composition requires evidence")
		}
		if qualification.State == QualificationDeferred && qualification.Reason == "" {
			return errors.New("deferred composition qualification requires a reason")
		}
	}
	return nil
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
	compositions  map[string]CompositionProfile
	compatibility []RuntimeCompatibility
}

type Snapshot struct {
	ProviderContract    string                 `json:"provider_contract"`
	RuntimeContract     string                 `json:"runtime_contract"`
	Providers           []ProviderProfile      `json:"providers"`
	Runtimes            []RuntimeProfile       `json:"runtimes"`
	Compatibility       []RuntimeCompatibility `json:"compatibility"`
	CompositionContract string                 `json:"composition_contract"`
	Compositions        []CompositionProfile   `json:"compositions"`
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
		for _, accelerator := range entry.Accelerators {
			if provideridentity.NormalizedAccelerator(accelerator) == "" {
				return errors.New("runtime compatibility names an empty accelerator")
			}
		}
		if entry.narrowed() && entry.State != QualificationReal && entry.State != QualificationLocal {
			return fmt.Errorf("a narrowed runtime compatibility records a qualified run, not state %q", entry.State)
		}
		key := entry.Runtime + "\x00" + entry.Adapter + "\x00" + string(entry.Mode) + "\x00" + entry.scopeKey()
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
	return &Registry{providers: map[string]ProviderProfile{}, runtimes: map[string]RuntimeProfile{}, compositions: map[string]CompositionProfile{}}
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

func (r *Registry) RegisterComposition(profile CompositionProfile) error {
	if r == nil {
		return errors.New("integration registry is nil")
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	key := profile.Kind + "\x00" + profile.Adapter
	if _, exists := r.compositions[key]; exists {
		return fmt.Errorf("%s composition adapter %q is already registered", profile.Kind, profile.Adapter)
	}
	r.compositions[key] = profile
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
	out := Snapshot{ProviderContract: ProviderContractV1, RuntimeContract: RuntimeContractV1, CompositionContract: CompositionV1}
	if r == nil {
		return out
	}
	for _, profile := range r.providers {
		out.Providers = append(out.Providers, profile)
	}
	for _, profile := range r.runtimes {
		out.Runtimes = append(out.Runtimes, profile)
	}
	for _, profile := range r.compositions {
		out.Compositions = append(out.Compositions, profile)
	}
	out.Compatibility = append([]RuntimeCompatibility(nil), r.compatibility...)
	sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].Adapter < out.Providers[j].Adapter })
	sort.Slice(out.Runtimes, func(i, j int) bool { return out.Runtimes[i].Runtime < out.Runtimes[j].Runtime })
	sort.Slice(out.Compositions, func(i, j int) bool {
		if out.Compositions[i].Kind != out.Compositions[j].Kind {
			return out.Compositions[i].Kind < out.Compositions[j].Kind
		}
		return out.Compositions[i].Adapter < out.Compositions[j].Adapter
	})
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
	// Images of the profile's engine release are current; an image of another
	// release must be marked legacy, so a revision that records it keeps its
	// engine and a new setup never gets it.
	seen := map[string]struct{}{}
	current := map[string]struct{}{}
	for _, image := range p.Images {
		if p.DefaultWorkload.Empty() {
			return fmt.Errorf("runtime %q declares images without a default workload", p.Runtime)
		}
		release := image.EngineVersion == p.EngineVersion && image.Status == support.ImageCurrent || image.EngineVersion != p.EngineVersion && image.Status == support.ImageLegacy
		if image.Runtime != p.Runtime || image.EngineVersion == "" || !release || image.Platform == "" {
			return fmt.Errorf("runtime %q image for platform %q does not match the runtime and engine version", p.Runtime, image.Platform)
		}
		key := image.EngineVersion + "/" + image.Platform
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("runtime %q declares platform %q more than once for engine %s", p.Runtime, image.Platform, image.EngineVersion)
		}
		seen[key] = struct{}{}
		if image.Status == support.ImageCurrent {
			current[image.Platform] = struct{}{}
		}
		if err := runtimecontract.ValidateImage(image.Image); err != nil {
			return fmt.Errorf("runtime %q image for platform %q: %w", p.Runtime, image.Platform, err)
		}
	}
	if len(p.Images) > 0 {
		if _, ok := current[provideridentity.PlatformCUDA]; !ok || p.Images[0].Status != support.ImageCurrent || p.DefaultWorkload.Image != p.Images[0].Image {
			return fmt.Errorf("runtime %q default workload must be its first, current CUDA image", p.Runtime)
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
	profile.Images = append([]support.RuntimeImage(nil), profile.Images...)
	for index := range profile.Images {
		profile.Images[index].Targets = append([]string(nil), profile.Images[index].Targets...)
	}
	sort.Slice(profile.Capabilities, func(i, j int) bool { return profile.Capabilities[i].Name < profile.Capabilities[j].Name })
	return profile
}

// CapabilityEngineEnvironment is a provider adapter passing a revision's
// reviewed engine environment variables to the runtime container.
const CapabilityEngineEnvironment = "engine_environment"

// Supports reports a capability the profile declares as supported. An
// absent, unknown, or unsupported declaration is not support.
func Supports(profile ProviderProfile, capability string) bool {
	for _, declared := range profile.Capabilities {
		if declared.Name == capability {
			return declared.State == CapabilitySupported
		}
	}
	return false
}

func HasMode(profile ProviderProfile, mode ComputeMode) bool {
	for _, declared := range profile.Modes {
		if declared == mode {
			return true
		}
	}
	return false
}
