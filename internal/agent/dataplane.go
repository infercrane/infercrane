package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/admission"
	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gateway"
	"github.com/infercrane/infercrane/internal/requestquota"
	"github.com/infercrane/infercrane/internal/routes"
)

// ErrDataPlaneStale means the agent could not refresh its data-plane
// snapshot inside the validity window the control plane set, so it no
// longer knows which API keys are still valid.
var ErrDataPlaneStale = errors.New("the data plane snapshot is stale")

// DataPlane serves an endpoint's traffic inside the customer's cloud. It runs
// the same gateway code as the hosted control plane (authentication,
// admission, request quota, routing, streaming, retries, and request
// records) on a snapshot it pulls over its outbound channel. It routes only
// to servers this agent found itself; the snapshot names them by server key,
// never by address. Request records leave the machine as content-free shapes
// keyed with the agent's local hashing key.
type DataPlane struct {
	agent     *Agent
	directory *routes.Directory
	admission *admission.Pool
	quota     *requestquota.Pool
	gateway   *gateway.Gateway
	upstream  *http.Client

	mu          sync.RWMutex
	snapshot    agentprotocol.DataPlaneSnapshot
	held        bool
	credentials map[string]domain.Principal
	targets     map[string]string // target ID -> server key
	published   map[string]bool   // endpoint -> published
	lastError   string
	// offline is the mode the routes were last compiled for: true while
	// serving past the snapshot's validity inside the operator's grace.
	offline bool
	// saved and savedGeneration are the validity and generation of the
	// snapshot last written to disk.
	saved           time.Time
	savedGeneration string
}

// Serving modes of the data plane.
const (
	modeCurrent = "current" // the snapshot is valid
	modeOffline = "offline" // past its validity, inside the operator's grace
	modeStale   = "stale"   // past both: refuse
)

// mode says how the data plane may serve at now. Past the snapshot's
// validity it serves only inside the operator's offline grace, and then
// only the endpoints' own servers.
func (d *DataPlane) mode(now time.Time) string {
	d.mu.RLock()
	held, validUntil := d.held, d.snapshot.ValidUntil
	d.mu.RUnlock()
	switch {
	case !held:
		return modeStale
	case !now.After(validUntil):
		return modeCurrent
	case d.agent.config.OfflineGrace > 0 && !now.After(validUntil.Add(d.agent.config.OfflineGrace)):
		return modeOffline
	default:
		return modeStale
	}
}

func newDataPlane(agent *Agent) *DataPlane {
	dp := &DataPlane{agent: agent, directory: routes.New(), admission: admission.New(), credentials: map[string]domain.Principal{}, targets: map[string]string{}, published: map[string]bool{}}
	dp.quota = requestquota.New(dataPlaneQuotaSource{dp})
	dp.quota.Interval = 250 * time.Millisecond
	base := http.DefaultTransport.(*http.Transport).Clone()
	// Model servers are local to the agent; an egress proxy is for the
	// control plane only.
	base.Proxy = nil
	base.MaxIdleConnsPerHost = 128
	base.ResponseHeaderTimeout = 5 * time.Minute
	dp.upstream = &http.Client{Transport: base}
	dp.gateway = &gateway.Gateway{
		Routes: dp.directory, Client: dp.upstream, Recorder: dataPlaneRecorder{dp}, Authenticator: dp,
		RequestAuthorizer: dp.quota, AdmissionAuthorizer: dp.admission, HashKey: agent.hashKey(), Telemetry: &gateway.Telemetry{},
		Ready: dp.ready,
	}
	return dp
}

func (d *DataPlane) now() time.Time { return d.agent.now() }

// Handler serves the OpenAI-compatible API. Inference fails closed with 503
// once the snapshot is older than its validity window.
func (d *DataPlane) Handler() http.Handler {
	inner := d.gateway.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// The control API never runs here.
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") && d.ready(r.Context()) != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"the InferCrane agent has no current data plane snapshot from its control plane, so it cannot check API keys","type":"server_error","param":null,"code":"data_plane_stale"}}`))
			return
		}
		inner.ServeHTTP(w, r)
	})
}

func (d *DataPlane) ready(context.Context) error {
	mode := d.mode(d.now())
	if mode == modeStale {
		return ErrDataPlaneStale
	}
	// Entering or leaving offline mode changes which servers are routed;
	// recompile before the request is routed.
	d.mu.RLock()
	recompile := d.offline != (mode == modeOffline)
	d.mu.RUnlock()
	if recompile {
		d.Compile()
	}
	return nil
}

