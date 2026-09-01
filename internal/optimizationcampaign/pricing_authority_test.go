package optimizationcampaign

import (
	"context"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/pricing"
)

func TestPricingAuthorityRequiresExactFreshPriceThroughExecutionWindow(t *testing.T) {
	now := time.Now().UTC()
	draft := optimizer.DeploymentDraft{}
	draft.Provider.Cloud, draft.Provider.Region, draft.Resources.GPU = "aws", "eu-central-1", "L40S"
	draft.Scaling.MaxReplicas = 2
	request := pricing.Request{Cloud: "aws", Region: "eu-central-1", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 2}
	authority := PricingAuthority{Provider: pricing.Catalog{Prices: map[pricing.Request]pricing.Estimate{request: {Currency: "USD", Source: "aws-price-list/2026-08-24", Hourly: 3.72, ObservedAt: now, StaleAfter: 2 * time.Hour, GuaranteedUntil: now.Add(2 * time.Hour)}}}, Now: func() time.Time { return now }}
	quote, err := authority.Quote(context.Background(), draft, now.Add(time.Hour))
	if err != nil || quote.HourlyUSD != 3.72 || quote.Source == "" || quote.ValidUntil != now.Add(2*time.Hour) {
		t.Fatalf("quote=%+v err=%v", quote, err)
	}
	draft.Scaling.MaxReplicas = 1
	if _, err = authority.Quote(context.Background(), draft, now.Add(time.Hour)); err == nil {
		t.Fatal("pricing for another replica count must not authorize execution")
	}
	draft.Scaling.MaxReplicas = 2
	if _, err = authority.Quote(context.Background(), draft, now.Add(3*time.Hour)); err == nil {
		t.Fatal("price that expires during execution must not authorize mutation")
	}
	authority.Provider = pricing.Catalog{Prices: map[pricing.Request]pricing.Estimate{request: {Currency: "USD", Source: "future", Hourly: 3.72, ObservedAt: now.Add(time.Minute), StaleAfter: 2 * time.Hour, GuaranteedUntil: now.Add(2 * time.Hour)}}}
	if _, err = authority.Quote(context.Background(), draft, now.Add(time.Hour)); err == nil {
		t.Fatal("future-dated price evidence was accepted")
	}
}

func TestPricingAuthorityResolvesRunPodAliasButRejectsUnlockedMarketPrice(t *testing.T) {
	now := time.Now().UTC()
	draft := optimizer.DeploymentDraft{}
	draft.Provider.Cloud, draft.Provider.Region, draft.Resources.GPU = "runpod", "EU-RO-1", "H100"
	draft.Resources.GPUCount, draft.Scaling.MaxReplicas = 1, 1
	request := pricing.Request{Cloud: "runpod", Region: "global", GPU: "NVIDIA H100 80GB HBM3", GPUCount: 1, Replicas: 1}
	market := pricing.Estimate{Currency: "USD", Source: "runpod provider API", Hourly: 2.69, ObservedAt: now, StaleAfter: 2 * time.Minute}
	authority := PricingAuthority{Provider: pricing.Catalog{Prices: map[pricing.Request]pricing.Estimate{request: market}}, Now: func() time.Time { return now }}
	if _, err := authority.Quote(context.Background(), draft, now.Add(time.Minute)); err == nil {
		t.Fatal("fresh but unlocked marketplace price authorized mutation")
	}
	market.GuaranteedUntil = now.Add(2 * time.Minute)
	authority.Provider = pricing.Catalog{Prices: map[pricing.Request]pricing.Estimate{request: market}}
	if quote, err := authority.Quote(context.Background(), draft, now.Add(time.Minute)); err != nil || quote.HourlyUSD != 2.69 {
		t.Fatalf("reviewed alias did not resolve to the exact locked provider SKU: quote=%#v err=%v", quote, err)
	}
}
