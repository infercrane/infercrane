package qualitycheck

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"

	"github.com/infercrane/infercrane/internal/openaicompat"
	"github.com/infercrane/infercrane/internal/qualityevidence"
)

// A change of numeric precision can move a model's answers without changing
// many words, and two servers of one setup already differ in long answers
// under load, so word agreement cannot tell a lossy change from noise. Forced
// decoding can: both servers read the same sequence, the reference's prompt
// and its own greedy answer, and report their next-token distribution at
// every position. Their KL divergence is measured on identical inputs, so it
// does not depend on where two answers first part. The prompt's positions
// (prefill) and the answer's (generation) are kept apart.

const (
	// DivergenceTopK is how many of the reference's most likely tokens each
	// position compares; the rest of the vocabulary is one bucket.
	DivergenceTopK = 16
	// maxDivergencePrompts bounds the requests: three per prompt.
	maxDivergencePrompts = 32
	// DivergenceAnswerTokens caps each reference answer the check reads:
	// 32 answers of up to 256 tokens give up to 8,192 answer positions.
	DivergenceAnswerTokens = 256
	// minProbability keeps a bucket the candidate gave no mass finite.
	minProbability = 1e-10
)

// DivergenceSetDigest pins the embedded long-answer prompts. Changing the
// file without changing this digest and the set's version fails the package
// tests, so readings on different prompts never carry one name.
const DivergenceSetDigest = "sha256:ff796fcd0b05d5f17d92cd4a401a56851ef5fd428c1cb736d7b3c077a4c69106"

// The built-in prompts ask for a number or a word, so their answers give a
// divergence reading a few positions each. These ask for about 150 words.
//
//go:embed suites/infercrane-divergence-v1.json
var divergenceSuite []byte

// DivergenceSet returns the embedded prompts with long open answers.
func DivergenceSet() EvalSet {
	set, err := DecodeEvalSet(divergenceSuite)
	if err != nil {
		panic("embedded divergence prompts are invalid: " + err.Error())
	}
	return set
}

// DivergencePrompts is what the divergence check reads when a campaign's
// answers are checked with set: a task set's own items, whose worked answers
// are long, and otherwise the embedded long-answer prompts. Each answer is
// capped at DivergenceAnswerTokens.
func DivergencePrompts(set EvalSet) EvalSet {
	chosen := DivergenceSet()
	if set.Task() {
		chosen = set
	}
	return capAnswers(chosen)
}

func capAnswers(set EvalSet) EvalSet {
	if set.MaxTokens < 1 || set.MaxTokens > DivergenceAnswerTokens {
		set.MaxTokens = DivergenceAnswerTokens
	}
	return set
}

// ErrForcedUnsupported is a server that does not return prompt log
// probabilities for a chat request, or a target that is not a model server
// InferCrane runs. The divergence is then unknown.
var ErrForcedUnsupported = errors.New("the server does not return prompt log probabilities")

// ErrDifferentTokenizers is returned when the two servers are not known to
// read text with the same tokenizer and chat template (Target.Tokenizer
// unset or unequal). Token IDs of two tokenizers name different strings, so
// the divergence is then unknown: never a pass and never a failure.
var ErrDifferentTokenizers = errors.New("the two servers are not known to read text with the same tokenizer and chat template")

// Forced is one server's reading of a fixed sequence.
type Forced struct {
	// PrefixTokens is how many tokens the prompt takes with its chat
	// template; positions from there on are the answer's.
	PrefixTokens int
	// Positions holds, for every position after the first, the log
	// probability of each token the server returned, keyed by token ID.
	Positions []map[string]float64
}

// ForcedScorer reads a server's next-token distributions on the prompt
// followed by a given answer.
type ForcedScorer interface {
	Forced(ctx context.Context, target Target, set EvalSet, prompt Prompt, answer string, topK int) (Forced, error)
}

