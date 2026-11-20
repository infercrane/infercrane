package routes

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/overflow"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

type Snapshot struct {
	DeploymentID         string
	TargetID             string
	RevisionID           string
	TenantID             string
	Alias                string
	UpstreamModel        string
	RouterURL            string
	RouterProcessID      string
	Provider             string
	ProviderResourceID   string
	Runtime              string
	ComputeMode          string
	UpstreamAPIKey       string `json:"-"`
	ExternalPolicyID     string
	SelectionReason      string
	ProviderWorkers      *int
	ProviderObservedAt   time.Time
	LogicalModelID       string
	EnvironmentID        string
	EndpointID           string
	ServingPlanID        string
	BindingID            string
	RoutingWeight        int
	ProtocolCapabilities runtimecontract.ProtocolCapabilities
}

type EndpointRoute struct {
	TenantID, Alias, RoutingPolicy string
	Routes                         []Snapshot
	PlannedBindingIDs              []string
}

// Directory is an atomic in-memory data-plane snapshot. Reads never touch the database.
type Directory struct {
	mu        sync.RWMutex
	items     map[string]Snapshot
	endpoints map[string]EndpointRoute
	smooth    map[string]*smoothState
	blocked   map[string]bool
	inflight  map[string]int
	retired   map[string]Snapshot
	health    map[string]overflow.State
	// resident marks endpoints whose traffic is served inside a customer's
	// cloud by an InferCrane agent. They never have a route here, and the
	// gateway must refuse them instead of trying any other supply.
	resident map[string]bool
	now      func() time.Time
}

// smoothState is where a weighted endpoint is in its smooth weighted
// round-robin: the running weight of each published route, in route order.
// signature names the routes and weights it was built for, so republishing
// the same routes keeps the interleaving going and any change starts over.
type smoothState struct {
	signature string
	current   []int
}

func New() *Directory {
	return &Directory{items: make(map[string]Snapshot), endpoints: make(map[string]EndpointRoute), smooth: make(map[string]*smoothState), blocked: make(map[string]bool), inflight: make(map[string]int), retired: make(map[string]Snapshot), health: make(map[string]overflow.State), resident: make(map[string]bool), now: time.Now}
}

// SetResidentForTenant marks or clears an endpoint whose request content
// must stay in the customer's cloud.
func (d *Directory) SetResidentForTenant(tenant, alias string, resident bool) {
	key := tenant + "\x00" + alias
	d.mu.Lock()
	defer d.mu.Unlock()
	if resident {
		d.resident[key] = true
		return
	}
	delete(d.resident, key)
}

// ResidentForTenant reports whether an endpoint is served only inside the
// customer's cloud. It performs no I/O.
func (d *Directory) ResidentForTenant(tenant, alias string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.resident[tenant+"\x00"+alias]
}

// ResidentAliasesForTenant lists a tenant's customer-cloud endpoints.
func (d *Directory) ResidentAliasesForTenant(tenant string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	prefix := tenant + "\x00"
	aliases := make([]string, 0)
	for key := range d.resident {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			aliases = append(aliases, key[len(prefix):])
		}
	}
	slices.Sort(aliases)
	return aliases
}

func routeID(route Snapshot) string {
	if route.RouterProcessID != "" {
		return "router:" + route.RouterProcessID
	}
	return "route:" + route.DeploymentID + "\x00" + route.RevisionID + "\x00" + route.RouterURL
}

func (d *Directory) Get(alias string) (Snapshot, bool) {
	return d.GetForTenant("global", alias)
}
func (d *Directory) GetForTenant(tenant, alias string) (Snapshot, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.blocked[tenant+"\x00"+alias] {
		return Snapshot{}, false
	}
	if endpoint, ok := d.endpoints[tenant+"\x00"+alias]; ok && len(endpoint.Routes) > 0 {
		return endpoint.Routes[0], true
	}
	route, ok := d.items[tenant+"\x00"+alias]
	return route, ok
}

// GetDeployment returns the concrete route published by lifecycle
// reconciliation, bypassing endpoint aliases. It is a control-plane compiler
// operation and is never called from the HTTP request path.
func (d *Directory) GetDeployment(deploymentID string) (Snapshot, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, route := range d.items {
		if route.DeploymentID == deploymentID {
			return route, true
		}
	}
	return Snapshot{}, false
}

