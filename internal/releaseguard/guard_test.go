package releaseguard

import (
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

func number(value float64) *float64 { return &value }

func TestEvaluateIsDeterministicAndConservative(t *testing.T) {
	policy := domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 20, MaxTTFTRegressionPercent: 15, MaxLatencyRegressionPercent: 15, MaxErrorRateIncrease: .01, MaxOutputThroughputDropPercent: 20}
	tests := []struct {
		name      string
		active    domain.RevisionMetrics
		candidate domain.RevisionMetrics
		decision  string
		code      string
	}{
		{"not ready", domain.RevisionMetrics{}, domain.RevisionMetrics{}, "REJECT", "candidate_not_ready"},
		{"insufficient evidence", domain.RevisionMetrics{Requests: 20}, domain.RevisionMetrics{ReadyReplicas: 1, Requests: 19}, "WAIT", "insufficient_requests"},
		{"ttft regression", domain.RevisionMetrics{ReadyReplicas: 1, Requests: 20, P95TTFTMS: number(100), P95LatencyMS: number(200)}, domain.RevisionMetrics{ReadyReplicas: 1, Requests: 20, P95TTFTMS: number(116), P95LatencyMS: number(200)}, "REJECT", "ttft_regression"},
		{"within policy", domain.RevisionMetrics{ReadyReplicas: 1, Requests: 20, P95TTFTMS: number(100), P95LatencyMS: number(200)}, domain.RevisionMetrics{ReadyReplicas: 1, Requests: 20, P95TTFTMS: number(110), P95LatencyMS: number(210)}, "ACCEPT", "within_policy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := Evaluate(Input{Policy: policy, Active: test.active, Candidate: test.candidate})
			if result.Decision != test.decision || len(result.Reasons) == 0 || result.Reasons[0].Code != test.code {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestEvaluateWaitsForMinimumRequestsBeforeMeasurementRejects(t *testing.T) {
	policy := domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 20, MaxTTFTRegressionPercent: 15, MaxLatencyRegressionPercent: 15, MaxErrorRateIncrease: .01, MaxOutputThroughputDropPercent: 15}
	active := domain.RevisionMetrics{ReadyReplicas: 2, Requests: 500, P95TTFTMS: number(100), P95LatencyMS: number(900), OutputTokensPerSecond: number(50)}
	// noisy is a canary's first few requests: one failure and one slow
	// request make every measurement look like a regression.
	noisy := func(requests int) domain.RevisionMetrics {
		return domain.RevisionMetrics{ReadyReplicas: 1, Requests: requests, ErrorRate: 1.0 / 3, P95TTFTMS: number(300), P95LatencyMS: number(2000), OutputTokensPerSecond: number(20)}
	}

	t.Run("below the minimum the canary waits", func(t *testing.T) {
		result := Evaluate(Input{Policy: policy, Active: active, Candidate: noisy(3)})
		if result.Decision != "WAIT" || len(result.Reasons) != 1 || result.Reasons[0].Code != "insufficient_requests" {
			t.Fatalf("result=%+v", result)
		}
	})

	t.Run("at the minimum the same measurements reject", func(t *testing.T) {
		result := Evaluate(Input{Policy: policy, Active: active, Candidate: noisy(20)})
		codes := map[string]bool{}
		for _, reason := range result.Reasons {
			codes[reason.Code] = true
		}
		if result.Decision != "REJECT" || !codes["error_rate_regression"] || !codes["ttft_regression"] || !codes["latency_regression"] || !codes["output_throughput_regression"] {
			t.Fatalf("result=%+v", result)
		}
	})

	t.Run("an active revision below the minimum also holds measurement", func(t *testing.T) {
		thin := active
		thin.Requests = 5
		if result := Evaluate(Input{Policy: policy, Active: thin, Candidate: noisy(20)}); result.Decision != "WAIT" || result.Reasons[0].Code != "insufficient_requests" {
			t.Fatalf("result=%+v", result)
		}
	})

	// Evidence that does not depend on how many requests were served still
	// rejects on the first evaluation.
	t.Run("a candidate with no ready replica rejects at once", func(t *testing.T) {
		candidate := noisy(3)
		candidate.ReadyReplicas = 0
		if result := Evaluate(Input{Policy: policy, Active: active, Candidate: candidate}); result.Decision != "REJECT" || len(result.Reasons) != 1 || result.Reasons[0].Code != "candidate_not_ready" {
			t.Fatalf("result=%+v", result)
		}
	})

	t.Run("incompatible evidence rejects at once", func(t *testing.T) {
		strict := policy
		strict.RequireCompatibilityEvidence = true
		compatible := false
		candidate := noisy(3)
		candidate.Compatible = &compatible
		if result := Evaluate(Input{Policy: strict, Active: active, Candidate: candidate}); result.Decision != "REJECT" || len(result.Reasons) != 1 || result.Reasons[0].Code != "compatibility_mismatch" {
			t.Fatalf("result=%+v", result)
		}
	})

	t.Run("a sourced cost regression rejects at once", func(t *testing.T) {
		costed := policy
		costed.MaxCostRegressionPercent = number(10)
		activeCost, candidate := active, noisy(3)
		activeCost.SourcedHourlyCost, candidate.SourcedHourlyCost = number(2), number(3)
		if result := Evaluate(Input{Policy: costed, Active: activeCost, Candidate: candidate}); result.Decision != "REJECT" || len(result.Reasons) != 1 || result.Reasons[0].Code != "cost_regression" {
			t.Fatalf("result=%+v", result)
		}
	})

	t.Run("a signed quality block rejects at once", func(t *testing.T) {
		gated := policy
		gated.RequireQualityEvidence, gated.MaxQualityRegressionPercent = true, number(3)
		result := Evaluate(Input{Policy: gated, Active: active, Candidate: noisy(3), QualityEvals: []domain.QualityEvalEvidence{qualityEval("answers", .90, .80)}})
		if result.Decision != "REJECT" || len(result.Reasons) != 1 || result.Reasons[0].Code != "quality_score_regression" {
			t.Fatalf("result=%+v", result)
		}
	})
}

func TestEvaluateFailsClosedOnCompatibilitySyntheticAndCost(t *testing.T) {
	compatible := true
	costLimit := 10.0
	policy := domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, RequireCompatibilityEvidence: true, RequireSyntheticEvidence: true, MaxCostRegressionPercent: &costLimit}
	baselineCost, candidateCost := 1.0, 1.2
	base := domain.RevisionMetrics{ReadyReplicas: 1, Requests: 10, P95TTFTMS: number(100), Compatible: &compatible, SyntheticValidation: true, SourcedHourlyCost: &baselineCost}
	candidate := domain.RevisionMetrics{ReadyReplicas: 1, Requests: 10, P95TTFTMS: number(100), Compatible: &compatible, SyntheticValidation: true, SourcedHourlyCost: &candidateCost}
	result := Evaluate(Input{Policy: policy, Active: base, Candidate: candidate})
	if result.Decision != "REJECT" || result.Reasons[0].Code != "cost_regression" {
		t.Fatalf("result=%+v", result)
	}
	candidate.SourcedHourlyCost = &baselineCost
	candidate.SyntheticValidation = false
	result = Evaluate(Input{Policy: policy, Active: base, Candidate: candidate})
	if result.Decision != "WAIT" || result.Reasons[0].Code != "synthetic_validation_unproven" {
		t.Fatalf("result=%+v", result)
	}
	candidate.SyntheticValidation = true
	compatible = false
	result = Evaluate(Input{Policy: policy, Active: base, Candidate: candidate})
	if result.Decision != "REJECT" || result.Reasons[0].Code != "compatibility_mismatch" {
		t.Fatalf("result=%+v", result)
	}
	candidate.Compatible = nil
	result = Evaluate(Input{Policy: policy, Active: base, Candidate: candidate})
	if result.Decision != "WAIT" || result.Reasons[0].Code != "compatibility_unproven" {
		t.Fatalf("result=%+v", result)
	}
}

func TestEvaluateWaitsWhenRequiredCostIsUnavailable(t *testing.T) {
	limit := 10.0
	policy := domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, MaxCostRegressionPercent: &limit}
	ttft := 100.0
	result := Evaluate(Input{Policy: policy, Active: domain.RevisionMetrics{ReadyReplicas: 1, Requests: 1, P95TTFTMS: &ttft}, Candidate: domain.RevisionMetrics{ReadyReplicas: 1, Requests: 1, P95TTFTMS: &ttft}})
	if result.Decision != "WAIT" || result.Reasons[0].Code != "cost_evidence_unavailable" {
		t.Fatalf("result=%+v", result)
	}
}

func TestEvaluateRequiresComparableSignedSemanticQuality(t *testing.T) {
	minimum, maxRegression := .8, 5.0
	policy := domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, RequireQualityEvidence: true, MinimumQualityScore: &minimum, MaxQualityRegressionPercent: &maxRegression}
	ttft := 100.0
	metrics := domain.RevisionMetrics{ReadyReplicas: 1, Requests: 1, P95TTFTMS: &ttft}
	eval := qualityEval("support-answers", .90, .84)

	result := Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics})
	if result.Decision != "WAIT" || result.Reasons[0].Code != "quality_evidence_unavailable" || result.Quality == nil || result.Quality.Verdict != QualityUnknown {
		t.Fatalf("missing evidence result=%+v", result)
	}

	result = Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{eval}})
	if result.Decision != "REJECT" || result.Reasons[0].Code != "quality_score_regression" {
		t.Fatalf("regression result=%+v", result)
	}

	eval.Candidate.Score = .86
	result = Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{eval}})
	if result.Decision != "ACCEPT" || result.Reasons[0].Code != "within_policy" {
		t.Fatalf("within policy result=%+v", result)
	}

	incomparable := eval
	incomparable.Reference = nil
	result = Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{incomparable}})
	if result.Decision != "WAIT" || result.Reasons[0].Code != "quality_evidence_not_comparable" {
		t.Fatalf("incomparable result=%+v", result)
	}

	eval.Candidate.Passed = false
	result = Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{eval}})
	if result.Decision != "REJECT" || result.Reasons[0].Code != "quality_evaluation_failed" {
		t.Fatalf("failed suite result=%+v", result)
	}
}

