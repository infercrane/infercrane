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
	request := pricing.Request{Cloud: "aws", Region: "eu-central-1", GPU: "L40S", GPUCount: 1, Replicas: 2}
	authority := PricingAuthority{Provider: pricing.Catalog{Prices: map[pricing.Request]pricing.Estimate{request: {Currency: "USD", Source: "aws-price-list/2026-08-24", Hourly: 3.72, ObservedAt: now, StaleAfter: 2 * time.Hour}}}, Now: func() time.Time { return now }}
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
	authority.Provider = pricing.Catalog{Prices: map[pricing.Request]pricing.Estimate{request: {Currency: "USD", Source: "future", Hourly: 3.72, ObservedAt: now.Add(time.Minute), StaleAfter: 2 * time.Hour}}}
	if _, err = authority.Quote(context.Background(), draft, now.Add(time.Hour)); err == nil {
		t.Fatal("future-dated price evidence was accepted")
	}
}
