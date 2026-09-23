// Package openrouterportfolio ranks open-weight model opportunities without
// confusing market demand with qualified serving economics.
package openrouterportfolio

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const (
	EvidenceMeasured    = "measured"
	EvidenceAnalogous   = "analogous"
	EvidenceModeled     = "modeled"
	DecisionLaunch      = "launch"
	DecisionQualify     = "qualify"
	DecisionInvestigate = "investigate"
	DecisionWatch       = "watch"
	DecisionReject      = "reject"
)

// MarketModel is a content-free daily market observation. DailyTotalTokens is
// prompt plus completion tokens because that is the public OpenRouter demand
// unit. Prices are USD per one million tokens.
type MarketModel struct {
	ModelID                  string            `json:"model_id"`
	CanonicalSlug            string            `json:"canonical_slug"`
	HuggingFaceID            string            `json:"hugging_face_id"`
	DailyTotalTokens         int64             `json:"daily_total_tokens"`
	ProviderCount            int               `json:"provider_count"`
	FloorInputUSDPerMillion  float64           `json:"floor_input_usd_per_million"`
	FloorOutputUSDPerMillion float64           `json:"floor_output_usd_per_million"`
	BestUptimePercent        float64           `json:"best_uptime_percent,omitempty"`
	ContextLength            int64             `json:"context_length,omitempty"`
	ParameterCount           int64             `json:"parameter_count,omitempty"`
	EstimatedFP8WeightGiB    float64           `json:"estimated_fp8_weight_gib,omitempty"`
	MinimumH10080GBCount     int               `json:"minimum_h100_80gb_count,omitempty"`
	SupportedParameters      []string          `json:"supported_parameters,omitempty"`
	CompetitorOffers         []CompetitorOffer `json:"competitor_offers,omitempty"`
	CapturedAt               time.Time         `json:"captured_at"`
}

type CompetitorOffer struct {
	Provider            string  `json:"provider"`
	InputUSDPerMillion  float64 `json:"input_usd_per_million"`
	OutputUSDPerMillion float64 `json:"output_usd_per_million"`
	UptimePercent       float64 `json:"uptime_percent,omitempty"`
}

type HardwareOffer struct {
	Provider        string    `json:"provider"`
	Region          string    `json:"region"`
	GPU             string    `json:"gpu"`
	GPUCount        int       `json:"gpu_count"`
	HourlyCostUSD   float64   `json:"hourly_cost_usd"`
	PriceSource     string    `json:"price_source"`
	PriceObservedAt time.Time `json:"price_observed_at"`
	PriceValidUntil time.Time `json:"price_valid_until"`
}

// ServingProfile is evidence for one immutable model/runtime/hardware/workload
// tuple. A supplier or hardware change must lower the evidence level until it
// is reproduced on the exact hosted endpoint.
type ServingProfile struct {
	ID                          string        `json:"id"`
	ModelID                     string        `json:"model_id"`
	Runtime                     string        `json:"runtime"`
	Hardware                    HardwareOffer `json:"hardware"`
	AggregateOutputTokensPerSec float64       `json:"aggregate_output_tokens_per_second"`
	InputTokensPerOutputToken   float64       `json:"input_tokens_per_output_token"`
	EvidenceLevel               string        `json:"evidence_level"`
	EvidenceRef                 string        `json:"evidence_ref"`
	TargetContextLength         int64         `json:"target_context_length"`
	MaximumQualifiedContext     int64         `json:"maximum_qualified_context"`
	FeatureCoverage             float64       `json:"feature_coverage"`
	AvailabilityFactor          float64       `json:"availability_factor"`
	ObservedAt                  time.Time     `json:"observed_at"`
	Limitations                 []string      `json:"limitations,omitempty"`
}

type Policy struct {
	AssumedMarketShare          float64       `json:"assumed_market_share"`
	TargetUtilization           float64       `json:"target_utilization"`
	TargetContributionMargin    float64       `json:"target_contribution_margin"`
	NonGPURevenueReserve        float64       `json:"non_gpu_revenue_reserve"`
	InputPriceMultiplier        float64       `json:"input_price_multiplier"`
	OutputPriceMultiplier       float64       `json:"output_price_multiplier"`
	MaximumBreakEvenMarketShare float64       `json:"maximum_break_even_market_share"`
	MinimumFeatureCoverage      float64       `json:"minimum_feature_coverage"`
	MinimumAvailabilityFactor   float64       `json:"minimum_availability_factor"`
	MaximumPriceAge             time.Duration `json:"-"`
}

