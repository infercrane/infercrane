package qualitycheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/releaseguard"
)

// The answer line rule against tools/fast-track-map/quality.py: each row's
// last value is what quality.score returned for it.
func TestAnswerLineScoresAsTheMapsRule(t *testing.T) {
	for _, test := range []struct {
		gold, text string
		right      bool
	}{
		{"70", "Working...\nAnswer: 70", true},
		{"70", "Answer: 1,070 coins", false},
		{"1070", "Answer: $1,070.", true},
		{"5", "no line, the result is 3 then 5", true},
		{"5", "Answer: none", false},
		{"B", "Answer: B", true},
		{"B", "Answer: Because B", true},
		{"C", "The answer is C", false},
		{"D", "answer：(D) 5,000 s", true},
		{"-3", "Answer: -3.0", true},
		{"0.5", "Answer: .5", true},
	} {
		got := Grade(GradingAnswerLine, Prompt{Expected: []string{test.gold}}, test.text) == 1
		if got != test.right {
			t.Errorf("gold %s, %q: right=%v, want %v", test.gold, test.text, got, test.right)
		}
	}
}

// The paired interval against quality.interval on the same differences: a
// count of items only the reference got right, a count only the candidate
// got right, and the rest the same.
func TestPairedTaskMatchesTheMapsInterval(t *testing.T) {
	for _, test := range []struct {
		onlyReference, onlyCandidate, items int
		difference, low, high               float64
		verdict                             string
	}{
		{36, 7, 1000, -0.029, -0.04173, -0.01627, TaskWorse},
		{12, 9, 1000, -0.003, -0.01198, 0.00598, TaskNoDifference},
		{30, 0, 200, -0.15, -0.19961, -0.10039, TaskWorse},
		{6, 3, 100, -0.03, -0.0888, 0.0288, TaskNotSettled},
		{0, 5, 50, 0.1, 0.016, 0.184, TaskBetter},
	} {
		reference, candidate := make([]float64, test.items), make([]float64, test.items)
		for index := range test.items {
			switch {
			case index < test.onlyReference:
				reference[index] = 1
			case index < test.onlyReference+test.onlyCandidate:
				candidate[index] = 1
			}
		}
		got := PairedTask(reference, candidate)
		if got.Verdict != test.verdict || math.Abs(got.Difference-test.difference) > 1e-4 || math.Abs(got.Low-test.low) > 1e-4 || math.Abs(got.High-test.high) > 1e-4 || got.OnlyReferenceRight != test.onlyReference || got.OnlyCandidateRight != test.onlyCandidate {
			t.Errorf("%+v: got %+v", test, got)
		}
	}
	if got := PairedTask([]float64{1}, []float64{0}); got.Verdict != TaskNotSettled {
		t.Fatalf("one item settled something: %+v", got)
	}
}

// taskServer answers each item as a setup does: right unless the item is one
// it gets wrong.
type taskServer map[string]map[string]bool

func (s taskServer) Complete(_ context.Context, target Target, _ EvalSet, prompt Prompt) (Answer, error) {
	gold := strings.TrimPrefix(prompt.Prompt, "item with gold ")
	if s[target.SubjectID][prompt.ID] {
		return Answer{Content: "Some working.\nAnswer: 999"}, nil
	}
	return Answer{Content: "Some working.\nAnswer: " + gold}, nil
}