// AuthenticatePrincipal checks a key against the digests the control plane
// sent. The key itself never leaves this machine.
func (d *DataPlane) AuthenticatePrincipal(_ context.Context, token string) (domain.Principal, error) {
	sum := sha256.Sum256([]byte(token))
	d.mu.RLock()
	principal, ok := d.credentials[hex.EncodeToString(sum[:])]
	d.mu.RUnlock()
	// Out of contact, inside the operator's grace, the last digests still
	// authenticate: a key revoked since is honoured until the agent
	// reconnects or the grace ends. Each key's own expiry always applies.
	if !ok || d.mode(d.now()) == modeStale {
		return domain.Principal{}, domain.ErrNotFound
	}
	if principal.ExpiresAt != nil && !d.now().Before(*principal.ExpiresAt) {
		return domain.Principal{}, domain.ErrNotFound
	}
	return principal, nil
}

// Refresh pulls the snapshot and applies it. An unchanged snapshot only
// extends its validity.
func (d *DataPlane) Refresh(ctx context.Context) error {
	d.mu.RLock()
	generation := ""
	if d.held {
		generation = d.snapshot.Generation
	}
	d.mu.RUnlock()
	snapshot, err := d.agent.config.Client.DataPlane(ctx, generation)
	if err != nil {
		d.setError(err)
		return err
	}
	if err = snapshot.Validate(d.now()); err != nil {
		err = fmt.Errorf("the control plane sent an invalid data plane snapshot: %w", err)
		d.setError(err)
		return err
	}
	if snapshot.Unchanged {
		d.mu.Lock()
		current := d.held && snapshot.Generation == d.snapshot.Generation && snapshot.TenantID == d.snapshot.TenantID
		if current {
			d.snapshot.ValidUntil, d.lastError = snapshot.ValidUntil, ""
		}
		d.mu.Unlock()
		if current {
			d.save()
			if d.mode(d.now()) == modeCurrent {
				d.Compile()
			}
			return nil
		}
		// The control plane answered for a snapshot this agent does not
		// hold; ask for the full one.
		if snapshot, err = d.agent.config.Client.DataPlane(ctx, ""); err != nil {
			d.setError(err)
			return err
		}
		if err = snapshot.Validate(d.now()); err != nil || snapshot.Unchanged {
			err = fmt.Errorf("the control plane sent an invalid data plane snapshot: %v", err)
			d.setError(err)
			return err
		}
	}
	d.apply(snapshot)
	d.save()
	// Apply the quota before any route is published, so a quota-limited
	// workspace is never served unlimited while the pool catches up. A lease
	// that fails leaves no tokens: requests get 429, not a free pass.
	if err := d.quota.Refresh(ctx); err != nil {
		d.agent.emit(Event{Kind: "retrying", Message: fmt.Sprintf("request quota lease: %v", err)})
	}
	d.Compile()
	return nil
}

// apply makes snapshot the one the data plane serves with.
func (d *DataPlane) apply(snapshot agentprotocol.DataPlaneSnapshot) {
	d.mu.Lock()
	credentials := make(map[string]domain.Principal, len(snapshot.Credentials))
	for _, credential := range snapshot.Credentials {
		credentials[credential.Hash] = domain.Principal{
			ID: credential.PrincipalID, TenantID: snapshot.TenantID, Name: credential.PrincipalID, Role: credential.Role,
			Kind: "inference_token", Scopes: credential.Scopes, EndpointNames: credential.EndpointNames, ExpiresAt: credential.ExpiresAt,
		}
	}
	d.snapshot, d.held, d.credentials, d.lastError = snapshot, true, credentials, ""
	d.mu.Unlock()
	policies := make([]admission.Policy, 0, len(snapshot.Admission))
	for _, item := range snapshot.Admission {
		allowed := map[string]struct{}{}
		for _, priority := range item.AllowedPriorities {
			allowed[priority] = struct{}{}
		}
		policies = append(policies, admission.Policy{
			Key: snapshot.TenantID + "\x00" + item.Endpoint, MaxConcurrency: item.MaxConcurrency, MaxQueueDepth: item.MaxQueueDepth,
			QueueTimeout: time.Duration(item.QueueTimeoutMS) * time.Millisecond, RequestTimeout: time.Duration(item.RequestTimeoutMS) * time.Millisecond,
			MaxRequestBytes: item.MaxRequestBytes, MaxOutputTokens: item.MaxOutputTokens, AllowedPriorities: allowed, Enabled: item.Enabled, RetryBudget: item.RetryBudget,
		})
	}
	d.admission.Replace(policies)
}