func DefaultPolicy() Policy {
	return Policy{
		AssumedMarketShare:          0.005,
		TargetUtilization:           0.60,
		TargetContributionMargin:    0.35,
		NonGPURevenueReserve:        0.10,
		InputPriceMultiplier:        0.99,
		OutputPriceMultiplier:       0.99,
		MaximumBreakEvenMarketShare: 0.01,
		MinimumFeatureCoverage:      0.90,
		MinimumAvailabilityFactor:   0.95,
		MaximumPriceAge:             24 * time.Hour,
	}
}

type MarketOpportunity struct {
	Rank                   int      `json:"rank"`
	ModelID                string   `json:"model_id"`
	HuggingFaceID          string   `json:"hugging_face_id"`
	DailyTotalTokens       int64    `json:"daily_total_tokens"`
	ProviderCount          int      `json:"provider_count"`
	DemandPerProvider      float64  `json:"demand_tokens_per_provider"`
	ParameterCount         int64    `json:"parameter_count,omitempty"`
	EstimatedFP8WeightGiB  float64  `json:"estimated_fp8_weight_gib,omitempty"`
	MinimumH10080GBCount   int      `json:"minimum_h100_80gb_count,omitempty"`
	MarketOpportunityIndex float64  `json:"market_opportunity_index"`
	Decision               string   `json:"decision"`
	Reasons                []string `json:"reasons"`
}

type EconomicDecision struct {
	Rank                              int      `json:"rank"`
	ProfileID                         string   `json:"profile_id"`
	ModelID                           string   `json:"model_id"`
	Decision                          string   `json:"decision"`
	EvidenceLevel                     string   `json:"evidence_level"`
	ConfidenceMultiplier              float64  `json:"confidence_multiplier"`
	RiskAdjustedOutputTokensPerSecond float64  `json:"risk_adjusted_output_tokens_per_second"`
	LaunchInputUSDPerMillion          float64  `json:"launch_input_usd_per_million"`
	LaunchOutputUSDPerMillion         float64  `json:"launch_output_usd_per_million"`
	RevenuePerFullUtilizationHourUSD  float64  `json:"revenue_per_full_utilization_hour_usd"`
	BreakEvenUtilization              float64  `json:"break_even_utilization"`
	BreakEvenMarketShare              float64  `json:"break_even_market_share"`
	ExpectedUtilization               float64  `json:"expected_utilization"`
	ExpectedMonthlyRevenueUSD         float64  `json:"expected_monthly_revenue_usd"`
	ExpectedMonthlyGPUCostUSD         float64  `json:"expected_monthly_gpu_cost_usd"`
	ExpectedMonthlyContributionUSD    float64  `json:"expected_monthly_contribution_usd"`
	ExpectedContributionMargin        float64  `json:"expected_contribution_margin"`
	ContributionMarginAtTargetUtil    float64  `json:"contribution_margin_at_target_utilization"`
	RequiredShareForTargetUtilization float64  `json:"required_market_share_for_target_utilization"`
	Reasons                           []string `json:"reasons"`
	Limitations                       []string `json:"limitations,omitempty"`
}

type Report struct {
	SchemaVersion      string              `json:"schema_version"`
	GeneratedAt        time.Time           `json:"generated_at"`
	MarketWindowStart  time.Time           `json:"market_window_start,omitempty"`
	MarketWindowEnd    time.Time           `json:"market_window_end,omitempty"`
	Sources            []string            `json:"sources,omitempty"`
	Policy             Policy              `json:"policy"`
	MarketShortlist    []MarketOpportunity `json:"market_shortlist"`
	EconomicCandidates []EconomicDecision  `json:"economic_candidates"`
	Methodology        []string            `json:"methodology"`
}