func taskSet(t *testing.T, items int) EvalSet {
	t.Helper()
	prompts := make([]map[string]any, 0, items)
	for index := range items {
		gold := fmt.Sprint(index + 1)
		prompts = append(prompts, map[string]any{"id": fmt.Sprintf("gsm8k.test.%d", index), "prompt": "item with gold " + gold, "expected": []string{gold}})
	}
	body, err := json.Marshal(map[string]any{"schema": EvalSetSchemaV1, "name": "task-set", "version": "1", "max_tokens": 1024, "grading": GradingAnswerLine, "prompts": prompts})
	if err != nil {
		t.Fatal(err)
	}
	set, err := DecodeEvalSet(body)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// Package 3's stand-in run: a speculation setup and an FP8 KV cache setup
// each get a task score against the current setup on a 400-item task set.
// The current setup gets 30 items wrong; the speculation setup two more and
// one fewer elsewhere (not measurably different); the FP8 KV cache setup 40
// more (worse, so blocked).
func TestASpeculationSetupAndAnFP8KVCacheEachGetATaskScore(t *testing.T) {
	set := taskSet(t, 400)
	wrong := func(from, to int, extra ...int) map[string]bool {
		out := map[string]bool{}
		for index := from; index < to; index++ {
			out[fmt.Sprintf("gsm8k.test.%d", index)] = true
		}
		for _, index := range extra {
			out[fmt.Sprintf("gsm8k.test.%d", index)] = true
		}
		return out
	}
	server := taskServer{"current": wrong(0, 30), "speculation": wrong(1, 30, 100, 101), "fp8-kv": wrong(0, 70)}
	now := func() time.Time { return time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC) }
	current := Target{SubjectID: "current"}
	for _, test := range []struct {
		candidate, verdict, code string
		score                    float64
	}{
		{"speculation", releaseguard.QualityPass, CodeTaskNoDifference, 0.9225},
		{"fp8-kv", releaseguard.QualityBlock, CodeTaskWorse, 0.825},
	} {
		comparison, err := Compare(context.Background(), server, set, current, Target{SubjectID: test.candidate}, now)
		if err != nil {
			t.Fatal(err)
		}
		if comparison.Task == nil || comparison.Task.ReferenceScore != 0.925 || comparison.Task.CandidateScore != test.score || comparison.Eval.Verdict != test.verdict || comparison.Eval.Code != test.code {
			t.Fatalf("%s: task=%+v eval=%+v", test.candidate, comparison.Task, comparison.Eval)
		}
		candidate, reference := comparison.Payload("qwen", false), comparison.Payload("qwen", true)
		if candidate.Evaluator != TaskEvaluator || reference.Score != 0.925 || reference.RevisionID != "current" || candidate.Passed != (test.verdict == releaseguard.QualityPass) || len(reference.Distribution.Scores) != 400 {
			t.Fatalf("%s: candidate=%+v reference=%+v", test.candidate, candidate, reference)
		}
		if !strings.Contains(comparison.Eval.Message, "92.5% on 400 items") {
			t.Fatalf("%s: message %q", test.candidate, comparison.Eval.Message)
		}
	}
}

