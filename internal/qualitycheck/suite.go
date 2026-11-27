// Package qualitycheck runs InferCrane's built-in answer check for an
// optimization candidate. It sends a small, versioned prompt suite to the
// candidate and to a reference revision with greedy decoding, scores how well
// the candidate's answers agree with the reference's, and classifies the
// result with the same PASS, WATCH, and BLOCK rules Release Guard uses. Cases
// that declare function tools are scored on the tool calls, and a candidate
// that breaks a call the reference made correctly is blocked.
//
// Answers are compared in memory and never stored. Evidence carries only
// aggregate and per-prompt scores plus digests that commit to the suite and
// the answers.
package qualitycheck

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

const (
	// EvalSetSchemaV1 is the schema of the default suite: single-turn prompts.
	EvalSetSchemaV1 = "infercrane.dev/quality-eval-set/v1"
	// EvalSetSchemaV2 adds conversations and function tools to a case.
	EvalSetSchemaV2 = "infercrane.dev/quality-eval-set/v2"
	// MaxEvalSetBytes bounds an uploaded eval set. A task set of 1,000
	// items with their instructions needs about 700 KiB.
	MaxEvalSetBytes = 1 << 20
	// maxGeneralEvalSetBytes bounds every set but a task set.
	maxGeneralEvalSetBytes = 512 << 10
	// MaxPrompts bounds the prompts in one eval set. Every prompt is sent to
	// two revisions during a campaign, so the bound also caps that load.
	MaxPrompts      = 200
	maxPromptBytes  = 8 << 10
	maxMessages     = 64
	maxTools        = 64
	maxNameBytes    = 128
	maxMaxTokens    = 512
	defaultMaxToken = 64
)

// DefaultSuiteDigest pins the embedded default suite. Changing the suite file
// without changing this digest and the suite version fails the package tests,
// so evidence from different prompts is never compared as the same suite.
const DefaultSuiteDigest = "sha256:f65db09bb9ba827c58c317236a8faf795f73b09b9380d84307b69334782611af"

//go:embed suites/infercrane-default-v1.json
var defaultSuite []byte

