package agent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
)

// Config describes one agent run.
type Config struct {
	Client  Client
	Version string
	// Servers are named explicitly; AutoDetect adds the default loopback
	// ports. Only named servers receive a credential.
	Servers    []ServerConfig
	AutoDetect bool
	// HashKey keys the gateway's request hashes. It is a secret kept on
	// this machine and never sent; nil means a random key for this run.
	HashKey []byte
	// Model and EndpointName apply when exactly one server is observed.
	Model, EndpointName string
	// Gateway is the local listen address of the recording pass-through;
	// empty turns it off. GatewayServer picks the server by URL or key when
	// several are observed.
	Gateway, GatewayServer string
	// Serve is the listen address of the data plane: the agent serves its
	// agent-managed endpoints there with InferCrane API keys, routes,
	// admission, and quota pulled from the control plane. Empty turns it off.
	Serve string
	// Executor runs candidate runs the control plane queues for this agent.
	// Nil means the agent takes no commands.
	Executor CandidateExecutor
	// Once runs one detect, register, and sync pass and returns.
	Once           bool
	Detector       Detector
	Hardware       HardwareProbe
	Events         func(Event)
	Now            func() time.Time
	ProbeInterval  time.Duration
	HardwareRescan time.Duration
}

// Event is progress the CLI shows.
type Event struct {
	Kind    string
	Server  *ServerStatus
	Message string
}

// ServerStatus is what the agent knows about one local server.
type ServerStatus struct {
	agentprotocol.Server
	Endpoint      string `json:"endpoint,omitempty"`
	Model         string `json:"model,omitempty"`
	Registered    bool   `json:"registered"`
	NotRegistered string `json:"not_registered,omitempty"`
	TrafficSource string `json:"traffic_source"`
	TrafficDetail string `json:"traffic_detail"`
}

// Status is the agent's current view, also printed by --once.
type Status struct {
	AgentID   string                 `json:"agent_id"`
	AgentName string                 `json:"agent_name"`
	Hardware  agentprotocol.Hardware `json:"hardware"`
	Servers   []ServerStatus         `json:"servers"`
	Gateway   string                 `json:"gateway,omitempty"`
	DataPlane *DataPlaneStatus       `json:"data_plane,omitempty"`
	Upgrade   agentprotocol.Upgrade  `json:"upgrade"`
	Sent      []string               `json:"sent"`
}

// Agent runs the sync loop.
type Agent struct {
	config     Config
	windows    *metricWindows
	shapes     *shapeBuffer
	mu         sync.Mutex
	detected   map[string]Detected
	status     map[string]*ServerStatus
	hardware   agentprotocol.Hardware
	agentID    string
	agentName  string
	upgrade    agentprotocol.Upgrade
	gatewayKey string
	poll       time.Duration
	dataPlane  *DataPlane
	serving    string
}

// New prepares an agent.
func New(config Config) *Agent {
	if config.ProbeInterval <= 0 {
		config.ProbeInterval = agentprotocol.ScrapeInterval
	}
	if config.HardwareRescan <= 0 {
		config.HardwareRescan = 10 * time.Minute
	}
	return &Agent{config: config, windows: newMetricWindows(), shapes: &shapeBuffer{}, detected: map[string]Detected{}, status: map[string]*ServerStatus{}, poll: agentprotocol.DefaultPollInterval}
}

func (a *Agent) setHardware(hardware agentprotocol.Hardware) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hardware = hardware
}

func (a *Agent) hashKey() []byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.config.HashKey) == 0 {
		a.config.HashKey = NewHashKey()
	}
	return a.config.HashKey
}

func (a *Agent) now() time.Time {
	if a.config.Now != nil {
		return a.config.Now()
	}
	return time.Now()
}

func (a *Agent) emit(event Event) {
	if a.config.Events != nil {
		a.config.Events(event)
	}
}

// NewHashKey returns a random key for request hashes.
func NewHashKey() []byte {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	return key
}

// Sent lists what leaves the machine, for the privacy line the CLI prints.
func Sent(gateway bool) []string {
	sent := []string{"host OS and accelerator inventory", "server address, engine, version, model names, and health", "engine metrics, filtered to the families InferCrane reads"}
	if gateway {
		sent = append(sent, "per-request timings, token counts, status, and keyed hashes of system prompts (no prompt or completion text)")
	}
	return sent
}

// SentWith lists what leaves the machine for a full agent configuration.
func SentWith(gateway, dataPlane, commands bool) []string {
	sent := Sent(gateway || dataPlane)
	if dataPlane {
		sent = append(sent, "for the data plane: quota lease requests (a count per minute), and the endpoint, binding, queue time, and retry count of each request (no request ID, header, prompt, or completion)")
	}
	if commands {
		sent = append(sent, "for candidate runs: progress phases, AIPerf timings and throughput, and answer-check scores and digests (no prompt, answer, or engine log)")
	}
	return sent
}