func Rank(now time.Time, markets []MarketModel, profiles []ServingProfile, policy Policy) (Report, error) {
	if err := validatePolicy(policy); err != nil {
		return Report{}, err
	}
	marketByID := make(map[string]MarketModel, len(markets))
	shortlist := make([]MarketOpportunity, 0, len(markets))
	for _, market := range markets {
		if err := validateMarket(market); err != nil {
			return Report{}, err
		}
		marketByID[market.ModelID] = market
		providers := max(market.ProviderCount, 1)
		minimumGPUs := max(market.MinimumH10080GBCount, 1)
		demandPerProvider := float64(market.DailyTotalTokens) / float64(providers)
		shortlist = append(shortlist, MarketOpportunity{
			ModelID: market.ModelID, HuggingFaceID: market.HuggingFaceID,
			DailyTotalTokens: market.DailyTotalTokens, ProviderCount: market.ProviderCount,
			DemandPerProvider: demandPerProvider, ParameterCount: market.ParameterCount,
			EstimatedFP8WeightGiB: market.EstimatedFP8WeightGiB, MinimumH10080GBCount: market.MinimumH10080GBCount,
			// Square-root crowding penalty keeps a large validated market from
			// disappearing merely because several providers already serve it.
			MarketOpportunityIndex: float64(market.DailyTotalTokens) / (math.Sqrt(float64(providers)) * float64(minimumGPUs)),
			Decision:               DecisionInvestigate,
			Reasons: []string{
				fmt.Sprintf("%d daily tokens across %d providers", market.DailyTotalTokens, market.ProviderCount),
				fmt.Sprintf("estimated FP8 weights require at least %d H100 80GB GPUs before KV-cache headroom", minimumGPUs),
			},
		})
	}
	sort.SliceStable(shortlist, func(i, j int) bool {
		if shortlist[i].MarketOpportunityIndex == shortlist[j].MarketOpportunityIndex {
			return shortlist[i].ModelID < shortlist[j].ModelID
		}
		return shortlist[i].MarketOpportunityIndex > shortlist[j].MarketOpportunityIndex
	})
	for i := range shortlist {
		shortlist[i].Rank = i + 1
	}

	decisions := make([]EconomicDecision, 0, len(profiles))
	for _, profile := range profiles {
		market, ok := marketByID[profile.ModelID]
		if !ok {
			return Report{}, fmt.Errorf("serving profile %q references model absent from market snapshot", profile.ID)
		}
		decision, err := evaluate(now, market, profile, policy)
		if err != nil {
			return Report{}, fmt.Errorf("evaluate profile %q: %w", profile.ID, err)
		}
		decisions = append(decisions, decision)
	}
	sort.SliceStable(decisions, func(i, j int) bool {
		left, right := decisionPriority(decisions[i].Decision), decisionPriority(decisions[j].Decision)
		if left != right {
			return left > right
		}
		if decisions[i].ExpectedMonthlyContributionUSD != decisions[j].ExpectedMonthlyContributionUSD {
			return decisions[i].ExpectedMonthlyContributionUSD > decisions[j].ExpectedMonthlyContributionUSD
		}
		return decisions[i].BreakEvenMarketShare < decisions[j].BreakEvenMarketShare
	})
	for i := range decisions {
		decisions[i].Rank = i + 1
	}
	return Report{
		SchemaVersion: "infercrane.dev/openrouter-portfolio-report/v1", GeneratedAt: now.UTC(), Policy: policy,
		MarketShortlist: shortlist, EconomicCandidates: decisions,
		Methodology: []string{
			"Market demand ranks opportunity; it does not prove serving profit.",
			"Batch variants are excluded from the interactive provider portfolio.",
			"FP8 weight footprint is a lower-bound topology screen; it does not model MoE active parameters or throughput.",
			"Modeled and cross-provider evidence receive throughput haircuts before economics are calculated.",
			"Dedicated GPU cost is charged for every hour, including idle capacity.",
			"Launch requires exact measured evidence; estimates can only request qualification.",
			"OpenRouter routing share is a scenario, not a forecast or guarantee.",
		},
	}, nil
}

