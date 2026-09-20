package controlapi

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

type sandboxPreviewLease struct {
	TenantID, SandboxID, ProviderPath string
	ExpiresAt                         time.Time
}

// SandboxPreviewBroker keeps provider lease capabilities server-side. The
// browser receives an unrelated InferCrane token that is useful only through
// an authenticated, tenant-authorized proxy request.
type SandboxPreviewBroker struct {
	mu     sync.Mutex
	leases map[string]sandboxPreviewLease
	clock  func() time.Time
}

func NewSandboxPreviewBroker() *SandboxPreviewBroker {
	return &SandboxPreviewBroker{leases: make(map[string]sandboxPreviewLease), clock: time.Now}
}

func (b *SandboxPreviewBroker) issue(tenantID, sandboxID, providerPath string, expiresAt time.Time) (string, bool) {
	if b == nil || tenantID == "" || sandboxID == "" || providerPath == "" || !expiresAt.After(b.clock()) {
		return "", false
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", false
	}
	token := base64.RawURLEncoding.EncodeToString(random)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.purgeLocked()
	b.leases[token] = sandboxPreviewLease{TenantID: tenantID, SandboxID: sandboxID, ProviderPath: providerPath, ExpiresAt: expiresAt}
	return token, true
}

func (b *SandboxPreviewBroker) resolve(token, tenantID, sandboxID string) (sandboxPreviewLease, bool) {
	if b == nil {
		return sandboxPreviewLease{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.purgeLocked()
	lease, ok := b.leases[token]
	if !ok || lease.TenantID != tenantID || lease.SandboxID != sandboxID {
		return sandboxPreviewLease{}, false
	}
	return lease, true
}

func (b *SandboxPreviewBroker) purgeLocked() {
	now := b.clock()
	for token, lease := range b.leases {
		if !lease.ExpiresAt.After(now) {
			delete(b.leases, token)
		}
	}
}