// AcquireForTenant atomically pins the selected immutable route generation for
// the lifetime of a request. Publication may replace the directory entry, but
// retirement cannot stop its router or capacity until release is called.
func (d *Directory) AcquireForTenant(tenant, alias string) (Snapshot, func(), bool) {
	return d.AcquirePreferredForTenant(tenant, alias, "", "")
}

// AcquirePreferredForTenant prefers a healthy compiled binding/target when it
// remains present. Ordinary routing is the reliability-preserving fallback.
func (d *Directory) AcquirePreferredForTenant(tenant, alias, bindingID, targetID string) (Snapshot, func(), bool) {
	d.mu.Lock()
	key := tenant + "\x00" + alias
	var route Snapshot
	ok := false
	if endpoint, exists := d.endpoints[key]; exists {
		for _, candidate := range endpoint.Routes {
			if (bindingID != "" && candidate.BindingID == bindingID) || (targetID != "" && candidate.TargetID == targetID) {
				route, ok = candidate, true
				break
			}
		}
	}
	if !ok {
		route, ok = d.selectLocked(key)
	}
	if !ok {
		d.mu.Unlock()
		return Snapshot{}, func() {}, false
	}
	release := d.pinLocked(route)
	d.mu.Unlock()
	return route, release, true
}

// AcquireFallbackForTenant pins the route a primary-fallback endpoint serves
// once every route in tried has failed the request: the first route of the
// current publication, in priority order, that the request has not tried. A
// route counts as tried when it is the same router generation or the same
// binding, so a republished generation of a failed binding is not tried
// again. It reports false for any other routing policy, a removed endpoint,
// or when no untried route is left. Like every acquisition it reads only the
// in-memory snapshot.
func (d *Directory) AcquireFallbackForTenant(tenant, alias string, tried []Snapshot) (Snapshot, func(), bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := tenant + "\x00" + alias
	endpoint, exists := d.endpoints[key]
	if d.blocked[key] || !exists || endpoint.RoutingPolicy != "primary-fallback" {
		return Snapshot{}, func() {}, false
	}
	for _, candidate := range endpoint.Routes {
		if !triedRoute(candidate, tried) {
			return candidate, d.pinLocked(candidate), true
		}
	}
	return Snapshot{}, func() {}, false
}

func triedRoute(route Snapshot, tried []Snapshot) bool {
	for _, previous := range tried {
		if routeID(previous) == routeID(route) || (route.BindingID != "" && previous.BindingID == route.BindingID) {
			return true
		}
	}
	return false
}

// pinLocked counts a request against the route's generation until the
// returned release runs; retirement waits for that count to reach zero.
func (d *Directory) pinLocked(route Snapshot) func() {
	id := routeID(route)
	d.inflight[id]++
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			if d.inflight[id] <= 1 {
				delete(d.inflight, id)
			} else {
				d.inflight[id]--
			}
			d.mu.Unlock()
		})
	}
}

func (d *Directory) selectLocked(key string) (Snapshot, bool) {
	if d.blocked[key] {
		return Snapshot{}, false
	}
	endpoint, exists := d.endpoints[key]
	if !exists || len(endpoint.Routes) == 0 {
		route, ok := d.items[key]
		return route, ok
	}
	if endpoint.RoutingPolicy != "weighted" || len(endpoint.Routes) == 1 {
		return endpoint.Routes[0], true
	}
	// Smooth weighted round-robin: every route gains its weight, the route
	// with the most running weight serves and gives back the total. Picks
	// interleave, so any window of requests sees close to the configured
	// share (9000:1000 sends one request in every ten to the smaller binding)
	// instead of a run of thousands. The sequence after a publication is the
	// same in every process and needs no request-path database or network
	// lookup.
	state := d.smooth[key]
	if state == nil || len(state.current) != len(endpoint.Routes) {
		state = &smoothState{signature: weightSignature(endpoint.Routes), current: make([]int, len(endpoint.Routes))}
		d.smooth[key] = state
	}
	total, best := 0, 0
	for index, route := range endpoint.Routes {
		weight := routeWeight(route)
		total += weight
		state.current[index] += weight
		if state.current[index] > state.current[best] {
			best = index
		}
	}
	state.current[best] -= total
	return endpoint.Routes[best], true
}

