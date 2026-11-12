package releaseguard

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

func qualityEval(suite string, reference, candidate float64) domain.QualityEvalEvidence {
	return domain.QualityEvalEvidence{Suite: suite, SuiteVersion: "v1", Evaluator: "customer-eval", EvaluatorVersion: "1.0.0", Candidate: domain.QualityEvalSide{EvidenceID: suite + "-candidate", Score: candidate, Passed: true, SampleCount: 200}, Reference: &domain.QualityEvalSide{EvidenceID: suite + "-reference", Score: reference, Passed: true, SampleCount: 200}}
}

func pairedEval(reference, candidate []float64) domain.QualityEvalEvidence {
	eval := qualityEval("paired", mean(reference), mean(candidate))
	eval.Reference.SampleCount, eval.Reference.PairingDigest, eval.Reference.Scores = len(reference), "sha256:pair", reference
	eval.Candidate.SampleCount, eval.Candidate.PairingDigest, eval.Candidate.Scores = len(candidate), "sha256:pair", candidate
	return eval
}

func watchPolicy(watch, limit float64) domain.ReleaseGuardPolicy {
	return domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, RequireQualityEvidence: true, MaxQualityRegressionPercent: &limit, QualityWatchRegressionPercent: &watch}
}

func gateReadyMetrics() domain.RevisionMetrics {
	ttft := 100.0
	return domain.RevisionMetrics{ReadyReplicas: 1, Requests: 10, P95TTFTMS: &ttft}
}

func TestCompareQualityClassifiesEachEvalAgainstTheWatchBand(t *testing.T) {
	policy := watchPolicy(1, 3)
	unknown := qualityEval("unreferenced", 0, .90)
	unknown.Reference = nil
	report := CompareQuality(policy, []domain.QualityEvalEvidence{
		qualityEval("steady", .90, .895),
		qualityEval("slipping", .90, .885),
		qualityEval("broken", .90, .86),
		unknown,
	})
	want := []struct {
		verdict, code string
		limit         *float64
	}{
		{QualityPass, "quality_within_policy", nil},
		{QualityWatch, "quality_watch", policy.QualityWatchRegressionPercent},
		{QualityBlock, "quality_score_regression", policy.MaxQualityRegressionPercent},
		{QualityUnknown, "quality_evidence_not_comparable", nil},
	}
	if report.Verdict != QualityBlock || len(report.Evals) != len(want) || report.ComparisonMode != "threshold" || *report.WatchRegressionPercent != 1 || *report.MaxRegressionPercent != 3 {
		t.Fatalf("report=%+v", report)
	}
	for index, expected := range want {
		eval := report.Evals[index]
		if eval.Verdict != expected.verdict || eval.Code != expected.code || eval.Message == "" || !reflect.DeepEqual(eval.Limit, expected.limit) {
			t.Fatalf("eval %d=%+v want %+v", index, eval, expected)
		}
	}
	slipping := report.Evals[1]
	if slipping.ReferenceScore == nil || *slipping.ReferenceScore != .90 || slipping.Delta == nil || math.Abs(*slipping.Delta+.015) > 1e-12 || slipping.RegressionPercent == nil || math.Abs(*slipping.RegressionPercent-1.6667) > 1e-3 {
		t.Fatalf("watch eval delta=%+v", slipping)
	}
	if slipping.FollowUp == "" || !strings.Contains(slipping.FollowUp, "slipping@v1") || slipping.ReferenceEvidenceID != "slipping-reference" || slipping.CandidateEvidenceID != "slipping-candidate" {
		t.Fatalf("watch eval follow-up=%+v", slipping)
	}
	if report.Evals[0].FollowUp != "" || report.Evals[2].FollowUp != "" {
		t.Fatalf("only WATCH records a follow-up: %+v", report.Evals)
	}
	if unknown := report.Evals[3]; unknown.ReferenceScore != nil || unknown.Delta != nil || unknown.RegressionPercent != nil {
		t.Fatalf("unknown eval invented a reference: %+v", unknown)
	}
	if again := CompareQuality(policy, []domain.QualityEvalEvidence{qualityEval("steady", .90, .895), qualityEval("slipping", .90, .885), qualityEval("broken", .90, .86), unknown}); !reflect.DeepEqual(report, again) {
		t.Fatalf("comparison is not deterministic: %+v != %+v", report, again)
	}
}