func (d *DataPlane) setError(err error) {
	d.mu.Lock()
	d.lastError = err.Error()
	d.mu.Unlock()
}

// Compile publishes routes for every endpoint in the snapshot from the
// servers this agent currently sees as healthy. A server key the agent did
// not find is never routed to, whatever the snapshot says.
func (d *DataPlane) Compile() {
	offline := d.mode(d.now()) == modeOffline
	d.mu.Lock()
	snapshot, held := d.snapshot, d.held
	d.offline = offline
	d.mu.Unlock()
	if !held {
		return
	}
	// A server is routed once this agent registered it. After a restart
	// with the control plane out of reach nothing is registered in this
	// process yet; then a server the snapshot names is routed when this
	// agent found it itself serving the binding's model.
	d.agent.mu.Lock()
	local := make(map[string]Detected, len(d.agent.detected))
	registered := map[string]bool{}
	for key, server := range d.agent.detected {
		local[key] = server
		if status := d.agent.status[key]; status != nil && status.Registered {
			registered[key] = true
		}
	}
	d.agent.mu.Unlock()
	targets := map[string]string{}
	published := map[string]bool{}
	for _, route := range snapshot.Routes {
		compiled := make([]routes.Snapshot, 0, len(route.Bindings))
		planned := make([]string, 0, len(route.Bindings))
		for _, binding := range route.Bindings {
			if offline && binding.Rollout {
				// Out of contact, only the endpoint's own servers serve.
				continue
			}
			planned = append(planned, binding.BindingID)
			server, ok := local[binding.ServerKey]
			if !ok || server.Health != agentprotocol.HealthHealthy || !registered[binding.ServerKey] && !slices.Contains(server.Models, binding.UpstreamModel) {
				continue
			}
			targets[binding.TargetID] = binding.ServerKey
			compiled = append(compiled, routes.Snapshot{
				TenantID: snapshot.TenantID, Alias: route.Endpoint, TargetID: binding.TargetID, UpstreamModel: binding.UpstreamModel,
				RouterURL: server.BaseURL, Provider: "external", Runtime: binding.Runtime, ComputeMode: agentprotocol.ComputeModeDataPlane,
				UpstreamAPIKey: bearerToken(server.authHeader), LogicalModelID: route.LogicalModelID, EnvironmentID: route.EnvironmentID,
				EndpointID: route.EndpointID, ServingPlanID: route.ServingPlanID, BindingID: binding.BindingID, RoutingWeight: binding.Weight,
				ProtocolCapabilities: binding.Capabilities,
			})
		}
		if len(compiled) == 0 {
			d.directory.RemoveEndpointForTenant(snapshot.TenantID, route.Endpoint)
			continue
		}
		d.directory.PublishEndpoint(routes.EndpointRoute{TenantID: snapshot.TenantID, Alias: route.Endpoint, RoutingPolicy: route.RoutingPolicy, Routes: compiled, PlannedBindingIDs: planned})
		published[route.Endpoint] = true
	}
	for _, alias := range d.directory.EndpointAliasesForTenant(snapshot.TenantID) {
		if !published[alias] {
			d.directory.RemoveEndpointForTenant(snapshot.TenantID, alias)
		}
	}
	d.mu.Lock()
	d.targets, d.published = targets, published
	d.mu.Unlock()
}

// bearerToken extracts the token from a local server's bearer header. Other
// header schemes are not forwarded by the data plane.
func bearerToken(header string) string {
	if token, ok := strings.CutPrefix(header, "Bearer "); ok {
		return strings.TrimSpace(token)
	}
	return ""
}

// Endpoints lists the endpoints this data plane currently serves.
func (d *DataPlane) Endpoints() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.published))
	for _, route := range d.snapshot.Routes {
		if d.published[route.Endpoint] {
			out = append(out, route.Endpoint)
		}
	}
	return out
}

