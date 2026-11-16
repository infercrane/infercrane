// Package qualitycheck runs InferCrane's built-in answer check for an
// optimization candidate. It sends a small, versioned prompt suite to the
// candidate and to a reference revision with greedy decoding, scores how well
// the candidate's answers agree with the reference's, and classifies the
// result with the same PASS, WATCH, and BLOCK rules Release Guard uses.
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
)

const (
	// EvalSetSchema is the schema of the default suite and of customer eval
	// sets.
	EvalSetSchema = "infercrane.dev/quality-eval-set/v1"
	// MaxEvalSetBytes bounds an uploaded eval set.
	MaxEvalSetBytes = 512 << 10
	// MaxPrompts bounds the prompts in one eval set. Every prompt is sent to
	// two revisions during a campaign, so the bound also caps that load.
	MaxPrompts      = 200
	maxPromptBytes  = 8 << 10
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

// Prompt is one single-turn request in an eval set.
type Prompt struct {
	ID     string `json:"id"`
	System string `json:"system,omitempty"`
	Prompt string `json:"prompt"`
}

// EvalSet is a bounded list of prompts. Digest is the SHA-256 of the exact
// bytes it was decoded from.
type EvalSet struct {
	Schema    string   `json:"schema"`
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	MaxTokens int      `json:"max_tokens,omitempty"`
	Prompts   []Prompt `json:"prompts"`
	Digest    string   `json:"-"`
}

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
// rejected so expected answers or other private fields are not accepted by
// accident; the built-in check scores agreement with the reference revision,
// not against stored answers.
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
	if set.Schema != EvalSetSchema {
		return EvalSet{}, fmt.Errorf("eval set schema must be %q", EvalSetSchema)
	}
	for field, value := range map[string]string{"name": set.Name, "version": set.Version} {
		if strings.TrimSpace(value) == "" || len(value) > maxNameBytes || !utf8.ValidString(value) || strings.ContainsAny(value, "@\n\r\t") {
			return EvalSet{}, fmt.Errorf("eval set %s must be 1 to %d characters without @ or control characters", field, maxNameBytes)
		}
	}
	if set.MaxTokens == 0 {
		set.MaxTokens = defaultMaxToken
	}
	if set.MaxTokens < 1 || set.MaxTokens > maxMaxTokens {
		return EvalSet{}, fmt.Errorf("eval set max_tokens must be between 1 and %d", maxMaxTokens)
	}
	if len(set.Prompts) < 1 || len(set.Prompts) > MaxPrompts {
		return EvalSet{}, fmt.Errorf("eval set must contain 1 to %d prompts", MaxPrompts)
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
		if strings.TrimSpace(prompt.Prompt) == "" || len(prompt.Prompt) > maxPromptBytes || len(prompt.System) > maxPromptBytes || !utf8.ValidString(prompt.Prompt) || !utf8.ValidString(prompt.System) {
			return EvalSet{}, fmt.Errorf("prompt %q must be valid UTF-8, non-empty, and at most %d KiB", prompt.ID, maxPromptBytes>>10)
		}
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
	ids := make([]string, 0, len(s.Prompts))
	for _, prompt := range s.Prompts {
		ids = append(ids, prompt.ID)
	}
	encoded, _ := json.Marshal(map[string]any{"eval_set": s.Digest, "prompts": ids})
	return Digest(encoded)
}
