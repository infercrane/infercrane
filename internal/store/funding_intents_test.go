package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/managedbilling"
)

func TestManagedFundingIntentIsDurableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "funding-intent-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Funding Intent"); err != nil {
		t.Fatal(err)
	}
	key := "checkout-1"
	id := managedbilling.FundingIntentID(tenant, key)
	requested := domain.ManagedFundingIntent{ID: id, TenantID: tenant, Provider: "stripe", IdempotencyKey: key, AmountMicrousd: 25_000_000, Currency: "USD"}
	first, firstLease, err := s.PrepareManagedFundingIntent(ctx, tenant, requested, time.Minute)
	if err != nil || first.ID != id || first.Status != "pending" || firstLease == "" {
		t.Fatalf("first=%+v lease=%q err=%v", first, firstLease, err)
	}
	retry, retryLease, err := s.PrepareManagedFundingIntent(ctx, tenant, requested, time.Minute)
	if err != nil || retry.ID != id || retryLease != "" {
		t.Fatalf("retry=%+v lease=%q err=%v", retry, retryLease, err)
	}
	changed := requested
	changed.AmountMicrousd = 50_000_000
	if _, _, err = s.PrepareManagedFundingIntent(ctx, tenant, changed, time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed amount error=%v", err)
	}
	if err = s.ReleaseManagedFundingIntentLease(ctx, tenant, id, firstLease); err != nil {
		t.Fatal(err)
	}
	_, reclaimedLease, err := s.PrepareManagedFundingIntent(ctx, tenant, requested, time.Minute)
	if err != nil || reclaimedLease == "" || reclaimedLease == firstLease {
		t.Fatalf("reclaimed lease=%q err=%v", reclaimedLease, err)
	}
	session := domain.ManagedCheckoutSession{FundingIntentID: id, Provider: "stripe", ProviderID: "cs_" + suffix, URL: "https://checkout.stripe.test/c/pay/" + suffix, AmountMicrousd: 25_000_000, Currency: "USD", ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}
	completed, err := s.CompleteManagedFundingIntent(ctx, tenant, id, reclaimedLease, session)
	if err != nil || completed.Status != "completed" || completed.CheckoutSessionID != session.ProviderID || completed.CheckoutURL != session.URL || !completed.CheckoutExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	adopted, adoptedLease, err := s.PrepareManagedFundingIntent(ctx, tenant, requested, time.Minute)
	if err != nil || adopted.Status != "completed" || adoptedLease != "" || adopted.CheckoutSessionID != session.ProviderID {
		t.Fatalf("adopted=%+v lease=%q err=%v", adopted, adoptedLease, err)
	}
	if same, sameErr := s.CompleteManagedFundingIntent(ctx, tenant, id, "stale-lease", session); sameErr != nil || same.CheckoutSessionID != session.ProviderID {
		t.Fatalf("idempotent completion=%+v err=%v", same, sameErr)
	}
	conflictingSession := session
	conflictingSession.ProviderID = "cs_other_" + suffix
	if _, err = s.CompleteManagedFundingIntent(ctx, tenant, id, "stale-lease", conflictingSession); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting completion error=%v", err)
	}
}

func TestManagedFundingIntentGrantsOneConcurrentCreationLease(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "-")
	tenant := "funding-concurrent-" + suffix
	if err := s.CreateTenant(ctx, tenant, "Funding Concurrent"); err != nil {
		t.Fatal(err)
	}
	key := "same-checkout"
	requested := domain.ManagedFundingIntent{ID: managedbilling.FundingIntentID(tenant, key), TenantID: tenant, Provider: "stripe", IdempotencyKey: key, AmountMicrousd: 25_000_000, Currency: "USD"}
	const callers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	leases := make([]string, 0, callers)
	errorsSeen := make([]error, 0)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			intent, lease, err := s.PrepareManagedFundingIntent(ctx, tenant, requested, time.Minute)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errorsSeen = append(errorsSeen, err)
				return
			}
			if intent.ID != requested.ID {
				errorsSeen = append(errorsSeen, fmt.Errorf("intent ID=%q", intent.ID))
			}
			if lease != "" {
				leases = append(leases, lease)
			}
		}()
	}
	wg.Wait()
	if len(errorsSeen) != 0 || len(leases) != 1 {
		t.Fatalf("errors=%v creation leases=%v", errorsSeen, leases)
	}
}
