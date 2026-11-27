package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/infercrane/infercrane/internal/qualitycheck"
)

const evaluationDivergenceUsage = "usage: infercrane evaluation divergence --reference URL --candidate URL --model NAME [--api-key-env VAR] [--eval-set FILE] [--output text|json]"

// evaluationDivergenceCommand measures how far a candidate server's next-token
// distribution moved from a reference server's on the reference's own greedy
// answers: the check InferCrane runs on a change of numeric precision. It
// calls only the two servers given, so it runs against any OpenAI-compatible
// server that returns prompt log probabilities, without a control plane.
func evaluationDivergenceCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("evaluation divergence")
	reference := fs.String("reference", "", "base URL of the reference server, for example http://127.0.0.1:8000")
	candidate := fs.String("candidate", "", "base URL of the candidate server")
	model := fs.String("model", "", "the served model name both servers answer to")
	keyEnv := fs.String("api-key-env", "", "environment variable holding a bearer key both servers accept")
	evalSet := fs.String("eval-set", "", "eval set JSON whose prompts are read as given; default: the built-in long-answer prompts")
	output := fs.String("output", "text", "text or json")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *reference == "" || *candidate == "" || strings.TrimSpace(*model) == "" || (*output != "text" && *output != "json") {
		return errors.New(evaluationDivergenceUsage)
	}
	// Long answers give the answer positions a reading needs; a set named
	// here is read as given, each answer capped like the built-in ones.
	set := qualitycheck.DivergenceSet()
	if *evalSet != "" {
		body, err := os.ReadFile(*evalSet)
		if err != nil {
			return fmt.Errorf("read eval set: %w", err)
		}
		if set, err = qualitycheck.DecodeEvalSet(body); err != nil {
			return err
		}
	}
	key := ""
	if *keyEnv != "" {
		key = os.Getenv(*keyEnv)
	}
	completer := qualitycheck.HTTPCompleter{}
	// Both servers serve the one model --model names, so they read text with
	// its tokenizer; two different checkpoints are compared inside a
	// campaign, which records each one's tokenizer digest.
	tokenizer := "model:" + strings.TrimSpace(*model)
	divergence, err := qualitycheck.MeasureDivergence(ctx, completer, completer, set,
		qualitycheck.Target{Endpoint: *reference, APIKey: key, Model: *model, SubjectID: "reference", Tokenizer: tokenizer},
		qualitycheck.Target{Endpoint: *candidate, APIKey: key, Model: *model, SubjectID: "candidate", Tokenizer: tokenizer})
	if errors.Is(err, qualitycheck.ErrForcedUnsupported) {
		return fmt.Errorf("a server does not return prompt log probabilities for a chat request, so the divergence is unknown: %w", err)
	}
	if err != nil {
		return err
	}
	if *output == "json" {
		return printJSON(divergence)
	}
	fmt.Printf("KL divergence of the candidate from the reference, %d prompts of %s (answers up to %d tokens), top %d tokens per position (%s):\n",
		divergence.Prompts, divergence.PromptSet, divergence.MaxAnswerTokens, divergence.TopK, divergence.Method)
	fmt.Printf("  prompt (prefill): %.6f mean over %d positions\n", divergence.PrefillMeanKL, divergence.PrefillTokens)
	fmt.Printf("  answer (generation): %.6f mean, %.6f largest, over %d positions\n", divergence.GenerationMeanKL, divergence.GenerationMaxKL, divergence.GenerationTokens)
	fmt.Println("Evidence only: no threshold decides on it yet.")
	return nil
}
