// Package external owns governed external-target selection and in-memory
// consumption of budget leases reserved durably by the control plane.
package external

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

var ErrBudgetExhausted = errors.New("external hard budget is exhausted")

type BudgetPool struct {
	mu        sync.Mutex
	leases    map[string]domain.ExternalBudgetLease
	refill    map[string]func(context.Context) (domain.ExternalBudgetLease, error)
	threshold map[string]int64
	refilling map[string]bool
}

func NewBudgetPool() *BudgetPool {
	return &BudgetPool{leases: map[string]domain.ExternalBudgetLease{}, refill: map[string]func(context.Context) (domain.ExternalBudgetLease, error){}, threshold: map[string]int64{}, refilling: map[string]bool{}}
}

// RegisterRefill installs a control-plane callback used only by a background
// prefetch. Authorize itself remains PostgreSQL-free and fail-closed.
func (p *BudgetPool) RegisterRefill(policyID string, threshold int64, refill func(context.Context) (domain.ExternalBudgetLease, error)) {
	if p == nil || policyID == "" || refill == nil {
		return
	}
	p.mu.Lock()
	p.refill[policyID] = refill
	p.threshold[policyID] = threshold
	p.mu.Unlock()
}

func (p *BudgetPool) Add(lease domain.ExternalBudgetLease) error {
	if p == nil || lease.PolicyID == "" || lease.Requests < 1 || lease.MaxRequestCostMicrousd < 1 || lease.ReservedCostMicrousd != lease.Requests*lease.MaxRequestCostMicrousd {
		return errors.New("valid external budget lease is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	current := p.leases[lease.PolicyID]
	current.PolicyID = lease.PolicyID
	current.Requests += lease.Requests
	current.ReservedCostMicrousd += lease.ReservedCostMicrousd
	current.MaxRequestCostMicrousd = lease.MaxRequestCostMicrousd
	p.leases[lease.PolicyID] = current
	return nil
}

func (p *BudgetPool) Authorize(policyID string) (domain.ExternalBudgetLease, error) {
	if p == nil || policyID == "" {
		return domain.ExternalBudgetLease{}, ErrBudgetExhausted
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	lease := p.leases[policyID]
	if lease.Requests < 1 || lease.ReservedCostMicrousd < lease.MaxRequestCostMicrousd || lease.MaxRequestCostMicrousd < 1 {
		return domain.ExternalBudgetLease{}, ErrBudgetExhausted
	}
	lease.Requests--
	lease.ReservedCostMicrousd -= lease.MaxRequestCostMicrousd
	p.leases[policyID] = lease
	p.prefetchLocked(policyID, lease.Requests)
	return domain.ExternalBudgetLease{PolicyID: policyID, Requests: 1, ReservedCostMicrousd: lease.MaxRequestCostMicrousd, MaxRequestCostMicrousd: lease.MaxRequestCostMicrousd}, nil
}

func (p *BudgetPool) prefetchLocked(policyID string, remaining int64) {
	refill := p.refill[policyID]
	if refill == nil || p.refilling[policyID] || remaining > p.threshold[policyID] {
		return
	}
	p.refilling[policyID] = true
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		lease, err := refill(ctx)
		if err == nil {
			_ = p.Add(lease)
		}
		p.mu.Lock()
		p.refilling[policyID] = false
		p.mu.Unlock()
	}()
}

func (p *BudgetPool) Remaining(policyID string) int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.leases[policyID].Requests
}
