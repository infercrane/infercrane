package continualoptimizer

import (
	"math"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func workload(source string, requests int) Workload {
	return Workload{
		Source: source, Digest: strings.Repeat("a", 64), RequestCount: requests,
		WindowStart: testNow.Add(-time.Hour), WindowEnd: testNow,
		RequestsPerSecond: 4, InputTokensMean: 4200, OutputTokensMean: 512,
		PeakConcurrency: 12, SharedPrefixRatio: .35, SessionReuseRatio: .20,
	}
}

func hypothesis(id, layer string, gain, cost float64) Hypothesis {
	return Hypothesis{ID: id, Fingerprint: strings.Repeat(id, 64/len(id)), Layer: layer, Description: id, ExpectedEndpointGain: gain, Confidence: .8, Risk: .1, EstimatedCostUSD: cost}
}

func TestPublicPriorCanScreenButNeverPromote(t *testing.T) {
	input := Input{Now: testNow, Policy: DefaultPolicy(), Current: workload(SourcePublicPrior, 150000), Hypotheses: []Hypothesis{hypothesis("a", "scheduler", .10, 20)}}
	decision, err := Evaluate(input)
	if err != nil || decision.Action != ActionScreenPublicPrior || decision.PromotionEligible || decision.AutomaticPromotion || len(decision.Selected) != 1 || decision.DecisionDigest == "" {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestCustomerReplayTriggersBoundedNovelExperiment(t *testing.T) {
	baseline := workload(SourceCustomerObserved, 1000)
	baseline.InputTokensMean = 500
	policy := DefaultPolicy()
	policy.DriftThreshold = .20
	input := Input{
		Now: testNow, Policy: policy, Baseline: &baseline,
		Current:       workload(SourceCustomerObserved, 1200),
		ActiveService: ServiceEvidence{RequestCount: 1200, Availability: .9999, ErrorRate: .001, ContributionMargin: .30},
		Hypotheses: []Hypothesis{
			hypothesis("a", "chunked-prefill", .12, 30),
			hypothesis("b", "kernel", .02, 10),
			hypothesis("c", "scheduler", .08, 120),
			hypothesis("d", "prefix-cache", .07, 20),
		},
		History: []Outcome{{Fingerprint: strings.Repeat("d", 64), Decision: "rejected", CompletedAt: testNow.Add(-24 * time.Hour)}},
	}
	decision, err := Evaluate(input)
	if err != nil || decision.Action != ActionStartExperiment || !decision.PromotionEligible || len(decision.Selected) != 1 || decision.Selected[0].ID != "a" || decision.MaximumCostUSD != 30 || decision.DriftScore < input.Policy.DriftThreshold {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestInsufficientCustomerEvidenceWaits(t *testing.T) {
	input := Input{Now: testNow, Policy: DefaultPolicy(), Current: workload(SourceCustomerObserved, 42)}
	decision, err := Evaluate(input)
	if err != nil || decision.Action != ActionWaitForEvidence || decision.PromotionEligible {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestKnownFailureAndProfilerlessKernelAreNotRepeated(t *testing.T) {
	input := Input{
		Now: testNow, Policy: DefaultPolicy(), Current: workload(SourcePublicPrior, 1000),
		Hypotheses: []Hypothesis{
			hypothesis("a", "runtime", .10, 10),
			func() Hypothesis { h := hypothesis("b", "kernel", .20, 10); h.RequiresProfiler = true; return h }(),
		},
		History: []Outcome{{Fingerprint: strings.Repeat("a", 64), Decision: "failed", CompletedAt: testNow.Add(-time.Hour)}},
	}
	decision, err := Evaluate(input)
	if err != nil || len(decision.Selected) != 0 {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestCanaryRollsBackOnReliabilityRegression(t *testing.T) {
	canary := ServiceEvidence{RequestCount: 1500, TTFTP95MS: 90, ErrorRate: .03, Availability: .9999, ContributionMargin: .50, QualifiedOutputTokensPerSecond: 200}
	input := Input{Now: testNow, Policy: DefaultPolicy(), Current: workload(SourceCustomerObserved, 2000), ActiveService: ServiceEvidence{RequestCount: 2000, TTFTP95MS: 100, ErrorRate: .001, Availability: .9999, ContributionMargin: .30, QualifiedOutputTokensPerSecond: 150}, CanaryService: &canary}
	decision, err := Evaluate(input)
	if err != nil || decision.Action != ActionRollbackCanary || decision.PromotionEligible {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestCanaryRecommendsButDoesNotAutomaticallyPromoteByDefault(t *testing.T) {
	active := ServiceEvidence{RequestCount: 2000, TTFTP95MS: 100, TPOTP95MS: 8, ErrorRate: .001, Availability: .9999, ContributionMargin: .25, QualifiedOutputTokensPerSecond: 150, ProductiveUtilization: .5, CostPerMillionOutputUSD: 1.5}
	canary := ServiceEvidence{RequestCount: 1500, TTFTP95MS: 80, TPOTP95MS: 7, ErrorRate: .001, Availability: .9999, ContributionMargin: .40, QualifiedOutputTokensPerSecond: 220, ProductiveUtilization: .65, CostPerMillionOutputUSD: 1.0}
	input := Input{Now: testNow, Policy: DefaultPolicy(), Current: workload(SourceCustomerObserved, 2000), ActiveService: active, CanaryService: &canary}
	decision, err := Evaluate(input)
	if err != nil || decision.Action != ActionRecommendPromotion || !decision.PromotionEligible || decision.AutomaticPromotion {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestCooldownAndActiveCampaignPreventExperimentStorms(t *testing.T) {
	baseline := workload(SourceCustomerObserved, 1000)
	baseline.OutputTokensMean = 8
	policy := DefaultPolicy()
	policy.DriftThreshold = .20
	last := testNow.Add(-time.Hour)
	input := Input{Now: testNow, Policy: policy, Baseline: &baseline, Current: workload(SourceCustomerObserved, 1000), LastExperimentAt: &last, Hypotheses: []Hypothesis{hypothesis("a", "decode", .10, 10)}}
	decision, err := Evaluate(input)
	if err != nil || decision.Action != ActionWaitForEvidence {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
	input.ActiveCampaigns = []Active{{CampaignID: "campaign", Phase: "measuring", Count: 1}}
	decision, err = Evaluate(input)
	if err != nil || decision.Action != ActionObserveExperiment {
		t.Fatalf("decision=%+v err=%v", decision, err)
	}
}

func TestWorkloadDriftIsSymmetricAndBounded(t *testing.T) {
	left, right := workload(SourceCustomerObserved, 1000), workload(SourceCustomerObserved, 1000)
	right.InputTokensMean *= 4
	right.PeakConcurrency *= 2
	a, b := WorkloadDrift(left, right), WorkloadDrift(right, left)
	if a <= 0 || a > 1 || a != b {
		t.Fatalf("drift left-right=%f right-left=%f", a, b)
	}
}

func TestValidationFailsClosed(t *testing.T) {
	input := Input{Now: testNow, Policy: DefaultPolicy(), Current: workload(SourceCustomerObserved, 1000), Hypotheses: []Hypothesis{hypothesis("a", "runtime", .1, 10)}}
	input.Hypotheses[0].Confidence = 2
	if _, err := Evaluate(input); err == nil {
		t.Fatal("invalid hypothesis confidence must fail closed")
	}
}

func TestServiceEvidenceValidationFailsClosed(t *testing.T) {
	input := Input{Now: testNow, Policy: DefaultPolicy(), Current: workload(SourceCustomerObserved, 1000), ActiveService: ServiceEvidence{Availability: math.NaN()}}
	if _, err := Evaluate(input); err == nil {
		t.Fatal("non-finite service evidence must fail closed")
	}
	input.ActiveService = ServiceEvidence{Availability: 1.1}
	if _, err := Evaluate(input); err == nil {
		t.Fatal("out-of-range service evidence must fail closed")
	}
}
