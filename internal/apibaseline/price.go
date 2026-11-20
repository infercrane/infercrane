// Package apibaseline prices a closed API's measured workload: the token rate
// card recorded on its external binding applied to the token volume of a
// replay trace captured from that binding's requests. Missing, expired, or
// incomparable evidence makes the price unknown, never an estimate.
package apibaseline

import (
	"errors"
	"math"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/modelapicatalog"
	"github.com/infercrane/infercrane/internal/replay"
)

// Authorities say where a rate card's price comes from.
const (
	AuthorityListPrice = "list_price"
	AuthorityContract  = "contract"
)

// RateCard is what one external binding charges per million tokens.
type RateCard struct {
	ID         string                  `json:"id"`
	BindingID  string                  `json:"binding_id"`
	Authority  string                  `json:"authority"`
	Pricing    modelapicatalog.Pricing `json:"pricing"`
	Source     string                  `json:"source,omitempty"`
	ValidUntil time.Time               `json:"valid_until"`
	RecordedBy string                  `json:"recorded_by,omitempty"`
	CreatedAt  time.Time               `json:"created_at"`
}

// Validate checks a rate card before it is recorded.
func (c RateCard) Validate(now time.Time) error {
	if c.Authority != AuthorityListPrice && c.Authority != AuthorityContract {
		return errors.New("authority must be list_price or contract")
	}
	if c.Pricing.Currency != "USD" {
		return errors.New("pricing.currency must be USD")
	}
	if c.Pricing.InputMicrousdPerMillion == nil || c.Pricing.OutputMicrousdPerMillion == nil {
		return errors.New("pricing needs input and output prices per million tokens")
	}
	for _, price := range []*int64{c.Pricing.InputMicrousdPerMillion, c.Pricing.CachedInputMicrousdPerMillion, c.Pricing.OutputMicrousdPerMillion} {
		if price != nil && (*price < 0 || *price > 1_000_000_000_000) {
			return errors.New("prices must be 0..1000000 USD per million tokens")
		}
	}
	if !c.ValidUntil.After(now) {
		return errors.New("valid_until must be in the future")
	}
	if c.ValidUntil.Sub(now) > 2*366*24*time.Hour {
		return errors.New("valid_until must be within two years")
	}
	if len(c.Source) > 512 || strings.ContainsAny(c.Source, "\r\n") {
		return errors.New("source must be one line of at most 512 characters")
	}
	return nil
}

// Unknown reasons. Each one names the evidence that is missing.
const (
	UnknownNoRateCard      = "no_rate_card"
	UnknownRateCardExpired = "rate_card_expired"
	UnknownNoTraffic       = "no_traffic"
	UnknownMissingTokens   = "token_counts_missing"
	UnknownSpanTooShort    = "capture_span_too_short"
)

// Cost is a closed API's measured cost. Every money field is nil when
// Unknown is set; MonthlyUSD alone is nil when MonthlyUnknown is set.
type Cost struct {
	// BlendedUSDPerMillionTokens is the total cost divided by every input
	// and output token, per million: what a million tokens of this workload's
	// mix costs on this rate card.
	BlendedUSDPerMillionTokens *float64 `json:"blended_usd_per_million_tokens"`
	// USDPerMillionOutputTokens is the total cost per million output tokens,
	// comparable with the candidates' cost per million output tokens.
	USDPerMillionOutputTokens *float64 `json:"usd_per_million_output_tokens"`
	// WindowUSD is the cost of the captured requests.
	WindowUSD *float64 `json:"window_usd"`
	// MonthlyUSD projects WindowUSD over HoursPerMonth at the rate the
	// captured requests arrived.
	MonthlyUSD     *float64           `json:"monthly_usd"`
	Volume         replay.TokenVolume `json:"volume"`
	RateCardID     string             `json:"rate_card_id,omitempty"`
	Authority      string             `json:"authority,omitempty"`
	ValidUntil     *time.Time         `json:"valid_until,omitempty"`
	Unknown        string             `json:"unknown,omitempty"`
	MonthlyUnknown string             `json:"monthly_unknown,omitempty"`
}

// minimumSpan is the shortest kept span a monthly projection is made from.
const minimumSpan = time.Minute

// HoursPerMonth is the month a monthly cost covers, the same month the
// outcome report projects the candidates' cost over.
const HoursPerMonth = 730.0

// Price applies a rate card to a trace's token volume at time now. card is
// nil when the binding has none. captureSpan is the time the volume's
// requests arrived over: the capture window, or the kept span when the
// capture was cut off at its request limit.
func Price(card *RateCard, volume replay.TokenVolume, captureSpan time.Duration, now time.Time) Cost {
	cost := Cost{Volume: volume}
	unknown := func(reason string) Cost {
		cost.Unknown, cost.MonthlyUnknown = reason, reason
		return cost
	}
	if card == nil {
		return unknown(UnknownNoRateCard)
	}
	cost.RateCardID, cost.Authority = card.ID, card.Authority
	validUntil := card.ValidUntil
	cost.ValidUntil = &validUntil
	if !card.ValidUntil.After(now) || card.Pricing.InputMicrousdPerMillion == nil || card.Pricing.OutputMicrousdPerMillion == nil {
		return unknown(UnknownRateCardExpired)
	}
	if volume.Requests == 0 {
		return unknown(UnknownNoTraffic)
	}
	if volume.MissingInput > 0 || volume.MissingOutput > 0 || volume.OutputTokens == 0 {
		return unknown(UnknownMissingTokens)
	}
	cachedRate := *card.Pricing.InputMicrousdPerMillion
	if card.Pricing.CachedInputMicrousdPerMillion != nil {
		cachedRate = *card.Pricing.CachedInputMicrousdPerMillion
	}
	uncached := volume.InputTokens - volume.CachedInputTokens
	microusd := float64(uncached)*float64(*card.Pricing.InputMicrousdPerMillion) +
		float64(volume.CachedInputTokens)*float64(cachedRate) +
		float64(volume.OutputTokens)*float64(*card.Pricing.OutputMicrousdPerMillion)
	windowUSD := microusd / 1e6 / 1e6
	blended := windowUSD / float64(volume.InputTokens+volume.OutputTokens) * 1e6
	perOutput := windowUSD / float64(volume.OutputTokens) * 1e6
	cost.WindowUSD, cost.BlendedUSDPerMillionTokens, cost.USDPerMillionOutputTokens = rounded(windowUSD), rounded(blended), rounded(perOutput)
	if captureSpan < minimumSpan {
		cost.MonthlyUnknown = UnknownSpanTooShort
		return cost
	}
	monthly := windowUSD * HoursPerMonth * 3600 / captureSpan.Seconds()
	cost.MonthlyUSD = rounded(monthly)
	return cost
}

func rounded(value float64) *float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	out := math.Round(value*1e6) / 1e6
	return &out
}