func TestCompareQualityBandEdgesAreInclusiveOfTheLowerVerdict(t *testing.T) {
	// 1.0 to 0.75 and 1.0 to 0.5 are exact 25% and 50% drops in binary.
	policy := watchPolicy(25, 50)
	report := CompareQuality(policy, []domain.QualityEvalEvidence{qualityEval("at-watch", 1, .75), qualityEval("at-limit", 1, .5), qualityEval("past-limit", 1, .49)})
	if report.Evals[0].Verdict != QualityPass || report.Evals[1].Verdict != QualityWatch || report.Evals[2].Verdict != QualityBlock {
		t.Fatalf("edges=%+v", report.Evals)
	}
}

func TestCompareQualityPerModelVerdictIsTheMostSevereEval(t *testing.T) {
	policy := watchPolicy(1, 3)
	unknown := qualityEval("unknown", 0, .9)
	unknown.Reference = nil
	pass, watch, block := qualityEval("pass", .9, .9), qualityEval("watch", .9, .885), qualityEval("block", .9, .8)
	for _, test := range []struct {
		name  string
		evals []domain.QualityEvalEvidence
		want  string
	}{
		{"no evidence", nil, QualityUnknown},
		{"all pass", []domain.QualityEvalEvidence{pass, pass}, QualityPass},
		{"watch over pass", []domain.QualityEvalEvidence{pass, watch}, QualityWatch},
		{"unknown over watch", []domain.QualityEvalEvidence{watch, unknown}, QualityUnknown},
		{"block over unknown", []domain.QualityEvalEvidence{unknown, block, watch}, QualityBlock},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := CompareQuality(policy, test.evals).Verdict; got != test.want {
				t.Fatalf("verdict=%s want %s", got, test.want)
			}
		})
	}
}

func TestCompareQualityBlocksFailedSuitesAndLowScoresBeforeRegression(t *testing.T) {
	minimum := .85
	policy := watchPolicy(1, 3)
	policy.MinimumQualityScore = &minimum
	failed := qualityEval("failed", .9, .9)
	failed.Candidate.Passed = false
	report := CompareQuality(policy, []domain.QualityEvalEvidence{failed, qualityEval("low", .86, .84)})
	if report.Evals[0].Code != "quality_evaluation_failed" || report.Evals[0].Verdict != QualityBlock {
		t.Fatalf("failed suite=%+v", report.Evals[0])
	}
	if low := report.Evals[1]; low.Code != "quality_score_below_minimum" || low.Verdict != QualityBlock || low.Limit == nil || *low.Limit != minimum {
		t.Fatalf("low score=%+v", low)
	}
}

func TestCompareQualityCannotRegressFromAZeroReference(t *testing.T) {
	report := CompareQuality(watchPolicy(1, 3), []domain.QualityEvalEvidence{qualityEval("zero", 0, 0)})
	if eval := report.Evals[0]; eval.Verdict != QualityPass || eval.RegressionPercent != nil || eval.Delta == nil || *eval.Delta != 0 {
		t.Fatalf("zero reference=%+v", eval)
	}
}

func TestEvaluateWatchKeepsAcceptAndRecordsTheFollowUp(t *testing.T) {
	policy := watchPolicy(1, 3)
	metrics := gateReadyMetrics()
	result := Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{qualityEval("steady", .90, .90), qualityEval("slipping", .90, .885)}})
	if result.Decision != "ACCEPT" || len(result.Reasons) != 2 || result.Quality == nil || result.Quality.Verdict != QualityWatch {
		t.Fatalf("result=%+v", result)
	}
	watch, accepted := result.Reasons[0], result.Reasons[1]
	if watch.Code != "quality_watch" || watch.Eval != "slipping@v1" || watch.Metric != "quality_score" || watch.FollowUp == "" || !strings.Contains(watch.Message, "Follow-up: ") {
		t.Fatalf("watch reason=%+v", watch)
	}
	if watch.Active == nil || *watch.Active != .90 || watch.Candidate == nil || *watch.Candidate != .885 || watch.Limit == nil || *watch.Limit != 1 {
		t.Fatalf("watch reason values=%+v", watch)
	}
	if accepted.Code != "within_policy" {
		t.Fatalf("accept reason=%+v", accepted)
	}
}

func TestEvaluatePassIsUnchangedByTheWatchBand(t *testing.T) {
	metrics := gateReadyMetrics()
	result := Evaluate(Input{Policy: watchPolicy(1, 3), Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{qualityEval("steady", .90, .899), qualityEval("improved", .80, .85)}})
	if result.Decision != "ACCEPT" || len(result.Reasons) != 1 || result.Reasons[0].Code != "within_policy" || result.Quality.Verdict != QualityPass {
		t.Fatalf("result=%+v", result)
	}
}