// weightSignature identifies the routes a weighted endpoint selects among and
// their weights, ignoring router generations: a binding republished with a
// new router keeps its place in the interleaving.
func weightSignature(routes []Snapshot) string {
	var signature strings.Builder
	for _, route := range routes {
		identity := route.BindingID
		if identity == "" {
			identity = routeID(route)
		}
		signature.WriteString(identity)
		signature.WriteByte(0)
		signature.WriteString(strconv.Itoa(routeWeight(route)))
		signature.WriteByte(1)
	}
	return signature.String()
}

func routeWeight(route Snapshot) int {
	if route.RoutingWeight > 0 {
		return route.RoutingWeight
	}
	return 1
}

// PublishEndpoint atomically replaces the routes future requests may acquire.
// Existing requests retain their pinned immutable Snapshot until release.
func (d *Directory) PublishEndpoint(endpoint EndpointRoute) int {
	if endpoint.TenantID == "" {
		endpoint.TenantID = "global"
	}
	key := endpoint.TenantID + "\x00" + endpoint.Alias
	d.mu.Lock()
	defer d.mu.Unlock()
	if endpoint.RoutingPolicy == "weighted" {
		endpoint.Routes = d.applyHealthDecayLocked(key, endpoint)
		signature := weightSignature(endpoint.Routes)
		if state := d.smooth[key]; state == nil || state.signature != signature {
			d.smooth[key] = &smoothState{signature: signature, current: make([]int, len(endpoint.Routes))}
		}
	} else {
		delete(d.smooth, key)
	}
	if previous, ok := d.endpoints[key]; ok {
		d.retireEndpointRoutesLocked(previous.Routes, endpoint.Routes)
	}
	d.endpoints[key] = EndpointRoute{TenantID: endpoint.TenantID, Alias: endpoint.Alias, RoutingPolicy: endpoint.RoutingPolicy, Routes: append([]Snapshot(nil), endpoint.Routes...)}
	delete(d.blocked, key)
	return len(endpoint.Routes)
}

// applyHealthDecayLocked keeps an unavailable binding at effective weight zero
// and requires two consecutive healthy publications before restoring its
// configured weight. This reuses the external-capacity hysteresis state
// machine; publication remains atomic and existing requests stay pinned.
func (d *Directory) applyHealthDecayLocked(key string, endpoint EndpointRoute) []Snapshot {
	planned := append([]string(nil), endpoint.PlannedBindingIDs...)
	if len(planned) == 0 {
		for _, route := range endpoint.Routes {
			planned = append(planned, route.BindingID)
		}
	}
	available := make(map[string]Snapshot, len(endpoint.Routes))
	for _, route := range endpoint.Routes {
		available[route.BindingID] = route
	}
	now := d.now().UTC()
	policy := overflow.Policy{Mode: "health", BreachIntervals: 2, RecoveryIntervals: 2, PrivacyAcknowledged: true, BudgetAvailable: true}
	effective := make([]Snapshot, 0, len(endpoint.Routes))
	for _, bindingID := range planned {
		stateKey := key + "\x00" + bindingID
		state := d.health[stateKey]
		_, healthy := available[bindingID]
		decision, err := overflow.Evaluate(policy, state, overflow.Signal{PrimaryHealthy: healthy}, now)
		if err != nil {
			continue
		}
		state.External = decision.Route == "external"
		state.ConsecutiveHigh, state.ConsecutiveLow = decision.ConsecutiveHigh, decision.ConsecutiveLow
		if decision.Action == "overflow" || decision.Action == "recover" {
			state.LastChangedAt = now
		}
		d.health[stateKey] = state
		if route, present := available[bindingID]; present && !state.External {
			effective = append(effective, route)
		}
	}
	return effective
}

func (d *Directory) RemoveEndpointForTenant(tenant, alias string) {
	key := tenant + "\x00" + alias
	d.mu.Lock()
	defer d.mu.Unlock()
	if previous, ok := d.endpoints[key]; ok {
		d.retireEndpointRoutesLocked(previous.Routes, nil)
	}
	delete(d.endpoints, key)
	delete(d.smooth, key)
	for stateKey := range d.health {
		if len(stateKey) > len(key) && stateKey[:len(key)+1] == key+"\x00" {
			delete(d.health, stateKey)
		}
	}
	d.blocked[key] = true
}