// MeasureDivergence asks the reference for its greedy answer, at most
// DivergenceAnswerTokens long, to each of up to maxDivergencePrompts prompts
// of set without tools, then reads both servers on that prompt and answer and
// averages the candidate's KL divergence from the reference per token. The
// reading names the prompts and the answer cap, and counts the answer
// positions, so a reader sees how large the sample is.
func MeasureDivergence(ctx context.Context, completer Completer, scorer ForcedScorer, set EvalSet, reference, candidate Target) (*qualityevidence.Divergence, error) {
	if completer == nil || scorer == nil || reference.Dialect != DialectModelServer || candidate.Dialect != DialectModelServer {
		return nil, ErrForcedUnsupported
	}
	if reference.Tokenizer == "" || reference.Tokenizer != candidate.Tokenizer {
		return nil, ErrDifferentTokenizers
	}
	set = capAnswers(set)
	var prefill, generation sum
	prompts := 0
	for _, prompt := range set.Prompts {
		if prompts == maxDivergencePrompts {
			break
		}
		if prompt.HasTools() {
			continue
		}
		answer, err := completer.Complete(ctx, reference, set, prompt)
		if err != nil {
			return nil, fmt.Errorf("reference revision: %w", err)
		}
		if answer.Content == "" {
			continue
		}
		expected, err := scorer.Forced(ctx, reference, set, prompt, answer.Content, DivergenceTopK)
		if err != nil {
			return nil, fmt.Errorf("reference revision: %w", err)
		}
		measured, err := scorer.Forced(ctx, candidate, set, prompt, answer.Content, DivergenceTopK)
		if err != nil {
			return nil, fmt.Errorf("candidate revision: %w", err)
		}
		// One model and revision tokenize one text alike; a mismatch means
		// the servers did not read the same sequence.
		if expected.PrefixTokens != measured.PrefixTokens || len(expected.Positions) != len(measured.Positions) || expected.PrefixTokens < 1 || len(expected.Positions) < expected.PrefixTokens {
			continue
		}
		for index := range expected.Positions {
			kl := PositionKL(expected.Positions[index], measured.Positions[index], DivergenceTopK)
			// Positions[index] predicts the token at index+1.
			if index+1 < expected.PrefixTokens {
				prefill.add(kl)
			} else {
				generation.add(kl)
			}
		}
		prompts++
	}
	if prompts == 0 || generation.count == 0 {
		return nil, errors.New("no prompt could be read the same way by both servers")
	}
	return &qualityevidence.Divergence{
		Method: qualityevidence.DivergenceMethod, TopK: DivergenceTopK, Prompts: prompts,
		PromptSet: set.Name, PromptSetVersion: set.SuiteVersion(), MaxAnswerTokens: set.MaxTokens,
		PrefillTokens: prefill.count, PrefillMeanKL: round6(prefill.mean()),
		GenerationTokens: generation.count, GenerationMeanKL: round6(generation.mean()), GenerationMaxKL: round6(generation.max),
	}, nil
}

type sum struct {
	total, max float64
	count      int
}

func (s *sum) add(value float64) {
	s.total += value
	s.max = math.Max(s.max, value)
	s.count++
}

func (s sum) mean() float64 {
	if s.count == 0 {
		return 0
	}
	return s.total / float64(s.count)
}

func round6(value float64) float64 { return math.Round(value*1e6) / 1e6 }

