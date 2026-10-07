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
