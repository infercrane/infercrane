package optimizationcampaign

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/pricing"
	"github.com/infercrane/infercrane/internal/provideridentity"
)

// PricingAuthority adapts InferCrane's timestamped provider pricing boundary
// to campaign execution authority. The provider must return a price for the
// exact cloud, region, accelerator, and maximum replica count that the
// candidate may consume.
type PricingAuthority struct {
	Provider pricing.Provider
	Now      func() time.Time
}

func (a PricingAuthority) Quote(ctx context.Context, _ string, draft optimizer.DeploymentDraft, requiredUntil time.Time) (CostQuote, error) {
	if a.Provider == nil {
		return CostQuote{}, errors.New("provider pricing is not configured")
	}
	replicas := draft.Scaling.MaxReplicas
	if replicas < 1 {
		replicas = 1
	}
	gpuCount := draft.Resources.GPUCount
	if gpuCount == 0 {
		gpuCount = 1
	}
	estimate, err := a.Provider.Estimate(ctx, pricing.Request{
		Cloud: draft.Provider.Cloud, Region: provideridentity.PriceRegion(draft.Provider.Cloud, draft.Provider.Region),
		GPU: provideridentity.GPUTypeID(draft.Provider.Cloud, draft.Resources.GPU), GPUCount: gpuCount, Replicas: replicas,
	})
	if err != nil {
		return CostQuote{}, err
	}
	now := time.Now().UTC()
	if a.Now != nil {
		now = a.Now().UTC()
	}
	if estimate.Currency != "USD" || estimate.Source == "" || estimate.Hourly <= 0 || estimate.ObservedAt.After(now) || estimate.Stale(now) || !estimate.DeploymentComparable() {
		return CostQuote{}, fmt.Errorf("exact provider price must be fresh deployment-comparable USD evidence for %s", requiredUntil.UTC().Format(time.RFC3339))
	}
	validUntil := estimate.ObservedAt.UTC().Add(estimate.StaleAfter)
	locked := !estimate.GuaranteedUntil.IsZero() && !estimate.GuaranteedUntil.Before(requiredUntil)
	if locked {
		validUntil = estimate.GuaranteedUntil.UTC()
	} else if estimate.Authority != pricing.PriceAuthorityProviderAPI {
		return CostQuote{}, fmt.Errorf("account price is not guaranteed through %s", requiredUntil.UTC().Format(time.RFC3339))
	}
	if !validUntil.After(now) {
		return CostQuote{}, errors.New("exact provider price expired before execution authorization")
	}
	return CostQuote{HourlyUSD: estimate.Hourly, Source: estimate.Source, ObservedAt: estimate.ObservedAt.UTC(), ValidUntil: validUntil, Locked: locked}, nil
}

// ComputeConnectionResolver decrypts one tenant-owned provider credential only
// at the network boundary. Implementations must not log or persist the returned
// credential outside their existing encrypted connection store.
type ComputeConnectionResolver interface {
	Resolve(context.Context, string, string, string) (domain.ComputeConnection, string, error)
}

// ConnectionPricingAuthority prevents a BYOC campaign from inheriting an
// operator-global account. A draft with a compute connection must resolve that
// exact tenant-owned credential and obtain a fresh provider-native quote. It
// never falls back to Managed when connection validation or pricing fails.
type ConnectionPricingAuthority struct {
	Managed     CostAuthority
	Connections ComputeConnectionResolver
	Providers   map[string]func(string) CostAuthority
}

func (a ConnectionPricingAuthority) Quote(ctx context.Context, tenant string, draft optimizer.DeploymentDraft, requiredUntil time.Time) (CostQuote, error) {
	connectionID := strings.TrimSpace(draft.ComputeConnectionID)
	if connectionID == "" {
		if a.Managed == nil {
			return CostQuote{}, errors.New("managed execution pricing is not configured")
		}
		return a.Managed.Quote(ctx, tenant, draft, requiredUntil)
	}
	provider := strings.ToLower(strings.TrimSpace(draft.Provider.Cloud))
	if strings.TrimSpace(tenant) == "" || provider == "" || a.Connections == nil {
		return CostQuote{}, errors.New("selected compute connection cannot be resolved for execution pricing")
	}
	_, credential, err := a.Connections.Resolve(ctx, tenant, connectionID, provider)
	if err != nil || strings.TrimSpace(credential) == "" {
		return CostQuote{}, errors.New("selected compute connection is unavailable; reconnect it before approving spend")
	}
	factory := a.Providers[provider]
	if factory == nil {
		return CostQuote{}, fmt.Errorf("selected %s compute connection does not support live execution pricing", provider)
	}
	authority := factory(credential)
	if authority == nil {
		return CostQuote{}, fmt.Errorf("selected %s compute connection pricing is not configured", provider)
	}
	quote, err := authority.Quote(ctx, tenant, draft, requiredUntil)
	if err != nil {
		return CostQuote{}, fmt.Errorf("selected %s compute connection could not provide a current price; reconnect it or choose another connection", provider)
	}
	return quote, nil
}

// RefreshingPricingAuthority refreshes a private, request-scoped catalog
// before every quote. It is used for tenant credentials so an approval proves
// the credential and current account-visible price immediately before spend.
type RefreshingPricingAuthority struct {
	Refresh  func(context.Context) error
	Delegate CostAuthority
}

func (a RefreshingPricingAuthority) Quote(ctx context.Context, tenant string, draft optimizer.DeploymentDraft, requiredUntil time.Time) (CostQuote, error) {
	if a.Refresh == nil || a.Delegate == nil {
		return CostQuote{}, errors.New("provider-native pricing refresh is not configured")
	}
	if err := a.Refresh(ctx); err != nil {
		return CostQuote{}, errors.New("provider-native pricing refresh failed")
	}
	return a.Delegate.Quote(ctx, tenant, draft, requiredUntil)
}