// Start runs the first pass: inventory, detection, a sync that learns the
// agent's name, registration, the optional gateway, and a sync with evidence.
// The gateway lives until ctx ends; its serve errors arrive on the returned
// channel.
func (a *Agent) Start(ctx context.Context) (<-chan error, error) {
	if err := CheckControlURL(a.config.Client.ControlURL); err != nil {
		return nil, err
	}
	hardware := a.config.Hardware.Inventory(ctx)
	a.setHardware(hardware)
	a.emit(Event{Kind: "hardware", Message: HardwareSummary(hardware)})
	a.Probe(ctx)
	if err := a.firstSync(ctx); err != nil {
		return nil, err
	}
	if err := a.register(ctx); err != nil {
		return nil, err
	}
	gatewayErrors := make(chan error, 2)
	if a.config.Gateway != "" {
		if err := a.startGateway(ctx, gatewayErrors); err != nil {
			return nil, err
		}
	}
	if a.config.Serve != "" {
		dataPlane := newDataPlane(a)
		a.mu.Lock()
		a.dataPlane = dataPlane
		a.mu.Unlock()
		if err := a.dataPlane.Refresh(ctx); fatal(err) {
			return nil, err
		} else if err != nil {
			a.emit(Event{Kind: "retrying", Message: fmt.Sprintf("data plane snapshot: %v", err)})
		}
		if err := a.startDataPlane(ctx, gatewayErrors); err != nil {
			return nil, err
		}
	}
	return gatewayErrors, a.sync(ctx, true)
}

func (a *Agent) refreshDataPlane(ctx context.Context) error {
	a.mu.Lock()
	dp := a.dataPlane
	a.mu.Unlock()
	if dp == nil {
		return nil
	}
	return dp.Refresh(ctx)
}

func (a *Agent) compileDataPlane() {
	a.mu.Lock()
	dp := a.dataPlane
	a.mu.Unlock()
	if dp != nil {
		dp.Compile()
	}
}

// firstSync tolerates a just-minted key: each control-plane replica learns
// new keys from a snapshot refreshed every second, so a key can be unknown
// for a moment after it was created.
func (a *Agent) firstSync(ctx context.Context) error {
	deadline := a.now().Add(10 * time.Second)
	for {
		err := a.sync(ctx, false)
		if !errors.Is(err, ErrUnauthenticated) || a.now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Step probes every server, registers new ones, refreshes the data plane,
// and syncs once.
func (a *Agent) Step(ctx context.Context) error {
	a.Probe(ctx)
	if err := a.register(ctx); err != nil && fatal(err) {
		return err
	}
	if err := a.refreshDataPlane(ctx); fatal(err) {
		return err
	}
	a.compileDataPlane()
	return a.sync(ctx, true)
}

// Run detects, registers, and syncs until the context ends or the control
// plane refuses the agent.
func (a *Agent) Run(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	gatewayErrors, err := a.Start(ctx)
	if err != nil || a.config.Once {
		return a.Status(), err
	}
	probeTicker := time.NewTicker(a.config.ProbeInterval)
	defer probeTicker.Stop()
	hardwareTicker := time.NewTicker(a.config.HardwareRescan)
	defer hardwareTicker.Stop()
	syncTimer := time.NewTimer(a.pollInterval())
	defer syncTimer.Stop()
	commandErrors := make(chan error, 1)
	if a.config.Executor != nil {
		go func() { commandErrors <- a.runCommands(ctx) }()
	}
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return a.Status(), nil
		case err := <-gatewayErrors:
			return a.Status(), fmt.Errorf("local gateway stopped: %w", err)
		case err := <-commandErrors:
			if fatal(err) {
				return a.Status(), err
			}
		case <-hardwareTicker.C:
			a.setHardware(a.config.Hardware.Inventory(ctx))
		case <-probeTicker.C:
			a.Probe(ctx)
			if err := a.register(ctx); err != nil && fatal(err) {
				return a.Status(), err
			}
			a.compileDataPlane()
		case <-syncTimer.C:
			if err := a.refreshDataPlane(ctx); fatal(err) {
				return a.Status(), err
			} else if err != nil {
				a.emit(Event{Kind: "retrying", Message: fmt.Sprintf("data plane snapshot: %v", err)})
			}
			err := a.sync(ctx, true)
			if fatal(err) {
				return a.Status(), err
			}
			wait := a.pollInterval()
			if err != nil && ctx.Err() == nil {
				failures++
				wait = backoff(wait, failures)
				a.emit(Event{Kind: "retrying", Message: fmt.Sprintf("%v; retrying in %s", err, wait.Round(time.Second))})
			} else {
				failures = 0
			}
			syncTimer.Reset(wait)
		}
	}
}

