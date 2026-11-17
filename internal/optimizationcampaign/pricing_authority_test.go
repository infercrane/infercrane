package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/priceingest"
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

func TestPricingAuthorityAuthorizesExactMaximumFromReviewedPerReplicaRate(t *testing.T) {
	now := time.Now().UTC()
	draft := optimizer.DeploymentDraft{}
	draft.Provider.Cloud, draft.Provider.Region, draft.Resources.GPU = "runpod", "global", "L40S"
	draft.Resources.GPUCount, draft.Scaling.MaxReplicas = 1, 2
	oneReplica := pricing.Request{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}
	market := pricing.Estimate{
		Currency: "USD", Source: "https://api.runpod.io/graphql", Hourly: 1.09,
		CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI,
		ScalesLinearlyByReplica: true, ObservedAt: now, StaleAfter: 2 * time.Minute,
	}
	provider := pricing.ReplicaScalingProvider{Delegate: pricing.Catalog{Prices: map[pricing.Request]pricing.Estimate{oneReplica: market}}}
	authority := PricingAuthority{Provider: provider, Now: func() time.Time { return now }}
	quote, err := authority.Quote(t.Context(), "tenant-1", draft, now.Add(time.Minute))
	if err != nil || quote.HourlyUSD != 2.18 || quote.Source != market.Source || quote.Locked {
		t.Fatalf("maximum replica quote=%+v err=%v", quote, err)
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

func TestConnectionPricingAuthorityExplainsUnavailableProviderOfferWithoutBlamingCredential(t *testing.T) {
	now := time.Now().UTC()
	resolver := &connectionResolverFixture{
		item:       domain.ComputeConnection{ID: "connection-1", TenantID: "tenant-1", Provider: "runpod", Status: "verified"},
		credential: "tenant-secret",
	}
	authority := ConnectionPricingAuthority{
		Connections: resolver,
		Providers: map[string]func(string) CostAuthority{"runpod": func(string) CostAuthority {
			return &recordingCostAuthority{err: pricing.ErrUnavailable}
		}},
	}
	draft := optimizer.DeploymentDraft{ComputeConnectionID: "connection-1"}
	draft.Provider.Cloud, draft.Resources.GPU = "runpod", "L40S"
	_, err := authority.Quote(t.Context(), "tenant-1", draft, now.Add(time.Minute))
	if !errors.Is(err, pricing.ErrUnavailable) || !strings.Contains(err.Error(), "no provider offer for L40S") || strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("unavailable offer remediation=%v", err)
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

// fakeVast serves Vast's offer search. Once limited is set, every request is
// refused with 429 and no reset time, as Vast does when its window is spent.
type fakeVast struct {
	requests atomic.Int32
	queried  sync.Map
	limited  atomic.Bool
}

func (f *fakeVast) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.requests.Add(1)
	if f.limited.Load() {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	var query struct {
		GPU struct {
			Eq string `json:"eq"`
		} `json:"gpu_name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&query)
	f.queried.Store(query.GPU.Eq, true)
	_, _ = w.Write([]byte(`{"offers":[{"id":57948831,"gpu_name":"` + query.GPU.Eq + `","num_gpus":1,"dph_total":0.328,"rentable":true,"rented":false,"verification":"verified"}]}`))
}

// The 2026-11-17 run: after a control-plane restart only the first sweep's
// four GPUs were priced, and approving an RTX 4090 run failed with a bare
// "pricing unavailable" until the rotation reached it about five minutes later.
func TestApprovalAfterRestartWarmsTheRequestedVastGPUOrNamesTheMissingRow(t *testing.T) {
	start := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	vast := &fakeVast{}
	server := httptest.NewServer(vast)
	defer server.Close()
	catalog := pricing.NewDynamicCatalog(nil)
	feed := &priceingest.VastFeed{BaseURL: server.URL, Client: server.Client(), ValidFor: 100 * time.Minute, Interval: 5 * time.Minute, Now: now}
	if err := feed.Refresh(t.Context(), catalog); err != nil {
		t.Fatal(err)
	}
	if _, swept := vast.queried.Load("RTX 4090"); swept || vast.requests.Load() != 4 {
		t.Fatalf("the startup sweep should cover four GPUs before RTX 4090; requests=%d", vast.requests.Load())
	}
	authority := PricingAuthority{Now: now, Provider: pricing.WarmingProvider{
		Delegate: pricing.ReplicaScalingProvider{Delegate: catalog},
		Warmers: map[string]pricing.Warmer{"vast": func(ctx context.Context, request pricing.Request) pricing.WarmResult {
			return feed.Warm(ctx, catalog, request)
		}},
		Now: now,
	}}
	draft := optimizer.DeploymentDraft{}
	draft.Provider.Cloud, draft.Resources.GPU, draft.Resources.GPUCount = "vast", "RTX4090", 1
	draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = 1, 1
	budget := Budget{MaxCostUSD: 5, ExpiresAt: start.Add(time.Hour)}

	if err := AuthorizeCost(t.Context(), authority, "tenant", draft, budget, now()); err != nil {
		t.Fatalf("approval did not warm the requested GPU: %v", err)
	}
	if vast.requests.Load() != 5 {
		t.Fatalf("warming should spend exactly one request; requests=%d", vast.requests.Load())
	}

	// Another GPU in the same window is not fetched: the warm budget is spent.
	// Approval names the exact row and when the rotation reaches it.
	draft.Resources.GPU = "RTX5090"
	clock.Store(start.Add(time.Minute).UnixNano())
	err := AuthorizeCost(t.Context(), authority, "tenant", draft, budget, now())
	var missing *pricing.MissingPriceError
	if !errors.As(err, &missing) || !errors.Is(err, pricing.ErrUnavailable) || vast.requests.Load() != 5 {
		t.Fatalf("err=%v requests=%d", err, vast.requests.Load())
	}
	for _, want := range []string{"no fresh vast price for 1 × RTX 5090 (region global, 1 replica)", "an on-demand Vast price fetch already ran at 02:00:00 UTC", "the Vast offer feed fetches it next at about 2026-11-17 02:20 UTC"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("approval error %q lacks %q", err, want)
		}
	}

	// When Vast refuses the on-demand fetch, the row is still named.
	vast.limited.Store(true)
	draft.Resources.GPU = "L4"
	clock.Store(start.Add(10 * time.Minute).UnixNano())
	err = AuthorizeCost(t.Context(), authority, "tenant", draft, budget, now())
	if !errors.As(err, &missing) || !strings.Contains(err.Error(), "no fresh vast price for 1 × L4 (region global, 1 replica); fetching it now failed (Vast L4 offers returned HTTP 429)") {
		t.Fatalf("err=%v", err)
	}
}
