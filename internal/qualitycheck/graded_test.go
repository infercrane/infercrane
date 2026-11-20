package qualitycheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/releaseguard"
)

// gradedSet builds a graded set of n arithmetic prompts whose expected answer
// is the prompt number.
func gradedSet(t *testing.T, n int, grading string) EvalSet {
	t.Helper()
	prompts := make([]map[string]any, 0, n)
	for i := range n {
		prompts = append(prompts, map[string]any{"id": fmt.Sprintf("p%d", i), "prompt": fmt.Sprintf("Say %d", i), "expected": []string{fmt.Sprint(i)}})
	}
	body, _ := json.Marshal(map[string]any{"schema": EvalSetSchema, "name": "support-graded", "version": "1", "grading": grading, "prompts": prompts})
	set, err := DecodeEvalSet(body)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestDecodeGradedEvalSet(t *testing.T) {
	set := gradedSet(t, 3, GradingExactMatch)
	if !set.Graded() || len(set.Prompts[1].Expected) != 1 || set.Prompts[1].Expected[0] != "1" {
		t.Fatalf("graded set = %+v", set)
	}
	for name, body := range map[string]string{
		"expected without grading": `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","prompts":[{"id":"a","prompt":"hi","expected":["x"]}]}`,
		"grading without expected": `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","grading":"exact_match","prompts":[{"id":"a","prompt":"hi"}]}`,
		"unknown grading":          `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","grading":"llm_judge","prompts":[{"id":"a","prompt":"hi","expected":["x"]}]}`,
		"empty expected answer":    `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","grading":"token_f1","prompts":[{"id":"a","prompt":"hi","expected":[" .. "]}]}`,
		"too many expected":        `{"schema":"infercrane.dev/quality-eval-set/v1","name":"s","version":"1","grading":"token_f1","prompts":[{"id":"a","prompt":"hi","expected":["1","2","3","4","5","6","7","8","9"]}]}`,
	} {
		if _, err := DecodeEvalSet([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestGradeScoresAgainstEveryExpectedAnswer(t *testing.T) {
	prompt := Prompt{ID: "a", Expected: []string{"Paris", "Paris, France"}}
	for _, tc := range []struct {
		grading, answer string
		want            float64
	}{
		{GradingExactMatch, "paris.", 1},
		{GradingExactMatch, "Paris, France!", 1},
		{GradingExactMatch, "It is Paris", 0},
		{GradingTokenF1, "It is Paris", 0.5},
		{GradingTokenF1, "", 0},
		{GradingExactMatch, "<think>Lyon?</think>", 0},
	} {
		if got := Grade(tc.grading, prompt, tc.answer); got != tc.want {
			t.Errorf("Grade(%s, %q) = %v, want %v", tc.grading, tc.answer, got, tc.want)
		}
	}
}

func TestCompareGradedScoresBothSidesAndStopsOnFailedRequests(t *testing.T) {
	set := gradedSet(t, 4, GradingExactMatch)
	completer := &scriptedCompleter{answers: map[string]map[string]string{
		"binding:closed": {"p0": "0", "p1": "1", "p2": "2", "p3": "three"},
		"rev-open":       {"p0": "0", "p1": "", "p2": "2", "p3": "3"},
	}}
	comparison, err := CompareGraded(context.Background(), completer, set, Target{SubjectID: "binding:closed", Dialect: DialectOpenAI}, Target{SubjectID: "rev-open"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(comparison.ReferenceScores) != "[1 1 1 0]" || fmt.Sprint(comparison.CandidateScores) != "[1 0 1 1]" {
		t.Fatalf("scores reference=%v candidate=%v", comparison.ReferenceScores, comparison.CandidateScores)
	}
	reference, candidate := comparison.Payload("support-bot", true, false), comparison.Payload("qwen-opt", false, true)
	if reference.RevisionID != "binding:closed" || reference.Deployment != "support-bot" || !reference.Passed || reference.Score != 0.75 || reference.Evaluator != GradedEvaluator {
		t.Fatalf("reference payload = %+v", reference)
	}
	if candidate.RevisionID != "rev-open" || candidate.Score != 0.75 || reference.Distribution.PairingDigest != candidate.Distribution.PairingDigest || reference.ArtifactDigest != candidate.ArtifactDigest {
		t.Fatalf("candidate payload = %+v", candidate)
	}
	if err := reference.Validate(); err != nil {
		t.Fatalf("reference payload invalid: %v", err)
	}

	failing := &scriptedCompleter{answers: completer.answers, fail: "binding:closed"}
	if _, err := CompareGraded(context.Background(), failing, set, Target{SubjectID: "binding:closed"}, Target{SubjectID: "rev-open"}, nil); err == nil || !strings.Contains(err.Error(), "current API") {
		t.Fatalf("a failed closed-API request was scored: %v", err)
	}
	if _, err := CompareGraded(context.Background(), completer, testSet(t), Target{SubjectID: "binding:closed"}, Target{SubjectID: "rev-open"}, nil); err == nil {
		t.Fatal("an ungraded set was graded")
	}
}

func scores(n int, value func(int) float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = value(i)
	}
	return out
}

func TestMigrationVerdictUsesThePairedBootstrap(t *testing.T) {
	set := gradedSet(t, 40, GradingExactMatch)
	verdict := func(reference, candidate []float64) releaseguard.QualityEval {
		comparison := GradedComparison{Set: set, ReferenceID: "binding:b", CandidateRevisionID: "rev", ReferenceScores: reference, CandidateScores: candidate, ResultDigest: set.Digest}
		return MigrationVerdict(comparison.Payload("ep", true, true), comparison.Payload("dep", false, true), 5)
	}
	allRight := scores(40, func(int) float64 { return 1 })
	if got := verdict(allRight, allRight); got.Verdict != releaseguard.QualityPass || got.Bootstrap == nil || got.Bootstrap.Status != "accept" {
		t.Fatalf("equal answers verdict = %+v", got)
	}
	worse := scores(40, func(i int) float64 { return float64(i % 2) })
	if got := verdict(allRight, worse); got.Verdict != releaseguard.QualityBlock {
		t.Fatalf("half wrong verdict = %+v", got)
	}
	// One miss in 40 straddles a 5% margin: not proven either way.
	oneMiss := scores(40, func(i int) float64 { return map[bool]float64{true: 0, false: 1}[i == 3] })
	if got := verdict(allRight, oneMiss); got.Verdict != releaseguard.QualityUnknown {
		t.Fatalf("one miss verdict = %+v", got)
	}
	small := gradedSet(t, 10, GradingExactMatch)
	comparison := GradedComparison{Set: small, ReferenceID: "binding:b", CandidateRevisionID: "rev", ReferenceScores: allRight[:10], CandidateScores: allRight[:10], ResultDigest: small.Digest}
	if got := MigrationVerdict(comparison.Payload("ep", true, true), comparison.Payload("dep", false, true), 5); got.Verdict != releaseguard.QualityUnknown {
		t.Fatalf("10 pairs verdict = %+v", got)
	}
	other := comparison.Payload("dep", false, true)
	other.Evaluator = "customer-evaluator"
	if got := MigrationVerdict(comparison.Payload("ep", true, true), other, 5); got.Verdict != releaseguard.QualityUnknown || got.Code != "quality_evidence_not_comparable" {
		t.Fatalf("different evaluators verdict = %+v", got)
	}
}

func TestDialectShapesTheRequest(t *testing.T) {
	for _, tc := range []struct {
		dialect           Dialect
		tokensField       string
		templateArguments bool
	}{
		{DialectModelServer, "max_tokens", true},
		{DialectOpenAI, "max_completion_tokens", false},
		{DialectOpenAICompatible, "max_tokens", false},
	} {
		var seen map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &seen)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
		}))
		if _, err := (HTTPCompleter{}).Complete(context.Background(), Target{Endpoint: server.URL, SubjectID: "s", Dialect: tc.dialect}, testSet(t), Prompt{ID: "a", Prompt: "x"}); err != nil {
			t.Fatal(err)
		}
		server.Close()
		_, hasTemplate := seen["chat_template_kwargs"]
		if seen[tc.tokensField] != float64(64) || hasTemplate != tc.templateArguments {
			t.Fatalf("dialect %q request = %v", tc.dialect, seen)
		}
		other := map[string]string{"max_tokens": "max_completion_tokens", "max_completion_tokens": "max_tokens"}[tc.tokensField]
		if _, present := seen[other]; present {
			t.Fatalf("dialect %q sent both token limits: %v", tc.dialect, seen)
		}
	}
}