func (d *Directory) retireEndpointRoutesLocked(previous, next []Snapshot) {
	retained := make(map[string]struct{}, len(next))
	for _, route := range next {
		retained[routeID(route)] = struct{}{}
	}
	for _, route := range previous {
		id := routeID(route)
		if _, ok := retained[id]; !ok {
			d.retired[id] = route
		}
	}
}

func (d *Directory) EndpointAliasesForTenant(tenant string) []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	aliases := make([]string, 0)
	for key, endpoint := range d.endpoints {
		_ = key
		if endpoint.TenantID == tenant {
			aliases = append(aliases, endpoint.Alias)
		}
	}
	slices.Sort(aliases)
	return aliases
}

func (d *Directory) Put(route Snapshot) {
	if route.TenantID == "" {
		route.TenantID = "global"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	key := route.TenantID + "\x00" + route.Alias
	if previous, ok := d.items[key]; ok && routeID(previous) != routeID(route) {
		d.retired[routeID(previous)] = previous
	}
	d.items[key] = route
}

func (d *Directory) ListForTenant(tenant string) []Snapshot {
	all := d.List()
	out := make([]Snapshot, 0, len(all))
	for _, route := range all {
		if route.TenantID == tenant {
			out = append(out, route)
		}
	}
	return out
}

// ConcreteRoutes returns lifecycle-owned deployment routes without applying
// endpoint alias precedence. Reconciliation must use this view when withdrawing
// stale deployments: an endpoint with the same alias may otherwise hide the
// concrete route that deletion is waiting to drain.
func (d *Directory) ConcreteRoutes() []Snapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Snapshot, 0, len(d.items))
	for _, route := range d.items {
		out = append(out, route)
	}
	slices.SortFunc(out, func(a, b Snapshot) int {
		if a.Alias < b.Alias {
			return -1
		}
		if a.Alias > b.Alias {
			return 1
		}
		return 0
	})
	return out
}

func (d *Directory) Remove(alias string) {
	d.RemoveForTenant("global", alias)
}
func (d *Directory) RemoveForTenant(tenant, alias string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := tenant + "\x00" + alias
	if previous, ok := d.items[key]; ok {
		d.retired[routeID(previous)] = previous
	}
	delete(d.items, key)
}

// RetiringInFlight reports requests still using withdrawn generations for a
// deployment. Lifecycle handlers use it as the deletion fence.
func (d *Directory) RetiringInFlight(deploymentID string) int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	total := 0
	for id, route := range d.retired {
		if route.DeploymentID == deploymentID {
			total += d.inflight[id]
		}
	}
	return total
}

func (d *Directory) HasCurrentDeployment(deploymentID string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, route := range d.items {
		if route.DeploymentID == deploymentID {
			return true
		}
	}
	return false
}

// RetiredReady returns withdrawn generations with no active requests. The
// reconciler owns stopping their router processes and then forgetting them.
func (d *Directory) RetiredReady() []Snapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	current := make(map[string]struct{}, len(d.items)+len(d.endpoints))
	for _, route := range d.items {
		current[routeID(route)] = struct{}{}
	}
	for _, endpoint := range d.endpoints {
		for _, route := range endpoint.Routes {
			current[routeID(route)] = struct{}{}
		}
	}
	ready := make([]Snapshot, 0)
	for id, route := range d.retired {
		_, stillPublished := current[id]
		if !stillPublished && d.inflight[id] == 0 {
			ready = append(ready, route)
		}
	}
	return ready
}

func (d *Directory) ForgetRetired(route Snapshot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.retired, routeID(route))
}

func (d *Directory) List() []Snapshot {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]Snapshot, 0, len(d.items)+len(d.endpoints))
	seen := make(map[string]struct{}, len(d.endpoints))
	for key, endpoint := range d.endpoints {
		if len(endpoint.Routes) == 0 {
			continue
		}
		out = append(out, endpoint.Routes[0])
		seen[key] = struct{}{}
	}
	for key, route := range d.items {
		if _, ok := seen[key]; ok {
			continue
		}
		if d.blocked[key] {
			continue
		}
		out = append(out, route)
	}
	slices.SortFunc(out, func(a, b Snapshot) int {
		if a.Alias < b.Alias {
			return -1
		}
		if a.Alias > b.Alias {
			return 1
		}
		return 0
	})
	return out
}