var (
	promptIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	digestPattern   = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// Prompt is one case in an eval set: a single prompt with an optional system
// message, or, in schema v2, a conversation in Messages. A v2 case may declare
// function Tools the model can call, and ToolChoice. Expected holds the
// accepted answers of a graded set and is empty otherwise.
type Prompt struct {
	ID         string          `json:"id"`
	System     string          `json:"system,omitempty"`
	Prompt     string          `json:"prompt,omitempty"`
	Messages   []Message       `json:"messages,omitempty"`
	Tools      []Tool          `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	Expected   []string        `json:"expected,omitempty"`
	// schemas holds each declared function's resolved parameters schema.
	schemas map[string]*jsonschema.Resolved
}

// EvalSet is a bounded list of prompts. Digest is the SHA-256 of the exact
// bytes it was decoded from. Grading is empty for a set scored by agreement
// with a reference, or names how a graded set scores an answer against its
// expected answers.
type EvalSet struct {
	Schema    string   `json:"schema"`
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	Grading   string   `json:"grading,omitempty"`
	Prompts   []Prompt `json:"prompts"`
	Digest    string   `json:"-"`
	// pairing is the pairing digest of a set whose prompts are held
	// elsewhere (RemoteSet); empty for a set decoded here.
	pairing string
}

// Grading methods of a graded eval set.
const (
	// GradingExactMatch scores 1 when the answer's words equal one expected
	// answer's words, ignoring case and punctuation, else 0.
	GradingExactMatch = "exact_match"
	// GradingTokenF1 scores the best token F1 against any expected answer.
	GradingTokenF1 = "token_f1"
	maxExpected    = 8
)

// Task reports whether the set is a task set: each answer is right or wrong
// against one gold answer, and setups are compared item by item.
func (s EvalSet) Task() bool { return s.Grading == GradingAnswerLine }

// Graded reports whether the set scores answers against expected answers.
func (s EvalSet) Graded() bool { return s.Grading != "" }

// Default returns the embedded default suite.
func Default() EvalSet {
	set, err := DecodeEvalSet(defaultSuite)
	if err != nil {
		panic("embedded default quality suite is invalid: " + err.Error())
	}
	return set
}

// DefaultSuiteBytes returns a copy of the embedded default suite file.
func DefaultSuiteBytes() []byte { return bytes.Clone(defaultSuite) }

// Digest returns the content digest of an eval set file.
func Digest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidDigest reports whether value is a sha256:<64 lowercase hex> digest.
func ValidDigest(value string) bool { return digestPattern.MatchString(value) }

// DecodeEvalSet strictly decodes and bounds an eval set. Unknown fields are
// rejected so private fields are not accepted by accident. Expected answers
// are accepted only in a set that names its grading, because only a graded
// set is scored against them; any other set scores agreement with the
// reference.
func DecodeEvalSet(body []byte) (EvalSet, error) {
	if len(body) == 0 || len(body) > MaxEvalSetBytes {
		return EvalSet{}, fmt.Errorf("eval set must be between 1 byte and %d KiB", MaxEvalSetBytes>>10)
	}
	if !utf8.Valid(body) {
		return EvalSet{}, errors.New("eval set must be UTF-8 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var set EvalSet
	if err := decoder.Decode(&set); err != nil {
		return EvalSet{}, fmt.Errorf("decode eval set: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return EvalSet{}, errors.New("eval set contains trailing JSON")
	}
	if set.Schema != EvalSetSchemaV1 && set.Schema != EvalSetSchemaV2 {
		return EvalSet{}, fmt.Errorf("eval set schema must be %q or %q", EvalSetSchemaV1, EvalSetSchemaV2)
	}
	for field, value := range map[string]string{"name": set.Name, "version": set.Version} {
		if strings.TrimSpace(value) == "" || len(value) > maxNameBytes || !utf8.ValidString(value) || strings.ContainsAny(value, "@\n\r\t") {
			return EvalSet{}, fmt.Errorf("eval set %s must be 1 to %d characters without @ or control characters", field, maxNameBytes)
		}
	}
	if set.Grading != "" && set.Grading != GradingExactMatch && set.Grading != GradingTokenF1 && set.Grading != GradingAnswerLine {
		return EvalSet{}, fmt.Errorf("eval set grading must be %s, %s, or %s", GradingExactMatch, GradingTokenF1, GradingAnswerLine)
	}
	// A task set holds more items, with room for working, than a set scored
	// by agreement.
	limitBytes, limitPrompts, limitTokens := maxGeneralEvalSetBytes, MaxPrompts, maxMaxTokens
	if set.Task() {
		limitBytes, limitPrompts, limitTokens = MaxEvalSetBytes, MaxTaskPrompts, maxTaskTokens
	}
	if len(body) > limitBytes {
		return EvalSet{}, fmt.Errorf("eval set must be at most %d KiB", limitBytes>>10)
	}
	if set.MaxTokens == 0 {
		set.MaxTokens = defaultMaxToken
	}
	if set.MaxTokens < 1 || set.MaxTokens > limitTokens {
		return EvalSet{}, fmt.Errorf("eval set max_tokens must be between 1 and %d", limitTokens)
	}
	if len(set.Prompts) < 1 || len(set.Prompts) > limitPrompts {
		return EvalSet{}, fmt.Errorf("eval set must contain 1 to %d prompts", limitPrompts)
	}
	seen := make(map[string]struct{}, len(set.Prompts))
	for index, prompt := range set.Prompts {
		if !promptIDPattern.MatchString(prompt.ID) {
			return EvalSet{}, fmt.Errorf("prompt %d id must be 1 to 64 letters, digits, dots, dashes, or underscores", index+1)
		}
		if _, duplicate := seen[prompt.ID]; duplicate {
			return EvalSet{}, fmt.Errorf("prompt id %q is repeated", prompt.ID)
		}
		seen[prompt.ID] = struct{}{}
		if set.Schema == EvalSetSchemaV1 && (prompt.Messages != nil || prompt.Tools != nil || prompt.ToolChoice != nil) {
			return EvalSet{}, fmt.Errorf("prompt %q uses messages, tools, or tool_choice, which need schema %q", prompt.ID, EvalSetSchemaV2)
		}
		if prompt.Messages != nil {
			if prompt.Prompt != "" || prompt.System != "" {
				return EvalSet{}, fmt.Errorf("prompt %q must set either prompt (with an optional system) or messages, not both", prompt.ID)
			}
		} else if strings.TrimSpace(prompt.Prompt) == "" || len(prompt.Prompt) > maxPromptBytes || len(prompt.System) > maxPromptBytes || !utf8.ValidString(prompt.Prompt) || !utf8.ValidString(prompt.System) {
			return EvalSet{}, fmt.Errorf("prompt %q must be valid UTF-8, non-empty, and at most %d KiB", prompt.ID, maxPromptBytes>>10)
		}
		if err := validExpected(set.Grading, prompt); err != nil {
			return EvalSet{}, err
		}
		if set.Graded() && prompt.HasTools() {
			// A graded set scores answer text against expected answers.
			return EvalSet{}, fmt.Errorf("prompt %q declares tools, which a graded set does not score; use a set without grading", prompt.ID)
		}
		resolved, err := validateTools(prompt)
		if err != nil {
			return EvalSet{}, fmt.Errorf("prompt %q: %w", prompt.ID, err)
		}
		if prompt.Messages != nil {
			if err = validateMessages(prompt.Messages, resolved); err != nil {
				return EvalSet{}, fmt.Errorf("prompt %q: %w", prompt.ID, err)
			}
		}
		set.Prompts[index].schemas = resolved
	}
	set.Digest = Digest(body)
	return set, nil
}

// SuiteVersion is the version recorded in evidence. It includes the content
// digest, so two eval sets that share a name and version but differ in content
// are never comparable.
func (s EvalSet) SuiteVersion() string { return s.Version + "@" + s.Digest }

// PairingDigest commits evidence from both revisions to the same ordered
// prompt identities without storing them.
func (s EvalSet) PairingDigest() string {
	if s.pairing != "" && len(s.Prompts) == 0 {
		return s.pairing
	}
	ids := make([]string, 0, len(s.Prompts))
	for _, prompt := range s.Prompts {
		ids = append(ids, prompt.ID)
	}
	encoded, _ := json.Marshal(map[string]any{"eval_set": s.Digest, "prompts": ids})
	return Digest(encoded)
}

func validExpected(grading string, prompt Prompt) error {
	if grading == "" {
		if len(prompt.Expected) > 0 {
			return fmt.Errorf("prompt %q has expected answers, which only a set with grading %s, %s, or %s uses", prompt.ID, GradingExactMatch, GradingTokenF1, GradingAnswerLine)
		}
		return nil
	}
	if grading == GradingAnswerLine {
		if len(prompt.Expected) != 1 || !validTaskGold(prompt.Expected[0]) {
			return fmt.Errorf("prompt %q of a task set needs one expected answer: a number or a letter from A to D", prompt.ID)
		}
		return nil
	}
	if len(prompt.Expected) < 1 || len(prompt.Expected) > maxExpected {
		return fmt.Errorf("prompt %q of a graded set needs 1 to %d expected answers", prompt.ID, maxExpected)
	}
	for _, expected := range prompt.Expected {
		if !Answered(expected) || len(expected) > maxPromptBytes || !utf8.ValidString(expected) {
			return fmt.Errorf("prompt %q expected answers must hold words and be at most %d KiB", prompt.ID, maxPromptBytes>>10)
		}
	}
	return nil
}
