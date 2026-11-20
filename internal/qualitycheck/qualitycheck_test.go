package qualitycheck

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/releaseguard"
)

func TestDefaultSuiteIsPinnedByDigest(t *testing.T) {
	body, err := os.ReadFile("suites/infercrane-default-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := Digest(body); got != DefaultSuiteDigest {
		t.Fatalf("default suite digest = %s, pinned %s; bump the suite version and the pinned digest together", got, DefaultSuiteDigest)
	}
	set := Default()
	if set.Digest != DefaultSuiteDigest || set.Name != "infercrane-default" || set.Version != "1" || len(set.Prompts) < 20 || set.MaxTokens != 64 {
		t.Fatalf("default suite = %+v", set)
	}
	if set.SuiteVersion() != "1@"+DefaultSuiteDigest {
		t.Fatalf("suite version = %s", set.SuiteVersion())
	}
	if string(DefaultSuiteBytes()) != string(body) {
		t.Fatal("embedded bytes differ from the suite file")
	}
}

func TestDecodeEvalSetRejectsUnsafeInput(t *testing.T) {
	valid := `{"schema":"infercrane.dev/quality-eval-set/v1","name":"support","version":"3","prompts":[{"id":"a","prompt":"hi"}]}`
	set, err := DecodeEvalSet([]byte(valid))
	if err != nil || set.MaxTokens != 64 || set.Digest != Digest([]byte(valid)) {
		t.Fatalf("valid eval set: %+v %v", set, err)
	}
	for name, body := range map[string]string{
		"unknown field":   `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","prompts":[{"id":"a","prompt":"hi","expected":"x"}]}`,
		"schema":          `{"schema":"other","name":"s","version":"1","prompts":[{"id":"a","prompt":"hi"}]}`,
		"no prompts":      `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","prompts":[]}`,
		"duplicate id":    `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","prompts":[{"id":"a","prompt":"hi"},{"id":"a","prompt":"yo"}]}`,
		"bad id":          `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","prompts":[{"id":"../a","prompt":"hi"}]}`,
		"empty prompt":    `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","prompts":[{"id":"a","prompt":"  "}]}`,
		"at in version":   `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1@x","prompts":[{"id":"a","prompt":"hi"}]}`,
		"max tokens":      `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","max_tokens":4096,"prompts":[{"id":"a","prompt":"hi"}]}`,
		"trailing":        valid + `{}`,
		"too large":       strings.Repeat(" ", MaxEvalSetBytes+1),
		"prompt too long": `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","prompts":[{"id":"a","prompt":"` + strings.Repeat("x", maxPromptBytes+1) + `"}]}`,
	} {
		if _, err := DecodeEvalSet([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestAgreementIgnoresFormattingAndReasoning(t *testing.T) {
	for _, tc := range []struct {
		reference, candidate string
		want                 float64
	}{
		{"843", "843.", 1},
		{"Tokyo", "tokyo", 1},
		{"<think>add them</think>843", "843", 1},
		{"<think>unterminated", "", 1},
		{"the template opened this reasoning</think>843", "843", 1},
		{"Paris", "Paris, France", 2.0 / 3.0},
		{"42", "41", 0},
		{"", "something", 0},
		{"red, green, blue", "blue green red", 1},
	} {
		if got := Agreement(tc.reference, tc.candidate); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("Agreement(%q, %q) = %v, want %v", tc.reference, tc.candidate, got, tc.want)
		}
	}
}

func TestClassifyUsesReleaseGuardBands(t *testing.T) {
	for score, want := range map[float64]string{1: releaseguard.QualityPass, 0.96: releaseguard.QualityPass, 0.951: releaseguard.QualityPass, 0.93: releaseguard.QualityWatch, 0.901: releaseguard.QualityWatch, 0.89: releaseguard.QualityBlock, 0: releaseguard.QualityBlock} {
		if got := VerdictForScore(score); got.Verdict != want {
			t.Errorf("score %.2f verdict = %s (%s), want %s", score, got.Verdict, got.Message, want)
		}
	}
}

type scriptedCompleter struct {
	mu      sync.Mutex
	answers map[string]map[string]string
	calls   []string
	fail    string
}

func (c *scriptedCompleter) Complete(_ context.Context, target Target, _ EvalSet, prompt Prompt) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, target.SubjectID+"/"+prompt.ID)
	if c.fail == target.SubjectID {
		return "", errors.New("unreachable")
	}
	return c.answers[target.SubjectID][prompt.ID], nil
}

func testSet(t *testing.T) EvalSet {
	t.Helper()
	set, err := DecodeEvalSet([]byte(`{"schema":"infercrane.dev/quality-eval-set/v1","name":"support","version":"1","prompts":[{"id":"a","prompt":"1"},{"id":"b","prompt":"2"},{"id":"c","prompt":"3"},{"id":"d","prompt":"4"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestCompareScoresCandidateAgainstReferenceInTurns(t *testing.T) {
	set := testSet(t)
	completer := &scriptedCompleter{answers: map[string]map[string]string{
		"ref":  {"a": "843", "b": "Tokyo", "c": "positive", "d": "SELECT * FROM orders;"},
		"cand": {"a": "843", "b": "Tokyo", "c": "negative", "d": "select * from orders"},
	}}
	stamp := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	comparison, err := Compare(context.Background(), completer, set, Target{SubjectID: "ref"}, Target{SubjectID: "cand"}, func() time.Time { return stamp })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(completer.calls, ",") != "ref/a,cand/a,ref/b,cand/b,ref/c,cand/c,ref/d,cand/d" {
		t.Fatalf("calls were not interleaved: %v", completer.calls)
	}
	if comparison.Score != 0.75 || comparison.Eval.Verdict != releaseguard.QualityBlock || len(comparison.Scores) != 4 {
		t.Fatalf("comparison = %+v", comparison)
	}
	candidate := comparison.Payload("svc-opt-1", false)
	if err = candidate.Validate(); err != nil {
		t.Fatal(err)
	}
	if candidate.Passed || candidate.Score != 0.75 || candidate.RevisionID != "cand" || candidate.Evaluator != Evaluator || candidate.SuiteVersion != set.SuiteVersion() || candidate.Distribution.PairingDigest != set.PairingDigest() {
		t.Fatalf("candidate payload = %+v", candidate)
	}
	reference := comparison.Payload("svc-opt-0", true)
	if err = reference.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reference.Passed || reference.Score != 1 || reference.RevisionID != "ref" || reference.ArtifactDigest != candidate.ArtifactDigest {
		t.Fatalf("reference payload = %+v", reference)
	}
	encoded, _ := json.Marshal(candidate)
	for _, answer := range []string{"Tokyo", "negative", "orders"} {
		if strings.Contains(string(encoded), answer) {
			t.Fatalf("evidence payload contains answer text %q", answer)
		}
	}
}

func TestCompareAgainstItselfAsksOnce(t *testing.T) {
	set := testSet(t)
	completer := &scriptedCompleter{answers: map[string]map[string]string{"ref": {"a": "1", "b": "2", "c": "3", "d": "4"}}}
	comparison, err := Compare(context.Background(), completer, set, Target{SubjectID: "ref"}, Target{SubjectID: "ref"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(completer.calls) != 4 || comparison.Score != 1 || comparison.Eval.Verdict != releaseguard.QualityPass {
		t.Fatalf("self comparison = %+v calls %v", comparison, completer.calls)
	}
}

func TestCompareFailsWhenARevisionDoesNotAnswer(t *testing.T) {
	set := testSet(t)
	completer := &scriptedCompleter{fail: "cand", answers: map[string]map[string]string{"ref": {"a": "1"}}}
	if _, err := Compare(context.Background(), completer, set, Target{SubjectID: "ref"}, Target{SubjectID: "cand"}, nil); err == nil || !strings.Contains(err.Error(), "candidate revision") {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPCompleterSendsGreedyBoundedRequest(t *testing.T) {
	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer worker-key" {
			http.Error(w, "wrong request", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &seen)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"  843 "}}]}`))
	}))
	defer server.Close()
	set := testSet(t)
	answer, err := HTTPCompleter{}.Complete(context.Background(), Target{Endpoint: server.URL, APIKey: "worker-key", Model: "qwen", SubjectID: "r"}, set, Prompt{ID: "a", System: "be brief", Prompt: "What is 487 + 356?"})
	if err != nil || answer != "843" {
		t.Fatalf("answer = %q, %v", answer, err)
	}
	if seen["temperature"] != float64(0) || seen["top_p"] != float64(1) || seen["seed"] != float64(Seed) || seen["max_tokens"] != float64(64) || seen["stream"] != false || seen["model"] != "qwen" {
		t.Fatalf("request = %v", seen)
	}
	if messages, _ := seen["messages"].([]any); len(messages) != 2 {
		t.Fatalf("messages = %v", seen["messages"])
	}
	if kwargs, _ := seen["chat_template_kwargs"].(map[string]any); kwargs["enable_thinking"] != false {
		t.Fatalf("chat_template_kwargs = %v, want reasoning turned off", seen["chat_template_kwargs"])
	}
}

