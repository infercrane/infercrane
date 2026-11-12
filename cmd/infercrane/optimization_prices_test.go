package main

import (
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/config"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/pricing"
)

func TestOwnedClusterRateCardAuthorizesKubernetesCampaignSpend(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	rateCard := config.OptimizationPrice{Cloud: "kubernetes", GPU: "LOCAL-CPU", GPUCount: 1, Replicas: 1, HourlyUSD: 0.5, Currency: "USD", Source: "operator-rate-card/poc-2026-11", ObservedAt: now.Add(-time.Hour), ValidUntil: now.Add(30 * 24 * time.Hour), Authority: pricing.PriceAuthorityAccountContract, CostScope: pricing.CostScopeInstanceTotal}
	advisory := rateCard
	advisory.Authority, advisory.CostScope = pricing.PriceAuthorityUnknown, pricing.CostScopeUnknown

	draft := optimizer.DeploymentDraft{}
	draft.Provider.Cloud = "kubernetes"
	draft.Resources.GPU, draft.Resources.GPUCount = "LOCAL-CPU", 1
	draft.Scaling.MaxReplicas = 1
	requiredUntil := now.Add(24 * time.Hour)
	quote := func(row config.OptimizationPrice) (optimizationcampaign.CostQuote, error) {
		catalog := pricing.NewDynamicCatalog(map[pricing.Request]pricing.Estimate{{Cloud: "kubernetes", GPU: "LOCAL-CPU", GPUCount: 1, Replicas: 1}: optimizationPriceEstimate(row)})
		return optimizationcampaign.PricingAuthority{Provider: catalog, Now: func() time.Time { return now }}.Quote(t.Context(), "tenant", draft, requiredUntil)
	}

	authorized, err := quote(rateCard)
	if err != nil || !authorized.Locked || authorized.HourlyUSD != 0.5 || !authorized.ValidUntil.Equal(rateCard.ValidUntil) {
		t.Fatalf("owned-cluster rate card did not authorize spend: %+v err=%v", authorized, err)
	}
	if _, err := quote(advisory); err == nil {
		t.Fatal("an advisory manual price authorized campaign spend")
	}
	short := rateCard
	short.ValidUntil = now.Add(time.Hour)
	if _, err := quote(short); err == nil {
		t.Fatal("a rate card that ends before the approval window authorized spend")
	}
}
