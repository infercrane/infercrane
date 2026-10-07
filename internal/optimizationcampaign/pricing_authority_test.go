package optimizationcampaign

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/pricing"
)

type connectionResolverFixture struct {
	item                         domain.ComputeConnection
	credential                   string
	err                          error
	tenant, connection, provider string
}

func (f *connectionResolverFixture) Resolve(_ context.Context, tenant, connection, provider string) (domain.ComputeConnection, string, error) {
	f.tenant, f.connection, f.provider = tenant, connection, provider
	if f.err != nil {
		return domain.ComputeConnection{}, "", f.err
	}
	if f.item.TenantID != tenant || f.item.ID != connection || f.item.Provider != provider {
		return domain.ComputeConnection{}, "", domain.ErrNotFound
	}
	return f.item, f.credential, nil
}

type recordingCostAuthority struct {
	quote  CostQuote
	err    error
	calls  int
	tenant string
}

func (f *recordingCostAuthority) Quote(_ context.Context, tenant string, _ optimizer.DeploymentDraft, _ time.Time) (CostQuote, error) {
	f.calls++
	f.tenant = tenant
	return f.quote, f.err
}

func TestPricingAuthorityRequiresExactFreshPriceThroughExecutionWindow(t *testing.T) {
	now := time.Now().UTC()
	draft := optimizer.DeploymentDraft{}
	draft.Provider.Cloud, draft.Provider.Region, draft.Resources.GPU = "aws", "eu-central-1", "L40S"
	draft.Scaling.MaxReplicas = 2
	request := pricing.Request{Cloud: "aws", Region: "eu-central-1", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 2}
	authority := PricingAuthority{Provider: pricing.Catalog{Prices: map[pricing.Request]pricing.Estimate{request: {Currency: "USD", Source: "aws-price-list/2026-08-24", Hourly: 3.72, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityAccountContract, ObservedAt: now, StaleAfter: 2 * time.Hour, GuaranteedUntil: now.Add(2 * time.Hour)}}}, Now: func() time.Time { return now }}
	quote, err := authority.Quote(context.Background(), "tenant-1", draft, now.Add(time.Hour))
	if err != nil || quote.HourlyUSD != 3.72 || quote.Source == "" || quote.ValidUntil != now.Add(2*time.Hour) || !quote.Locked {
		t.Fatalf("quote=%+v err=%v", quote, err)
	}
	draft.Scaling.MaxReplicas = 1
	if _, err = authority.Quote(context.Background(), "tenant-1", draft, now.Add(time.Hour)); err == nil {
		t.Fatal("pricing for another replica count must not authorize execution")
	}
	draft.Scaling.MaxReplicas = 2
	if _, err = authority.Quote(context.Background(), "tenant-1", draft, now.Add(3*time.Hour)); err == nil {
		t.Fatal("price that expires during execution must not authorize mutation")
	}
	authority.Provider = pricing.Catalog{Prices: map[pricing.Request]pricing.Estimate{request: {Currency: "USD", Source: "future", Hourly: 3.72, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityAccountContract, ObservedAt: now.Add(time.Minute), StaleAfter: 2 * time.Hour, GuaranteedUntil: now.Add(2 * time.Hour)}}}
	if _, err = authority.Quote(context.Background(), "tenant-1", draft, now.Add(time.Hour)); err == nil {
		t.Fatal("future-dated price evidence was accepted")
	}
}

func TestPricingAuthorityResolvesRunPodAliasAndMarksLiveMarketPriceUnlocked(t *testing.T) {
	now := time.Now().UTC()
	draft := optimizer.DeploymentDraft{}
	draft.Provider.Cloud, draft.Provider.Region, draft.Resources.GPU = "runpod", "EU-RO-1", "H100"
	draft.Resources.GPUCount, draft.Scaling.MaxReplicas = 1, 1
	request := pricing.Request{Cloud: "runpod", Region: "global", GPU: "NVIDIA H100 80GB HBM3", GPUCount: 1, Replicas: 1}
	market := pricing.Estimate{Currency: "USD", Source: "runpod provider API", Hourly: 2.69, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, ObservedAt: now, StaleAfter: 2 * time.Minute}
	authority := PricingAuthority{Provider: pricing.Catalog{Prices: map[pricing.Request]pricing.Estimate{request: market}}, Now: func() time.Time { return now }}
	quote, err := authority.Quote(context.Background(), "tenant-1", draft, now.Add(time.Minute))
	if err != nil || quote.HourlyUSD != 2.69 || quote.Locked || !quote.ValidUntil.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("fresh provider-native pay-as-you-go quote was not available: quote=%#v err=%v", quote, err)
	}
	market.GuaranteedUntil = now.Add(2 * time.Minute)
	authority.Provider = pricing.Catalog{Prices: map[pricing.Request]pricing.Estimate{request: market}}
	if quote, err := authority.Quote(context.Background(), "tenant-1", draft, now.Add(time.Minute)); err != nil || quote.HourlyUSD != 2.69 || !quote.Locked {
		t.Fatalf("reviewed alias did not resolve to the exact locked provider SKU: quote=%#v err=%v", quote, err)
	}
}

