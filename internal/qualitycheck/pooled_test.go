package qualitycheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func wrongItems(ranges ...[2]int) map[string]bool {
	out := map[string]bool{}
	for _, span := range ranges {
		for index := span[0]; index < span[1]; index++ {
			out[fmt.Sprintf("gsm8k.test.%d", index)] = true
		}
	}
	return out
}

// Two servers of each setup disagree on different items, as servers of one
// setup do from run to run. On one pair the interval is too wide to show the
// answers held within a point; pooled over both pairs, each item's score is
// the share of servers right, the disagreement halves, and it is shown held.
func TestPoolingAFreshServerPairNarrowsTheInterval(t *testing.T) {
	set := taskSet(t, 1000)
	first := taskServer{"current": wrongItems([2]int{0, 25}), "fp8": wrongItems([2]int{100, 125})}
	fresh := taskServer{"current": wrongItems([2]int{200, 225}), "fp8": wrongItems([2]int{300, 325})}
	current, fp8 := Target{SubjectID: "current"}, Target{SubjectID: "fp8"}

	alone, err := Compare(context.Background(), first, set, current, fp8, nil, RequireAnswersHeld())
	if err != nil {
		t.Fatal(err)
	}
	if alone.Task.Verdict != TaskNotShownHeld || alone.Task.ServersPerSide != 1 {
		t.Fatalf("one pair: %+v", alone.Task)
	}
	pair, ok := alone.Pair()
	if !ok || pair.ReferenceID != "current" || pair.CandidateID != "fp8" || pair.EvalSet != set.Digest || len(pair.Reference) != 1000 {
		t.Fatalf("pair = %+v %v", pair, ok)
	}

	pooled, err := Compare(context.Background(), fresh, set, current, fp8, nil, RequireAnswersHeld(), PoolWith(pair))
	if err != nil {
		t.Fatal(err)
	}
	task := pooled.Task
	if task.Verdict != TaskHeld || task.ServersPerSide != 2 || task.PoolRefused != "" || task.Low <= -HeldMargin {
		t.Fatalf("pooled: %+v", task)
	}
	if pooled.Eval.Code != CodeTaskHeld || pooled.Payload("qwen", false).Distribution.TaskServers != 2 {
		t.Fatalf("pooled eval %+v distribution %+v", pooled.Eval, pooled.Payload("qwen", false).Distribution)
	}
	// The fresh pair alone is no better than the first; only pooling shows it.
	freshAlone, _ := Compare(context.Background(), fresh, set, current, fp8, nil, RequireAnswersHeld())
	if freshAlone.Task.Verdict != TaskNotShownHeld || freshAlone.ResultDigest == pooled.ResultDigest {
		t.Fatalf("fresh pair alone: %+v (digest must commit to the pooled pair)", freshAlone.Task)
	}
	// The pooled scores are the share of servers right: 97.5% each side.
	if task.ReferenceScore != 0.975 || task.CandidateScore != 0.975 || task.Difference != 0 {
		t.Fatalf("pooled scores: %+v", task)
	}
}

// Pairs pool only when every server of a side ran the same setup on the same
// task set; otherwise the verdict rests on the servers asked last and says
// why the earlier pair was left out.
func TestPairsOfOtherSetupsAreNotPooled(t *testing.T) {
	set := taskSet(t, 1000)
	server := taskServer{"current": wrongItems([2]int{0, 25}), "fp8": wrongItems([2]int{100, 125})}
	scores := func(value float64, count int) []float64 {
		out := make([]float64, count)
		for index := range out {
			out[index] = value
		}
		return out
	}
	for name, earlier := range map[string]ServerPair{
		"another candidate setup": {EvalSet: set.Digest, ReferenceID: "current", CandidateID: "fp8-other-args", Reference: scores(1, 1000), Candidate: scores(1, 1000)},
		"another reference setup": {EvalSet: set.Digest, ReferenceID: "current-v2", CandidateID: "fp8", Reference: scores(1, 1000), Candidate: scores(1, 1000)},
		"another task set":        {EvalSet: "sha256:" + strings.Repeat("0", 64), ReferenceID: "current", CandidateID: "fp8", Reference: scores(1, 1000), Candidate: scores(1, 1000)},
		"fewer items":             {EvalSet: set.Digest, ReferenceID: "current", CandidateID: "fp8", Reference: scores(1, 999), Candidate: scores(1, 999)},
	} {
		comparison, err := Compare(context.Background(), server, set, Target{SubjectID: "current"}, Target{SubjectID: "fp8"}, nil, RequireAnswersHeld(), PoolWith(earlier))
		if err != nil {
			t.Fatal(err)
		}
		if comparison.Task.ServersPerSide != 1 || comparison.Task.Verdict != TaskNotShownHeld || !strings.Contains(comparison.Task.PoolRefused, "cannot be pooled") {
			t.Fatalf("%s: %+v", name, comparison.Task)
		}
	}
	if _, err := PooledTask(nil); !errors.Is(err, ErrNotPoolable) {
		t.Fatalf("no pairs: %v", err)
	}
}

// The 1-point bar is exact: a lower end past -1 point is never rounded up to
// pass, pooled or not.
func TestTheOnePointBarIsNotRoundedUp(t *testing.T) {
	for name, low := range map[string]float64{"-1.03 points": -0.0103, "-1.004 points": -0.01004, "exactly -1 point": -0.01} {
		if got := RequireHeld(TaskComparison{Items: 1000, Low: round(low), High: 0.01, ServersPerSide: 2}).Verdict; got != TaskNotShownHeld {
			t.Errorf("%s: %s, want %s", name, got, TaskNotShownHeld)
		}
	}
	if got := RequireHeld(TaskComparison{Items: 1000, Low: -0.0099, High: 0.01, ServersPerSide: 2}).Verdict; got != TaskHeld {
		t.Errorf("-0.99 points: %s, want %s", got, TaskHeld)
	}
}