func backoff(base time.Duration, failures int) time.Duration {
	wait := base
	for index := 1; index < failures && wait < 5*time.Minute; index++ {
		wait *= 2
	}
	if wait > 5*time.Minute {
		wait = 5 * time.Minute
	}
	return wait
}

func (a *Agent) pollInterval() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.poll
}

// Probe reads every candidate server, records health, and keeps each engine
// scrape for the next relay.
func (a *Agent) Probe(ctx context.Context) {
	candidates := Candidates(a.config.Servers, a.config.AutoDetect)
	explicit := map[string]bool{}
	for _, server := range a.config.Servers {
		if base, err := NormalizeBaseURL(server.URL); err == nil {
			explicit[base] = true
		}
	}
	found := map[string]Detected{}
	for _, candidate := range candidates {
		server, ok := a.config.Detector.Probe(ctx, candidate, explicit[candidate.URL])
		if !ok {
			continue
		}
		found[server.Key] = server
		if server.Exposition != "" {
			_ = a.windows.Observe(server.Key, server.Exposition, a.now())
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, previous := range a.detected {
		if _, still := found[key]; still {
			continue
		}
		status := a.status[key]
		if status == nil || !status.Registered {
			delete(a.detected, key)
			delete(a.status, key)
			a.windows.Forget(key)
			continue
		}
		// A registered server that stopped answering is reported, not
		// forgotten, so its endpoint shows it as unhealthy.
		previous.Health, previous.HealthDetail = agentprotocol.HealthUnhealthy, "the server stopped answering on "+previous.Address
		previous.MetricsAvailable, previous.Exposition = false, ""
		found[key] = previous
	}
	for key, server := range found {
		_, known := a.detected[key]
		a.detected[key] = server
		status := a.status[key]
		if status == nil {
			status = &ServerStatus{}
			a.status[key] = status
		}
		status.Server = server.Server
		if !known {
			copy := *status
			a.emit(Event{Kind: "server_found", Server: &copy})
		}
	}
}

// register asks the control plane to observe every healthy server not yet
// observed. A server whose model cannot be chosen is skipped with a reason.
func (a *Agent) register(ctx context.Context) error {
	a.mu.Lock()
	pending := []Detected{}
	for key, server := range a.detected {
		if status := a.status[key]; status != nil && !status.Registered && server.Health == agentprotocol.HealthHealthy {
			pending = append(pending, server)
		}
	}
	single := len(a.detected) == 1
	agentName := a.agentName
	a.mu.Unlock()
	sort.Slice(pending, func(i, j int) bool { return pending[i].Key < pending[j].Key })
	for _, server := range pending {
		model, err := SelectModel(server, map[bool]string{true: a.config.Model}[single])
		if err != nil {
			a.skip(server.Key, err.Error())
			continue
		}
		name := EndpointName(model)
		if single && a.config.EndpointName != "" {
			name = a.config.EndpointName
		}
		request := agentprotocol.RegisterServerRequest{ServerKey: server.Key, Endpoint: name, Model: model, Engine: server.Engine, Address: server.Address}
		response, err := a.config.Client.Register(ctx, request)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict && agentName != "" && !(single && a.config.EndpointName != "") {
			request.Endpoint = EndpointName(agentName + "-" + name)
			response, err = a.config.Client.Register(ctx, request)
		}
		if err != nil {
			if fatal(err) {
				return err
			}
			a.skip(server.Key, err.Error())
			continue
		}
		a.mu.Lock()
		status := a.status[server.Key]
		status.Registered, status.NotRegistered = true, ""
		status.Endpoint, status.Model = response.Server.Endpoint, response.Server.Model
		status.TrafficSource, status.TrafficDetail = response.Server.TrafficSource, response.Server.TrafficDetail
		copy := *status
		a.mu.Unlock()
		a.emit(Event{Kind: "server_registered", Server: &copy})
	}
	return nil
}

func (a *Agent) skip(key, reason string) {
	a.mu.Lock()
	status := a.status[key]
	changed := status.NotRegistered != reason
	status.NotRegistered = reason
	copy := *status
	a.mu.Unlock()
	if changed {
		a.emit(Event{Kind: "server_skipped", Server: &copy, Message: reason})
	}
}

func (a *Agent) startGateway(ctx context.Context, failures chan<- error) error {
	a.mu.Lock()
	var chosen *Detected
	for key, server := range a.detected {
		server := server
		status := a.status[key]
		if a.config.GatewayServer != "" {
			base, _ := NormalizeBaseURL(a.config.GatewayServer)
			if key == a.config.GatewayServer || server.BaseURL == base {
				chosen = &server
			}
			continue
		}
		if status != nil && status.Registered {
			if chosen != nil {
				a.mu.Unlock()
				return errors.New("several servers are observed; choose the gateway's server with --gateway-server URL")
			}
			chosen = &server
		}
	}
	a.mu.Unlock()
	if chosen == nil {
		return errors.New("the local gateway needs one observed server; none matched")
	}
	gateway := &Gateway{Upstream: chosen.BaseURL, ServerKey: chosen.Key, HashKey: a.hashKey(), Shapes: a.shapes, Now: a.config.Now}
	handler, err := gateway.Handler()
	if err != nil {
		return err
	}
	listener, err := ListenGateway(ctx, a.config.Gateway)
	if err != nil {
		return fmt.Errorf("the local gateway could not listen on %s: %w", a.config.Gateway, err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			failures <- serveErr
		}
	}()
	a.mu.Lock()
	a.gatewayKey = chosen.Key
	if status := a.status[chosen.Key]; status != nil {
		status.TrafficSource = "agent-gateway"
	}
	a.mu.Unlock()
	a.emit(Event{Kind: "gateway_listening", Message: "http://" + listener.Addr().String() + "/v1 → " + chosen.BaseURL})
	return nil
}

func (a *Agent) syncRequest() (agentprotocol.SyncRequest, map[string]time.Time, []agentprotocol.TrafficShape) {
	now := a.now()
	a.mu.Lock()
	servers := make([]agentprotocol.Server, 0, len(a.detected))
	for _, server := range a.detected {
		servers = append(servers, server.Server)
	}
	gatewayKey := a.gatewayKey
	hardware := a.hardware
	a.mu.Unlock()
	sort.Slice(servers, func(i, j int) bool { return servers[i].Key < servers[j].Key })
	if len(servers) > agentprotocol.MaxServers {
		servers = servers[:agentprotocol.MaxServers]
	}
	relays, relayed := a.windows.Relays(now)
	relays = withinBudget(relays, relayed)
	request := agentprotocol.SyncRequest{
		Protocol: agentprotocol.Version, AgentVersion: a.config.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
		SentAt: now.UTC(), Hardware: hardware, Servers: servers, Metrics: relays,
	}
	var shapes []agentprotocol.TrafficShape
	if gatewayKey != "" {
		request.Gateway = &agentprotocol.Gateway{Listen: a.config.Gateway, ServerKey: gatewayKey}
	}
	if gatewayKey != "" || a.config.Serve != "" {
		shapes = a.shapes.take(agentprotocol.MaxTrafficShapes)
		request.Traffic = shapes
	}
	return request, relayed, shapes
}

func (a *Agent) sync(ctx context.Context, withEvidence bool) error {
	request, relayed, shapes := a.syncRequest()
	if !withEvidence {
		request.Metrics, request.Traffic = nil, nil
		a.shapes.restore(shapes)
		shapes = nil
	}
	response, err := a.config.Client.Sync(ctx, request)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != http.StatusTooManyRequests {
		// The control plane refused this batch as invalid. Resending it
		// would fail the same way forever, so its evidence is dropped and
		// the next sync carries only new evidence.
		a.windows.Sent(relayed)
		a.emit(Event{Kind: "rejected", Message: fmt.Sprintf("the control plane refused a sync (%v); its metrics and %d request shapes were dropped", err, len(shapes))})
		return err
	}
	if err != nil {
		a.shapes.restore(shapes)
		return err
	}
	if withEvidence {
		a.windows.Sent(relayed)
	}
	a.mu.Lock()
	a.agentID, a.agentName, a.upgrade = response.AgentID, response.AgentName, response.Upgrade
	if interval := time.Duration(response.PollIntervalSeconds) * time.Second; interval >= agentprotocol.MinPollInterval && interval <= agentprotocol.MaxPollInterval {
		a.poll = interval
	}
	observed := map[string]bool{}
	for _, server := range response.Servers {
		observed[server.Key] = true
	}
	for key, status := range a.status {
		if status.Registered && !observed[key] {
			// Its endpoint was deleted; register the server again.
			status.Registered, status.Endpoint, status.Model = false, "", ""
		}
	}
	for _, server := range response.Servers {
		status := a.status[server.Key]
		if status == nil {
			continue
		}
		status.Registered, status.NotRegistered = true, ""
		status.Endpoint, status.Model = server.Endpoint, server.Model
		status.TrafficSource, status.TrafficDetail = server.TrafficSource, server.TrafficDetail
	}
	a.mu.Unlock()
	for _, rejection := range response.Rejected {
		a.emit(Event{Kind: "rejected", Message: fmt.Sprintf("%s %s: %s", rejection.ServerKey, rejection.Kind, rejection.Reason)})
	}
	if response.Upgrade.Detail != "" {
		a.emit(Event{Kind: "upgrade_advice", Message: response.Upgrade.Detail})
	}
	a.emit(Event{Kind: "synced", Message: fmt.Sprintf("%d measurements, %d requests", response.Accepted.Measurements, response.Accepted.Requests)})
	return nil
}

// relayBudget keeps one sync well under the control plane's 8 MiB body
// limit. Baselines go first, then whole relays; a relay left out is retried
// with the next sync.
const relayBudget = 6 << 20

func withinBudget(relays []agentprotocol.MetricRelay, relayed map[string]time.Time) []agentprotocol.MetricRelay {
	size := func() int {
		total := 0
		for _, relay := range relays {
			total += len(relay.Current.Text)
			if relay.Baseline != nil {
				total += len(relay.Baseline.Text)
			}
		}
		return total
	}
	kept := relays[:0]
	for _, relay := range relays {
		if relay.Validate() != nil {
			// Too large or malformed to relay at all; never retried.
			continue
		}
		kept = append(kept, relay)
	}
	relays = kept
	for index := range relays {
		if size() <= relayBudget {
			break
		}
		relays[index].Baseline = nil
	}
	for size() > relayBudget && len(relays) > 0 {
		delete(relayed, relays[len(relays)-1].ServerKey)
		relays = relays[:len(relays)-1]
	}
	return relays
}

// Status returns a copy of the agent's current view.
func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := Status{AgentID: a.agentID, AgentName: a.agentName, Hardware: a.hardware, Servers: []ServerStatus{}, Upgrade: a.upgrade, Sent: SentWith(a.gatewayKey != "", a.config.Serve != "", a.config.Executor != nil)}
	if a.gatewayKey != "" {
		out.Gateway = a.config.Gateway
	}
	if a.dataPlane != nil {
		dp, listen := a.dataPlane, a.serving
		a.mu.Unlock()
		status := dp.status(listen)
		a.mu.Lock()
		out.DataPlane = &status
	}
	for _, status := range a.status {
		item := *status
		if item.TrafficSource == "" {
			item.TrafficSource = "none"
		}
		if item.TrafficDetail == "" {
			item.TrafficDetail = "No traffic is visible: requests go straight to the server. Route them through the agent's local gateway (--gateway) to record their shape."
		}
		out.Servers = append(out.Servers, item)
	}
	sort.Slice(out.Servers, func(i, j int) bool { return out.Servers[i].Key < out.Servers[j].Key })
	return out
}

// PendingShapes reports buffered and dropped request shapes.
func (a *Agent) PendingShapes() (int, int) { return a.shapes.stats() }

// HardwareSummary is a one-line accelerator summary for terminal output.
func HardwareSummary(hardware agentprotocol.Hardware) string {
	if len(hardware.Accelerators) == 0 {
		return "no accelerator found"
	}
	parts := make([]string, 0, len(hardware.Accelerators))
	for _, accelerator := range hardware.Accelerators {
		part := fmt.Sprintf("%d × %s", accelerator.Count, accelerator.Name)
		if accelerator.MemoryMiB != nil {
			part += fmt.Sprintf(" (%d MiB)", *accelerator.MemoryMiB)
		}
		if accelerator.Vendor != "nvidia" {
			part += ", telemetry not collected"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

// Describe is a one-line summary of a server for terminal output.
func Describe(server ServerStatus) string {
	parts := []string{server.Engine}
	if server.EngineVersion != "" {
		parts[0] += " " + server.EngineVersion
	}
	parts = append(parts, server.Address)
	switch {
	case server.Model != "":
		parts = append(parts, server.Model)
	case len(server.Models) == 1:
		parts = append(parts, server.Models[0])
	case len(server.Models) > 1 && len(server.Models) <= 3:
		parts = append(parts, strings.Join(server.Models, ", "))
	case len(server.Models) > 3:
		parts = append(parts, fmt.Sprintf("%s and %d more models", strings.Join(server.Models[:3], ", "), len(server.Models)-3))
	}
	if server.ActiveModel != "" && server.Model == "" {
		parts = append(parts, "loaded "+server.ActiveModel)
	}
	return strings.Join(parts, " · ")
}
