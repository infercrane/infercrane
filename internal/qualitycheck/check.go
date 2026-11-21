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
	// Version 3 sends v2 cases' conversations and tools, scores tool cases on
	// the calls, counts a call as an answer, and blocks a candidate that
	// breaks a call the reference made correctly.
	// Version 4 leaves a tool case unchecked, with no score, when the
	// reference refuses requests with tools, and blocks a candidate that
	// refuses tools the reference accepted.
	EvaluatorVersion = "4"
	// WatchRegressionPercent and BlockRegressionPercent are the built-in
	// thresholds. A candidate whose answers agree with the reference less than
	// 95% of the way is WATCH; less than 90% is BLOCK.
	WatchRegressionPercent = 5.0
	BlockRegressionPercent = 10.0
)

// ErrReferenceUnanswered means the reference gave no answer text and made no
// tool call to at least one prompt within the eval set's token limit, so its
// answers cannot serve as the reference. Asking again does not help: decoding
// is greedy.
var ErrReferenceUnanswered = errors.New("the reference gave no answer text")

// ErrNothingChecked means every case declares tools and the reference
// refused requests with tools, so no case could be checked. A set with any
// case the reference answers is checked on those cases instead.
var ErrNothingChecked = errors.New("the reference refused requests with tools, and every case declares tools")

// CodeToolCallBroken is the verdict code of a candidate that broke a tool
// call the reference made correctly.
const CodeToolCallBroken = "tool_call_broken"

