package modelapireconciliation

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestReconcileUsesPinnedRateAndIntegerCOGS(t *testing.T) {
	rate := fixtureRate(t)
	usage := fixtureUsage(rate)
	result, err := Reconcile(usage, rate)
	if err != nil {
		t.Fatal(err)
	}
	if result.UncachedInputCOGSMicrousd != 80_000 || result.CachedInputCOGSMicrousd != 10_000 || result.OutputCOGSMicrousd != 100_000 {
		t.Fatalf("unexpected COGS components: %#v", result)
	}
	if result.SupplierCOGSMicrousd != 190_000 || result.GrossProfitMicrousd != 60_000 {
		t.Fatalf("unexpected reconciliation totals: %#v", result)
	}
	if !result.GrossMarginDefined || result.GrossMarginBPS != 2_400 {
		t.Fatalf("gross margin=%d defined=%t want=2400,true", result.GrossMarginBPS, result.GrossMarginDefined)
	}
	if !strings.HasPrefix(result.Digest, "sha256:") || len(result.Digest) != len("sha256:")+64 {
		t.Fatalf("unexpected reconciliation digest %q", result.Digest)
	}
	second, err := Reconcile(usage, rate)
	if err != nil || second.Digest != result.Digest {
		t.Fatalf("reconciliation is not deterministic: %q err=%v", second.Digest, err)
	}
}

func TestReconcileRoundsDimensionsIndependentlyAndFloorsLossMargin(t *testing.T) {
	rate := fixtureRate(t)
	rate.SupplierRateDraft.InputMicrousdPerMillion = 1
	rate.SupplierRateDraft.OutputMicrousdPerMillion = 1
	rate.SupplierRateDraft.HasCachedInputRate = true
	rate.SupplierRateDraft.CachedInputMicrousdPerMillion = 1
	var err error
	rate, err = NewSupplierRate(rate.SupplierRateDraft)
	if err != nil {
		t.Fatal(err)
	}
	usage := fixtureUsage(rate)
	usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.RetailMicrousd = 2, 1, 1, 3
	result, err := Reconcile(usage, rate)
	if err != nil {
		t.Fatal(err)
	}
	if result.UncachedInputCOGSMicrousd != 1 || result.CachedInputCOGSMicrousd != 1 || result.OutputCOGSMicrousd != 1 || result.SupplierCOGSMicrousd != 3 {
		t.Fatalf("fractional microdollars were not rounded per dimension: %#v", result)
	}

	rate.SupplierRateDraft.InputMicrousdPerMillion = 1_000_000
	rate.SupplierRateDraft.OutputMicrousdPerMillion = 1_000_000
	rate.SupplierRateDraft.HasCachedInputRate = true
	rate.SupplierRateDraft.CachedInputMicrousdPerMillion = 1_000_000
	rate, err = NewSupplierRate(rate.SupplierRateDraft)
	if err != nil {
		t.Fatal(err)
	}
	usage = fixtureUsage(rate)
	usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.RetailMicrousd = 3, 1, 1, 3
	result, err = Reconcile(usage, rate)
	if err != nil {
		t.Fatal(err)
	}
	if result.UncachedInputCOGSMicrousd != 2 || result.CachedInputCOGSMicrousd != 1 || result.OutputCOGSMicrousd != 1 || result.SupplierCOGSMicrousd != 4 {
		t.Fatalf("each dimension was not reconciled independently: %#v", result)
	}
	if result.GrossProfitMicrousd != -1 || result.GrossMarginBPS != -3_334 {
		t.Fatalf("loss margin=%d profit=%d want=-3334,-1", result.GrossMarginBPS, result.GrossProfitMicrousd)
	}
}

func TestReconcileMarksZeroRevenueMarginUndefined(t *testing.T) {
	rate := fixtureRate(t)
	usage := fixtureUsage(rate)
	usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.RetailMicrousd = 0, 0, 0, 0
	result, err := Reconcile(usage, rate)
	if err != nil {
		t.Fatal(err)
	}
	if result.GrossMarginDefined || result.GrossMarginBPS != 0 || result.SupplierCOGSMicrousd != 0 {
		t.Fatalf("unexpected zero-revenue result: %#v", result)
	}
}

