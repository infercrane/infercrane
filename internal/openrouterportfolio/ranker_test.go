package openrouterportfolio

import (
	"math"
	"testing"
	"time"
)

func TestRankSeparatesMarketDemandFromLaunchEvidence(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	markets := []MarketModel{
		{ModelID: "model/popular", CanonicalSlug: "model/popular-v1", HuggingFaceID: "org/popular", DailyTotalTokens: 10_000_000_000, ProviderCount: 20, FloorInputUSDPerMillion: 0.10, FloorOutputUSDPerMillion: 2, CapturedAt: now},
		{ModelID: "model/less-crowded", CanonicalSlug: "model/less-crowded-v1", HuggingFaceID: "org/less-crowded", DailyTotalTokens: 4_000_000_000, ProviderCount: 1, FloorInputUSDPerMillion: 0.10, FloorOutputUSDPerMillion: 2, CapturedAt: now},
	}
	profile := ServingProfile{
		ID: "popular-on-h100", ModelID: "model/popular", Runtime: "sglang", EvidenceLevel: EvidenceAnalogous,
		EvidenceRef: "evidence.json", AggregateOutputTokensPerSec: 900, InputTokensPerOutputToken: 2.5,
		FeatureCoverage: 1, AvailabilityFactor: 0.99, TargetContextLength: 1000, MaximumQualifiedContext: 1000, ObservedAt: now,
		Hardware: HardwareOffer{Provider: "provider", Region: "us", GPU: "H100", GPUCount: 1, HourlyCostUSD: 1.68, PriceSource: "provider", PriceObservedAt: now, PriceValidUntil: now.Add(time.Hour)},
	}
	report, err := Rank(now, markets, []ServingProfile{profile}, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if report.MarketShortlist[0].ModelID != "model/less-crowded" || report.MarketShortlist[0].Decision != DecisionInvestigate {
		t.Fatalf("unexpected opportunity rank: %+v", report.MarketShortlist)
	}
	if len(report.EconomicCandidates) != 1 || report.EconomicCandidates[0].Decision != DecisionQualify {
		t.Fatalf("analog evidence must request qualification, not launch: %+v", report.EconomicCandidates)
	}
}

func TestMeasuredEvidenceCanLaunchAndChargesIdleHours(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	market := MarketModel{ModelID: "model/a", CanonicalSlug: "model/a-v1", HuggingFaceID: "org/a", DailyTotalTokens: 100_000_000_000, ProviderCount: 5, FloorInputUSDPerMillion: 0.2, FloorOutputUSDPerMillion: 3, CapturedAt: now}
	profile := ServingProfile{
		ID: "a-exact", ModelID: "model/a", Runtime: "runtime", EvidenceLevel: EvidenceMeasured,
		EvidenceRef: "receipt.json", AggregateOutputTokensPerSec: 1200, InputTokensPerOutputToken: 2,
		FeatureCoverage: 1, AvailabilityFactor: 0.999, TargetContextLength: 1000, MaximumQualifiedContext: 1000, ObservedAt: now,
		Hardware: HardwareOffer{Provider: "provider", Region: "us", GPU: "H200", GPUCount: 1, HourlyCostUSD: 3, PriceSource: "api", PriceObservedAt: now, PriceValidUntil: now.Add(time.Hour)},
	}
	report, err := Rank(now, []MarketModel{market}, []ServingProfile{profile}, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	decision := report.EconomicCandidates[0]
	if decision.Decision != DecisionLaunch {
		t.Fatalf("expected launch, got %+v", decision)
	}
	if decision.ExpectedMonthlyGPUCostUSD != 2190 {
		t.Fatalf("dedicated cost must include idle hours: %.2f", decision.ExpectedMonthlyGPUCostUSD)
	}
}

func TestUnprofitableProfileIsRejected(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	market := MarketModel{ModelID: "model/a", HuggingFaceID: "org/a", DailyTotalTokens: 1_000_000, ProviderCount: 20, FloorInputUSDPerMillion: 0.01, FloorOutputUSDPerMillion: 0.02, CapturedAt: now}
	profile := ServingProfile{ID: "bad", ModelID: "model/a", Runtime: "runtime", EvidenceLevel: EvidenceMeasured, EvidenceRef: "evidence", AggregateOutputTokensPerSec: 10, InputTokensPerOutputToken: 2, FeatureCoverage: 1, AvailabilityFactor: 1, TargetContextLength: 1, MaximumQualifiedContext: 1, Hardware: HardwareOffer{Provider: "p", GPU: "gpu", GPUCount: 1, HourlyCostUSD: 10, PriceSource: "api", PriceObservedAt: now, PriceValidUntil: now.Add(time.Hour)}}
	report, err := Rank(now, []MarketModel{market}, []ServingProfile{profile}, DefaultPolicy())
	if err != nil {
		t.Fatal(err)
	}
	decision := report.EconomicCandidates[0]
	if decision.Decision != DecisionReject || !math.IsInf(decision.BreakEvenUtilization, 0) && decision.BreakEvenUtilization <= 1 {
		t.Fatalf("expected rejected economics: %+v", decision)
	}
}

func TestStalePriceFailsClosed(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	market := MarketModel{ModelID: "model/a", HuggingFaceID: "org/a", DailyTotalTokens: 1, ProviderCount: 1, FloorInputUSDPerMillion: 1, FloorOutputUSDPerMillion: 1}
	profile := ServingProfile{ID: "stale", ModelID: "model/a", Runtime: "runtime", EvidenceLevel: EvidenceModeled, EvidenceRef: "evidence", AggregateOutputTokensPerSec: 1, InputTokensPerOutputToken: 1, FeatureCoverage: 1, AvailabilityFactor: 1, TargetContextLength: 1, MaximumQualifiedContext: 1, Hardware: HardwareOffer{Provider: "p", GPU: "g", GPUCount: 1, HourlyCostUSD: 1, PriceObservedAt: now.Add(-25 * time.Hour)}}
	if _, err := Rank(now, []MarketModel{market}, []ServingProfile{profile}, DefaultPolicy()); err == nil {
		t.Fatal("expected stale price rejection")
	}
}
