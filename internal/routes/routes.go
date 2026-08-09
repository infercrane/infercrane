package routes

import (
	"slices"
	"sync"
)

type Snapshot struct {
	DeploymentID    string
	TenantID        string
	Alias           string
	UpstreamModel   string
	RouterURL       string
	RouterProcessID string
}

// Directory is an atomic in-memory data-plane snapshot. Reads never touch the database.
type Directory struct {
	mu    sync.RWMutex
	items map[string]Snapshot
}

func New() *Directory { return &Directory{items: make(map[string]Snapshot)} }

func (d *Directory) Get(alias string) (Snapshot, bool) {
	return d.GetForTenant("global", alias)
}
func (d *Directory) GetForTenant(tenant, alias string) (Snapshot, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	route, ok := d.items[tenant+"\x00"+alias]
	return route, ok
}

func (d *Directory) Put(route Snapshot) {
	if route.TenantID == "" {
		route.TenantID = "global"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.items[route.TenantID+"\x00"+route.Alias] = route
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
	delete(d.items, tenant+"\x00"+alias)
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
