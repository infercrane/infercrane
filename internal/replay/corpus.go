package replay

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// PromptCorpusSchemaVersion is the corpus file format an operator supplies.
	PromptCorpusSchemaVersion = "infercrane.prompt-corpus/v1"
	// PromptTextOperatorCorpus names the text source in TraceProperties.
	PromptTextOperatorCorpus = "operator_corpus"

	// MaxPromptCorpusBytes bounds the corpus file a control plane reads.
	MaxPromptCorpusBytes = 64 << 20
	maxCorpusEntries     = 20000
	maxCorpusQuestions   = 64
	maxCorpusEntryTokens = 200000
	maxCorpusLabel       = 512
	// maxCorpusRenderTokens bounds one rendered pass, so a staged AIPerf
	// input stays a bounded file (about four bytes per token).
	maxCorpusRenderTokens = 64_000_000
	// corpusFramingTokens is the prompt tokens every corpus request spends
	// outside its passages and question: the chain line that opens the system
	// message and the chat template around two messages.
	corpusFramingTokens = 24
)

// PromptCorpus is real text an operator supplies for replay measurement:
// passages with questions about them, and standalone prompts. A campaign
// that replays a content-free trace composes its requests from the corpus
// instead of asking AIPerf to synthesize text, so setups whose speed depends
// on what the model writes (speculative decoding) are measured on real
// answers. The corpus is the operator's file. It is never built from, and
// never holds, the prompts the gateway served.
type PromptCorpus struct {
	identity CorpusIdentity
	passages []corpusPassage
	prompts  []corpusText
}

// CorpusIdentity names a corpus in evidence: its content digest and the
// provenance its file declares. No corpus text is part of it.
type CorpusIdentity struct {
	Digest    string `json:"digest"`
	Name      string `json:"name"`
	License   string `json:"license"`
	Source    string `json:"source"`
	Tokenizer string `json:"tokenizer"`
	Passages  int    `json:"passages"`
	Prompts   int    `json:"prompts"`
}

type corpusText struct {
	Tokens int    `json:"tokens"`
	Text   string `json:"text"`
}

type corpusPassage struct {
	Tokens    int          `json:"tokens"`
	Text      string       `json:"text"`
	Questions []corpusText `json:"questions"`
}

type corpusFile struct {
	SchemaVersion string          `json:"schema_version"`
	Name          string          `json:"name"`
	License       string          `json:"license"`
	Source        string          `json:"source"`
	Tokenizer     string          `json:"tokenizer"`
	Passages      []corpusPassage `json:"passages"`
	Prompts       []corpusText    `json:"prompts"`
}

// Identity returns the corpus identity recorded in evidence.
func (c *PromptCorpus) Identity() CorpusIdentity { return c.identity }

// LoadPromptCorpus reads and validates a corpus file. Its digest is the
// SHA-256 of the file's bytes, so evidence names exactly the text measured.
func LoadPromptCorpus(path string) (*PromptCorpus, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read prompt corpus: %w", err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, MaxPromptCorpusBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read prompt corpus: %w", err)
	}
	if len(body) > MaxPromptCorpusBytes {
		return nil, fmt.Errorf("prompt corpus exceeds %d MiB", MaxPromptCorpusBytes>>20)
	}
	return ParsePromptCorpus(body)
}

// ParsePromptCorpus validates a corpus document. Every passage needs a token
// count and at least one question, because the control plane has no
// tokenizer and a request must end in a task its context can answer.
func ParsePromptCorpus(body []byte) (*PromptCorpus, error) {
	var file corpusFile
	if err := json.Unmarshal(body, &file); err != nil {
		return nil, fmt.Errorf("decode prompt corpus: %w", err)
	}
	if file.SchemaVersion != PromptCorpusSchemaVersion {
		return nil, fmt.Errorf("prompt corpus schema %q is not supported; expected %s", file.SchemaVersion, PromptCorpusSchemaVersion)
	}
	for name, value := range map[string]string{"name": file.Name, "license": file.License, "source": file.Source, "tokenizer": file.Tokenizer} {
		if strings.TrimSpace(value) == "" || len(value) > maxCorpusLabel || value != strings.TrimSpace(value) {
			return nil, fmt.Errorf("prompt corpus %s must be set, trimmed, and at most %d bytes", name, maxCorpusLabel)
		}
	}
	if len(file.Passages) == 0 || len(file.Prompts) == 0 {
		return nil, errors.New("prompt corpus needs at least one passage and one standalone prompt")
	}
	if len(file.Passages) > maxCorpusEntries || len(file.Prompts) > maxCorpusEntries {
		return nil, fmt.Errorf("prompt corpus holds at most %d passages and %d prompts", maxCorpusEntries, maxCorpusEntries)
	}
	for index, passage := range file.Passages {
		if err := validCorpusText(passage.Tokens, passage.Text); err != nil {
			return nil, fmt.Errorf("prompt corpus passage %d: %w", index, err)
		}
		if len(passage.Questions) == 0 || len(passage.Questions) > maxCorpusQuestions {
			return nil, fmt.Errorf("prompt corpus passage %d needs between 1 and %d questions", index, maxCorpusQuestions)
		}
		for _, question := range passage.Questions {
			if err := validCorpusText(question.Tokens, question.Text); err != nil {
				return nil, fmt.Errorf("prompt corpus passage %d question: %w", index, err)
			}
		}
	}
	for index, prompt := range file.Prompts {
		if err := validCorpusText(prompt.Tokens, prompt.Text); err != nil {
			return nil, fmt.Errorf("prompt corpus prompt %d: %w", index, err)
		}
	}
	digest := sha256.Sum256(body)
	return &PromptCorpus{
		identity: CorpusIdentity{Digest: "sha256:" + hex.EncodeToString(digest[:]), Name: file.Name, License: file.License, Source: file.Source, Tokenizer: file.Tokenizer, Passages: len(file.Passages), Prompts: len(file.Prompts)},
		passages: file.Passages, prompts: file.Prompts,
	}, nil
}

