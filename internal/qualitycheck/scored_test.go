package qualitycheck

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Scores an agent computed in the customer's cluster are judged exactly as a
// check run here: the same verdict, task comparison, and signed payload, for
// a task set (by both rules) and for the agreement suite.
func TestScoredAnswersAreJudgedLikeACheckRunHere(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC) }
	set := taskSet(t, 400)
	wrong := func(from, to int, extra ...string) map[string]bool {
		out := map[string]bool{}
		for index := from; index < to; index++ {
			out["gsm8k.test."+itoa(index)] = true
		}
		for _, id := range extra {
			out[id] = true
		}
		return out
	}
	server := taskServer{"current": wrong(0, 30), "fp8": wrong(0, 33), "close": wrong(1, 30, "gsm8k.test.200")}
	remote := RemoteSet(set.Name, set.Version, set.Digest, set.Grading, set.PairingDigest())
	// earlier is a pair other servers of the same two setups answered
	// before, wrong on other items.
	earlier := func(candidate string) ServerPair {
		reference, other := make([]float64, 400), make([]float64, 400)
		for index := range 400 {
			reference[index], other[index] = 1, 1
			if index >= 100 && index < 130 {
				reference[index] = 0
			}
			if index >= 100 && index < 131 {
				other[index] = 0
			}
		}
		return ServerPair{EvalSet: set.Digest, ReferenceID: "current", CandidateID: candidate, Reference: reference, Candidate: other}
	}
	for _, candidate := range []string{"fp8", "close", "current"} {
		pool := PoolWith(earlier(candidate))
		for _, test := range []struct {
			options []CompareOption
			pools   bool
		}{{nil, false}, {[]CompareOption{RequireAnswersHeld()}, false}, {[]CompareOption{pool}, true}, {[]CompareOption{RequireAnswersHeld(), pool}, true}} {
			options := test.options
			local, err := Compare(context.Background(), server, set, Target{SubjectID: "current"}, Target{SubjectID: candidate}, now, options...)
			if err != nil {
				t.Fatal(err)
			}
			scored, err := Scored(remote, "current", candidate, local.ReferenceScores, local.Scores, nil, local.ItemsAtOnce, local.ResultDigest, local.EvaluatedAt, options...)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(local.Eval, scored.Eval) || !reflect.DeepEqual(local.Task, scored.Task) {
				t.Fatalf("%s (%d options): local %+v %+v, scored %+v %+v", candidate, len(options), local.Eval, local.Task, scored.Eval, scored.Task)
			}
			// A setup compared with itself pools nothing.
			if pooled := local.Task.ServersPerSide == 2; pooled != (test.pools && candidate != "current") {
				t.Fatalf("%s (%d options): pooled=%v", candidate, len(options), pooled)
			} else if pooled {
				// Both digests commit to the earlier pair the verdict read.
				if scored.ResultDigest == local.ResultDigest || !ValidDigest(scored.ResultDigest) {
					t.Fatalf("%s: scored digest %s does not commit to the pooled pair", candidate, scored.ResultDigest)
				}
				continue
			}
			for _, reference := range []bool{false, true} {
				if want, got := local.Payload("support-bot", reference), scored.Payload("support-bot", reference); !reflect.DeepEqual(want, got) {
					t.Fatalf("%s reference=%v: payloads differ:\n%+v\n%+v", candidate, reference, want, got)
				}
			}
		}
	}

	agreement := Default()
	local, err := Compare(context.Background(), echo{}, agreement, Target{SubjectID: "current"}, Target{SubjectID: "candidate", Model: "other"}, now)
	if err != nil {
		t.Fatal(err)
	}
	scored, err := Scored(RemoteSet(agreement.Name, agreement.Version, agreement.Digest, agreement.Grading, agreement.PairingDigest()), "current", "candidate", nil, local.Scores, local.ToolCalls, 0, local.ResultDigest, local.EvaluatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if want, got := local.Payload("support-bot", false), scored.Payload("support-bot", false); !reflect.DeepEqual(want, got) || !reflect.DeepEqual(local.Eval, scored.Eval) {
		t.Fatalf("agreement payloads differ:\n%+v\n%+v", want, got)
	}
	if _, err = Scored(remote, "current", "fp8", nil, local.Scores, nil, 0, local.ResultDigest, local.EvaluatedAt); err == nil {
		t.Fatal("a task set was judged without the reference's scores")
	}
	if _, err = Scored(remote, "current", "fp8", local.Scores, local.Scores, nil, 0, "answers", local.EvaluatedAt); err == nil {
		t.Fatal("scores without a result digest were judged")
	}
}

// echo answers every prompt with its own ID, and differently for the model
// "other" on one prompt in three, so agreement is below 1.
type echo struct{}

func (echo) Complete(_ context.Context, target Target, _ EvalSet, prompt Prompt) (Answer, error) {
	if target.Model == "other" && len(prompt.ID)%3 == 0 {
		return Answer{Content: "something else entirely " + strings.ToUpper(prompt.ID)}, nil
	}
	return Answer{Content: "answer for " + prompt.ID}, nil
}

func itoa(value int) string {
	digits := []byte{}
	for {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
		if value == 0 {
			return string(digits)
		}
	}
}
