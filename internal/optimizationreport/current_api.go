package optimizationreport

import (
	"encoding/json"
	"math"
	"strconv"
	"time"

	"github.com/infercrane/infercrane/internal/apibaseline"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
	"github.com/infercrane/infercrane/internal/replay"
)

// Basis of the current API's figures.
const (
	BasisRateCard            = "rate_card_at_replayed_volume"
	BasisObservedLiveTraffic = "observed_live_traffic"
	BasisMigrationVerdict    = "signed_migration_verdict"
	// CurrentAPILatencyNote says why the closed API's latency is not a
	// controlled comparison with the setups'.
	CurrentAPILatencyNote = "Observed on your live traffic while it was captured, a different window from the setups' measurements; not a side-by-side run."
)

// CurrentAPIEvidence is what a migration campaign's report reads about the
// closed API it would replace. Now is when the rate card's validity is
// judged.
type CurrentAPIEvidence struct {
	Endpoint, Binding, BindingID, Model string
	RateCard                            *apibaseline.RateCard
	Verdicts                            []domain.MigrationQualityVerdict
	Reason                              string
	Now                                 time.Time
}

// CurrentAPI is the closed API a migration campaign compares the open-model
// setups with: its cost from its rate card at the replayed volume, its
// latency as observed on live traffic, and the winner's quality verdict
// against it. Every pointer is null when its evidence is missing.
type CurrentAPI struct {
	Endpoint string            `json:"endpoint"`
	Binding  string            `json:"binding"`
	Model    string            `json:"model"`
	Cost     CurrentAPICost    `json:"cost"`
	Latency  CurrentAPILatency `json:"latency"`
	Quality  CurrentAPIQuality `json:"quality"`
}

// CurrentAPICost is the closed API's measured cost. WinnerMonthlyUSD is the
// winning setup at the same replayed volume, so the change is the switch.
type CurrentAPICost struct {
	Basis                      string     `json:"basis"`
	Authority                  string     `json:"authority,omitempty"`
	RateCardID                 string     `json:"rate_card_id,omitempty"`
	Source                     string     `json:"source,omitempty"`
	ValidUntil                 *time.Time `json:"valid_until"`
	InputTokens                *int64     `json:"input_tokens"`
	CachedInputTokens          *int64     `json:"cached_input_tokens"`
	OutputTokens               *int64     `json:"output_tokens"`
	BlendedUSDPerMillionTokens *float64   `json:"blended_usd_per_million_tokens"`
	USDPerMillionOutputTokens  *float64   `json:"usd_per_million_output_tokens"`
	MonthlyUSD                 *float64   `json:"monthly_usd"`
	WinnerMonthlyUSD           *float64   `json:"winner_monthly_usd"`
	MonthlyChangeUSD           *float64   `json:"monthly_change_usd"`
	MonthlyChangePercent       *float64   `json:"monthly_change_percent"`
	Reason                     string     `json:"reason,omitempty"`
}

// CurrentAPILatency is what the closed API's live requests observed.
type CurrentAPILatency struct {
	Basis           string     `json:"basis"`
	WindowStart     *time.Time `json:"window_start"`
	WindowEnd       *time.Time `json:"window_end"`
	Requests        int        `json:"requests"`
	TTFTP95MS       *float64   `json:"ttft_p95_ms"`
	LatencyP95MS    *float64   `json:"latency_p95_ms"`
	WinnerTTFTP95MS *float64   `json:"winner_ttft_p95_ms"`
	Note            string     `json:"note"`
	Reason          string     `json:"reason,omitempty"`
}

// CurrentAPIQuality is the winner's signed verdict against the closed API.
type CurrentAPIQuality struct {
	Verdict              string   `json:"verdict"`
	Basis                string   `json:"basis"`
	VerdictID            string   `json:"verdict_id,omitempty"`
	Suite                string   `json:"suite,omitempty"`
	Evaluator            string   `json:"evaluator,omitempty"`
	MarginPercent        *float64 `json:"margin_percent"`
	CurrentAPIScore      *float64 `json:"current_api_score"`
	WinnerScore          *float64 `json:"winner_score"`
	IntervalLowerPercent *float64 `json:"interval_lower_percent"`
	IntervalUpperPercent *float64 `json:"interval_upper_percent"`
	Reason               string   `json:"reason,omitempty"`
}

