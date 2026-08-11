// Package contextpassport owns bounded logical inference-session identity.
// It does not own or promise durable KV state.
package contextpassport

import (
	"sync"
	"time"
)

type Hint struct {
	ID, TenantID, SubjectID, PreferredBindingID, PreferredTargetID string
	ExpiresAt                                                      time.Time
}
type Directory struct {
	mu    sync.RWMutex
	items map[string]Hint
}

func New() *Directory { return &Directory{items: map[string]Hint{}} }
func (d *Directory) Put(h Hint) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.items[h.TenantID+"\x00"+h.ID] = h
}
func (d *Directory) Resolve(tenant, id, subject string, now time.Time) (Hint, bool) {
	d.mu.RLock()
	h, ok := d.items[tenant+"\x00"+id]
	d.mu.RUnlock()
	if !ok || !h.ExpiresAt.After(now) || h.SubjectID != subject {
		return Hint{}, false
	}
	return h, true
}
