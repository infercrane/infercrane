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

func TestEvaluateV2FailsClosedOnCompatibilitySyntheticAndCost(t *testing.T) {
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

func TestEvaluateV2WaitsWhenRequiredCostIsUnavailable(t *testing.T) {
	limit := 10.0
	policy := domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, MaxCostRegressionPercent: &limit}
	ttft := 100.0
	result := Evaluate(Input{Policy: policy, Active: domain.RevisionMetrics{ReadyReplicas: 1, Requests: 1, P95TTFTMS: &ttft}, Candidate: domain.RevisionMetrics{ReadyReplicas: 1, Requests: 1, P95TTFTMS: &ttft}})
	if result.Decision != "WAIT" || result.Reasons[0].Code != "cost_evidence_unavailable" {
		t.Fatalf("result=%+v", result)
	}
}