// DataPlaneStatus is what the CLI prints about the data plane.
type DataPlaneStatus struct {
	Listen     string    `json:"listen"`
	Endpoints  []string  `json:"endpoints"`
	Keys       int       `json:"api_keys"`
	ValidUntil time.Time `json:"valid_until,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func (d *DataPlane) status(listen string) DataPlaneStatus {
	endpoints := d.Endpoints()
	d.mu.RLock()
	defer d.mu.RUnlock()
	return DataPlaneStatus{Listen: listen, Endpoints: endpoints, Keys: len(d.credentials), ValidUntil: d.snapshot.ValidUntil, Error: d.lastError}
}

// dataPlaneQuotaSource spends the workspace request quota only through
// leases from the control plane, like every hosted gateway replica.
type dataPlaneQuotaSource struct{ dp *DataPlane }

func (s dataPlaneQuotaSource) RequestQuotaPolicies(context.Context) ([]requestquota.Policy, error) {
	s.dp.mu.RLock()
	defer s.dp.mu.RUnlock()
	if !s.dp.held || s.dp.snapshot.RequestQuota == nil {
		return nil, nil
	}
	return []requestquota.Policy{{TenantID: s.dp.snapshot.TenantID, MaxRequestsPerMinute: s.dp.snapshot.RequestQuota.MaxRequestsPerMinute}}, nil
}

func (s dataPlaneQuotaSource) ReserveRequestQuota(ctx context.Context, _ string, window time.Time, requested int) (int, error) {
	if requested > agentprotocol.MaxQuotaLease {
		requested = agentprotocol.MaxQuotaLease
	}
	lease, err := s.dp.agent.config.Client.LeaseQuota(ctx, agentprotocol.QuotaLeaseRequest{Window: window, Requested: requested})
	if err != nil {
		return 0, err
	}
	if lease.Unlimited {
		return requested, nil
	}
	return lease.Granted, nil
}

// dataPlaneRecorder turns the gateway's request record into a content-free
// shape for the next sync. The record holds identities, timings, counts,
// and keyed hashes only; the request ID the caller chose is not sent.
type dataPlaneRecorder struct{ dp *DataPlane }

func (r dataPlaneRecorder) RecordRequest(_ context.Context, record domain.InferenceRecord) error {
	r.dp.mu.RLock()
	key := r.dp.targets[record.TargetID]
	r.dp.mu.RUnlock()
	if key == "" {
		return nil
	}
	operation := map[string]string{"chat": "chat", "completions": "text_completion", "embeddings": "embeddings", "responses": "responses"}[record.OperationName]
	if operation == "" {
		operation = "other"
	}
	retries := record.RetryCount
	if retries > 3 {
		retries = 3
	}
	shape := agentprotocol.TrafficShape{
		ID: newShapeID(), ServerKey: key, StartedAt: record.StartedAt.UTC(), Operation: operation,
		RequestModel: agentprotocol.CleanText(record.RequestModel, agentprotocol.MaxText), StatusCode: record.StatusCode, LatencyMS: record.LatencyMS,
		TTFTMS: record.TTFTMS, Streaming: record.Streaming, ErrorType: record.ErrorType,
		SharedPrefixHash: record.SharedPrefixHash, SessionIDHash: record.SessionIDHash,
		Source: agentprotocol.ShapeSourceDataPlane, BindingID: record.BindingID,
		ResponseModel: agentprotocol.CleanText(record.ResponseModel, agentprotocol.MaxText), QueueMS: record.QueueMS, RetryCount: retries,
		ParentSessionIDHash: record.ParentSessionIDHash, ToolPauseMS: record.ToolPauseMS,
		FallbackFromBindingID: record.FallbackFromBindingID,
	}
	shape.InputTokens, shape.OutputTokens = boundedTokens(record.InputTokens, record.OutputTokens)
	if shape.Validate() != nil {
		// Never buffer a shape the control plane would refuse.
		return nil
	}
	r.dp.agent.shapes.add(shape)
	return nil
}

// startDataPlane listens on the configured address and serves until ctx
// ends. Serve errors arrive on failures.
func (a *Agent) startDataPlane(ctx context.Context, failures chan<- error) error {
	listener, err := ListenGateway(ctx, a.config.Serve)
	if err != nil {
		return fmt.Errorf("the data plane could not listen on %s: %w", a.config.Serve, err)
	}
	server := &http.Server{Handler: a.dataPlane.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			failures <- serveErr
		}
	}()
	go func() {
		_ = a.dataPlane.quota.Run(ctx)
	}()
	a.mu.Lock()
	a.serving = listener.Addr().String()
	a.mu.Unlock()
	a.emit(Event{Kind: "data_plane_listening", Message: "http://" + listener.Addr().String() + "/v1"})
	return nil
}

// ServeAddress returns the data plane's listen address once it listens.
func (a *Agent) ServeAddress() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serving
}