func evaluate(now time.Time, market MarketModel, profile ServingProfile, policy Policy) (EconomicDecision, error) {
	if err := validateProfile(now, profile, policy); err != nil {
		return EconomicDecision{}, err
	}
	confidence := evidenceConfidence(profile.EvidenceLevel)
	riskTPS := profile.AggregateOutputTokensPerSec * confidence * profile.AvailabilityFactor
	competitiveInput, competitiveOutput := competitivePrice(market, profile.InputTokensPerOutputToken)
	inputPrice := competitiveInput * policy.InputPriceMultiplier
	outputPrice := competitiveOutput * policy.OutputPriceMultiplier
	valuePerMillionOutput := outputPrice + profile.InputTokensPerOutputToken*inputPrice
	revenuePerFullHour := riskTPS * 3600 / 1_000_000 * valuePerMillionOutput
	monthlyGPUCost := profile.Hardware.HourlyCostUSD * 730
	usableRevenuePerFullHour := revenuePerFullHour * (1 - policy.NonGPURevenueReserve)
	breakEvenUtil := math.Inf(1)
	if usableRevenuePerFullHour > 0 {
		breakEvenUtil = profile.Hardware.HourlyCostUSD / usableRevenuePerFullHour
	}
	outputCapacityPerDay := riskTPS * 86400
	marketOutputEquivalent := float64(market.DailyTotalTokens) / (1 + profile.InputTokensPerOutputToken)
	breakEvenShare := math.Inf(1)
	targetShare := math.Inf(1)
	if marketOutputEquivalent > 0 {
		breakEvenShare = outputCapacityPerDay * breakEvenUtil / marketOutputEquivalent
		targetShare = outputCapacityPerDay * policy.TargetUtilization / marketOutputEquivalent
	}
	expectedOutputPerDay := marketOutputEquivalent * policy.AssumedMarketShare
	expectedUtil := math.Min(policy.TargetUtilization, expectedOutputPerDay/outputCapacityPerDay)
	expectedMonthlyRevenue := revenuePerFullHour * expectedUtil * 730
	expectedContribution := expectedMonthlyRevenue*(1-policy.NonGPURevenueReserve) - monthlyGPUCost
	expectedMargin := ratio(expectedContribution, expectedMonthlyRevenue)
	targetRevenue := revenuePerFullHour * policy.TargetUtilization
	targetContribution := targetRevenue*(1-policy.NonGPURevenueReserve) - profile.Hardware.HourlyCostUSD
	targetMargin := ratio(targetContribution, targetRevenue)

	decision := DecisionWatch
	reasons := []string{
		fmt.Sprintf("risk-adjusted capacity %.1f output tokens/s", riskTPS),
		fmt.Sprintf("break-even requires %.1f%% utilization and %.3f%% market share", breakEvenUtil*100, breakEvenShare*100),
		fmt.Sprintf("at %.3f%% assumed share, contribution is $%.0f/month", policy.AssumedMarketShare*100, expectedContribution),
	}
	profitable := expectedContribution > 0 && expectedMargin >= policy.TargetContributionMargin
	shareReachable := breakEvenShare <= policy.MaximumBreakEvenMarketShare
	contextQualified := profile.TargetContextLength > 0 && profile.MaximumQualifiedContext >= profile.TargetContextLength
	operationallyEligible := profile.FeatureCoverage >= policy.MinimumFeatureCoverage && profile.AvailabilityFactor >= policy.MinimumAvailabilityFactor && contextQualified
	switch {
	case breakEvenUtil > 1 || !shareReachable:
		decision = DecisionReject
		reasons = append(reasons, "economics do not clear the configured utilization or routing-share boundary")
	case profile.EvidenceLevel == EvidenceMeasured && profitable && operationallyEligible:
		decision = DecisionLaunch
		reasons = append(reasons, "exact measured economics and operational coverage clear launch policy")
	case profile.EvidenceLevel != EvidenceMeasured && targetMargin >= policy.TargetContributionMargin && operationallyEligible:
		decision = DecisionQualify
		reasons = append(reasons, "modeled economics justify exact hosted qualification but cannot authorize launch")
	case !operationallyEligible:
		decision = DecisionWatch
		reasons = append(reasons, "feature, context, or availability coverage is below the launch boundary")
	default:
		decision = DecisionWatch
		reasons = append(reasons, "demand or margin at the assumed routing share is not yet sufficient")
	}
	limitations := append([]string(nil), profile.Limitations...)
	if profile.EvidenceLevel != EvidenceMeasured {
		limitations = append(limitations, "throughput was not measured on this exact hosted supplier tuple")
	}
	return EconomicDecision{
		ProfileID: profile.ID, ModelID: profile.ModelID, Decision: decision, EvidenceLevel: profile.EvidenceLevel,
		ConfidenceMultiplier: confidence, RiskAdjustedOutputTokensPerSecond: riskTPS,
		LaunchInputUSDPerMillion: inputPrice, LaunchOutputUSDPerMillion: outputPrice,
		RevenuePerFullUtilizationHourUSD: revenuePerFullHour, BreakEvenUtilization: breakEvenUtil,
		BreakEvenMarketShare: breakEvenShare, ExpectedUtilization: expectedUtil,
		ExpectedMonthlyRevenueUSD: expectedMonthlyRevenue, ExpectedMonthlyGPUCostUSD: monthlyGPUCost,
		ExpectedMonthlyContributionUSD: expectedContribution, ExpectedContributionMargin: expectedMargin,
		ContributionMarginAtTargetUtil: targetMargin, RequiredShareForTargetUtilization: targetShare,
		Reasons: reasons, Limitations: limitations,
	}, nil
}

