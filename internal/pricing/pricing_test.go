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
