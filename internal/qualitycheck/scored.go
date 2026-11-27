package qualitycheck

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/infercrane/infercrane/internal/qualityevidence"
)

// RemoteSet names an eval set whose prompts the control plane does not hold:
// one an InferCrane agent keeps beside it in the customer's cluster. It
// carries the identity the agent reported (name, version, digest, grading,
// pairing digest), so evidence names it exactly as a set decoded here would.
func RemoteSet(name, version, digest, grading, pairing string) EvalSet {
	return EvalSet{Name: name, Version: version, Grading: grading, Digest: digest, pairing: pairing}
}

// Scored judges answers that were asked and scored elsewhere, by an agent in
// the customer's cluster, by the same rules Compare applies: on a task set
// the paired comparison of each side's score per item (read by the rule for
// setups that change answers when RequireAnswersHeld is passed); otherwise
// the candidate's agreement with the reference per prompt and its tool-call
// scores. A task set pools pairs other servers answered earlier (PoolWith)
// exactly as Compare does. referenceScores is empty for an agreement suite.
// resultDigest is the agent's commitment to the answers it kept.
func Scored(set EvalSet, referenceID, candidateID string, referenceScores, candidateScores []float64, toolCalls *qualityevidence.ToolCallScores, itemsAtOnce int, resultDigest string, evaluatedAt time.Time, options ...CompareOption) (Comparison, error) {
	var chosen compareOptions
	for _, option := range options {
		option(&chosen)
	}
	if !ValidDigest(set.Digest) || !ValidDigest(resultDigest) || referenceID == "" || candidateID == "" || len(candidateScores) == 0 || evaluatedAt.IsZero() {
		return Comparison{}, errors.New("scored answers need the eval set's digest, both subjects, the scores, the result digest, and when they were scored")
	}
	comparison := Comparison{Set: set, ReferenceID: referenceID, CandidateRevisionID: candidateID, Scores: candidateScores, ResultDigest: resultDigest, EvaluatedAt: evaluatedAt.UTC().Truncate(time.Second)}
	if set.Task() {
		if len(referenceScores) != len(candidateScores) {
			return Comparison{}, errors.New("a task set needs both sides' score for every item")
		}
		task, pooled := judgeTask(set, referenceID, candidateID, referenceScores, candidateScores, chosen)
		comparison.ReferenceScores, comparison.Task, comparison.Score, comparison.ItemsAtOnce = referenceScores, &task, task.CandidateScore, itemsAtOnce
		if len(pooled) > 0 {
			// The digest commits to the agent's answers and to the earlier
			// servers' scores the verdict read, as compareTask's does.
			bundle, _ := json.Marshal(map[string]any{"result_digest": resultDigest, "pooled_pairs": pooled})
			comparison.ResultDigest = Digest(bundle)
		}
		comparison.Eval = ClassifyTask(set, task)
		return comparison, nil
	}
	if len(referenceScores) != 0 {
		return Comparison{}, errors.New("an agreement suite scores the candidate against the reference only")
	}
	comparison.ToolCalls, comparison.Score = toolCalls, round(mean(candidateScores))
	comparison.Eval = Classify(set, comparison.Score, candidateScores, toolCalls)
	return comparison, nil
}
