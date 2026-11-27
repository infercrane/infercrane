package qualitycheck

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPositionKLIsZeroForTheSameDistributionAndGrowsWithTheShift(t *testing.T) {
	same := map[string]float64{"1": math.Log(0.7), "2": math.Log(0.2), "3": math.Log(0.05)}
	if kl := PositionKL(same, same, 16); kl > 1e-12 {
		t.Fatalf("identical distributions: KL = %g", kl)
	}
	// Over the tokens {1, 2} and the rest: p = (0.7, 0.2, 0.1), q = (0.5, 0.4, 0.1).
	shifted := map[string]float64{"1": math.Log(0.5), "2": math.Log(0.4), "3": math.Log(0.05)}
	want := 0.7*math.Log(0.7/0.5) + 0.2*math.Log(0.2/0.4) + 0.05*math.Log(0.05/0.05) + 0.05*math.Log(0.05/0.05)
	if kl := PositionKL(same, shifted, 16); math.Abs(kl-want) > 1e-9 {
		t.Fatalf("KL = %g, want %g", kl, want)
	}
	// Only the reference's top tokens are compared; the rest is one bucket.
	top1 := 0.7*math.Log(0.7/0.5) + 0.3*math.Log(0.3/0.5)
	if kl := PositionKL(same, shifted, 1); math.Abs(kl-top1) > 1e-9 {
		t.Fatalf("top-1 KL = %g, want %g", kl, top1)
	}
	// A token the candidate did not return shares its unreturned mass.
	missing := map[string]float64{"2": math.Log(0.6), "3": math.Log(0.3)}
	if kl := PositionKL(same, missing, 16); kl < 1 {
		t.Fatalf("the reference's likeliest token got almost no mass from the candidate, yet KL = %g", kl)
	}
}

// vllmStandIn answers the two requests Forced sends as vLLM does: the prompt's
// length in usage, and prompt_logprobs for the prompt and the answer.
func vllmStandIn(t *testing.T, prefix int, shift float64, refuse bool) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		if _, forced := body["prompt_logprobs"]; !forced {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "x"}}}, "usage": map[string]any{"prompt_tokens": prefix}})
			return
		}
		if refuse {
			http.Error(w, `{"error":"unknown field prompt_logprobs"}`, http.StatusBadRequest)
			return
		}
		positions := []any{nil}
		for index := 1; index < prefix+4; index++ {
			p := 0.8
			if index >= prefix {
				p -= shift
			}
			positions = append(positions, map[string]any{"11": map[string]any{"logprob": math.Log(p), "rank": 1}, "12": map[string]any{"logprob": math.Log(0.1), "rank": 2}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}, "prompt_logprobs": positions})
	}))
	t.Cleanup(server.Close)
	return server, &bodies
}

func TestForcedReadsThePromptAndTheAnswerAsOneSequence(t *testing.T) {
	server, bodies := vllmStandIn(t, 5, 0, false)
	prompt := Prompt{ID: "p1", Prompt: "Name a colour."}
	forced, err := HTTPCompleter{}.Forced(context.Background(), Target{Endpoint: server.URL, Model: "m", SubjectID: "rev"}, EvalSet{}, prompt, "Blue.", 16)
	if err != nil {
		t.Fatal(err)
	}
	if forced.PrefixTokens != 5 || len(forced.Positions) != 8 {
		t.Fatalf("forced = %+v", forced)
	}
	request := (*bodies)[1]
	messages, _ := request["messages"].([]any)
	last, _ := messages[len(messages)-1].(map[string]any)
	if request["continue_final_message"] != true || request["add_generation_prompt"] != false || request["prompt_logprobs"] != float64(16) || last["role"] != "assistant" || last["content"] != "Blue." {
		t.Fatalf("forced request = %v", request)
	}
	if len(prompt.requestMessages()) != 1 {
		t.Fatal("the prompt's own messages were appended to")
	}
	refusing, _ := vllmStandIn(t, 5, 0, true)
	if _, err = (HTTPCompleter{}).Forced(context.Background(), Target{Endpoint: refusing.URL, Model: "m"}, EvalSet{}, prompt, "Blue.", 16); !errors.Is(err, ErrForcedUnsupported) {
		t.Fatalf("a server that refuses prompt_logprobs: err = %v", err)
	}
	if _, err = (HTTPCompleter{}).Forced(context.Background(), Target{Endpoint: server.URL, Model: "m", Dialect: DialectOpenAI}, EvalSet{}, prompt, "Blue.", 16); !errors.Is(err, ErrForcedUnsupported) {
		t.Fatalf("a closed API: err = %v", err)
	}
}