func currentAPI(e Evidence, report Report) *CurrentAPI {
	if e.CurrentAPI == nil {
		return nil
	}
	evidence := *e.CurrentAPI
	result := &CurrentAPI{Endpoint: evidence.Endpoint, Binding: evidence.Binding, Model: evidence.Model,
		Cost:    CurrentAPICost{Basis: BasisUnknown},
		Latency: CurrentAPILatency{Basis: BasisUnknown, Note: CurrentAPILatencyNote},
		Quality: CurrentAPIQuality{Verdict: releaseguard.QualityUnknown, Basis: BasisUnknown}}
	if evidence.Reason != "" {
		result.Cost.Reason, result.Latency.Reason, result.Quality.Reason = evidence.Reason, evidence.Reason, evidence.Reason
		return result
	}
	for _, metric := range report.Metrics {
		if metric.Key == MetricTTFTP95 {
			result.Latency.WinnerTTFTP95MS = metric.Winner
		}
	}
	currentAPICost(&result.Cost, evidence, e.Replay, e.ReplayReason, report.Monthly)
	currentAPILatency(&result.Latency, e.Replay, e.ReplayReason)
	currentAPIQuality(&result.Quality, evidence, report.Outcome.WinnerCandidateID)
	return result
}

func currentAPICost(cost *CurrentAPICost, evidence CurrentAPIEvidence, trace *domain.ReplayTrace, replayReason string, monthly Monthly) {
	if trace == nil {
		cost.Reason = replayReason
		if cost.Reason == "" {
			cost.Reason = "the replayed traffic record was not found"
		}
		return
	}
	volume, err := replay.TraceTokenVolume(*trace)
	if err != nil {
		cost.Reason = "the replayed traffic could not be read"
		return
	}
	span := time.Duration(0)
	if monthly.WindowSeconds != nil {
		span = time.Duration(*monthly.WindowSeconds * float64(time.Second))
	}
	priced := apibaseline.Price(evidence.RateCard, volume, span, evidence.Now)
	cost.Authority, cost.RateCardID, cost.ValidUntil = priced.Authority, priced.RateCardID, priced.ValidUntil
	if evidence.RateCard != nil {
		cost.Source = evidence.RateCard.Source
	}
	if volume.MissingInput == 0 && volume.MissingOutput == 0 {
		cost.InputTokens, cost.CachedInputTokens, cost.OutputTokens = &volume.InputTokens, &volume.CachedInputTokens, &volume.OutputTokens
	}
	if priced.Unknown != "" {
		cost.Reason = costUnknownReason(priced.Unknown)
		return
	}
	cost.Basis, cost.BlendedUSDPerMillionTokens, cost.USDPerMillionOutputTokens = BasisRateCard, priced.BlendedUSDPerMillionTokens, priced.USDPerMillionOutputTokens
	switch {
	case monthly.WindowSeconds == nil:
		cost.Reason = "monthly cost is unknown: " + monthly.Reason
	case priced.MonthlyUnknown != "":
		cost.Reason = "monthly cost is unknown: " + costUnknownReason(priced.MonthlyUnknown)
	default:
		cost.MonthlyUSD = priced.MonthlyUSD
	}
	cost.WinnerMonthlyUSD = monthly.WinnerUSD
	if cost.MonthlyUSD != nil && cost.WinnerMonthlyUSD != nil && *cost.MonthlyUSD > 0 {
		cost.MonthlyChangeUSD = ptr(*cost.WinnerMonthlyUSD - *cost.MonthlyUSD)
		cost.MonthlyChangePercent = ptr(math.Round(*cost.MonthlyChangeUSD / *cost.MonthlyUSD * 10000) / 100)
	} else if cost.MonthlyUSD != nil && cost.Reason == "" {
		cost.Reason = "the winning setup's monthly cost at this volume is unknown"
	}
}

func costUnknownReason(code string) string {
	switch code {
	case apibaseline.UnknownNoRateCard:
		return "no rate card is recorded for the current API"
	case apibaseline.UnknownRateCardExpired:
		return "the current API's rate card has expired"
	case apibaseline.UnknownNoTraffic:
		return "the replayed traffic has no generation requests"
	case apibaseline.UnknownMissingTokens:
		return "some replayed requests have no token counts"
	case apibaseline.UnknownSpanTooShort:
		return "the replayed requests arrived over less than a minute"
	}
	return code
}

func currentAPILatency(latency *CurrentAPILatency, trace *domain.ReplayTrace, replayReason string) {
	if trace == nil {
		latency.Reason = replayReason
		if latency.Reason == "" {
			latency.Reason = "the replayed traffic record was not found"
		}
		return
	}
	observed, err := replay.TraceLatency(*trace)
	if err != nil || observed.Requests == 0 {
		latency.Reason = "the replayed traffic holds no generation requests"
		return
	}
	start, end := trace.WindowStart, trace.WindowEnd
	latency.Basis, latency.WindowStart, latency.WindowEnd, latency.Requests = BasisObservedLiveTraffic, &start, &end, observed.Requests
	latency.TTFTP95MS, latency.LatencyP95MS = observed.TTFTP95MS, observed.LatencyP95MS
	if latency.TTFTP95MS == nil {
		latency.Reason = "the live requests recorded no time to first token"
	}
}