// Comparison is the in-memory result of one candidate against its reference.
// It holds scores and digests only; answers are discarded.
type Comparison struct {
	Set EvalSet
	// ReferenceID is the reference's subject: a revision ID, or
	// binding:<id> for a closed API.
	ReferenceID         string
	CandidateRevisionID string
	// Scores holds one agreement score per checked prompt, in eval-set order.
	Scores []float64
	Score  float64
	// Checked lists the IDs of the scored prompts, in eval-set order. It
	// leaves out tool cases the reference refused.
	Checked []string
	// ToolCalls is the score over cases that declare tools, or nil when the
	// eval set has none.
	ToolCalls *qualityevidence.ToolCallScores
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
//
// A tool case the reference refuses (ErrToolsRefused) is not checked: it is
// not asked of the candidate, has no score, and counts only in
// ToolCalls.NotChecked. A candidate that refuses a tool case the reference
// accepted breaks it, which blocks the candidate.
func Compare(ctx context.Context, completer Completer, set EvalSet, reference, candidate Target, now func() time.Time) (Comparison, error) {
	if completer == nil || len(set.Prompts) == 0 || !ValidDigest(set.Digest) || reference.SubjectID == "" || candidate.SubjectID == "" {
		return Comparison{}, errors.New("quality check requires a completer, a decoded eval set, and both revisions")
	}
	self := reference.SubjectID == candidate.SubjectID
	scores := make([]float64, 0, len(set.Prompts))
	checked := make([]string, 0, len(set.Prompts))
	referenceAnswers := make([]string, 0, len(set.Prompts))
	candidateAnswers := make([]string, 0, len(set.Prompts))
	var unanswered []string
	var toolScores []float64
	notChecked, broken, referenceInvalid := 0, 0, 0
	var candidateKinds, referenceKinds qualityevidence.ToolCallFailures
	for _, prompt := range set.Prompts {
		referenceAnswer, err := completer.Complete(ctx, reference, set, prompt)
		if prompt.HasTools() && errors.Is(err, ErrToolsRefused) {
			// The reference cannot run the case, so there is nothing to
			// compare the candidate with. The case stays unknown.
			notChecked++
			continue
		}
		if err != nil {
			return Comparison{}, fmt.Errorf("reference revision: %w", err)
		}
		if !referenceAnswer.answered() {
			// Scoring this prompt would compare nothing with nothing.
			unanswered = append(unanswered, prompt.ID)
		}
		if len(unanswered) > 0 {
			// The check cannot pass any more; only count what is left.
			continue
		}
		candidateAnswer, candidateRefused := referenceAnswer, false
		if !self {
			candidateAnswer, err = completer.Complete(ctx, candidate, set, prompt)
			candidateRefused = prompt.HasTools() && errors.Is(err, ErrToolsRefused)
			if err != nil && !candidateRefused {
				return Comparison{}, fmt.Errorf("candidate revision: %w", err)
			}
		}
		score := Agreement(referenceAnswer.Content, candidateAnswer.Content)
		candidateDigest := candidateAnswer.digest()
		if prompt.HasTools() {
			result := scoreToolCase(prompt, referenceAnswer, candidateAnswer)
			if candidateRefused {
				result, candidateDigest = refusedToolCase(prompt, referenceAnswer), answerDigest(ErrToolsRefused.Error())
			}
			if self {
				// The reference agrees with itself by definition.
				result.score, result.broken, result.candidate = 1, false, qualityevidence.ToolCallFailures{}
			}
			candidateKinds, referenceKinds = candidateKinds.Add(result.candidate), referenceKinds.Add(result.reference)
			score = result.score
			toolScores = append(toolScores, score)
			if result.broken {
				broken++
			}
			if result.referenceInvalid {
				referenceInvalid++
			}
		}
		scores = append(scores, score)
		checked = append(checked, prompt.ID)
		referenceAnswers = append(referenceAnswers, referenceAnswer.digest())
		candidateAnswers = append(candidateAnswers, candidateDigest)
	}
	if len(unanswered) > 0 {
		return Comparison{}, fmt.Errorf("%w to %d of %d prompts within %d tokens (%s)", ErrReferenceUnanswered, len(unanswered), len(set.Prompts), set.MaxTokens, promptList(unanswered))
	}
	if len(scores) == 0 {
		return Comparison{}, fmt.Errorf("%w (%d cases)", ErrNothingChecked, len(set.Prompts))
	}
	score := round(mean(scores))
	var toolCalls *qualityevidence.ToolCallScores
	if len(toolScores)+notChecked > 0 {
		toolCalls = &qualityevidence.ToolCallScores{Cases: len(toolScores), NotChecked: notChecked, Broken: broken, ReferenceInvalid: referenceInvalid, Candidate: candidateKinds, Reference: referenceKinds}
		if len(toolScores) > 0 {
			toolCalls.Score = round(mean(toolScores))
		}
	}
	bundle, _ := json.Marshal(map[string]any{
		"evaluator": Evaluator, "evaluator_version": EvaluatorVersion, "eval_set": set.Digest,
		"reference_revision_id": reference.SubjectID, "candidate_revision_id": candidate.SubjectID, "checked": checked,
		"reference_answers": referenceAnswers, "candidate_answers": candidateAnswers, "scores": scores, "tool_calls": toolCalls,
	})
	stamp := time.Now().UTC()
	if now != nil {
		stamp = now().UTC()
	}
	comparison := Comparison{Set: set, ReferenceID: reference.SubjectID, CandidateRevisionID: candidate.SubjectID, Scores: scores, Score: score, Checked: checked, ToolCalls: toolCalls, ResultDigest: Digest(bundle), EvaluatedAt: stamp.Truncate(time.Second)}
	comparison.Eval = Classify(set, comparison.PairingDigest(), score, scores, toolCalls)
	return comparison, nil
}

// PairingDigest commits both sides' evidence to the scored prompts, in order.
// It equals the eval set's pairing digest when every prompt was scored.
func (c Comparison) PairingDigest() string {
	if len(c.Checked) == len(c.Set.Prompts) {
		return c.Set.PairingDigest()
	}
	return pairingDigest(c.Set.Digest, c.Checked)
}

// Policy is the fixed policy the built-in check applies through Release
// Guard's own classifier.
func Policy() domain.ReleaseGuardPolicy {
	watch, block := WatchRegressionPercent, BlockRegressionPercent
	return domain.ReleaseGuardPolicy{Enabled: true, RequireQualityEvidence: true, QualityComparisonMode: "threshold", MaxQualityRegressionPercent: &block, QualityWatchRegressionPercent: &watch}
}

// Classify returns the PASS, WATCH, or BLOCK verdict for a candidate whose
// answers agree with the reference by score. The reference's own score is 1.
// scores holds one score per scored prompt and pairing commits to those
// prompts; both are empty when only the aggregate is known. A candidate that
// broke a tool call the reference made correctly is BLOCK whatever its score.
func Classify(set EvalSet, pairing string, score float64, scores []float64, toolCalls *qualityevidence.ToolCallScores) releaseguard.QualityEval {
	broken := toolCalls != nil && toolCalls.Broken > 0
	reference := make([]float64, len(scores))
	for index := range reference {
		reference[index] = 1
	}
	eval := domain.QualityEvalEvidence{
		Suite: set.Name, SuiteVersion: set.SuiteVersion(), Evaluator: Evaluator, EvaluatorVersion: EvaluatorVersion,
		Candidate: domain.QualityEvalSide{Score: score, Passed: !broken, SampleCount: len(scores), PairingDigest: pairing, Scores: scores},
		Reference: &domain.QualityEvalSide{Score: 1, Passed: true, SampleCount: len(scores), PairingDigest: pairing, Scores: reference},
	}
	report := releaseguard.CompareQuality(Policy(), []domain.QualityEvalEvidence{eval})
	verdict := report.Evals[0]
	if broken {
		verdict.Code, verdict.Message = CodeToolCallBroken, toolCallsBrokenMessage(*toolCalls)
	}
	return verdict
}

// toolCallsBrokenMessage says how many tool calls a candidate broke.
func toolCallsBrokenMessage(toolCalls qualityevidence.ToolCallScores) string {
	return fmt.Sprintf("Candidate broke %d of %d tool calls: it refused tools the reference accepted, or the reference's call validated against the function's schema and the candidate's did not or called a different function", toolCalls.Broken, toolCalls.Cases)
}

// VerdictForScore classifies a stored built-in score. It applies the same
// thresholds as Classify and needs no prompts.
func VerdictForScore(score float64) releaseguard.QualityEval {
	return Classify(EvalSet{Name: "stored", Version: "stored"}, "", score, nil, nil)
}

// Payload binds one side of the comparison to an exact revision. The
// reference side records score 1: its answers are the reference.
func (c Comparison) Payload(deployment string, reference bool) qualityevidence.Payload {
	revision, score, passed := c.CandidateRevisionID, c.Score, c.Eval.Verdict != releaseguard.QualityBlock
	scores, toolCalls := c.Scores, c.ToolCalls
	if reference {
		revision, score, passed = c.ReferenceID, 1, true
		scores = make([]float64, len(c.Scores))
		for index := range scores {
			scores[index] = 1
		}
		if toolCalls != nil {
			// The reference's own row: it agrees with itself, and its own
			// failures are still its own.
			toolCalls = &qualityevidence.ToolCallScores{Cases: toolCalls.Cases, NotChecked: toolCalls.NotChecked, ReferenceInvalid: toolCalls.ReferenceInvalid, Reference: toolCalls.Reference}
			if toolCalls.Cases > 0 {
				toolCalls.Score = 1
			}
		}
	}
	return qualityevidence.Payload{
		Schema: qualityevidence.Schema, Deployment: deployment, RevisionID: revision,
		Suite: c.Set.Name, SuiteVersion: c.Set.SuiteVersion(), Evaluator: Evaluator, EvaluatorVersion: EvaluatorVersion,
		Score: score, Passed: passed, SampleCount: len(c.Scores), ArtifactDigest: c.ResultDigest, EvaluatedAt: c.EvaluatedAt,
		Distribution: &qualityevidence.Distribution{Schema: qualityevidence.DistributionSchema, Kind: "paired_scores", PairingDigest: c.PairingDigest(), Scores: scores, ToolCalls: toolCalls},
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
