package qualitycheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

const (
	// Evaluator names InferCrane's built-in check in evidence. The control API
	// refuses customer evidence that claims this name.
	Evaluator = "infercrane-builtin"
	// EvaluatorVersion covers the request settings, the agreement score, and
	// the thresholds below. Changing any of them requires a new version.
	// Version 2 turns reasoning off where the chat template allows it, removes
	// reasoning a template opened in the prompt, and stops instead of scoring
	// a prompt the reference left without answer text.
	EvaluatorVersion = "2"
	// WatchRegressionPercent and BlockRegressionPercent are the built-in
	// thresholds. A candidate whose answers agree with the reference less than
	// 95% of the way is WATCH; less than 90% is BLOCK.
	WatchRegressionPercent = 5.0
	BlockRegressionPercent = 10.0
)

// ErrReferenceUnanswered means the reference gave no answer text to at least
// one prompt within the eval set's token limit, so its answers cannot serve as
// the reference. Asking again does not help: decoding is greedy.
var ErrReferenceUnanswered = errors.New("the reference gave no answer text")

// Comparison is the in-memory result of one candidate against its reference.
// It holds scores and digests only; answers are discarded.
type Comparison struct {
	Set EvalSet
	// ReferenceID is the reference's subject: a revision ID, or
	// binding:<id> for a closed API.
	ReferenceID         string
	CandidateRevisionID string
	// Scores holds one agreement score per prompt, in eval-set order.
	Scores []float64
	Score  float64
	// ResultDigest commits to the eval set, both revisions, a digest of every
	// answer, and the scores, without storing the answers.
	ResultDigest string
	Eval         releaseguard.QualityEval
	EvaluatedAt  time.Time
}

// Compare asks the reference and the candidate every prompt in turn and
// scores the candidate's answers against the reference's. When both targets
// are the same revision the candidate is the reference, so each prompt is
// asked once and agreement is 1 by definition.
func Compare(ctx context.Context, completer Completer, set EvalSet, reference, candidate Target, now func() time.Time) (Comparison, error) {
	if completer == nil || len(set.Prompts) == 0 || !ValidDigest(set.Digest) || reference.SubjectID == "" || candidate.SubjectID == "" {
		return Comparison{}, errors.New("quality check requires a completer, a decoded eval set, and both revisions")
	}
	self := reference.SubjectID == candidate.SubjectID
	scores := make([]float64, 0, len(set.Prompts))
	referenceAnswers := make([]string, 0, len(set.Prompts))
	candidateAnswers := make([]string, 0, len(set.Prompts))
	var unanswered []string
	for _, prompt := range set.Prompts {
		referenceAnswer, err := completer.Complete(ctx, reference, set, prompt)
		if err != nil {
			return Comparison{}, fmt.Errorf("reference revision: %w", err)
		}
		if !Answered(referenceAnswer) {
			// Scoring this prompt would compare nothing with nothing.
			unanswered = append(unanswered, prompt.ID)
		}
		if len(unanswered) > 0 {
			// The check cannot pass any more; only count what is left.
			continue
		}
		candidateAnswer := referenceAnswer
		if !self {
			if candidateAnswer, err = completer.Complete(ctx, candidate, set, prompt); err != nil {
				return Comparison{}, fmt.Errorf("candidate revision: %w", err)
			}
		}
		scores = append(scores, Agreement(referenceAnswer, candidateAnswer))
		referenceAnswers = append(referenceAnswers, answerDigest(referenceAnswer))
		candidateAnswers = append(candidateAnswers, answerDigest(candidateAnswer))
	}
	if len(unanswered) > 0 {
		return Comparison{}, fmt.Errorf("%w to %d of %d prompts within %d tokens (%s)", ErrReferenceUnanswered, len(unanswered), len(set.Prompts), set.MaxTokens, promptList(unanswered))
	}
	score := round(mean(scores))
	bundle, _ := json.Marshal(map[string]any{
		"evaluator": Evaluator, "evaluator_version": EvaluatorVersion, "eval_set": set.Digest,
		"reference_revision_id": reference.SubjectID, "candidate_revision_id": candidate.SubjectID,
		"reference_answers": referenceAnswers, "candidate_answers": candidateAnswers, "scores": scores,
	})
	stamp := time.Now().UTC()
	if now != nil {
		stamp = now().UTC()
	}
	comparison := Comparison{Set: set, ReferenceID: reference.SubjectID, CandidateRevisionID: candidate.SubjectID, Scores: scores, Score: score, ResultDigest: Digest(bundle), EvaluatedAt: stamp.Truncate(time.Second)}
	comparison.Eval = Classify(set, score, scores)
	return comparison, nil
}

