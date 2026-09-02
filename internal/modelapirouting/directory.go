// Package modelapirouting owns the request-path snapshot for InferCrane's
// shared hosted Model API catalog. Control-plane code publishes immutable
// snapshots; request-path reads never query the database.
package modelapirouting

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

var (
	ErrRouteUnavailable = errors.New("hosted model API route is unavailable")
	ErrUnauthorized     = errors.New("hosted model API entitlement is not active")
)

// RetailRate is the immutable customer price captured by a request lease.
// Supplier prices and identities never belong in this contract.
type RetailRate struct {
	ID, ProductID, ContractDigest                     string
	Version                                           int
	InputMicrousdPerMillion, OutputMicrousdPerMillion int64
	CachedInputMicrousdPerMillion                     *int64
	ValidFrom, ValidUntil                             time.Time
}

func (r RetailRate) currentAt(at time.Time) bool {
	return r.ID != "" && r.ProductID != "" && r.Version > 0 && strings.HasPrefix(r.ContractDigest, "sha256:") &&
		r.InputMicrousdPerMillion > 0 && r.OutputMicrousdPerMillion > 0 &&
		!at.Before(r.ValidFrom) && at.Before(r.ValidUntil)
}

// Entitlement is customer-owned policy that authorizes one public product to
// one operator publication. It contains no supplier or credential details.
type Entitlement struct {
	ID, CustomerTenantID, ProductID, OperatorTenantID string
	ServingPlanID, RetailRateID                       string
	RetailRateVersion                                 int
	State                                             string
	MaxRequestMicrousd                                int64
	ValidFrom                                         time.Time
	ValidUntil                                        *time.Time
}

func (e Entitlement) activeAt(at time.Time) bool {
	return e.ID != "" && e.CustomerTenantID != "" && e.ProductID != "" && e.OperatorTenantID != "" &&
		e.CustomerTenantID != e.OperatorTenantID && e.ServingPlanID != "" && e.RetailRateID != "" &&
		e.RetailRateVersion > 0 && e.State == "active" && e.MaxRequestMicrousd > 0 &&
		!at.Before(e.ValidFrom) && (e.ValidUntil == nil || at.Before(*e.ValidUntil))
}

// Publication is the operator-owned current route contract.
type Publication struct {
	ProductID, OperatorTenantID, ServingPlanID, SupplyPlanID string
	CompatibilityKey                                         string
	EvidenceID                                               string
	EvidenceValidUntil                                       time.Time
	ValidUntil                                               time.Time
}

func (p Publication) currentAt(at time.Time) bool {
	return p.ProductID != "" && p.OperatorTenantID != "" && p.ServingPlanID != "" && p.SupplyPlanID != "" && p.CompatibilityKey != "" &&
		p.EvidenceID != "" && at.Before(p.EvidenceValidUntil) && at.Before(p.ValidUntil)
}

// Candidate is operator-private. Endpoint and credential are deliberately
// hidden from JSON so customer projections cannot leak supply details.
type Candidate struct {
	ID, ProductID, OperatorTenantID, ServingPlanID, SupplyPlanID string
	OfferID, QualificationEvidenceID, CompatibilityKey           string
	OfferVersion                                                 int64
	Protocol, Supplier, SupplierModelID                          string
	Operations                                                   []string
	Endpoint, Credential                                         string `json:"-"`
	Qualified, Available                                         bool
	ValidUntil                                                   time.Time
}

func (c Candidate) supports(operation string) bool {
	for _, supported := range c.Operations {
		if supported == operation {
			return true
		}
	}
	return false
}

func (c Candidate) validFor(p Publication, at time.Time) bool {
	if c.ID == "" || c.ProductID != p.ProductID || c.OperatorTenantID != p.OperatorTenantID ||
		c.ServingPlanID != p.ServingPlanID || c.SupplyPlanID != p.SupplyPlanID || c.OfferID == "" || c.OfferVersion <= 0 ||
		c.QualificationEvidenceID == "" || c.CompatibilityKey == "" || c.Protocol != "openai" ||
		c.Supplier == "" || c.SupplierModelID == "" || !c.Qualified || !c.Available || !at.Before(c.ValidUntil) {
		return false
	}
	parsed, err := url.Parse(c.Endpoint)
	return err == nil && parsed.Scheme == "https" && parsed.Host != ""
}

// PublishedRoute is built by the control plane from durable catalog, supply,
// publication, rate, and entitlement records.
type PublishedRoute struct {
	Entitlement Entitlement
	Publication Publication
	Rate        RetailRate
	Candidates  []Candidate
}

// RouteSource is the durable, secret-free control-plane projection compiled by
// Store. Credential references cross into plaintext only inside Publisher.
type RouteSource struct {
	Entitlement Entitlement
	Publication Publication
	Rate        RetailRate
	Candidates  []CandidateSource
}

type CandidateSource struct {
	Candidate           Candidate
	Adapter             string `json:"-"`
	CredentialReference string `json:"-"`
}