func TestConnectionPricingAuthorityUsesExactTenantCredentialWithoutManagedFallback(t *testing.T) {
	now := time.Now().UTC()
	managed := &recordingCostAuthority{quote: CostQuote{HourlyUSD: 99}}
	private := &recordingCostAuthority{quote: CostQuote{HourlyUSD: 0.74, Source: "runpod-live", ObservedAt: now, ValidUntil: now.Add(2 * time.Minute)}}
	resolver := &connectionResolverFixture{
		item:       domain.ComputeConnection{ID: "connection-1", TenantID: "tenant-1", Provider: "runpod", Status: "verified"},
		credential: "tenant-secret",
	}
	factoryCredential := ""
	authority := ConnectionPricingAuthority{
		Managed: managed, Connections: resolver,
		Providers: map[string]func(string) CostAuthority{"runpod": func(credential string) CostAuthority {
			factoryCredential = credential
			return private
		}},
	}
	draft := optimizer.DeploymentDraft{ComputeConnectionID: "connection-1"}
	draft.Provider.Cloud = "RunPod"
	quote, err := authority.Quote(t.Context(), "tenant-1", draft, now.Add(time.Minute))
	if err != nil || quote.HourlyUSD != 0.74 || private.calls != 1 || private.tenant != "tenant-1" {
		t.Fatalf("tenant quote=%+v err=%v private=%+v", quote, err, private)
	}
	if resolver.tenant != "tenant-1" || resolver.connection != "connection-1" || resolver.provider != "runpod" || factoryCredential != "tenant-secret" {
		t.Fatalf("wrong connection resolution: resolver=%+v credential matched=%v", resolver, factoryCredential == "tenant-secret")
	}
	if managed.calls != 0 {
		t.Fatalf("tenant pricing fell back to managed authority: %d calls", managed.calls)
	}
}

func TestConnectionPricingAuthorityFailsClosedForWrongTenantOrRejectedCredential(t *testing.T) {
	now := time.Now().UTC()
	managed := &recordingCostAuthority{quote: CostQuote{HourlyUSD: 99}}
	resolver := &connectionResolverFixture{item: domain.ComputeConnection{ID: "connection-1", TenantID: "tenant-1", Provider: "runpod"}, credential: "tenant-secret"}
	factoryCalls := 0
	authority := ConnectionPricingAuthority{
		Managed: managed, Connections: resolver,
		Providers: map[string]func(string) CostAuthority{"runpod": func(string) CostAuthority {
			factoryCalls++
			return &recordingCostAuthority{err: errors.New("HTTP 403 with secret tenant-secret")}
		}},
	}
	draft := optimizer.DeploymentDraft{ComputeConnectionID: "connection-1"}
	draft.Provider.Cloud = "runpod"
	if _, err := authority.Quote(t.Context(), "tenant-2", draft, now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("wrong-tenant connection did not fail closed: %v", err)
	}
	if factoryCalls != 0 || managed.calls != 0 {
		t.Fatalf("wrong tenant reached pricing: factory=%d managed=%d", factoryCalls, managed.calls)
	}
	if _, err := authority.Quote(t.Context(), "tenant-1", draft, now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "current price") || strings.Contains(err.Error(), "tenant-secret") {
		t.Fatalf("provider rejection was not sanitized: %v", err)
	}
	if factoryCalls != 1 || managed.calls != 0 {
		t.Fatalf("rejected tenant pricing fell back: factory=%d managed=%d", factoryCalls, managed.calls)
	}
}

func TestConnectionPricingAuthorityKeepsManagedPathWithoutConnection(t *testing.T) {
	managed := &recordingCostAuthority{quote: CostQuote{HourlyUSD: 3.72}}
	authority := ConnectionPricingAuthority{Managed: managed}
	quote, err := authority.Quote(t.Context(), "tenant-1", optimizer.DeploymentDraft{}, time.Now().Add(time.Hour))
	if err != nil || quote.HourlyUSD != 3.72 || managed.calls != 1 || managed.tenant != "tenant-1" {
		t.Fatalf("managed quote=%+v err=%v calls=%d tenant=%q", quote, err, managed.calls, managed.tenant)
	}
}

func TestRefreshingPricingAuthorityRequiresSuccessfulRefresh(t *testing.T) {
	delegate := &recordingCostAuthority{quote: CostQuote{HourlyUSD: 1}}
	authority := RefreshingPricingAuthority{Refresh: func(context.Context) error { return errors.New("provider rejected credential") }, Delegate: delegate}
	if _, err := authority.Quote(t.Context(), "tenant-1", optimizer.DeploymentDraft{}, time.Now().Add(time.Minute)); err == nil || delegate.calls != 0 {
		t.Fatalf("failed refresh reached stale delegate: err=%v calls=%d", err, delegate.calls)
	}
	refreshed := false
	authority.Refresh = func(context.Context) error { refreshed = true; return nil }
	if _, err := authority.Quote(t.Context(), "tenant-1", optimizer.DeploymentDraft{}, time.Now().Add(time.Minute)); err != nil || !refreshed || delegate.calls != 1 {
		t.Fatalf("successful refresh did not reach delegate: err=%v refreshed=%v calls=%d", err, refreshed, delegate.calls)
	}
}
