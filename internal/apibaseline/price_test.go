package apibaseline

import (
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/modelapicatalog"
	"github.com/infercrane/infercrane/internal/replay"
)

func price(v int64) *int64 { return &v }

var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

func card() *RateCard {
	return &RateCard{ID: "card-1", BindingID: "binding-1", Authority: AuthorityListPrice, Pricing: modelapicatalog.Pricing{Currency: "USD", InputMicrousdPerMillion: price(2_000_000), CachedInputMicrousdPerMillion: price(500_000), OutputMicrousdPerMillion: price(8_000_000)}, ValidUntil: now.Add(24 * time.Hour)}
}

func TestPriceBlendsInputCachedAndOutputAtTheRateCard(t *testing.T) {
	volume := replay.TokenVolume{Requests: 1000, InputTokens: 1_000_000, CachedInputTokens: 400_000, CachedReported: 1000, OutputTokens: 250_000}
	cost := Price(card(), volume, time.Hour, now)
	if cost.Unknown != "" || cost.MonthlyUnknown != "" {
		t.Fatalf("unknown=%q monthly=%q", cost.Unknown, cost.MonthlyUnknown)
	}
	// 600k uncached × $2 + 400k cached × $0.5 + 250k output × $8 = 1.2 + 0.2 + 2.0 = $3.40.
	if cost.WindowUSD == nil || *cost.WindowUSD != 3.4 {
		t.Fatalf("window=%v", cost.WindowUSD)
	}
	if *cost.BlendedUSDPerMillionTokens != 2.72 || *cost.USDPerMillionOutputTokens != 13.6 {
		t.Fatalf("blended=%v per-output=%v", *cost.BlendedUSDPerMillionTokens, *cost.USDPerMillionOutputTokens)
	}
	if *cost.MonthlyUSD != 3.4*HoursPerMonth {
		t.Fatalf("monthly=%v", *cost.MonthlyUSD)
	}
	if cost.RateCardID != "card-1" || cost.Authority != AuthorityListPrice {
		t.Fatalf("citation=%+v", cost)
	}
}

func TestPriceChargesUncachedRateWhenNoCachedPriceIsListed(t *testing.T) {
	rates := card()
	rates.Pricing.CachedInputMicrousdPerMillion = nil
	cost := Price(rates, replay.TokenVolume{Requests: 1, InputTokens: 1_000_000, CachedInputTokens: 1_000_000, OutputTokens: 1}, time.Hour, now)
	if cost.WindowUSD == nil || *cost.WindowUSD != 2.000008 {
		t.Fatalf("window=%v", cost.WindowUSD)
	}
}

func TestPriceStaysUnknownWithoutComparableEvidence(t *testing.T) {
	complete := replay.TokenVolume{Requests: 2, InputTokens: 10, OutputTokens: 10}
	expired := card()
	expired.ValidUntil = now
	for name, test := range map[string]struct {
		card   *RateCard
		volume replay.TokenVolume
		want   string
	}{
		"no rate card":     {nil, complete, UnknownNoRateCard},
		"expired":          {expired, complete, UnknownRateCardExpired},
		"no traffic":       {card(), replay.TokenVolume{}, UnknownNoTraffic},
		"missing input":    {card(), replay.TokenVolume{Requests: 2, InputTokens: 10, OutputTokens: 10, MissingInput: 1}, UnknownMissingTokens},
		"missing output":   {card(), replay.TokenVolume{Requests: 2, InputTokens: 10, OutputTokens: 10, MissingOutput: 1}, UnknownMissingTokens},
		"no output tokens": {card(), replay.TokenVolume{Requests: 2, InputTokens: 10}, UnknownMissingTokens},
	} {
		cost := Price(test.card, test.volume, time.Hour, now)
		if cost.Unknown != test.want || cost.MonthlyUnknown != test.want || cost.WindowUSD != nil || cost.MonthlyUSD != nil || cost.BlendedUSDPerMillionTokens != nil {
			t.Fatalf("%s: %+v", name, cost)
		}
	}
	short := Price(card(), complete, 10*time.Second, now)
	if short.WindowUSD == nil || short.MonthlyUSD != nil || short.MonthlyUnknown != UnknownSpanTooShort || short.Unknown != "" {
		t.Fatalf("short span: %+v", short)
	}
}

func TestRateCardValidation(t *testing.T) {
	valid := card()
	if err := valid.Validate(now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RateCard){
		"authority":      func(c *RateCard) { c.Authority = "guess" },
		"currency":       func(c *RateCard) { c.Pricing.Currency = "EUR" },
		"no output":      func(c *RateCard) { c.Pricing.OutputMicrousdPerMillion = nil },
		"negative":       func(c *RateCard) { c.Pricing.InputMicrousdPerMillion = price(-1) },
		"past":           func(c *RateCard) { c.ValidUntil = now.Add(-time.Second) },
		"too far":        func(c *RateCard) { c.ValidUntil = now.Add(3 * 366 * 24 * time.Hour) },
		"multiline note": func(c *RateCard) { c.Source = "a\nb" },
	} {
		c := card()
		mutate(c)
		if c.Validate(now) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