func TestEvaluateBlockRejectsEvenWhenAnotherEvalIsWatch(t *testing.T) {
	metrics := gateReadyMetrics()
	result := Evaluate(Input{Policy: watchPolicy(1, 3), Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{qualityEval("slipping", .90, .885), qualityEval("broken", .90, .80)}})
	if result.Decision != "REJECT" || len(result.Reasons) != 1 || result.Reasons[0].Code != "quality_score_regression" || result.Reasons[0].Eval != "broken@v1" || result.Quality.Verdict != QualityBlock {
		t.Fatalf("result=%+v", result)
	}
}

func TestEvaluateWatchDoesNotOverrideOtherWaits(t *testing.T) {
	metrics := gateReadyMetrics()
	candidate := metrics
	candidate.Requests = 0
	result := Evaluate(Input{Policy: watchPolicy(1, 3), Active: metrics, Candidate: candidate, QualityEvals: []domain.QualityEvalEvidence{qualityEval("slipping", .90, .885)}})
	if result.Decision != "WAIT" || result.Reasons[0].Code != "insufficient_requests" || result.Quality == nil || result.Quality.Verdict != QualityWatch {
		t.Fatalf("result=%+v", result)
	}
}

func TestEvaluateMissingQualityEvidenceStaysFailClosed(t *testing.T) {
	limit := 3.0
	metrics := gateReadyMetrics()
	unreferenced := qualityEval("new-suite", 0, .99)
	unreferenced.Reference = nil
	for _, test := range []struct {
		name   string
		policy domain.ReleaseGuardPolicy
		evals  []domain.QualityEvalEvidence
		code   string
	}{
		{"required without evidence", watchPolicy(1, 3), nil, "quality_evidence_unavailable"},
		{"regression limit without evidence", domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, MaxQualityRegressionPercent: &limit}, nil, "quality_evidence_unavailable"},
		{"one eval without reference", watchPolicy(1, 3), []domain.QualityEvalEvidence{qualityEval("steady", .9, .9), unreferenced}, "quality_evidence_not_comparable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := Evaluate(Input{Policy: test.policy, Active: metrics, Candidate: metrics, QualityEvals: test.evals})
			if result.Decision != "WAIT" || result.Reasons[0].Code != test.code || result.Quality == nil || result.Quality.Verdict != QualityUnknown {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func TestEvaluateIgnoresQualityWhenPolicyDoesNotGateIt(t *testing.T) {
	metrics := gateReadyMetrics()
	failed := qualityEval("failed", .9, .1)
	failed.Candidate.Passed = false
	for _, policy := range []domain.ReleaseGuardPolicy{{Enabled: true, MinimumRequests: 1}, {Enabled: false, RequireQualityEvidence: true}} {
		result := Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{failed}})
		if result.Decision != "ACCEPT" || result.Quality != nil || QualityGated(policy) {
			t.Fatalf("policy=%+v result=%+v", policy, result)
		}
	}
}

func TestEvaluateBootstrapWatchUsesTheObservedPairedRegression(t *testing.T) {
	limit, watch := 5.0, 1.0
	policy := domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, RequireQualityEvidence: true, MaxQualityRegressionPercent: &limit, QualityWatchRegressionPercent: &watch, QualityComparisonMode: "bootstrap", QualityBootstrapAlpha: .05, QualityBootstrapMinSamples: 10, QualityBootstrapSeed: 42}
	reference := []float64{.90, .88, .92, .86, .91, .89, .93, .87, .90, .92}
	candidate := make([]float64, len(reference))
	for index, score := range reference {
		candidate[index] = score - .018
	}
	metrics := gateReadyMetrics()
	result := Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{pairedEval(reference, candidate)}})
	if result.Decision != "ACCEPT" || result.Reasons[0].Code != "quality_watch" || result.Reasons[0].Metric != "quality_distribution" || result.Reasons[0].Bootstrap == nil || result.Reasons[0].Bootstrap.Status != "accept" {
		t.Fatalf("bootstrap watch result=%+v", result)
	}
	watch = 3
	result = Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{pairedEval(reference, candidate)}})
	if result.Decision != "ACCEPT" || len(result.Reasons) != 1 || result.Quality.Verdict != QualityPass {
		t.Fatalf("bootstrap pass result=%+v", result)
	}
	mismatched := pairedEval(reference, candidate)
	mismatched.Candidate.PairingDigest = "sha256:other"
	result = Evaluate(Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: []domain.QualityEvalEvidence{mismatched}})
	if result.Decision != "WAIT" || result.Reasons[0].Code != "quality_bootstrap_evidence_unavailable" || result.Quality.Evals[0].Verdict != QualityUnknown {
		t.Fatalf("mismatched pairing result=%+v", result)
	}
}
