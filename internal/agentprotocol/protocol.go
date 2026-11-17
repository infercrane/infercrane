// Package agentprotocol defines the wire contract between the InferCrane
// agent and the control plane. The agent only ever dials out: every message
// is an HTTPS request the agent makes, and every instruction from the control
// plane rides in a response. Payloads carry content-free shapes and engine
// metrics only; prompts and completions never appear in any type here.
package agentprotocol

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Version is the protocol spoken by this build. The control plane refuses a
// version it no longer supports with HTTP 426 so an old agent stops cleanly.
const Version = "infercrane.agent/v1"

const (
	// DefaultPollInterval is how often the agent syncs when the control plane
	// does not advise another interval.
	DefaultPollInterval = 15 * time.Second
	MinPollInterval     = 5 * time.Second
	MaxPollInterval     = 60 * time.Second

	// ScrapeInterval matches the control plane's own engine scrape cadence so
	// relayed and scraped evidence share one validity window.
	ScrapeInterval = 30 * time.Second

	MaxServers         = 32
	MaxModelsPerServer = 64
	MaxTrafficShapes   = 1000
	// MaxExpositionBytes bounds one filtered engine scrape. Filtering keeps
	// only the families InferCrane parses, so real servers stay far below it.
	MaxExpositionBytes = 1 << 20
	// MaxSyncBodyBytes bounds one sync request: every server may carry a
	// current and a baseline scrape, plus traffic shapes.
	MaxSyncBodyBytes = 8 << 20
	// MaxText bounds every free-text field.
	MaxText = 256
)

// Engines the agent recognizes. Unknown OpenAI-compatible servers are
// reported as EngineOpenAICompatible.
const (
	EngineVLLM             = "vllm"
	EngineSGLang           = "sglang"
	EngineLlamaCPP         = "llama.cpp"
	EngineOllama           = "ollama"
	EngineLMStudio         = "lm-studio"
	EngineOpenAICompatible = "openai-compatible"
)

// Server health as the agent observed it on its own machine.
const (
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

var (
	serverKeyPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,127}$`)
	endpointNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{0,62}[a-z0-9])?$`)
	shapeIDPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)
	hashPattern         = regexp.MustCompile(`^[a-f0-9]{64}$`)
	errorTypePattern    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// Accelerator is one kind of accelerator found on the agent's host. A field
// the agent could not read is nil or empty and is named in Unknown, never
// guessed.
type Accelerator struct {
	Vendor    string   `json:"vendor"`
	Name      string   `json:"name"`
	Count     int      `json:"count"`
	MemoryMiB *int     `json:"memory_mib"`
	Driver    string   `json:"driver,omitempty"`
	Source    string   `json:"source"`
	Unknown   []string `json:"unknown,omitempty"`
}

// Hardware is the content-free inventory of the agent's host.
type Hardware struct {
	State        string        `json:"state"`
	Accelerators []Accelerator `json:"accelerators"`
	Limitations  []string      `json:"limitations"`
}

// Server is one local model server the agent can see.
type Server struct {
	Key           string   `json:"key"`
	Engine        string   `json:"engine"`
	EngineVersion string   `json:"engine_version,omitempty"`
	Address       string   `json:"address"`
	Models        []string `json:"models"`
	// ActiveModel is the model the server has loaded, when the server says.
	ActiveModel      string `json:"active_model,omitempty"`
	Health           string `json:"health"`
	HealthDetail     string `json:"health_detail,omitempty"`
	MetricsAvailable bool   `json:"metrics_available"`
	MetricsDetail    string `json:"metrics_detail,omitempty"`
	Authenticated    bool   `json:"authenticated"`
}

// Exposition is one engine scrape, already filtered to the families the
// control plane parses. AgeMS is how long before the request was sent the
// scrape was taken, so the control plane can date it on its own clock.
type Exposition struct {
	AgeMS int64  `json:"age_ms"`
	Text  string `json:"text"`
}

// MetricRelay carries the newest scrape of one server and, when the agent has
// one, the oldest scrape of the same engine process inside the trailing
// window. The control plane derives window values from the pair exactly as it
// does for two scrapes it made itself, so it keeps no per-agent state.
type MetricRelay struct {
	ServerKey string      `json:"server_key"`
	Current   Exposition  `json:"current"`
	Baseline  *Exposition `json:"baseline,omitempty"`
}