func evidenceConfidence(level string) float64 {
	switch level {
	case EvidenceMeasured:
		return 1
	case EvidenceAnalogous:
		return 0.85
	case EvidenceModeled:
		return 0.60
	default:
		return 0
	}
}

func validatePolicy(policy Policy) error {
	values := map[string]float64{
		"assumed market share": policy.AssumedMarketShare, "target utilization": policy.TargetUtilization,
		"target contribution margin": policy.TargetContributionMargin, "non-GPU reserve": policy.NonGPURevenueReserve,
		"input price multiplier": policy.InputPriceMultiplier, "output price multiplier": policy.OutputPriceMultiplier,
		"maximum break-even market share": policy.MaximumBreakEvenMarketShare,
		"minimum feature coverage":        policy.MinimumFeatureCoverage, "minimum availability": policy.MinimumAvailabilityFactor,
	}
	for name, value := range values {
		if value <= 0 || value > 1 {
			return fmt.Errorf("%s must be in (0,1]", name)
		}
	}
	if policy.NonGPURevenueReserve >= 1 || policy.TargetContributionMargin >= 1 {
		return errors.New("reserve and target margin must be below one")
	}
	return nil
}

func validateMarket(market MarketModel) error {
	if strings.TrimSpace(market.ModelID) == "" || strings.TrimSpace(market.HuggingFaceID) == "" {
		return errors.New("market model requires OpenRouter and Hugging Face identities")
	}
	if market.DailyTotalTokens < 0 || market.ProviderCount < 0 || market.FloorInputUSDPerMillion <= 0 || market.FloorOutputUSDPerMillion <= 0 {
		return fmt.Errorf("market model %q has invalid demand, competition, or pricing", market.ModelID)
	}
	return nil
}

func validateProfile(now time.Time, profile ServingProfile, policy Policy) error {
	if strings.TrimSpace(profile.ID) == "" || strings.TrimSpace(profile.ModelID) == "" || strings.TrimSpace(profile.Runtime) == "" || strings.TrimSpace(profile.EvidenceRef) == "" {
		return errors.New("profile requires id, model, runtime, and evidence reference")
	}
	if evidenceConfidence(profile.EvidenceLevel) == 0 {
		return fmt.Errorf("unknown evidence level %q", profile.EvidenceLevel)
	}
	if profile.AggregateOutputTokensPerSec <= 0 || profile.InputTokensPerOutputToken < 0 || profile.Hardware.HourlyCostUSD <= 0 || profile.Hardware.GPUCount < 1 {
		return errors.New("profile requires positive throughput, cost, GPU count, and a non-negative input/output ratio")
	}
	if profile.FeatureCoverage <= 0 || profile.FeatureCoverage > 1 || profile.AvailabilityFactor <= 0 || profile.AvailabilityFactor > 1 {
		return errors.New("feature coverage and availability must be in (0,1]")
	}
	if profile.TargetContextLength < 1 || profile.MaximumQualifiedContext < 1 {
		return errors.New("profile requires target and qualified context lengths")
	}
	if profile.Hardware.PriceObservedAt.IsZero() || now.Sub(profile.Hardware.PriceObservedAt) > policy.MaximumPriceAge || (!profile.Hardware.PriceValidUntil.IsZero() && !profile.Hardware.PriceValidUntil.After(now)) {
		return errors.New("hardware price evidence is stale or expired")
	}
	return nil
}

func ratio(numerator, denominator float64) float64 {
	if denominator <= 0 {
		return math.Inf(-1)
	}
	return numerator / denominator
}

func competitivePrice(market MarketModel, inputTokensPerOutput float64) (float64, float64) {
	bestInput, bestOutput := market.FloorInputUSDPerMillion, market.FloorOutputUSDPerMillion
	bestBlended := bestOutput + inputTokensPerOutput*bestInput
	for _, offer := range market.CompetitorOffers {
		if offer.InputUSDPerMillion <= 0 || offer.OutputUSDPerMillion <= 0 {
			continue
		}
		blended := offer.OutputUSDPerMillion + inputTokensPerOutput*offer.InputUSDPerMillion
		if blended < bestBlended {
			bestInput, bestOutput, bestBlended = offer.InputUSDPerMillion, offer.OutputUSDPerMillion, blended
		}
	}
	return bestInput, bestOutput
}

func decisionPriority(value string) int {
	switch value {
	case DecisionLaunch:
		return 5
	case DecisionQualify:
		return 4
	case DecisionInvestigate:
		return 3
	case DecisionWatch:
		return 2
	case DecisionReject:
		return 1
	default:
		return 0
	}
}
