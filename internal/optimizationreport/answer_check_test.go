package optimizationreport

import (
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

func builtinAnswers(id, revision, reference string, score float64, distribution string) *domain.QualityEvidence {
	return &domain.QualityEvidence{ID: id, TenantID: "tenant-a", RevisionID: revision, Suite: "infercrane-answer-check", SuiteVersion: "v1", Evaluator: "infercrane", EvaluatorVersion: "v1", Source: domain.QualityEvidenceSourceInferCrane, ReferenceRevisionID: reference, Score: score, Passed: score >= 0.9, SampleCount: 64, DistributionJSON: distribution}
}

// answerFixture is a kept control with one candidate that lost, so the
// report's top-level quality says nothing about the candidate's answers.
func answerFixture(candidate armFixture) fixture {
	control := armFixture{id: "control", proposalID: "proposal-control", state: optimizationcampaign.CandidateQualified, rank: 1, engine: "vllm", gpu: "L40S", gpus: 1, maxReplicas: 1, tps: 1000, rps: 4, ttft: 400, quality: builtinAnswers("answers-control", "rev-control", "rev-control", 1, "")}
	candidate.proposalID, candidate.rank, candidate.engine, candidate.gpu, candidate.gpus, candidate.maxReplicas = "proposal-candidate", 2, "vllm", "L40S", 1, 1
	return fixture{intent: optimizationcampaign.IntentNewEndpoint, state: optimizationcampaign.CampaignQualified, objective: "throughput", profile: "throughput", arms: []armFixture{control, candidate}}
}

func TestEverySetupCarriesItsOwnAnswerCheck(t *testing.T) {
	t.Parallel()
	brokenTool := `{"schema":"infercrane.quality-distribution/v1","kind":"paired_scores","tool_calls":{"cases":8,"score":0.8,"broken":1,"reference_invalid":0,"candidate":{}}}`
	customerPassed := *signedQuality("signed-pass", "rev-candidate", "v1", 0.91, true)
	customerFailed := *signedQuality("signed-fail", "rev-candidate", "v1", 0.4, false)
	cases := []struct {
		name, failure, state, verdict, reason string
		quality                               *domain.QualityEvidence
	}{
		{"held", optimizationcampaign.FailureNoMeasurableDifference, optimizationcampaign.CandidateRejected, releaseguard.QualityPass, "matched the reference 98%", builtinAnswers("answers-candidate", "rev-candidate", "rev-control", 0.98, "")},
		{"watch band", optimizationcampaign.FailureNoMeasurableDifference, optimizationcampaign.CandidateRejected, releaseguard.QualityWatch, "watch band", builtinAnswers("answers-candidate", "rev-candidate", "rev-control", 0.93, "")},
		{"blocked", optimizationcampaign.FailureQualityRejected, optimizationcampaign.CandidateRejected, releaseguard.QualityBlock, "more than 10% different", builtinAnswers("answers-candidate", "rev-candidate", "rev-control", 0.8, "")},
		{"broken tool call", optimizationcampaign.FailureQualityRejected, optimizationcampaign.CandidateRejected, releaseguard.QualityBlock, "broke 1 of 8 tool calls", builtinAnswers("answers-candidate", "rev-candidate", "rev-control", 0.97, brokenTool)},
		{"customer eval passed", optimizationcampaign.FailureNoMeasurableDifference, optimizationcampaign.CandidateRejected, releaseguard.QualityPass, "signed eval gsm8k passed", &customerPassed},
		{"customer eval failed", optimizationcampaign.FailureQualityRejected, optimizationcampaign.CandidateRejected, releaseguard.QualityBlock, "signed eval gsm8k failed", &customerFailed},
		{"rejected without its record", optimizationcampaign.FailureQualityRejected, optimizationcampaign.CandidateRejected, releaseguard.QualityBlock, "blocked this setup", nil},
		{"no reference", optimizationcampaign.FailureQualityReferenceUnavailable, optimizationcampaign.CandidateFailed, releaseguard.QualityUnknown, "no serving reference", nil},
		{"never checked", "optimization_machine_lost_repeatedly", optimizationcampaign.CandidateFailed, releaseguard.QualityUnknown, "did not run", nil},
		{"still checking", "", optimizationcampaign.CandidateValidating, releaseguard.QualityUnknown, "still running", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := answerFixture(armFixture{id: "candidate", state: tc.state, failure: tc.failure, tps: 1010, rps: 4, ttft: 398, quality: tc.quality})
			report := Assemble(f.evidence())
			if report.Quality.Basis != BasisBaselineKept && report.Outcome.Kind == OutcomeBaselineKept {
				t.Fatalf("the top-level quality only describes the kept setup: %+v", report.Quality)
			}
			check := candidateByID(t, report, "candidate").AnswerCheck
			if check == nil || check.Verdict != tc.verdict || !strings.Contains(check.Reason, tc.reason) {
				t.Fatalf("answer check %+v, want %s mentioning %q", check, tc.verdict, tc.reason)
			}
			if tc.quality != nil && tc.quality.Source == domain.QualityEvidenceSourceInferCrane && (check.WatchThresholdPercent == nil || *check.WatchThresholdPercent != 5 || *check.BlockThresholdPercent != 10 || check.EvidenceID != tc.quality.ID) {
				t.Fatalf("built-in check carries its thresholds and evidence: %+v", check)
			}
			reference := candidateByID(t, report, "control").AnswerCheck
			if reference == nil || !reference.Reference || reference.Verdict != releaseguard.QualityPass {
				t.Fatalf("the control's answers are the reference: %+v", reference)
			}
		})
	}
	waiting := answerFixture(armFixture{id: "candidate", state: optimizationcampaign.CandidateMeasuring})
	if check := candidateByID(t, Assemble(waiting.evidence()), "candidate").AnswerCheck; check != nil {
		t.Fatalf("a setup not yet measured has no answer check: %+v", check)
	}
}