func TestMeasureDivergenceKeepsThePromptAndTheAnswerApart(t *testing.T) {
	reference, _ := vllmStandIn(t, 5, 0, false)
	candidate, _ := vllmStandIn(t, 5, 0.3, false)
	set := EvalSet{Prompts: []Prompt{{ID: "a", Prompt: "one"}, {ID: "b", Prompt: "two"}}}
	completer := HTTPCompleter{}
	divergence, err := MeasureDivergence(context.Background(), completer, completer, set, Target{Endpoint: reference.URL, Model: "m", SubjectID: "ref", Tokenizer: "t"}, Target{Endpoint: candidate.URL, Model: "m", SubjectID: "cand", Tokenizer: "t"})
	if err != nil {
		t.Fatal(err)
	}
	// Positions 1 to 4 are the prompt's (identical), 5 to 8 the answer's.
	if divergence.Prompts != 2 || divergence.PrefillTokens != 8 || divergence.GenerationTokens != 8 || divergence.PrefillMeanKL != 0 || divergence.GenerationMeanKL <= 0 || divergence.GenerationMaxKL < divergence.GenerationMeanKL || divergence.TopK != DivergenceTopK {
		t.Fatalf("divergence = %+v", divergence)
	}
	same, err := MeasureDivergence(context.Background(), completer, completer, set, Target{Endpoint: reference.URL, Model: "m", SubjectID: "ref", Tokenizer: "t"}, Target{Endpoint: reference.URL, Model: "m", SubjectID: "copy", Tokenizer: "t"})
	if err != nil || same.GenerationMeanKL != 0 {
		t.Fatalf("same server: %+v %v", same, err)
	}
	refusing, _ := vllmStandIn(t, 5, 0, true)
	if _, err = MeasureDivergence(context.Background(), completer, completer, set, Target{Endpoint: reference.URL, Model: "m", Tokenizer: "t"}, Target{Endpoint: refusing.URL, Model: "m", Tokenizer: "t"}); !errors.Is(err, ErrForcedUnsupported) {
		t.Fatalf("refusing candidate: err = %v", err)
	}
}

