// Package pricing defines timestamped provider-price estimates.
package pricing

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

var ErrUnavailable = errors.New("pricing unavailable")

type CostScope string

const (
	// CostScopeUnknown means the collector has not proven whether the amount is
	// a complete instance total or only one billing component. Unknown values
	// remain useful observations, but fail closed for deployment decisions.
	CostScopeUnknown CostScope = "unknown"
	// CostScopeInstanceTotal is the complete hourly price for the described
	// provider instance or marketplace offer. Only complete totals are eligible
	// for cross-provider ranking and launch-cost decisions.
	CostScopeInstanceTotal CostScope = "instance_total"
	// CostScopeAcceleratorOnly is one billing component of a larger instance
	// price. It is useful market evidence, but cannot be compared with a complete
	// instance total until the remaining components are assembled.
	CostScopeAcceleratorOnly CostScope = "accelerator_only"
)

type PriceAuthority string

const (
	// PriceAuthorityUnknown is the default for imported, manual, and historical
	// observations. A URL or source label alone never creates spend authority.
	PriceAuthorityUnknown PriceAuthority = "unknown"
	// PriceAuthorityProviderAPI identifies a quote fetched directly from the
	// provider's current pricing or marketplace API by a reviewed collector.
	PriceAuthorityProviderAPI PriceAuthority = "provider_api"
	// PriceAuthorityAccountContract is reserved for a verified customer-specific
	// contract or rate card. It is not inferred from user-supplied metadata.
	PriceAuthorityAccountContract PriceAuthority = "account_contract"
	// PriceAuthorityMeasuredInvoice identifies a complete rate derived from a
	// verified provider invoice or allocation export.
	PriceAuthorityMeasuredInvoice PriceAuthority = "measured_invoice"
)

type Request struct {
	Cloud, Region, GPU string
	GPUCount, Replicas int
}
type Estimate struct {
	Currency, Source string
	Hourly           float64
	CostScope        CostScope
	Authority        PriceAuthority
	ObservedAt       time.Time
	StaleAfter       time.Duration
	// GuaranteedUntil is set only when the provider/operator actually locks or
	// guarantees the quoted rate. Fresh marketplace observations leave it zero:
	// they may rank plans, but cannot authorize future spend.
	GuaranteedUntil time.Time
}

func (e Estimate) ComparableTotal() bool {
	return e.CostScope == CostScopeInstanceTotal
}

// RepositorySnapshotSource reports whether a price came from a repository-
// hosted snapshot rather than a provider or account pricing authority. These
// snapshots are useful for discovery and historical comparison, but their
// publication cadence and availability semantics are outside InferCrane's
// control, so they must never authorize an optimization run or deployment.
func RepositorySnapshotSource(source string) bool {
	parsed, err := url.Parse(strings.TrimSpace(source))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "raw.githubusercontent.com" || host == "github.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

func (e Estimate) DeploymentComparable() bool {
	if !e.ComparableTotal() || RepositorySnapshotSource(e.Source) {
		return false
	}
	return e.Authority == PriceAuthorityProviderAPI || e.Authority == PriceAuthorityAccountContract || e.Authority == PriceAuthorityMeasuredInvoice
}

func (e Estimate) Stale(now time.Time) bool {
	return e.ObservedAt.IsZero() || e.StaleAfter <= 0 || now.Sub(e.ObservedAt) > e.StaleAfter
}

type Provider interface {
	Estimate(context.Context, Request) (Estimate, error)
}

type Catalog struct{ Prices map[Request]Estimate }

func (c Catalog) Estimate(_ context.Context, request Request) (Estimate, error) {
	estimate, ok := c.Prices[request]
	if !ok {
		return Estimate{}, ErrUnavailable
	}
	return estimate, nil
}

// DynamicCatalog combines operator-supplied price evidence with periodically
// refreshed provider catalogs. Manual evidence always wins for an exact tuple.
// Provider refreshes replace only that provider's prior feed rows, so one
// unavailable source cannot erase other fresh observations.
type DynamicCatalog struct {
	mu     sync.RWMutex
	manual map[Request]Estimate
	feed   map[Request]Estimate
}

func NewDynamicCatalog(manual map[Request]Estimate) *DynamicCatalog {
	return &DynamicCatalog{manual: clonePrices(manual), feed: map[Request]Estimate{}}
}

func (c *DynamicCatalog) Estimate(_ context.Context, request Request) (Estimate, error) {
	if c == nil {
		return Estimate{}, ErrUnavailable
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if estimate, ok := c.manual[request]; ok {
		return estimate, nil
	}
	estimate, ok := c.feed[request]
	if !ok {
		return Estimate{}, ErrUnavailable
	}
	return estimate, nil
}

func (c *DynamicCatalog) ReplaceProvider(provider string, prices map[Request]Estimate) {
	if c == nil || provider == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for request := range c.feed {
		if request.Cloud == provider {
			delete(c.feed, request)
		}
	}
	for request, estimate := range prices {
		if request.Cloud == provider {
			c.feed[request] = estimate
		}
	}
}

// ReplaceProviderGPU atomically replaces one provider/GPU shard. Marketplace
// APIs with strict request budgets can refresh exact accelerator SKUs over
// several rate-limit windows without presenting a capped mixed response as a
// complete provider snapshot.
func (c *DynamicCatalog) ReplaceProviderGPU(provider, gpu string, prices map[Request]Estimate) {
	if c == nil || provider == "" || gpu == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for request := range c.feed {
		if request.Cloud == provider && request.GPU == gpu {
			delete(c.feed, request)
		}
	}
	for request, estimate := range prices {
		if request.Cloud == provider && request.GPU == gpu {
			c.feed[request] = estimate
		}
	}
}

func (c *DynamicCatalog) Snapshot() map[Request]Estimate {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	prices := clonePrices(c.feed)
	for request, estimate := range c.manual {
		prices[request] = estimate
	}
	return prices
}

func clonePrices(source map[Request]Estimate) map[Request]Estimate {
	cloned := make(map[Request]Estimate, len(source))
	for request, estimate := range source {
		cloned[request] = estimate
	}
	return cloned
}
