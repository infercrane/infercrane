package optimizationreport

import (
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
)

// A setup held back because its answers were not shown to hold says what it
// gained and why that is not enough, in its row and in the warnings.
func TestASetupHeldBackOnAnswersSaysItsGainAndWhy(t *testing.T) {
	faster := &Comparison{Metric: sourceOutputTPS, ChangePercent: 62.4, Decision: DecisionImproved}
	reason := answersNotShownHeld(faster, sourceOutputTPS, domain.QualityEvidence{DistributionJSON: `{"task_servers":1}`})
	if reason != "faster by 62% (output throughput), but its answers were not shown to hold within 1 point on one server each" {
		t.Fatalf("reason = %q", reason)
	}
	if two := answersNotShownHeld(faster, sourceOutputTPS, domain.QualityEvidence{DistributionJSON: `{"task_servers":2}`}); !strings.HasSuffix(two, "on 2 servers each") {
		t.Fatalf("two servers: %q", two)
	}
	if plain := answersNotShownHeld(nil, sourceOutputTPS, domain.QualityEvidence{}); plain != "its answers were not shown to hold within 1 point on one server each" {
		t.Fatalf("no gain: %q", plain)
	}
	warned := warnings([]CandidateResult{{Label: "FP8 weights", FailureCode: optimizationcampaign.FailureQualityAnswersNotShownHeld, Reason: reason}})
	if len(warned) != 1 || warned[0] != "“FP8 weights” was held back: "+reason+"." {
		t.Fatalf("warnings = %v", warned)
	}
	if words := statusWords(optimizationcampaign.CandidateInconclusive, optimizationcampaign.FailureQualityAnswersNotShownHeld); !strings.Contains(words, "answers not shown to hold within 1 point") {
		t.Fatalf("status = %q", words)
	}
}