func TestHTTPCompleterReturnsNoTextWhenOnlyReasoningCameBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":null,"reasoning_content":"Let me add 487"},"finish_reason":"length"}]}`))
	}))
	defer server.Close()
	answer, err := HTTPCompleter{}.Complete(context.Background(), Target{Endpoint: server.URL, SubjectID: "r"}, testSet(t), Prompt{ID: "a", Prompt: "x"})
	if err != nil || answer != "" {
		t.Fatalf("answer = %q, %v; want no text and no error", answer, err)
	}
}

func TestCompareStopsWhenTheReferenceGivesNoAnswerText(t *testing.T) {
	set := testSet(t)
	completer := &scriptedCompleter{answers: map[string]map[string]string{
		"ref":  {"a": "843", "b": "<think>still adding", "c": "", "d": "4"},
		"cand": {"a": "843", "b": "<think>still adding", "c": "", "d": "4"},
	}}
	_, err := Compare(context.Background(), completer, set, Target{SubjectID: "ref"}, Target{SubjectID: "cand"}, nil)
	if !errors.Is(err, ErrReferenceUnanswered) {
		t.Fatalf("err = %v, want ErrReferenceUnanswered", err)
	}
	if !strings.Contains(err.Error(), "2 of 4 prompts within 64 tokens (b, c)") {
		t.Fatalf("err = %v, want the unanswered prompts named", err)
	}
	// Once the check cannot pass, only the reference is asked.
	if strings.Join(completer.calls, ",") != "ref/a,cand/a,ref/b,ref/c,ref/d" {
		t.Fatalf("calls = %v", completer.calls)
	}
}