func validCorpusText(tokens int, text string) error {
	if tokens < 1 || tokens > maxCorpusEntryTokens {
		return fmt.Errorf("tokens must be between 1 and %d", maxCorpusEntryTokens)
	}
	if strings.TrimSpace(text) == "" || strings.ContainsRune(text, 0) {
		return errors.New("text must be non-empty and hold no NUL byte")
	}
	return nil
}

// PromptText records where a rendering's prompt text came from when it is
// not AIPerf's synthetic text. It is part of the workload identity: runs on
// another corpus are another workload.
type PromptText struct {
	Source string         `json:"source"`
	Corpus CorpusIdentity `json:"corpus"`
	// PlannedInputTokensMean is the mean prompt length the rendering planned,
	// under the corpus's own token counts. Passages are never cut, so it
	// differs from the trace's InputTokensMean by less than one passage a
	// request.
	PlannedInputTokensMean int `json:"planned_input_tokens_mean"`
	// StandaloneRequests are requests too short for a passage; each sends one
	// standalone prompt.
	StandaloneRequests int `json:"standalone_requests"`
	PassagesUsed       int `json:"passages_used"`
	PassageUses        int `json:"passage_uses"`
}

// corpusRow is one request's plan: the passages its blocks hold, the passage
// its question belongs to, or the standalone prompt it sends.
type corpusRow struct {
	passages         []int
	question, prompt int
	standalone       bool
	questionChoice   int
}

// WithPromptCorpus returns the rendering with every request composed from
// corpus text. The structure the synthetic rendering keeps stays: request
// order, output lengths, which requests share a leading prefix, and which
// session turns resend their history. A request's blocks are filled with
// whole passages to the nearest fit of its recorded input length, and a
// block two requests share holds the same passages in both, so the shared
// prefix is the same text.
func (t AIPerfTrace) WithPromptCorpus(corpus *PromptCorpus) (AIPerfTrace, error) {
	if corpus == nil || len(corpus.passages) == 0 || len(corpus.prompts) == 0 {
		return AIPerfTrace{}, errors.New("prompt corpus is empty")
	}
	if len(t.rows) == 0 {
		return AIPerfTrace{}, errors.New("replay trace is not rendered")
	}
	blocks := map[int][]int{}
	blockTokens := map[int]int{}
	used := map[int]int{}
	cursor, total := 0, 0
	plan := make([]corpusRow, len(t.rows))
	text := &PromptText{Source: PromptTextOperatorCorpus, Corpus: corpus.identity}
	for index, row := range t.rows {
		target := row.input - corpusFramingTokens
		planned, lastNew := 0, -1
		var passages []int
		for position, block := range row.blocks {
			if assigned, seen := blocks[block]; seen {
				passages = append(passages, assigned...)
				planned += blockTokens[block]
				continue
			}
			// A block's share of the request ends at its boundary, or at the
			// room left for the question in the request's last block.
			limit := min((position+1)*AIPerfHashBlockTokens, target)
			last := position == len(row.blocks)-1
			var assigned []int
			tokens := 0
			for {
				room := limit - planned - tokens
				next, found := nextPassage(corpus.passages, cursor, func(passage corpusPassage) bool {
					question := passage.Questions[0].Tokens
					free := room
					if last {
						free -= question
					}
					// Whole passages only: take one when that lands nearer
					// the limit than stopping here.
					if free >= (passage.Tokens+1)/2 {
						return true
					}
					// A request's first passage goes in its first block
					// whenever the request has room for it, so a leading block
					// other requests share is not left empty by a passage
					// longer than a block.
					return planned+tokens == 0 && !last && passage.Tokens+question <= target
				})
				if !found {
					break
				}
				assigned = append(assigned, next)
				tokens += corpus.passages[next].Tokens
				used[next]++
				cursor = next + 1
			}
			blocks[block], blockTokens[block] = assigned, tokens
			passages = append(passages, assigned...)
			planned += tokens
			if len(assigned) > 0 {
				lastNew = assigned[len(assigned)-1]
			}
		}
		entry := corpusRow{passages: passages, questionChoice: index}
		switch {
		case len(passages) == 0:
			entry.standalone, entry.prompt = true, nearestPrompt(corpus.prompts, target, index)
			planned = corpus.prompts[entry.prompt].Tokens
			text.StandaloneRequests++
		case lastNew >= 0:
			entry.question = lastNew
		default:
			entry.question = passages[len(passages)-1]
		}
		if !entry.standalone {
			questions := corpus.passages[entry.question].Questions
			planned += questions[index%len(questions)].Tokens
		}
		planned += corpusFramingTokens
		total += planned
		if total > maxCorpusRenderTokens {
			return AIPerfTrace{}, errors.New("replay trace is too large to render with a prompt corpus")
		}
		plan[index] = entry
	}
	for _, count := range used {
		text.PassageUses += count
	}
	text.PassagesUsed, text.PlannedInputTokensMean = len(used), total/len(t.rows)
	result := t
	result.corpus, result.corpusRows = corpus, plan
	result.Properties.PromptText = text
	result.Properties.Preserved = replaceName(t.Properties.Preserved, "input_token_lengths", "input_token_lengths_to_the_nearest_passage")
	result.Properties.NotPreserved = append(append([]string(nil), t.Properties.NotPreserved...), "exact_input_token_lengths")
	return result, nil
}

