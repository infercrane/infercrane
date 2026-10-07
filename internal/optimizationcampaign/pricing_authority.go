package optimizationcampaign

import (
	"context"
	"errors"
	"fmt"
	"time"

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

func (a PricingAuthority) Quote(ctx context.Context, draft optimizer.DeploymentDraft, requiredUntil time.Time) (CostQuote, error) {
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