func TestACandidateCarriesItsFreshServerConfirmation(t *testing.T) {
	t.Parallel()
	first := interleaved()
	first.Confirmation = &optimizationcampaign.ConfirmationEvidence{
		Control:        first.Control,
		Comparison:     map[string]optimizationcampaign.MetricComparison{"output_token_throughput": {HigherIsBetter: true, ControlMean: 1000, CandidateMean: 1260, Delta: 0.26, NoiseFloor: 0.04, Decision: optimizationcampaign.DecisionImproved}},
		FreshCandidate: true, FreshControl: true,
	}
	f := answerFixture(armFixture{id: "candidate", state: optimizationcampaign.CandidateQualified, tps: 1300, rps: 5.2, ttft: 380, measured: first})
	f.arms[0].state = optimizationcampaign.CandidateRejected
	report := Assemble(f.evidence())
	candidate := candidateByID(t, report, "candidate")
	if candidate.VersusBaseline == nil || candidate.Confirmation == nil {
		t.Fatalf("first window %+v, confirmation %+v", candidate.VersusBaseline, candidate.Confirmation)
	}
	if !near(&candidate.Confirmation.ChangePercent, 26) || !near(&candidate.Confirmation.NoiseFloorPercent, 4) || candidate.Confirmation.Decision != optimizationcampaign.DecisionImproved {
		t.Fatalf("confirmation %+v", candidate.Confirmation)
	}
	invalid := interleaved()
	invalid.Confirmation = &optimizationcampaign.ConfirmationEvidence{Control: optimizationcampaign.ControlEvidence{Role: "drifted"}, Comparison: first.Confirmation.Comparison}
	f.arms[1].measured = invalid
	if got := candidateByID(t, Assemble(f.evidence()), "candidate").Confirmation; got != nil {
		t.Fatalf("an invalid second window is not a confirmation: %+v", got)
	}
}