// passageLookAhead is how many passages past the cursor are tried for one
// that fits, so one long passage does not stop a run of short requests.
const passageLookAhead = 8

// nextPassage returns the first passage at or after the cursor, in corpus
// order and wrapping around, that fits. The corpus is used in order, so a
// rendering depends only on the trace and the corpus.
func nextPassage(passages []corpusPassage, cursor int, fits func(corpusPassage) bool) (int, bool) {
	for offset := range min(passageLookAhead, len(passages)) {
		index := (cursor + offset) % len(passages)
		if fits(passages[index]) {
			return index, true
		}
	}
	return 0, false
}

// nearestPrompt picks the standalone prompt nearest a target length. Equal
// fits rotate by request, so short requests do not all send one prompt.
func nearestPrompt(prompts []corpusText, target, rotation int) int {
	best, distance := []int{}, -1
	for index, prompt := range prompts {
		gap := prompt.Tokens - target
		if gap < 0 {
			gap = -gap
		}
		switch {
		case distance < 0 || gap < distance:
			best, distance = []int{index}, gap
		case gap == distance:
			best = append(best, index)
		}
	}
	return best[rotation%len(best)]
}

func replaceName(names []string, old, replacement string) []string {
	out := make([]string, len(names))
	for index, name := range names {
		if name == old {
			name = replacement
		}
		out[index] = name
	}
	return out
}

type corpusMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// writeCorpus renders the trace as AIPerf mooncake_trace rows that carry
// their messages verbatim. Passages repeat across requests, so every request
// opens with a line derived from the namespace and its first block: requests
// that share that block share the line and the text after it, and no other
// two requests, in this pass or another, start alike. That keeps a server's
// prefix cache to the reuse the trace recorded.
func (t AIPerfTrace) writeCorpus(w io.Writer, namespace int) error {
	buffered := bufio.NewWriter(w)
	encoder := json.NewEncoder(buffered)
	encoder.SetEscapeHTML(false)
	for index, row := range t.rows {
		entry := t.corpusRows[index]
		var system strings.Builder
		system.WriteString("Session ")
		system.WriteString(chainLine(t.Properties.TraceDigest, namespace, row.blocks[0]))
		var user string
		if entry.standalone {
			user = t.corpus.prompts[entry.prompt].Text
		} else {
			for _, passage := range entry.passages {
				system.WriteString("\n\n")
				system.WriteString(t.corpus.passages[passage].Text)
			}
			questions := t.corpus.passages[entry.question].Questions
			user = questions[entry.questionChoice%len(questions)].Text
		}
		if err := encoder.Encode(struct {
			Messages     []corpusMessage `json:"messages"`
			OutputLength int             `json:"output_length"`
		}{[]corpusMessage{{"system", system.String()}, {"user", user}}, row.output}); err != nil {
			return err
		}
	}
	return buffered.Flush()
}

// chainLine is the short identifier that opens a request chain's system
// message. It depends on nothing but the trace, the namespace, and the
// chain's first block, so a rendering is reproducible.
func chainLine(traceDigest string, namespace, block int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d/%d", traceDigest, namespace, block))
	return hex.EncodeToString(sum[:8])
}