// TrafficShape describes one request that passed through the agent's local
// gateway. It has no prompt, completion, header, or tool content. Hashes are
// keyed with a random secret created on the agent's machine and never sent,
// independent of the agent key, so they only say whether two requests shared
// a system prompt or session.
type TrafficShape struct {
	ID               string    `json:"id"`
	ServerKey        string    `json:"server_key"`
	StartedAt        time.Time `json:"started_at"`
	Operation        string    `json:"operation"`
	RequestModel     string    `json:"request_model,omitempty"`
	StatusCode       int       `json:"status_code"`
	LatencyMS        float64   `json:"latency_ms"`
	TTFTMS           *float64  `json:"ttft_ms,omitempty"`
	InputTokens      *int      `json:"input_tokens,omitempty"`
	OutputTokens     *int      `json:"output_tokens,omitempty"`
	Streaming        bool      `json:"streaming"`
	ErrorType        string    `json:"error_type,omitempty"`
	SharedPrefixHash string    `json:"shared_prefix_hash,omitempty"`
	SessionIDHash    string    `json:"session_id_hash,omitempty"`
}

// Gateway says whether requests reach a server through the agent.
type Gateway struct {
	Listen    string `json:"listen"`
	ServerKey string `json:"server_key"`
}

// SyncRequest is the agent's one periodic call.
type SyncRequest struct {
	Protocol     string         `json:"protocol"`
	AgentVersion string         `json:"agent_version"`
	OS           string         `json:"os"`
	Arch         string         `json:"arch"`
	SentAt       time.Time      `json:"sent_at"`
	Hardware     Hardware       `json:"hardware"`
	Servers      []Server       `json:"servers"`
	Metrics      []MetricRelay  `json:"metrics,omitempty"`
	Traffic      []TrafficShape `json:"traffic,omitempty"`
	Gateway      *Gateway       `json:"gateway,omitempty"`
}

// RegisteredServer is a local server the control plane observes through this
// agent.
type RegisteredServer struct {
	Key           string `json:"key"`
	Endpoint      string `json:"endpoint"`
	TargetID      string `json:"target_id"`
	Model         string `json:"model"`
	Ownership     string `json:"ownership"`
	Health        string `json:"health"`
	TrafficSource string `json:"traffic_source"`
	TrafficDetail string `json:"traffic_detail"`
}

