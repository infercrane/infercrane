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