// PositionKL is the KL divergence of the candidate's distribution from the
// reference's at one position, over the reference's topK most likely tokens
// and one bucket for the rest. A token the candidate did not return shares
// the candidate's unreturned mass, never more than its least likely returned
// token. Coarser buckets can only lower a divergence, so this reads low
// rather than high.
func PositionKL(reference, candidate map[string]float64, topK int) float64 {
	tokens := make([]string, 0, len(reference))
	for token := range reference {
		tokens = append(tokens, token)
	}
	sort.Slice(tokens, func(i, j int) bool {
		if reference[tokens[i]] != reference[tokens[j]] {
			return reference[tokens[i]] > reference[tokens[j]]
		}
		return tokens[i] < tokens[j]
	})
	if len(tokens) > topK {
		tokens = tokens[:topK]
	}
	returned, least := 0.0, 1.0
	for _, logprob := range candidate {
		probability := math.Exp(logprob)
		returned += probability
		least = math.Min(least, probability)
	}
	missing := 0
	for _, token := range tokens {
		if _, ok := candidate[token]; !ok {
			missing++
		}
	}
	unreturned := math.Max(0, 1-returned)
	kl, pTop, qTop := 0.0, 0.0, 0.0
	for _, token := range tokens {
		p := math.Exp(reference[token])
		q, ok := 0.0, false
		if logprob, found := candidate[token]; found {
			q, ok = math.Exp(logprob), true
		}
		if !ok {
			q = math.Min(least, unreturned/float64(missing))
		}
		q = math.Max(q, minProbability)
		pTop, qTop = pTop+p, qTop+q
		if p > 0 {
			kl += p * math.Log(p/q)
		}
	}
	if pRest := 1 - pTop; pRest > 1e-12 {
		kl += pRest * math.Log(pRest/math.Max(1-qTop, minProbability))
	}
	return math.Max(0, kl)
}

// Forced reads a vLLM-compatible server twice: once for how many tokens the
// prompt takes, and once on the prompt followed by the answer as the final
// assistant message, with prompt_logprobs. A server that refuses either
// request's options returns ErrForcedUnsupported.
func (c HTTPCompleter) Forced(ctx context.Context, target Target, set EvalSet, prompt Prompt, answer string, topK int) (Forced, error) {
	if target.Dialect != DialectModelServer {
		return Forced{}, ErrForcedUnsupported
	}
	base := func() map[string]any {
		body := map[string]any{"model": target.Model, "temperature": 0, "top_p": 1, "seed": Seed, "n": 1, "stream": false, "max_tokens": 1,
			"chat_template_kwargs": map[string]any{"enable_thinking": false}}
		if prompt.HasTools() {
			body["tools"] = prompt.Tools
		}
		return body
	}
	prefix := base()
	prefix["messages"] = prompt.requestMessages()
	var counted struct {
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := c.post(ctx, target, prefix, &counted); err != nil {
		return Forced{}, err
	}
	forced := base()
	// A copy: the prompt's own messages are never appended to.
	forced["messages"] = append(append([]Message(nil), prompt.requestMessages()...), Message{Role: "assistant", Content: answer})
	forced["add_generation_prompt"], forced["continue_final_message"], forced["prompt_logprobs"] = false, true, topK
	var read struct {
		PromptLogprobs []map[string]struct {
			Logprob float64 `json:"logprob"`
		} `json:"prompt_logprobs"`
	}
	if err := c.post(ctx, target, forced, &read); err != nil {
		return Forced{}, err
	}
	if counted.Usage.PromptTokens < 1 || len(read.PromptLogprobs) < 2 {
		return Forced{}, ErrForcedUnsupported
	}
	positions := make([]map[string]float64, 0, len(read.PromptLogprobs)-1)
	// The first position has no distribution: nothing comes before it.
	for _, entries := range read.PromptLogprobs[1:] {
		position := make(map[string]float64, len(entries))
		for token, entry := range entries {
			if math.IsNaN(entry.Logprob) || entry.Logprob > 0 {
				return Forced{}, errors.New("server returned an invalid log probability")
			}
			position[token] = entry.Logprob
		}
		positions = append(positions, position)
	}
	return Forced{PrefixTokens: counted.Usage.PromptTokens, Positions: positions}, nil
}

func (c HTTPCompleter) post(ctx context.Context, target Target, body map[string]any, out any) error {
	url, err := openaicompat.Endpoint(target.Endpoint, "chat/completions")
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if target.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+target.APIKey)
	}
	response, err := c.client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	// Prompt log probabilities of a long answer are large: 16 tokens per
	// position. They are read in memory and never stored.
	data, err := io.ReadAll(io.LimitReader(response.Body, 64*maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > 64*maxResponseBytes {
		return errors.New("prompt log probabilities exceed 64 MiB")
	}
	if response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnprocessableEntity {
		return ErrForcedUnsupported
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned HTTP %d", response.StatusCode)
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode prompt log probabilities: %w", err)
	}
	return nil
}