func TestReconcileRejectsUnpinnedOrIncompleteAccountingInputs(t *testing.T) {
	rate := fixtureRate(t)
	base := fixtureUsage(rate)
	tests := []struct {
		name   string
		mutate func(*SettledUsage, *SupplierRate)
	}{
		{name: "not settled", mutate: func(usage *SettledUsage, _ *SupplierRate) { usage.State = "pending_reconciliation" }},
		{name: "rate digest mismatch", mutate: func(usage *SettledUsage, _ *SupplierRate) { usage.SupplierRateDigest = "sha256:other" }},
		{name: "offer mismatch", mutate: func(usage *SettledUsage, _ *SupplierRate) { usage.OfferVersion++ }},
		{name: "rate inactive at reservation", mutate: func(usage *SettledUsage, _ *SupplierRate) { usage.ReservedAt = rate.ValidUntil }},
		{name: "cached exceeds input", mutate: func(usage *SettledUsage, _ *SupplierRate) { usage.CachedInputTokens = usage.InputTokens + 1 }},
		{name: "cached rate missing", mutate: func(usage *SettledUsage, rate *SupplierRate) {
			rate.SupplierRateDraft.HasCachedInputRate = false
			rate.SupplierRateDraft.CachedInputMicrousdPerMillion = 0
			*rate, _ = NewSupplierRate(rate.SupplierRateDraft)
			usage.SupplierRateDigest = rate.Digest
		}},
		{name: "mutated immutable rate", mutate: func(_ *SettledUsage, rate *SupplierRate) { rate.InputMicrousdPerMillion++ }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage, candidateRate := base, rate
			test.mutate(&usage, &candidateRate)
			if _, err := Reconcile(usage, candidateRate); err == nil {
				t.Fatal("expected reconciliation rejection")
			}
		})
	}
}

func TestReconcileRejectsCOGSOverflow(t *testing.T) {
	rate := fixtureRate(t)
	rate.SupplierRateDraft.InputMicrousdPerMillion = math.MaxInt64
	var err error
	rate, err = NewSupplierRate(rate.SupplierRateDraft)
	if err != nil {
		t.Fatal(err)
	}
	usage := fixtureUsage(rate)
	usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens = math.MaxInt64, 0, 0
	if _, err = Reconcile(usage, rate); err == nil {
		t.Fatal("expected supplier COGS overflow rejection")
	}
}

func fixtureRate(t *testing.T) SupplierRate {
	t.Helper()
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.FixedZone("fixture", 2*60*60))
	rate, err := NewSupplierRate(SupplierRateDraft{
		ID: "deepseek-price-2026-09", Version: 1, OfferID: "deepseek-direct", OfferVersion: 7,
		TupleKey: "deepseek|deepseek-v4-flash|chat-completions|openai|global", Supplier: "deepseek",
		SupplierModelID: "deepseek-v4-flash", Currency: "USD", InputMicrousdPerMillion: 100_000,
		OutputMicrousdPerMillion: 400_000, HasCachedInputRate: true, CachedInputMicrousdPerMillion: 50_000,
		Provenance: "supplier contract revision 2026-09", ValidFrom: now.Add(-time.Hour), ValidUntil: now.Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	return rate
}

func fixtureUsage(rate SupplierRate) SettledUsage {
	reserved := rate.ValidFrom.Add(time.Hour)
	return SettledUsage{
		ReservationID: "reservation-1", State: SettlementSettled, OfferID: rate.OfferID, OfferVersion: rate.OfferVersion,
		TupleKey: rate.TupleKey, Supplier: rate.Supplier, SupplierModelID: rate.SupplierModelID,
		SupplierRateID: rate.ID, SupplierRateVersion: rate.Version, SupplierRateDigest: rate.Digest, Currency: rate.Currency,
		InputTokens: 1_000_000, CachedInputTokens: 200_000, OutputTokens: 250_000, RetailMicrousd: 250_000,
		ReservedAt: reserved, SettledAt: reserved.Add(2 * time.Minute),
	}
}