func TestCompareAgainstItselfNeedsAnswerText(t *testing.T) {
	set := testSet(t)
	completer := &scriptedCompleter{answers: map[string]map[string]string{"ref": {"a": "", "b": "", "c": "", "d": ""}}}
	if _, err := Compare(context.Background(), completer, set, Target{SubjectID: "ref"}, Target{SubjectID: "ref"}, nil); !errors.Is(err, ErrReferenceUnanswered) {
		t.Fatalf("err = %v, want ErrReferenceUnanswered", err)
	}
}

func TestPromptListNamesFiveAndCountsTheRest(t *testing.T) {
	if got := promptList([]string{"a", "b", "c", "d", "e", "f", "g"}); got != "a, b, c, d, e and 2 more" {
		t.Fatalf("promptList = %q", got)
	}
}

func TestHTTPCompleterRejectsErrorsAndEmptyAnswers(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"status":   func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "busy", http.StatusServiceUnavailable) },
		"no reply": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"choices":[]}`)) },
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://example.invalid/", http.StatusFound)
		},
		"too big": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + strings.Repeat("x", maxResponseBytes) + `"}}]}`))
		},
	} {
		server := httptest.NewServer(handler)
		if _, err := (HTTPCompleter{}).Complete(context.Background(), Target{Endpoint: server.URL, SubjectID: "r"}, testSet(t), Prompt{ID: "a", Prompt: "x"}); err == nil {
			t.Errorf("%s accepted", name)
		}
		server.Close()
	}
}
