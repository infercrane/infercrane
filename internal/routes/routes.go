package routes

import (
	"slices"
	"sync"
	"time"
)

type Snapshot struct {
	DeploymentID       string
	TargetID           string
	RevisionID         string
	TenantID           string
	Alias              string
	UpstreamModel      string
	RouterURL          string
	RouterProcessID    string
	Provider           string
	ProviderResourceID string
	Runtime            string
	ComputeMode        string
	UpstreamAPIKey     string
	ExternalPolicyID   string
	SelectionReason    string
	ProviderWorkers    *int
	ProviderObservedAt time.Time
}

// Directory is an atomic in-memory data-plane snapshot. Reads never touch the database.
type Directory struct {
	mu       sync.RWMutex
	items    map[string]Snapshot
	inflight map[string]int
	retired  map[string]Snapshot
}

func New() *Directory {
	return &Directory{items: make(map[string]Snapshot), inflight: make(map[string]int), retired: make(map[string]Snapshot)}
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
	route, ok := d.items[tenant+"\x00"+alias]
	return route, ok
}

// AcquireForTenant atomically pins the selected immutable route generation for
// the lifetime of a request. Publication may replace the directory entry, but
// retirement cannot stop its router or capacity until release is called.
func (d *Directory) AcquireForTenant(tenant, alias string) (Snapshot, func(), bool) {
	d.mu.Lock()
	route, ok := d.items[tenant+"\x00"+alias]
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
	ready := make([]Snapshot, 0)
	for id, route := range d.retired {
		if d.inflight[id] == 0 {
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