// Policy is the fixed policy the built-in check applies through Release
// Guard's own classifier.
func Policy() domain.ReleaseGuardPolicy {
	watch, block := WatchRegressionPercent, BlockRegressionPercent
	return domain.ReleaseGuardPolicy{Enabled: true, RequireQualityEvidence: true, QualityComparisonMode: "threshold", MaxQualityRegressionPercent: &block, QualityWatchRegressionPercent: &watch}
}

// Classify returns the PASS, WATCH, or BLOCK verdict for a candidate whose
// answers agree with the reference by score. The reference's own score is 1.
// scores may be nil when only the aggregate is known.
func Classify(set EvalSet, score float64, scores []float64) releaseguard.QualityEval {
	reference := make([]float64, len(scores))
	for index := range reference {
		reference[index] = 1
	}
	pairing := ""
	if len(scores) > 0 {
		pairing = set.PairingDigest()
	}
	eval := domain.QualityEvalEvidence{
		Suite: set.Name, SuiteVersion: set.SuiteVersion(), Evaluator: Evaluator, EvaluatorVersion: EvaluatorVersion,
		Candidate: domain.QualityEvalSide{Score: score, Passed: true, SampleCount: len(set.Prompts), PairingDigest: pairing, Scores: scores},
		Reference: &domain.QualityEvalSide{Score: 1, Passed: true, SampleCount: len(set.Prompts), PairingDigest: pairing, Scores: reference},
	}
	report := releaseguard.CompareQuality(Policy(), []domain.QualityEvalEvidence{eval})
	return report.Evals[0]
}

// VerdictForScore classifies a stored built-in score. It applies the same
// thresholds as Classify and needs no prompts.
func VerdictForScore(score float64) releaseguard.QualityEval {
	return Classify(EvalSet{Name: "stored", Version: "stored"}, score, nil)
}

// Payload binds one side of the comparison to an exact revision. The
// reference side records score 1: its answers are the reference.
func (c Comparison) Payload(deployment string, reference bool) qualityevidence.Payload {
	revision, score, passed := c.CandidateRevisionID, c.Score, c.Eval.Verdict != releaseguard.QualityBlock
	scores := c.Scores
	if reference {
		revision, score, passed = c.ReferenceID, 1, true
		scores = make([]float64, len(c.Scores))
		for index := range scores {
			scores[index] = 1
		}
	}
	return qualityevidence.Payload{
		Schema: qualityevidence.Schema, Deployment: deployment, RevisionID: revision,
		Suite: c.Set.Name, SuiteVersion: c.Set.SuiteVersion(), Evaluator: Evaluator, EvaluatorVersion: EvaluatorVersion,
		Score: score, Passed: passed, SampleCount: len(c.Scores), ArtifactDigest: c.ResultDigest, EvaluatedAt: c.EvaluatedAt,
		Distribution: &qualityevidence.Distribution{Schema: qualityevidence.DistributionSchema, Kind: "paired_scores", PairingDigest: c.Set.PairingDigest(), Scores: scores},
	}
}

// promptList names up to five prompts and counts the rest.
func promptList(ids []string) string {
	const shown = 5
	if len(ids) <= shown {
		return strings.Join(ids, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(ids[:shown], ", "), len(ids)-shown)
}

func answerDigest(answer string) string {
	sum := sha256.Sum256([]byte(answer))
	return hex.EncodeToString(sum[:])
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func round(value float64) float64 { return math.Round(value*10000) / 10000 }