func TestTaskSetLimits(t *testing.T) {
	set := taskSet(t, MaxTaskPrompts)
	if !set.Task() || set.MaxTokens != 1024 {
		t.Fatalf("set=%s %d", set.Grading, set.MaxTokens)
	}
	for name, body := range map[string]string{
		"a letter outside A to D":                     `{"schema":"infercrane.dev/quality-eval-set/v1","name":"t","version":"1","grading":"answer_line","prompts":[{"id":"a","prompt":"q","expected":["E"]}]}`,
		"two gold answers":                            `{"schema":"infercrane.dev/quality-eval-set/v1","name":"t","version":"1","grading":"answer_line","prompts":[{"id":"a","prompt":"q","expected":["1","2"]}]}`,
		"a set scored by agreement with 1,024 tokens": `{"schema":"infercrane.dev/quality-eval-set/v1","name":"t","version":"1","max_tokens":1024,"prompts":[{"id":"a","prompt":"q"}]}`,
	} {
		if _, err := DecodeEvalSet([]byte(body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// pacedServer answers like taskServer after a short wait, and counts how many
// of one server's items it was asked at once.
type pacedServer struct {
	taskServer
	mu       sync.Mutex
	running  map[string]int
	most     map[string]int
	failAt   string
	answered int
}

func (s *pacedServer) Complete(ctx context.Context, target Target, set EvalSet, prompt Prompt) (Answer, error) {
	s.mu.Lock()
	s.running[target.SubjectID]++
	s.most[target.SubjectID] = max(s.most[target.SubjectID], s.running[target.SubjectID])
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running[target.SubjectID]--
		s.answered++
		s.mu.Unlock()
	}()
	select {
	case <-time.After(2 * time.Millisecond):
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	}
	if target.SubjectID+"/"+prompt.ID == s.failAt {
		return Answer{}, errors.New("unreachable")
	}
	return s.taskServer.Complete(ctx, target, set, prompt)
}

// A task set is asked taskAtOnce items at a time, so 1,000 worked answers fit
// the check's limit on a small GPU; each answer keeps its item's place, so the
// scores and the verdict are those of asking one by one, and the first
// failure stops the rest.
func TestATaskSetIsAskedManyItemsAtOnceInItsOrder(t *testing.T) {
	set := taskSet(t, 200)
	wrong := map[string]bool{"gsm8k.test.3": true, "gsm8k.test.150": true}
	server := &pacedServer{taskServer: taskServer{"current": {}, "candidate": wrong}, running: map[string]int{}, most: map[string]int{}}
	comparison, err := Compare(context.Background(), server, set, Target{SubjectID: "current"}, Target{SubjectID: "candidate"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{"current", "candidate"} {
		if server.most[subject] < 2 || server.most[subject] > taskAtOnce {
			t.Fatalf("%s was asked %d items at once, want 2 to %d", subject, server.most[subject], taskAtOnce)
		}
	}
	one := func(index int, scores []float64) float64 { return scores[index] }
	if comparison.Task.ReferenceScore != 1 || comparison.Task.CandidateScore != 0.99 || one(3, comparison.Scores) != 0 || one(150, comparison.Scores) != 0 || one(4, comparison.Scores) != 1 {
		t.Fatalf("task = %+v", comparison.Task)
	}
	// Both rows record the concurrency the two servers were asked at.
	for _, reference := range []bool{false, true} {
		if got := comparison.Payload("qwen", reference).Distribution.ItemsAtOnce; got != taskAtOnce {
			t.Fatalf("reference=%v: items_at_once = %d", reference, got)
		}
	}
	sequential, err := Compare(context.Background(), server.taskServer, set, Target{SubjectID: "current"}, Target{SubjectID: "candidate"}, nil)
	if err != nil || sequential.ResultDigest != comparison.ResultDigest {
		t.Fatalf("the same answers gave another record: %v", err)
	}

	failing := &pacedServer{taskServer: server.taskServer, running: map[string]int{}, most: map[string]int{}, failAt: "candidate/gsm8k.test.10"}
	if _, err := Compare(context.Background(), failing, set, Target{SubjectID: "current"}, Target{SubjectID: "candidate"}, nil); err == nil || !strings.Contains(err.Error(), "candidate revision") {
		t.Fatalf("a failed item: %v", err)
	}
	if failing.answered >= 2*len(set.Prompts) {
		t.Fatalf("the failure did not stop the rest: %d answers", failing.answered)
	}
}

// A setup that changes answers passes only when its answers are shown to hold
// within 1 point: a 1-point loss whose interval reaches −2 points is "no
// measurable difference" by the ±3-point rule and not shown to hold by this
// one, and it never passes; an even split holds.
func TestASetupThatChangesAnswersMustShowThemHeldWithinOnePoint(t *testing.T) {
	set := taskSet(t, 1000)
	ids := func(ranges ...[2]int) map[string]bool {
		out := map[string]bool{}
		for _, span := range ranges {
			for index := span[0]; index < span[1]; index++ {
				out[fmt.Sprintf("gsm8k.test.%d", index)] = true
			}
		}
		return out
	}
	server := taskServer{"current": ids([2]int{0, 30}), "lost-a-point": ids([2]int{10, 30}, [2]int{100, 120}), "even": ids([2]int{5, 30}, [2]int{100, 105})}
	current := Target{SubjectID: "current"}
	for _, test := range []struct {
		candidate, rule, verdict, code string
		held                           bool
		passed                         bool
	}{
		{"lost-a-point", RuleWithinMargin, TaskNoDifference, CodeTaskNoDifference, false, true},
		{"lost-a-point", RuleHeld, TaskNotShownHeld, CodeTaskNotShownHeld, true, false},
		{"even", RuleHeld, TaskHeld, CodeTaskHeld, true, true},
	} {
		var options []CompareOption
		if test.held {
			options = append(options, RequireAnswersHeld())
		}
		comparison, err := Compare(context.Background(), server, set, current, Target{SubjectID: test.candidate}, nil, options...)
		if err != nil {
			t.Fatal(err)
		}
		task, payload := comparison.Task, comparison.Payload("qwen", false)
		if task.Rule != test.rule || task.Verdict != test.verdict || comparison.Eval.Code != test.code || payload.Passed != test.passed || task.ServersPerSide != 1 {
			t.Fatalf("%s %s: task=%+v eval=%+v passed=%v", test.candidate, test.rule, task, comparison.Eval, payload.Passed)
		}
		if payload.Distribution.TaskVerdict != test.code || payload.Distribution.TaskRule != test.rule || payload.Distribution.TaskServers != 1 {
			t.Fatalf("%s %s: candidate row distribution %+v", test.candidate, test.rule, payload.Distribution)
		}
		if reference := comparison.Payload("qwen", true).Distribution; reference.TaskVerdict != "" {
			t.Fatalf("the reference's row carries a verdict: %+v", reference)
		}
	}
	notShown, _ := Compare(context.Background(), server, set, current, Target{SubjectID: "lost-a-point"}, nil, RequireAnswersHeld())
	if notShown.Eval.Verdict != releaseguard.QualityUnknown || !strings.Contains(notShown.Eval.Message, "not shown to hold within 1 point on one server each") {
		t.Fatalf("eval = %+v", notShown.Eval)
	}
	for name, test := range map[string]struct {
		task TaskComparison
		want string
	}{
		"whole interval below zero": {TaskComparison{Items: 1000, Low: -0.009, High: -0.001}, TaskWorse},
		"whole interval above zero": {TaskComparison{Items: 1000, Low: 0.001, High: 0.02}, TaskBetter},
		"one item":                  {TaskComparison{Items: 1}, TaskNotShownHeld},
	} {
		if got := RequireHeld(test.task).Verdict; got != test.want {
			t.Errorf("%s: %s, want %s", name, got, test.want)
		}
	}
}