// Lease pins an immutable route generation and retail contract for one
// request. Later publications cannot change its settlement price.
type Lease struct {
	Entitlement Entitlement
	Publication Publication
	Rate        RetailRate
	Candidates  []Candidate
}

type snapshot struct{ routes map[string]PublishedRoute }

// Directory publishes complete immutable generations with atomic pointer
// replacement. Reads are lock-free and cannot observe a partially compiled
// product route.
type Directory struct {
	state atomic.Pointer[snapshot]
	now   func() time.Time
}

func NewDirectory() *Directory {
	d := &Directory{now: time.Now}
	d.state.Store(&snapshot{routes: map[string]PublishedRoute{}})
	return d
}

func key(tenant, product string) string { return tenant + "\x00" + product }

// Publish replaces the full hosted routing generation. Invalid entries fail
// the publication; the previous known-good generation remains active.
func (d *Directory) Publish(routes []PublishedRoute) error {
	next := make(map[string]PublishedRoute, len(routes))
	for index, route := range routes {
		if err := validatePublishedRoute(route); err != nil {
			return fmt.Errorf("route %d: %w", index, err)
		}
		entryKey := key(route.Entitlement.CustomerTenantID, route.Entitlement.ProductID)
		if _, exists := next[entryKey]; exists {
			return fmt.Errorf("route %d duplicates customer product", index)
		}
		next[entryKey] = cloneRoute(route)
	}
	d.state.Store(&snapshot{routes: next})
	return nil
}

func validatePublishedRoute(route PublishedRoute) error {
	e, p, rate := route.Entitlement, route.Publication, route.Rate
	if e.ProductID != p.ProductID || e.ProductID != rate.ProductID || e.OperatorTenantID != p.OperatorTenantID ||
		e.ServingPlanID != p.ServingPlanID || e.RetailRateID != rate.ID || e.RetailRateVersion != rate.Version {
		return errors.New("entitlement, publication, and immutable retail rate do not match")
	}
	if len(route.Candidates) == 0 {
		return errors.New("route requires at least one candidate")
	}
	seen := make(map[string]struct{}, len(route.Candidates))
	for _, candidate := range route.Candidates {
		if _, duplicate := seen[candidate.ID]; duplicate {
			return errors.New("candidate identity is duplicated")
		}
		seen[candidate.ID] = struct{}{}
	}
	return nil
}

// Acquire performs all time-dependent checks at request admission. Expired
// publication, price, entitlement, or evidence fails closed.
func (d *Directory) Acquire(customerTenant, productID string) (Lease, error) {
	if customerTenant == "" || productID == "" {
		return Lease{}, ErrUnauthorized
	}
	entry, exists := d.state.Load().routes[key(customerTenant, productID)]
	if !exists {
		return Lease{}, ErrUnauthorized
	}
	at := d.now().UTC()
	if !entry.Entitlement.activeAt(at) {
		return Lease{}, ErrUnauthorized
	}
	if !entry.Publication.currentAt(at) || !entry.Rate.currentAt(at) {
		return Lease{}, ErrRouteUnavailable
	}
	valid := make([]Candidate, 0, len(entry.Candidates))
	for _, candidate := range entry.Candidates {
		if !candidate.validFor(entry.Publication, at) {
			continue
		}
		if candidate.CompatibilityKey == entry.Publication.CompatibilityKey {
			valid = append(valid, candidate)
		}
	}
	if len(valid) == 0 {
		return Lease{}, ErrRouteUnavailable
	}
	return Lease{Entitlement: entry.Entitlement, Publication: entry.Publication, Rate: cloneRate(entry.Rate), Candidates: append([]Candidate(nil), valid...)}, nil
}

// ListForTenant returns product IDs only; it never returns private candidate
// or operator route details.
func (d *Directory) ListForTenant(tenant string) []string {
	at := d.now().UTC()
	state := d.state.Load()
	products := make([]string, 0)
	for _, route := range state.routes {
		if route.Entitlement.CustomerTenantID == tenant && route.Entitlement.activeAt(at) && route.Publication.currentAt(at) && route.Rate.currentAt(at) {
			products = append(products, route.Entitlement.ProductID)
		}
	}
	sort.Strings(products)
	return products
}

func cloneRoute(route PublishedRoute) PublishedRoute {
	copy := route
	copy.Rate = cloneRate(route.Rate)
	copy.Candidates = append([]Candidate(nil), route.Candidates...)
	for index := range copy.Candidates {
		copy.Candidates[index].Operations = append([]string(nil), route.Candidates[index].Operations...)
	}
	return copy
}

func cloneRate(rate RetailRate) RetailRate {
	copy := rate
	if rate.CachedInputMicrousdPerMillion != nil {
		value := *rate.CachedInputMicrousdPerMillion
		copy.CachedInputMicrousdPerMillion = &value
	}
	return copy
}