func TestEvaluateMinimumQualityThresholdFailsClosedWithoutEvidence(t *testing.T) {
	minimum, ttft := .8, 100.0
	policy := domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, MinimumQualityScore: &minimum}
	metrics := domain.RevisionMetrics{ReadyReplicas: 1, Requests: 1, P95TTFTMS: &ttft}
	result := Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics})
	if result.Decision != "WAIT" || len(result.Reasons) != 1 || result.Reasons[0].Code != "quality_evidence_unavailable" {
		t.Fatalf("missing threshold evidence result=%+v", result)
	}
}

func TestEvaluateBootstrapQualityPersistsStatisticalDecisionEvidence(t *testing.T) {
	limit, ttft := 5.0, 100.0
	policy := domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, RequireQualityEvidence: true, MaxQualityRegressionPercent: &limit, QualityComparisonMode: "bootstrap", QualityBootstrapAlpha: .05, QualityBootstrapMinSamples: 10, QualityBootstrapSeed: 42}
	metrics := domain.RevisionMetrics{ReadyReplicas: 1, Requests: 1, P95TTFTMS: &ttft}
	eval := pairedEval([]float64{.90, .88, .92, .86, .91, .89, .93, .87, .90, .92}, []float64{.70, .68, .72, .66, .71, .69, .73, .67, .70, .72})
	result := Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{eval}})
	if result.Decision != "REJECT" || len(result.Reasons) != 1 || result.Reasons[0].Code != "quality_bootstrap_regression" || result.Reasons[0].Bootstrap == nil || result.Reasons[0].Bootstrap.Seed != 42 || result.Reasons[0].Bootstrap.ActiveSamples != 10 || result.Reasons[0].Bootstrap.IntervalLowerPercent == nil {
		t.Fatalf("result=%+v", result)
	}
	eval.Candidate.Scores = eval.Candidate.Scores[:9]
	result = Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{eval}})
	if result.Decision != "WAIT" || result.Reasons[len(result.Reasons)-1].Code != "quality_bootstrap_insufficient" || result.Reasons[len(result.Reasons)-1].Bootstrap == nil {
		t.Fatalf("insufficient result=%+v", result)
	}
}
