package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/qualityevidence"
)

// forcedServer answers as vLLM does: an answer and the prompt's length, and
// prompt_logprobs when asked. shift moves the answer's positions.
func forcedServer(t *testing.T, shift float64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, forced := body["prompt_logprobs"]; !forced {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "an answer"}}}, "usage": map[string]any{"prompt_tokens": 6}})
			return
		}
		positions := []any{nil}
		for index := 1; index < 10; index++ {
			p := 0.7
			if index >= 6 {
				p -= shift
			}
			positions = append(positions, map[string]any{"5": map[string]any{"logprob": math.Log(p)}, "9": map[string]any{"logprob": math.Log(0.2)}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}, "prompt_logprobs": positions})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestEvaluationDivergenceReadsTwoServersWithoutAControlPlane(t *testing.T) {
	reference, candidate := forcedServer(t, 0), forcedServer(t, 0.3)
	output, err := captureStdout(t, func() error {
		return evaluationCommand(context.Background(), []string{"divergence", "--reference", reference.URL, "--candidate", candidate.URL, "--model", "m", "--output", "json"})
	})
	var divergence qualityevidence.Divergence
	if err != nil || json.Unmarshal([]byte(output), &divergence) != nil {
		t.Fatalf("output=%q err=%v", output, err)
	}
	if divergence.Method != qualityevidence.DivergenceMethod || divergence.PrefillMeanKL != 0 || divergence.GenerationMeanKL <= 0 || divergence.Prompts == 0 {
		t.Fatalf("divergence = %+v", divergence)
	}
	// Without --eval-set the long-answer prompts are read, each answer capped.
	if divergence.PromptSet != "infercrane-divergence" || divergence.Prompts != 32 || divergence.MaxAnswerTokens != 256 {
		t.Fatalf("default prompts: %+v", divergence)
	}
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, forced := body["prompt_logprobs"]; forced {
			http.Error(w, "unknown field", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "an answer"}}}, "usage": map[string]any{"prompt_tokens": 6}})
	}))
	defer refusing.Close()
	err = evaluationCommand(context.Background(), []string{"divergence", "--reference", reference.URL, "--candidate", refusing.URL, "--model", "m"})
	if err == nil || !strings.Contains(err.Error(), "divergence is unknown") {
		t.Fatalf("a server without prompt log probabilities: %v", err)
	}
	if err = evaluationCommand(context.Background(), []string{"divergence", "--reference", reference.URL}); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("missing flags: %v", err)
	}
}
