package routes

import (
	"slices"
	"sync"
	"time"

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
}

// Directory is an atomic in-memory data-plane snapshot. Reads never touch the database.
type Directory struct {
	mu         sync.RWMutex
	items      map[string]Snapshot
	endpoints  map[string]EndpointRoute
	selections map[string]uint64
	blocked    map[string]bool
	inflight   map[string]int
	retired    map[string]Snapshot
}

func New() *Directory {
	return &Directory{items: make(map[string]Snapshot), endpoints: make(map[string]EndpointRoute), selections: make(map[string]uint64), blocked: make(map[string]bool), inflight: make(map[string]int), retired: make(map[string]Snapshot)}
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
	id := routeID(route)
	d.inflight[id]++
	d.mu.Unlock()
	var once sync.Once
	return route, func() {
		once.Do(func() {
			d.mu.Lock()
			if d.inflight[id] <= 1 {
				delete(d.inflight, id)
			} else {
				d.inflight[id]--
			}
			d.mu.Unlock()
		})
	}, true
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
	// Plan compilation expands each binding by its bounded weight. Selection is
	// stable across processes for a given acquisition ordinal and requires no
	// request-path database or network lookup.
	total := uint64(0)
	for _, route := range endpoint.Routes {
		weight := routeWeight(route)
		total += uint64(weight)
	}
	ordinal := d.selections[key]
	d.selections[key] = ordinal + 1
	point := ordinal % total
	for _, route := range endpoint.Routes {
		weight := uint64(routeWeight(route))
		if point < weight {
			return route, true
		}
		point -= weight
	}
	return endpoint.Routes[0], true
}

func routeWeight(route Snapshot) int {
	if route.RoutingWeight > 0 {
		return route.RoutingWeight
	}
	return 1
}

// PublishEndpoint atomically replaces the routes future requests may acquire.
// Existing requests retain their pinned immutable Snapshot until release.
func (d *Directory) PublishEndpoint(endpoint EndpointRoute) {
	if endpoint.TenantID == "" {
		endpoint.TenantID = "global"
	}
	key := endpoint.TenantID + "\x00" + endpoint.Alias
	d.mu.Lock()
	defer d.mu.Unlock()
	if previous, ok := d.endpoints[key]; ok {
		d.retireEndpointRoutesLocked(previous.Routes, endpoint.Routes)
	}
	d.endpoints[key] = EndpointRoute{TenantID: endpoint.TenantID, Alias: endpoint.Alias, RoutingPolicy: endpoint.RoutingPolicy, Routes: append([]Snapshot(nil), endpoint.Routes...)}
	delete(d.blocked, key)
}

func (d *Directory) RemoveEndpointForTenant(tenant, alias string) {
	key := tenant + "\x00" + alias
	d.mu.Lock()
	defer d.mu.Unlock()
	if previous, ok := d.endpoints[key]; ok {
		d.retireEndpointRoutesLocked(previous.Routes, nil)
	}
	delete(d.endpoints, key)
	delete(d.selections, key)
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