func TestOnlyTheCandidatesPayloadCarriesTheDivergence(t *testing.T) {
	reference, _ := vllmStandIn(t, 5, 0, false)
	candidate, _ := vllmStandIn(t, 5, 0.3, false)
	set := Default()
	completer := HTTPCompleter{}
	divergence, err := MeasureDivergence(context.Background(), completer, completer, set, Target{Endpoint: reference.URL, Model: "m", Tokenizer: "t"}, Target{Endpoint: candidate.URL, Model: "m", Tokenizer: "t"})
	if err != nil {
		t.Fatal(err)
	}
	comparison := Comparison{Set: set, ReferenceID: "ref", CandidateRevisionID: "cand", Scores: make([]float64, divergence.Prompts), Score: 1, Divergence: divergence,
		ResultDigest: "sha256:" + strings.Repeat("a", 64), EvaluatedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	if got := comparison.Payload("chat", false).Distribution.Divergence; got != divergence {
		t.Fatalf("candidate payload divergence = %+v", got)
	}
	if got := comparison.Payload("chat", true).Distribution.Divergence; got != nil {
		t.Fatalf("the reference's own evidence carries a divergence: %+v", got)
	}
	payload := comparison.Payload("chat", false)
	payload.Distribution.Scores = make([]float64, payload.SampleCount)
	if err = payload.Validate(); err != nil {
		t.Fatalf("a measured divergence must validate: %v", err)
	}
	broken := *divergence
	broken.GenerationMeanKL = math.NaN()
	payload.Distribution.Divergence = &broken
	if err = payload.Validate(); err == nil {
		t.Fatal("a NaN divergence validated")
	}
}

// The long-answer prompts are pinned like the default suite, so readings on
// other prompts never carry their name.
func TestTheLongAnswerPromptsArePinnedByDigest(t *testing.T) {
	if got := Digest(divergenceSuite); got != DivergenceSetDigest {
		t.Fatalf("divergence prompts digest = %s, pinned %s; bump the set's version and the pinned digest together", got, DivergenceSetDigest)
	}
	set := DivergenceSet()
	if set.Name != "infercrane-divergence" || set.Version != "1" || len(set.Prompts) != maxDivergencePrompts || set.MaxTokens != DivergenceAnswerTokens || set.Graded() {
		t.Fatalf("set = %s %s, %d prompts, %d tokens, grading %q", set.Name, set.Version, len(set.Prompts), set.MaxTokens, set.Grading)
	}
	for _, prompt := range set.Prompts {
		if prompt.HasTools() || !strings.Contains(prompt.Prompt, "150 words") {
			t.Fatalf("prompt %s does not ask for a long answer without tools", prompt.ID)
		}
	}
}

// A task set's worked answers are long, so its items are read; any other set
// gives way to the long-answer prompts. Every answer is capped.
func TestDivergenceReadsTheTaskSetOrTheLongAnswerPrompts(t *testing.T) {
	task := EvalSet{Name: "tasks", Version: "1", Grading: GradingAnswerLine, MaxTokens: 1024, Prompts: []Prompt{{ID: "q1", Prompt: "Work it out. Answer: <n>"}}}
	if chosen := DivergencePrompts(task); chosen.Name != "tasks" || chosen.MaxTokens != DivergenceAnswerTokens || len(chosen.Prompts) != 1 {
		t.Fatalf("task set: %s, %d tokens", chosen.Name, chosen.MaxTokens)
	}
	if task.MaxTokens != 1024 {
		t.Fatal("the campaign's own set was changed")
	}
	graded := EvalSet{Name: "support", Version: "1", Grading: GradingExactMatch, MaxTokens: 32, Prompts: []Prompt{{ID: "a", Prompt: "Refund window?", Expected: []string{"30"}}}}
	for _, set := range []EvalSet{Default(), graded} {
		if chosen := DivergencePrompts(set); chosen.Name != "infercrane-divergence" || chosen.MaxTokens != DivergenceAnswerTokens {
			t.Fatalf("%s: read %s", set.Name, chosen.Name)
		}
	}
}

// The reading names the prompts and the cap, and asks each answer at most
// that long, so a reader sees what the sample was.
func TestTheReadingNamesItsPromptsAndCapsTheAnswers(t *testing.T) {
	reference, bodies := vllmStandIn(t, 5, 0, false)
	candidate, _ := vllmStandIn(t, 5, 0.3, false)
	set := EvalSet{Name: "tasks", Version: "2", Digest: "sha256:" + strings.Repeat("d", 64), Grading: GradingAnswerLine, MaxTokens: 2048, Prompts: []Prompt{{ID: "a", Prompt: "one"}}}
	completer := HTTPCompleter{}
	divergence, err := MeasureDivergence(context.Background(), completer, completer, set, Target{Endpoint: reference.URL, Model: "m", Tokenizer: "t"}, Target{Endpoint: candidate.URL, Model: "m", Tokenizer: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if divergence.PromptSet != "tasks" || divergence.PromptSetVersion != "2@sha256:"+strings.Repeat("d", 64) || divergence.MaxAnswerTokens != DivergenceAnswerTokens {
		t.Fatalf("divergence = %+v", divergence)
	}
	if answer := (*bodies)[0]; answer["max_tokens"] != float64(DivergenceAnswerTokens) {
		t.Fatalf("the reference's answer was asked with %v tokens", answer["max_tokens"])
	}
}

// Token IDs name strings only within one tokenizer: two servers are compared
// only when both are known to read with the same one, and no server is
// asked anything otherwise.
func TestMeasureDivergenceRefusesDifferentOrUnknownTokenizers(t *testing.T) {
	reference, _ := vllmStandIn(t, 5, 0, false)
	candidate, _ := vllmStandIn(t, 5, 0.3, false)
	set := EvalSet{Prompts: []Prompt{{ID: "a", Prompt: "one"}}}
	completer := HTTPCompleter{}
	for name, pair := range map[string][2]string{"different": {"a", "b"}, "both unknown": {"", ""}, "one unknown": {"a", ""}} {
		divergence, err := MeasureDivergence(context.Background(), completer, completer, set,
			Target{Endpoint: reference.URL, Model: "m", Tokenizer: pair[0]}, Target{Endpoint: candidate.URL, Model: "m", Tokenizer: pair[1]})
		if !errors.Is(err, ErrDifferentTokenizers) || divergence != nil {
			t.Fatalf("%s: divergence = %+v err = %v", name, divergence, err)
		}
	}
	divergence, err := MeasureDivergence(context.Background(), completer, completer, set,
		Target{Endpoint: reference.URL, Model: "m", Tokenizer: "a"}, Target{Endpoint: candidate.URL, Model: "m", Tokenizer: "a"})
	if err != nil || divergence == nil || divergence.Prompts != 1 {
		t.Fatalf("same tokenizer: divergence = %+v err = %v", divergence, err)
	}
}
