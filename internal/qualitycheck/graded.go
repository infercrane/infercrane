package qualitycheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

const (
	// GradedEvaluator names InferCrane's graded check in evidence: both the
	// closed API and the candidate answer a graded eval set and each answer
	// is scored against the expected answers.
	GradedEvaluator        = "infercrane-graded"
	GradedEvaluatorVersion = "1"
	// The migration verdict's paired bootstrap. Fixed so the same scores
	// always give the same verdict.
	MigrationBootstrapAlpha      = 0.05
	MigrationBootstrapMinSamples = 30
	MigrationBootstrapSeed       = 20261119
)

// GradedComparison is the in-memory result of one candidate and a closed API
// answering the same graded eval set. Answers are discarded; only scores and
// digests remain.
type GradedComparison struct {
	Set                 EvalSet
	ReferenceID         string
	CandidateRevisionID string
	ReferenceScores     []float64
	CandidateScores     []float64
	ResultDigest        string
	EvaluatedAt         time.Time
}

// CompareGraded asks the reference (the closed API) and the candidate every
// prompt of a graded set and scores each answer against the expected
// answers. A failed request on either side stops the check: a missing answer
// must never be scored as a wrong one, or a candidate could win when the
// closed API is merely rate limited. An answer with no text scores 0 on
// either side.
func CompareGraded(ctx context.Context, completer Completer, set EvalSet, reference, candidate Target, now func() time.Time) (GradedComparison, error) {
	if completer == nil || !set.Graded() || len(set.Prompts) == 0 || !ValidDigest(set.Digest) || reference.SubjectID == "" || candidate.SubjectID == "" || reference.SubjectID == candidate.SubjectID {
		return GradedComparison{}, errors.New("graded check requires a completer, a graded eval set, and two different subjects")
	}
	comparison := GradedComparison{Set: set, ReferenceID: reference.SubjectID, CandidateRevisionID: candidate.SubjectID}
	referenceAnswers := make([]string, 0, len(set.Prompts))
	candidateAnswers := make([]string, 0, len(set.Prompts))
	for _, prompt := range set.Prompts {
		referenceAnswer, err := completer.Complete(ctx, reference, set, prompt)
		if err != nil {
			return GradedComparison{}, fmt.Errorf("current API: %w", err)
		}
		candidateAnswer, err := completer.Complete(ctx, candidate, set, prompt)
		if err != nil {
			return GradedComparison{}, fmt.Errorf("candidate revision: %w", err)
		}
		comparison.ReferenceScores = append(comparison.ReferenceScores, Grade(set.Grading, prompt, referenceAnswer.Content))
		comparison.CandidateScores = append(comparison.CandidateScores, Grade(set.Grading, prompt, candidateAnswer.Content))
		referenceAnswers = append(referenceAnswers, referenceAnswer.digest())
		candidateAnswers = append(candidateAnswers, candidateAnswer.digest())
	}
	bundle, _ := json.Marshal(map[string]any{
		"evaluator": GradedEvaluator, "evaluator_version": GradedEvaluatorVersion, "eval_set": set.Digest, "grading": set.Grading,
		"reference_id": reference.SubjectID, "candidate_revision_id": candidate.SubjectID,
		"reference_answers": referenceAnswers, "candidate_answers": candidateAnswers,
		"reference_scores": comparison.ReferenceScores, "candidate_scores": comparison.CandidateScores,
	})
	comparison.ResultDigest = Digest(bundle)
	stamp := time.Now().UTC()
	if now != nil {
		stamp = now().UTC()
	}
	comparison.EvaluatedAt = stamp.Truncate(time.Second)
	return comparison, nil
}

// Payload binds one side of a graded comparison to its subject: the closed
// API's binding:<id> under its endpoint, or the candidate's exact revision
// under its deployment. passed is the candidate's verdict; the reference
// side always records passed, since its scores are the baseline.
func (g GradedComparison) Payload(name string, reference, passed bool) qualityevidence.Payload {
	subject, scores := g.CandidateRevisionID, g.CandidateScores
	if reference {
		subject, scores, passed = g.ReferenceID, g.ReferenceScores, true
	}
	return qualityevidence.Payload{
		Schema: qualityevidence.Schema, Deployment: name, RevisionID: subject,
		Suite: g.Set.Name, SuiteVersion: g.Set.SuiteVersion(), Evaluator: GradedEvaluator, EvaluatorVersion: GradedEvaluatorVersion,
		Score: round(mean(scores)), Passed: passed, SampleCount: len(scores), ArtifactDigest: g.ResultDigest, EvaluatedAt: g.EvaluatedAt,
		Distribution: &qualityevidence.Distribution{Schema: qualityevidence.DistributionSchema, Kind: "paired_scores", PairingDigest: g.Set.PairingDigest(), Scores: append([]float64(nil), scores...)},
	}
}

// MigrationPolicy is the policy a migration verdict applies: a paired
// bootstrap whose interval must stay within margin percent of the closed
// API's score.
func MigrationPolicy(margin float64) domain.ReleaseGuardPolicy {
	return domain.ReleaseGuardPolicy{Enabled: true, RequireQualityEvidence: true, QualityComparisonMode: "bootstrap", MaxQualityRegressionPercent: &margin, QualityBootstrapAlpha: MigrationBootstrapAlpha, QualityBootstrapMinSamples: MigrationBootstrapMinSamples, QualityBootstrapSeed: MigrationBootstrapSeed}
}

// MigrationVerdict compares a candidate's evidence with the closed API's on
// the same suite. PASS means the upper bound of the paired-bootstrap interval
// of the candidate's quality drop is within margin; BLOCK means the lower
// bound is above it or the candidate failed its suite; UNKNOWN covers too few
// pairs, an interval that straddles the margin, evidence from different
// suites or evaluators, and evidence without paired scores.
func MigrationVerdict(reference, candidate qualityevidence.Payload, margin float64) releaseguard.QualityEval {
	side := func(payload qualityevidence.Payload) domain.QualityEvalSide {
		out := domain.QualityEvalSide{Score: payload.Score, Passed: payload.Passed, SampleCount: payload.SampleCount}
		if payload.Distribution != nil {
			out.PairingDigest, out.Scores = payload.Distribution.PairingDigest, payload.Distribution.Scores
		}
		return out
	}
	referenceSide := side(reference)
	eval := domain.QualityEvalEvidence{Suite: candidate.Suite, SuiteVersion: candidate.SuiteVersion, Evaluator: candidate.Evaluator, EvaluatorVersion: candidate.EvaluatorVersion, Candidate: side(candidate)}
	if qualityevidence.Comparable(reference, candidate) {
		eval.Reference = &referenceSide
	}
	return releaseguard.CompareQuality(MigrationPolicy(margin), []domain.QualityEvalEvidence{eval}).Evals[0]
}