func currentAPIQuality(quality *CurrentAPIQuality, evidence CurrentAPIEvidence, winnerID string) {
	if winnerID == "" {
		quality.Reason = "no setup was chosen, so no verdict compares one with the current API"
		return
	}
	for _, verdict := range evidence.Verdicts {
		if verdict.CandidateRunID != winnerID || verdict.BindingID != evidence.BindingID {
			continue
		}
		var payload qualityevidence.MigrationVerdict
		if json.Unmarshal([]byte(verdict.PayloadJSON), &payload) != nil {
			continue
		}
		margin, winner := payload.MarginPercent, payload.CandidateScore
		quality.Verdict, quality.Basis, quality.VerdictID = verdict.Verdict, BasisMigrationVerdict, verdict.ID
		quality.Suite, quality.Evaluator, quality.MarginPercent = payload.Suite, payload.Evaluator, &margin
		quality.CurrentAPIScore, quality.WinnerScore = payload.ReferenceScore, &winner
		if payload.Bootstrap != nil {
			quality.IntervalLowerPercent, quality.IntervalUpperPercent = payload.Bootstrap.IntervalLowerPercent, payload.Bootstrap.IntervalUpperPercent
		}
		if verdict.Verdict != releaseguard.QualityPass {
			quality.Reason = verdict.Code
		}
		return
	}
	quality.Reason = "the winning setup has no signed verdict against the current API"
}

// currentAPIFields formats the current API section for every renderer.
func currentAPIFields(current CurrentAPI) []ViewField {
	fields := []ViewField{{"Endpoint", current.Endpoint + " · binding " + current.Binding}}
	if current.Model != "" {
		fields = append(fields, ViewField{"Model", current.Model})
	}
	cost := current.Cost
	if cost.MonthlyUSD != nil || cost.BlendedUSDPerMillionTokens != nil {
		authority := map[string]string{apibaseline.AuthorityListPrice: "list price", apibaseline.AuthorityContract: "your contract"}[cost.Authority]
		line := orUnknown(cost.MonthlyUSD, usd) + " a month at your traffic · " + orUnknown(cost.BlendedUSDPerMillionTokens, usd) + " per 1M tokens (" + authority
		if cost.ValidUntil != nil {
			line += ", valid until " + cost.ValidUntil.Format("2 Jan 2006")
		}
		fields = append(fields, ViewField{"Cost", line + ")"})
	} else {
		fields = append(fields, ViewField{"Cost", "unknown: " + emptyAs(cost.Reason, "missing input")})
	}
	if cost.WinnerMonthlyUSD != nil {
		line := usd(*cost.WinnerMonthlyUSD) + " a month for the chosen setup at the same traffic"
		if cost.MonthlyChangeUSD != nil && cost.MonthlyChangePercent != nil {
			line += " · " + signedUSD(*cost.MonthlyChangeUSD) + " (" + signedPercent(*cost.MonthlyChangePercent) + ")"
		}
		fields = append(fields, ViewField{"After switching", line})
	} else if cost.MonthlyUSD != nil {
		fields = append(fields, ViewField{"After switching", "unknown: " + emptyAs(cost.Reason, "the chosen setup's monthly cost is unknown")})
	}
	latency := current.Latency
	if latency.Basis == BasisObservedLiveTraffic {
		line := "p95 first token " + orUnknown(latency.TTFTP95MS, ms) + " · p95 total " + orUnknown(latency.LatencyP95MS, ms) + " on " + strconv.Itoa(latency.Requests) + " live requests"
		if latency.WinnerTTFTP95MS != nil {
			line += " · chosen setup p95 first token " + ms(*latency.WinnerTTFTP95MS)
		}
		fields = append(fields, ViewField{"Observed latency", line + ". " + latency.Note})
	} else {
		fields = append(fields, ViewField{"Observed latency", "unknown: " + emptyAs(latency.Reason, "missing input")})
	}
	quality := current.Quality
	if quality.Basis == BasisMigrationVerdict {
		line := quality.Verdict + " · current API " + orUnknown(quality.CurrentAPIScore, score) + ", chosen setup " + orUnknown(quality.WinnerScore, score)
		if quality.IntervalUpperPercent != nil && quality.MarginPercent != nil {
			line += " · quality drop at most " + percent(math.Max(0, *quality.IntervalUpperPercent)) + " (95% interval) against a " + percent(*quality.MarginPercent) + " margin"
		}
		if quality.Suite != "" {
			line += " · " + quality.Suite + " graded by " + quality.Evaluator
		}
		fields = append(fields, ViewField{"Answer quality", line})
	} else {
		fields = append(fields, ViewField{"Answer quality", "unknown: " + emptyAs(quality.Reason, "missing input")})
	}
	return fields
}