// Rejection explains one relayed item the control plane did not accept.
type Rejection struct {
	ServerKey string `json:"server_key,omitempty"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
}

// Accepted counts what one sync stored.
type Accepted struct {
	Measurements int `json:"measurements"`
	Requests     int `json:"requests"`
}

// Upgrade is advice, never an instruction to self-update: the agent prints
// it, and only Required stops the agent.
type Upgrade struct {
	Minimum     string `json:"minimum,omitempty"`
	Recommended string `json:"recommended,omitempty"`
	Required    bool   `json:"required"`
	Detail      string `json:"detail,omitempty"`
}

// Command is reserved for work the control plane asks of the agent. Protocol
// v1 never sends one; see the agent architecture page for the candidate-run
// commands that will use it.
type Command struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
}

// SyncResponse is the control plane's answer to one sync.
type SyncResponse struct {
	AgentID             string             `json:"agent_id"`
	AgentName           string             `json:"agent_name"`
	PollIntervalSeconds int                `json:"poll_interval_seconds"`
	Servers             []RegisteredServer `json:"servers"`
	Accepted            Accepted           `json:"accepted"`
	Rejected            []Rejection        `json:"rejected"`
	Upgrade             Upgrade            `json:"upgrade"`
	Commands            []Command          `json:"commands"`
}

// RegisterServerRequest asks the control plane to observe one local server
// as an observe-only endpoint.
type RegisterServerRequest struct {
	ServerKey string `json:"server_key"`
	Endpoint  string `json:"endpoint"`
	Model     string `json:"model"`
	Engine    string `json:"engine"`
	Address   string `json:"address"`
}

// RegisterServerResponse returns the registration and whether it was new.
type RegisterServerResponse struct {
	Server  RegisteredServer `json:"server"`
	Created bool             `json:"created"`
}

// ValidServerKey reports whether key is a bounded agent-local server key.
func ValidServerKey(key string) bool { return serverKeyPattern.MatchString(key) }

// ValidEndpointName reports whether name is a valid endpoint name.
func ValidEndpointName(name string) bool { return endpointNamePattern.MatchString(name) }

// KnownEngine reports whether engine is one the agent may report.
func KnownEngine(engine string) bool {
	switch engine {
	case EngineVLLM, EngineSGLang, EngineLlamaCPP, EngineOllama, EngineLMStudio, EngineOpenAICompatible:
		return true
	}
	return false
}

func boundedText(field, value string, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", field)
	}
	if len(value) > MaxText {
		return fmt.Errorf("%s must be at most %d bytes", field, MaxText)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains a control character", field)
		}
	}
	return nil
}

// Validate bounds the envelope of a sync: identity, hardware, counts, and
// the gateway. Servers, metric relays, and traffic shapes are validated one
// by one (Server.Validate, MetricRelay.Validate, TrafficShape.Validate) so
// one bad item is rejected on its own and never blocks the agent's
// heartbeat.
func (r SyncRequest) Validate(now time.Time) error {
	if r.Protocol != Version {
		return fmt.Errorf("protocol must be %s", Version)
	}
	for field, value := range map[string]string{"agent_version": r.AgentVersion, "os": r.OS, "arch": r.Arch} {
		if err := boundedText(field, value, true); err != nil {
			return err
		}
	}
	if r.SentAt.IsZero() {
		return errors.New("sent_at is required")
	}
	if err := r.Hardware.validate(); err != nil {
		return err
	}
	if len(r.Servers) > MaxServers {
		return fmt.Errorf("at most %d servers may be reported", MaxServers)
	}
	if len(r.Metrics) > MaxServers {
		return fmt.Errorf("at most %d metric relays may be sent", MaxServers)
	}
	if len(r.Traffic) > MaxTrafficShapes {
		return fmt.Errorf("at most %d traffic shapes may be sent", MaxTrafficShapes)
	}
	if r.Gateway != nil {
		if err := boundedText("gateway.listen", r.Gateway.Listen, true); err != nil {
			return err
		}
		if !ValidServerKey(r.Gateway.ServerKey) {
			return errors.New("gateway.server_key is invalid")
		}
	}
	return nil
}

// Validate bounds one metric relay.
func (r MetricRelay) Validate() error {
	if !ValidServerKey(r.ServerKey) {
		return errors.New("metric relay server_key is invalid")
	}
	for _, exposition := range []*Exposition{&r.Current, r.Baseline} {
		if exposition == nil {
			continue
		}
		if exposition.AgeMS < 0 || exposition.AgeMS > int64(24*time.Hour/time.Millisecond) || len(exposition.Text) > MaxExpositionBytes || exposition.Text == "" {
			return fmt.Errorf("metric relay for %s has an invalid scrape", r.ServerKey)
		}
	}
	return nil
}

// CleanText removes control characters and truncates to max bytes on a
// character boundary, so a value read from a local server always passes
// validation.
func CleanText(value string, max int) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(value) {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			continue
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}

func (h Hardware) validate() error {
	if h.State != "ready" && h.State != "partial" && h.State != "unavailable" {
		return errors.New("hardware.state must be ready, partial, or unavailable")
	}
	if len(h.Accelerators) > 16 || len(h.Limitations) > 16 {
		return errors.New("hardware inventory is too large")
	}
	for _, accelerator := range h.Accelerators {
		if accelerator.Vendor != "nvidia" && accelerator.Vendor != "amd" && accelerator.Vendor != "apple" && accelerator.Vendor != "intel" {
			return errors.New("accelerator vendor must be nvidia, amd, apple, or intel")
		}
		if accelerator.Count < 0 || accelerator.Count > 1024 || (accelerator.MemoryMiB != nil && (*accelerator.MemoryMiB < 0 || *accelerator.MemoryMiB > 1<<24)) || len(accelerator.Unknown) > 16 {
			return errors.New("accelerator inventory is out of bounds")
		}
		for field, value := range map[string]string{"accelerator.name": accelerator.Name, "accelerator.driver": accelerator.Driver, "accelerator.source": accelerator.Source} {
			if err := boundedText(field, value, false); err != nil {
				return err
			}
		}
		for _, unknown := range accelerator.Unknown {
			if err := boundedText("accelerator.unknown", unknown, true); err != nil {
				return err
			}
		}
	}
	for _, limitation := range h.Limitations {
		if err := boundedText("hardware.limitation", limitation, true); err != nil {
			return err
		}
	}
	return nil
}

// Validate bounds one server report.
func (s Server) Validate() error {
	if !ValidServerKey(s.Key) {
		return fmt.Errorf("server key %q is invalid", s.Key)
	}
	if !KnownEngine(s.Engine) {
		return fmt.Errorf("server %s reports an unknown engine", s.Key)
	}
	if s.Health != HealthHealthy && s.Health != HealthUnhealthy {
		return fmt.Errorf("server %s health must be healthy or unhealthy", s.Key)
	}
	if len(s.Models) > MaxModelsPerServer {
		return fmt.Errorf("server %s reports more than %d models", s.Key, MaxModelsPerServer)
	}
	for field, value := range map[string]string{"engine_version": s.EngineVersion, "address": s.Address, "active_model": s.ActiveModel, "health_detail": s.HealthDetail, "metrics_detail": s.MetricsDetail} {
		if err := boundedText(field, value, field == "address"); err != nil {
			return fmt.Errorf("server %s: %w", s.Key, err)
		}
	}
	for _, model := range s.Models {
		if err := boundedText("model", model, true); err != nil {
			return fmt.Errorf("server %s: %w", s.Key, err)
		}
	}
	return nil
}

// Validate bounds one traffic shape.
func (s TrafficShape) Validate() error {
	if !shapeIDPattern.MatchString(s.ID) {
		return errors.New("id must be 8..64 URL-safe characters")
	}
	if !ValidServerKey(s.ServerKey) {
		return errors.New("server_key is invalid")
	}
	if s.StartedAt.IsZero() {
		return errors.New("started_at is required")
	}
	switch s.Operation {
	case "chat", "text_completion", "embeddings", "responses", "other":
	default:
		return errors.New("operation must be chat, text_completion, embeddings, responses, or other")
	}
	if s.StatusCode < 100 || s.StatusCode > 599 {
		return errors.New("status_code must be an HTTP status")
	}
	maxMS := float64(24 * time.Hour / time.Millisecond)
	if math.IsNaN(s.LatencyMS) || math.IsInf(s.LatencyMS, 0) || s.LatencyMS < 0 || s.LatencyMS > maxMS {
		return errors.New("latency_ms must be finite and between 0 and 86400000")
	}
	if s.TTFTMS != nil && (math.IsNaN(*s.TTFTMS) || math.IsInf(*s.TTFTMS, 0) || *s.TTFTMS < 0 || *s.TTFTMS > maxMS) {
		return errors.New("ttft_ms must be finite and between 0 and 86400000")
	}
	for name, tokens := range map[string]*int{"input_tokens": s.InputTokens, "output_tokens": s.OutputTokens} {
		if tokens != nil && (*tokens < 0 || *tokens > 10_000_000) {
			return fmt.Errorf("%s must be between 0 and 10000000", name)
		}
	}
	for name, hash := range map[string]string{"shared_prefix_hash": s.SharedPrefixHash, "session_id_hash": s.SessionIDHash} {
		if hash != "" && !hashPattern.MatchString(hash) {
			return fmt.Errorf("%s must be a lowercase SHA-256 hex digest", name)
		}
	}
	if err := boundedText("request_model", s.RequestModel, false); err != nil {
		return err
	}
	if s.ErrorType != "" && !errorTypePattern.MatchString(s.ErrorType) {
		return errors.New("error_type must be a bounded lowercase identifier")
	}
	return nil
}

// Validate bounds a server registration.
func (r RegisterServerRequest) Validate() error {
	if !ValidServerKey(r.ServerKey) {
		return errors.New("server_key is invalid")
	}
	if !ValidEndpointName(r.Endpoint) {
		return errors.New("endpoint must be a lowercase name of 1..64 letters, digits, dots, underscores, or hyphens")
	}
	if !KnownEngine(r.Engine) {
		return errors.New("engine is not one the agent reports")
	}
	if err := boundedText("model", r.Model, true); err != nil {
		return err
	}
	return boundedText("address", r.Address, true)
}
