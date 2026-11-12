package store

import (
	"encoding/json"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

func qualityRow(id, revision, suite, suiteVersion string, score float64) domain.QualityEvidence {
	return domain.QualityEvidence{ID: id, RevisionID: revision, Suite: suite, SuiteVersion: suiteVersion, Evaluator: "customer-eval", EvaluatorVersion: "1.0.0", Score: score, Passed: true, SampleCount: 2, DistributionJSON: `{}`}
}

func TestQualityEvalsPairTheNewestCandidateEvidencePerEval(t *testing.T) {
	// Rows arrive newest first, as QualityEvidenceForDeployment returns them.
	rows := []domain.QualityEvidence{
		qualityRow("candidate-support-v2", "candidate", "support", "v2", .88),
		qualityRow("candidate-math", "candidate", "math", "v1", .70),
		qualityRow("active-support-v2", "active", "support", "v2", .90),
		qualityRow("candidate-support-v1", "candidate", "support", "v1", .80),
		qualityRow("active-support-v1", "active", "support", "v1", .95),
		qualityRow("other-revision", "retired", "math", "v1", .99),
	}
	rows[0].DistributionJSON = `{"pairing_digest":"sha256:pair","scores":[0.9,0.86]}`
	rows[2].DistributionJSON = `{"pairing_digest":"sha256:pair","scores":[0.91,0.89]}`
	evals := qualityEvals(rows, "active", "candidate")
	if len(evals) != 2 {
		t.Fatalf("evals=%+v", evals)
	}
	support, math := evals[0], evals[1]
	if support.Suite != "support" || support.SuiteVersion != "v2" || support.Candidate.EvidenceID != "candidate-support-v2" || support.Reference == nil || support.Reference.EvidenceID != "active-support-v2" {
		t.Fatalf("newest support eval must pair v2 with v2: %+v", support)
	}
	if support.Candidate.PairingDigest != "sha256:pair" || len(support.Candidate.Scores) != 2 || len(support.Reference.Scores) != 2 {
		t.Fatalf("paired distribution was not carried: %+v", support)
	}
	if math.Suite != "math" || math.Reference != nil {
		t.Fatalf("an eval without active evidence must keep a nil reference: %+v", math)
	}
	if got := qualityEvals(rows, "active", ""); len(got) != 0 {
		t.Fatalf("no candidate must yield no evals: %+v", got)
	}
	if got := qualityEvals(rows, "", "candidate"); len(got) != 2 || got[0].Reference != nil || got[1].Reference != nil {
		t.Fatalf("no reference revision must yield unknown references: %+v", got)
	}
}

func TestApplyQualityEvidenceSummarizesTheNewestEval(t *testing.T) {
	var active, candidate domain.RevisionMetrics
	evals := qualityEvals([]domain.QualityEvidence{qualityRow("c", "candidate", "support", "v1", .88), qualityRow("a", "active", "support", "v1", .9)}, "active", "candidate")
	applyQualityEvidence(&active, &candidate, evals)
	if candidate.QualityScore == nil || *candidate.QualityScore != .88 || candidate.QualityComparable == nil || !*candidate.QualityComparable || candidate.QualitySuite != "support@v1" || candidate.QualityEvidenceID != "c" {
		t.Fatalf("candidate=%+v", candidate)
	}
	if active.QualityScore == nil || *active.QualityScore != .9 || active.QualityEvidenceID != "a" {
		t.Fatalf("active=%+v", active)
	}
	active, candidate = domain.RevisionMetrics{}, domain.RevisionMetrics{}
	applyQualityEvidence(&active, &candidate, qualityEvals([]domain.QualityEvidence{qualityRow("c", "candidate", "support", "v1", .88)}, "active", "candidate"))
	if candidate.QualityComparable == nil || *candidate.QualityComparable || active.QualityScore != nil {
		t.Fatalf("incomparable summary active=%+v candidate=%+v", active, candidate)
	}
}

func TestQualityWatchFollowUpIsRecordedOnlyForAcceptedWatch(t *testing.T) {
	watch, limit := 1.0, 3.0
	policy := domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 1, RequireQualityEvidence: true, MaxQualityRegressionPercent: &limit, QualityWatchRegressionPercent: &watch}
	ttft := 100.0
	metrics := domain.RevisionMetrics{ReadyReplicas: 1, Requests: 1, P95TTFTMS: &ttft}
	evals := qualityEvals([]domain.QualityEvidence{qualityRow("c", "candidate", "support", "v1", .885), qualityRow("a", "active", "support", "v1", .9)}, "active", "candidate")
	result := releaseguard.Evaluate(releaseguard.Input{Policy: policy, Active: metrics, Candidate: metrics, QualityEvals: evals})
	result.Quality.Candidate.Model = "model-v2"
	evaluation := domain.ReleaseGuardEvaluation{ID: "evaluation-1", ActiveRevisionID: "active", CandidateRevisionID: "candidate", Decision: result.Decision}
	payload, ok := qualityWatchFollowUp(evaluation, result)
	if !ok {
		t.Fatalf("accepted WATCH must record a follow-up: %+v", result)
	}
	var decoded struct {
		EvaluationID   string `json:"evaluation_id"`
		CandidateModel string `json:"candidate_model"`
		Evals          []struct {
			Eval     string `json:"eval"`
			FollowUp string `json:"follow_up"`
		} `json:"evals"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil || decoded.EvaluationID != "evaluation-1" || decoded.CandidateModel != "model-v2" || len(decoded.Evals) != 1 || decoded.Evals[0].Eval != "support@v1" || decoded.Evals[0].FollowUp == "" {
		t.Fatalf("payload=%s err=%v", payload, err)
	}
	evaluation.Decision = "WAIT"
	if _, ok = qualityWatchFollowUp(evaluation, result); ok {
		t.Fatal("a waiting evaluation must not record a follow-up")
	}
	evaluation.Decision = "ACCEPT"
	result.Quality.Verdict = releaseguard.QualityPass
	if _, ok = qualityWatchFollowUp(evaluation, result); ok {
		t.Fatal("PASS must not record a follow-up")
	}
}
