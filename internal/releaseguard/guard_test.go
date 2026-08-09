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
