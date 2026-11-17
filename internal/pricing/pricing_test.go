package pricing

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCatalogAndStaleness(t *testing.T) {
	now := time.Now()
	req := Request{Cloud: "test", GPU: "L4", Replicas: 1}
	catalog := Catalog{Prices: map[Request]Estimate{req: {Hourly: 1, Currency: "USD", ObservedAt: now, StaleAfter: time.Hour}}}
	e, err := catalog.Estimate(context.Background(), req)
	if err != nil || e.Stale(now.Add(time.Minute)) {
		t.Fatalf("unexpected estimate: %#v %v", e, err)
	}
	_, err = catalog.Estimate(context.Background(), Request{Cloud: "missing"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReplicaScalingProviderRequiresExplicitLinearRate(t *testing.T) {
	request := Request{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 2}
	oneReplica := request
	oneReplica.Replicas = 1
	base := Estimate{Currency: "USD", Source: "runpod", Hourly: 1.09, CostScope: CostScopeInstanceTotal, Authority: PriceAuthorityProviderAPI}
	provider := ReplicaScalingProvider{Delegate: Catalog{Prices: map[Request]Estimate{oneReplica: base}}}
	if _, err := provider.Estimate(t.Context(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreviewed one-replica price authorized scale-out: %v", err)
	}
	base.ScalesLinearlyByReplica = true
	provider = ReplicaScalingProvider{Delegate: Catalog{Prices: map[Request]Estimate{oneReplica: base}}}
	estimate, err := provider.Estimate(t.Context(), request)
	if err != nil || estimate.Hourly != 2.18 || estimate.Source != base.Source || estimate.Authority != base.Authority {
		t.Fatalf("replica total=%+v err=%v", estimate, err)
	}
}

func TestReplicaScalingProviderPrefersExactQuote(t *testing.T) {
	request := Request{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 2}
	oneReplica := request
	oneReplica.Replicas = 1
	provider := ReplicaScalingProvider{Delegate: Catalog{Prices: map[Request]Estimate{
		oneReplica: {Hourly: 1.09, ScalesLinearlyByReplica: true},
		request:    {Hourly: 2.50, Source: "exact-two-replica-quote"},
	}}}
	estimate, err := provider.Estimate(t.Context(), request)
	if err != nil || estimate.Hourly != 2.50 || estimate.Source != "exact-two-replica-quote" {
		t.Fatalf("exact quote was not preferred: %+v err=%v", estimate, err)
	}
}

func TestComparableTotalRejectsPartialBillingComponents(t *testing.T) {
	if (Estimate{}).ComparableTotal() || !(Estimate{CostScope: CostScopeInstanceTotal}).ComparableTotal() {
		t.Fatal("only explicitly scoped complete totals may be comparable")
	}
	if (Estimate{CostScope: CostScopeAcceleratorOnly}).ComparableTotal() {
		t.Fatal("accelerator-only billing component became a complete deployment cost")
	}
}

func TestDeploymentComparableRejectsRepositorySnapshots(t *testing.T) {
	for _, source := range []string{
		"https://raw.githubusercontent.com/example/catalog/main/prices.csv",
		"https://github.com/example/catalog/releases/download/latest/prices.csv",
	} {
		if (Estimate{CostScope: CostScopeInstanceTotal, Authority: PriceAuthorityProviderAPI, Source: source}).DeploymentComparable() {
			t.Fatalf("repository snapshot became deployment price authority: %s", source)
		}
	}
	if !(Estimate{CostScope: CostScopeInstanceTotal, Authority: PriceAuthorityProviderAPI, Source: "https://api.runpod.io/graphql"}).DeploymentComparable() {
		t.Fatal("provider-owned price source was rejected")
	}
	if (Estimate{CostScope: CostScopeInstanceTotal, Source: "https://api.runpod.io/graphql"}).DeploymentComparable() {
		t.Fatal("a source label without positive authority became deployment-comparable")
	}
}

func TestDynamicCatalogReplacesOneProviderGPUShard(t *testing.T) {
	catalog := NewDynamicCatalog(nil)
	h100 := Request{Cloud: "vast", Region: "global", GPU: "H100 SXM", GPUCount: 1, Replicas: 1}
	l40s := Request{Cloud: "vast", Region: "global", GPU: "L40S", GPUCount: 1, Replicas: 1}
	catalog.ReplaceProvider("vast", map[Request]Estimate{
		h100: {Hourly: 2, Source: "old-h100"},
		l40s: {Hourly: 1, Source: "old-l40s"},
	})
	catalog.ReplaceProviderGPU("vast", "H100 SXM", map[Request]Estimate{
		h100: {Hourly: 1.5, Source: "new-h100"},
	})

	snapshot := catalog.Snapshot()
	if snapshot[h100].Source != "new-h100" || snapshot[l40s].Source != "old-l40s" {
		t.Fatalf("unexpected shard replacement: %#v", snapshot)
	}
	catalog.ReplaceProviderGPU("vast", "H100 SXM", nil)
	snapshot = catalog.Snapshot()
	if _, ok := snapshot[h100]; ok || snapshot[l40s].Source != "old-l40s" {
		t.Fatalf("empty shard did not remove only H100: %#v", snapshot)
	}
}

func TestDynamicCatalogDoesNotLetStaleManualEvidenceShadowProviderQuote(t *testing.T) {
	now := time.Now().UTC()
	request := Request{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}
	catalog := NewDynamicCatalog(map[Request]Estimate{request: {
		Currency: "USD", Hourly: .75, Source: "operator-snapshot",
		ObservedAt: now.Add(-2 * time.Hour), StaleAfter: time.Hour,
	}})
	catalog.ReplaceProvider("runpod", map[Request]Estimate{request: {
		Currency: "USD", Hourly: 1.09, CostScope: CostScopeInstanceTotal,
		Authority: PriceAuthorityProviderAPI, Source: "https://api.runpod.io/graphql",
		ObservedAt: now, StaleAfter: time.Minute,
	}})

	estimate, err := catalog.Estimate(t.Context(), request)
	if err != nil || estimate.Hourly != 1.09 || estimate.Authority != PriceAuthorityProviderAPI {
		t.Fatalf("stale manual evidence shadowed provider quote: %#v err=%v", estimate, err)
	}
}

func TestDynamicCatalogKeepsFreshAuthoritativeManualOverride(t *testing.T) {
	now := time.Now().UTC()
	request := Request{Cloud: "runpod", Region: "global", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 1}
	manual := Estimate{
		Currency: "USD", Hourly: .88, CostScope: CostScopeInstanceTotal,
		Authority: PriceAuthorityAccountContract, Source: "account-contract",
		ObservedAt: now, StaleAfter: time.Hour,
	}
	catalog := NewDynamicCatalog(map[Request]Estimate{request: manual})
	catalog.ReplaceProvider("runpod", map[Request]Estimate{request: {
		Currency: "USD", Hourly: 1.09, CostScope: CostScopeInstanceTotal,
		Authority: PriceAuthorityProviderAPI, Source: "https://api.runpod.io/graphql",
		ObservedAt: now, StaleAfter: time.Minute,
	}})

	estimate, err := catalog.Estimate(t.Context(), request)
	if err != nil || estimate.Hourly != manual.Hourly || estimate.Authority != PriceAuthorityAccountContract {
		t.Fatalf("fresh account contract did not remain authoritative: %#v err=%v", estimate, err)
	}
}

func TestWarmingProviderFetchesOnlyWhatIsMissingAndNamesTheRow(t *testing.T) {
	now := time.Date(2026, 11, 17, 2, 0, 0, 0, time.UTC)
	request := Request{Cloud: "vast", Region: "global", GPU: "RTX 4090", GPUCount: 1, Replicas: 1}
	fresh := Estimate{Currency: "USD", Hourly: 0.33, Source: "vast-offer", CostScope: CostScopeInstanceTotal, Authority: PriceAuthorityProviderAPI, ObservedAt: now, StaleAfter: time.Hour}
	catalog := NewDynamicCatalog(nil)
	warms := 0
	provider := WarmingProvider{Delegate: catalog, Now: func() time.Time { return now }, Warmers: map[string]Warmer{"vast": func(context.Context, Request) WarmResult {
		warms++
		catalog.ReplaceProviderGPU("vast", "RTX 4090", map[Request]Estimate{request: fresh})
		return WarmResult{Feed: "the Vast offer feed", Fetched: true}
	}}}
	if estimate, err := provider.Estimate(context.Background(), request); err != nil || estimate.Source != "vast-offer" || warms != 1 {
		t.Fatalf("estimate=%+v err=%v warms=%d", estimate, err, warms)
	}
	if _, err := provider.Estimate(context.Background(), request); err != nil || warms != 1 {
		t.Fatalf("a fresh row was fetched again: err=%v warms=%d", err, warms)
	}

	// A stale row is refreshed like a missing one.
	stale := fresh
	stale.ObservedAt = now.Add(-2 * time.Hour)
	catalog.ReplaceProviderGPU("vast", "RTX 4090", map[Request]Estimate{request: stale})
	if _, err := provider.Estimate(context.Background(), request); err != nil || warms != 2 {
		t.Fatalf("stale row: err=%v warms=%d", err, warms)
	}

	// A row no feed refreshes is named, and stays retryable.
	_, err := provider.Estimate(context.Background(), Request{Cloud: "aws", Region: "us-east-1", GPU: "NVIDIA L40S", GPUCount: 1, Replicas: 2})
	var missing *MissingPriceError
	if !errors.As(err, &missing) || !errors.Is(err, ErrUnavailable) || err.Error() != "no fresh aws price for 1 × NVIDIA L40S (region us-east-1, 2 replicas); no provider feed refreshes this row" {
		t.Fatalf("err=%v", err)
	}

	// A fetch that cannot produce the row says why and when the schedule runs.
	next := now.Add(5 * time.Minute)
	provider.Warmers["vast"] = func(context.Context, Request) WarmResult {
		return WarmResult{Feed: "the Vast offer feed", NextRefreshAt: next, Detail: "a Vast price refresh is running now; retry in a minute"}
	}
	_, err = provider.Estimate(context.Background(), Request{Cloud: "vast", Region: "global", GPU: "L4", GPUCount: 1, Replicas: 1})
	if err == nil || err.Error() != "no fresh vast price for 1 × L4 (region global, 1 replica); a Vast price refresh is running now; retry in a minute; the Vast offer feed fetches it next at about 2026-11-17 02:05 UTC" {
		t.Fatalf("err=%v", err)
	}

	// Errors other than a missing price are not hidden behind a fetch.
	broken := WarmingProvider{Delegate: failingProvider{errors.New("catalog offline")}, Warmers: provider.Warmers}
	if _, err := broken.Estimate(context.Background(), request); err == nil || err.Error() != "catalog offline" {
		t.Fatalf("err=%v", err)
	}
}

type failingProvider struct{ err error }

func (f failingProvider) Estimate(context.Context, Request) (Estimate, error) {
	return Estimate{}, f.err
}
