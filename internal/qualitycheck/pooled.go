package qualitycheck

import (
	"errors"
	"fmt"
)

// ServerPair is one reference server's and one candidate server's score per
// item of one task set, in eval-set order, with the exact setups that served
// them. A campaign keeps the pair a setup's first servers answered before a
// confirmation retires them, so the verdict can pool it with the pair its
// fresh servers answer.
type ServerPair struct {
	EvalSet     string    `json:"eval_set"`
	ReferenceID string    `json:"reference_id"`
	CandidateID string    `json:"candidate_id"`
	Reference   []float64 `json:"reference_scores"`
	Candidate   []float64 `json:"candidate_scores"`
}

// ErrNotPoolable means server pairs cannot be pooled into one verdict: their
// servers ran different setups, or answered different task sets or items.
var ErrNotPoolable = errors.New("server pairs cannot be pooled")

// Pair is the pair of servers this task-set comparison asked.
func (c Comparison) Pair() (ServerPair, bool) {
	if c.Task == nil || len(c.ReferenceScores) == 0 || len(c.ReferenceScores) != len(c.Scores) {
		return ServerPair{}, false
	}
	return ServerPair{EvalSet: c.Set.Digest, ReferenceID: c.ReferenceID, CandidateID: c.CandidateRevisionID,
		Reference: append([]float64(nil), c.ReferenceScores...), Candidate: append([]float64(nil), c.Scores...)}, true
}

// PoolWith adds pairs answered earlier by other servers of the same two
// setups to a task-set comparison: the verdict is read over every pair
// (PooledTask). Pairs that cannot be pooled are left out and the comparison
// records why (TaskComparison.PoolRefused); the verdict then rests on the
// servers asked now.
func PoolWith(pairs ...ServerPair) CompareOption {
	return func(options *compareOptions) { options.earlier = append(options.earlier, pairs...) }
}

// PooledTask is PairedTask over several server pairs of the same two setups:
// each item's score on a side is the share of that side's servers that got
// it right, the difference is the mean over items of candidate minus
// reference, and its 95% interval is read over items, as the map's harness
// pools servers. Items stay the unit, so the interval narrows only as the
// servers' disagreement on the same items averages out. Pairs pool only when
// every one answered the same task set and number of items, every reference
// server ran one setup, and every candidate server one setup; otherwise
// ErrNotPoolable says which.
func PooledTask(pairs []ServerPair) (TaskComparison, error) {
	if len(pairs) == 0 {
		return TaskComparison{}, fmt.Errorf("%w: no server pair", ErrNotPoolable)
	}
	first := pairs[0]
	for index, pair := range pairs {
		switch {
		case len(pair.Reference) != len(pair.Candidate) || len(pair.Reference) == 0:
			return TaskComparison{}, fmt.Errorf("%w: pair %d has %d reference and %d candidate scores", ErrNotPoolable, index+1, len(pair.Reference), len(pair.Candidate))
		case len(pair.Reference) != len(first.Reference):
			return TaskComparison{}, fmt.Errorf("%w: pair %d answered %d items, pair 1 answered %d", ErrNotPoolable, index+1, len(pair.Reference), len(first.Reference))
		case pair.EvalSet == "" || pair.EvalSet != first.EvalSet:
			return TaskComparison{}, fmt.Errorf("%w: pair %d answered another task set", ErrNotPoolable, index+1)
		case pair.ReferenceID == "" || pair.ReferenceID != first.ReferenceID:
			return TaskComparison{}, fmt.Errorf("%w: pair %d's reference ran another setup (%s, pair 1 %s)", ErrNotPoolable, index+1, pair.ReferenceID, first.ReferenceID)
		case pair.CandidateID == "" || pair.CandidateID != first.CandidateID:
			return TaskComparison{}, fmt.Errorf("%w: pair %d's candidate ran another setup (%s, pair 1 %s)", ErrNotPoolable, index+1, pair.CandidateID, first.CandidateID)
		}
	}
	reference, candidate := make([]float64, len(first.Reference)), make([]float64, len(first.Reference))
	for _, pair := range pairs {
		for item := range pair.Reference {
			reference[item] += pair.Reference[item]
			candidate[item] += pair.Candidate[item]
		}
	}
	servers := float64(len(pairs))
	for item := range reference {
		reference[item] /= servers
		candidate[item] /= servers
	}
	task := PairedTask(reference, candidate)
	task.ServersPerSide = len(pairs)
	return task, nil
}
