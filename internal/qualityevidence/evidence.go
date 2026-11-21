// Package qualityevidence validates signed semantic-evaluation evidence, from a
// customer's evaluator or from InferCrane's built-in answer check. InferCrane
// verifies provenance and applies deterministic policy; it does not run an LLM
// judge and the evidence never contains prompt or output bodies.
package qualityevidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/passport"
)

const (
	Schema       = "infercrane.dev/quality-evidence/v2"
	ResultSchema = "infercrane.dev/evaluator-result/v2"
	MaxFileSize  = 1 << 20
)

var sha256Pattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type Payload struct {
	Schema           string        `json:"schema"`
	Deployment       string        `json:"deployment"`
	RevisionID       string        `json:"revision_id"`
	Suite            string        `json:"suite"`
	SuiteVersion     string        `json:"suite_version"`
	Evaluator        string        `json:"evaluator"`
	EvaluatorVersion string        `json:"evaluator_version"`
	Score            float64       `json:"score"`
	Passed           bool          `json:"passed"`
	SampleCount      int           `json:"sample_count"`
	Distribution     *Distribution `json:"distribution,omitempty"`
	ArtifactDigest   string        `json:"artifact_digest"`
	EvaluatedAt      time.Time     `json:"evaluated_at"`
}

// Distribution contains content-free, bounded per-example scores in the
// evaluator's canonical pair order. PairingDigest commits both active and
// candidate evidence to the same sample identities without storing prompts,
// responses, or identifiers in InferCrane.
type Distribution struct {
	Schema        string    `json:"schema"`
	Kind          string    `json:"kind"`
	PairingDigest string    `json:"pairing_digest"`
	Scores        []float64 `json:"scores"`
	// ToolCalls is set only by InferCrane's built-in answer check, for eval
	// sets with cases that declare function tools.
	ToolCalls *ToolCallScores `json:"tool_calls,omitempty"`
}

// ToolCallScores is the built-in check's score over the cases that declare
// function tools. Those cases' scores are also in Scores.
type ToolCallScores struct {
	// Cases counts the tool cases the check scored.
	Cases int `json:"cases"`
	// NotChecked counts the tool cases the check could not run because the
	// reference refused requests with tools. They have no score and are in
	// neither Cases, Scores, nor the sample count. When Cases is 0, no tool
	// call was checked and every other field is 0.
	NotChecked int `json:"not_checked"`
	// Score is the mean of the scored cases: 1 for a case when the candidate
	// called the same functions with the same arguments as the reference.
	Score float64 `json:"score"`
	// Broken counts cases where the reference's calls validated against the
	// declared schemas and the candidate's did not. One blocks the candidate.
	Broken int `json:"broken"`
	// ReferenceInvalid counts cases where the reference's own calls did not
	// validate; those cases measure agreement only.
	ReferenceInvalid int `json:"reference_invalid"`
	// Candidate counts, per kind, the cases where the candidate's calls
	// failed against the reference's. A case can count under several kinds.
	Candidate ToolCallFailures `json:"candidate"`
	// Reference counts the cases where the reference's own calls failed.
	// Missing, extra, and unexpected calls are relative to the reference, so
	// they stay 0 here, and a case the reference refused is not checked.
	Reference ToolCallFailures `json:"reference"`
}

// ToolCallFailures counts tool cases by how their calls failed. Each count is
// a number of cases, never call content.
type ToolCallFailures struct {
	// MissingCall: the reference called a function this side did not call.
	MissingCall int `json:"missing_call"`
	// MalformedArguments: arguments that are not JSON.
	MalformedArguments int `json:"malformed_arguments"`
	// WrongFunction: an undeclared function, or another function in place of
	// the reference's.
	WrongFunction int `json:"wrong_function"`
	// SchemaInvalid: arguments that do not validate against the declared
	// parameters.
	SchemaInvalid int `json:"schema_invalid"`
	// ExtraCalls: more calls than the reference made.
	ExtraCalls int `json:"extra_calls"`
	// UnexpectedCall: a call where the reference answered without one.
	UnexpectedCall int `json:"unexpected_call"`
	// ToolsRefused: the server refused the request with tools, which the
	// reference accepted.
	ToolsRefused int `json:"tools_refused"`
}

// Add returns the sum of two counts.
func (f ToolCallFailures) Add(other ToolCallFailures) ToolCallFailures {
	return ToolCallFailures{
		MissingCall: f.MissingCall + other.MissingCall, MalformedArguments: f.MalformedArguments + other.MalformedArguments,
		WrongFunction: f.WrongFunction + other.WrongFunction, SchemaInvalid: f.SchemaInvalid + other.SchemaInvalid,
		ExtraCalls: f.ExtraCalls + other.ExtraCalls, UnexpectedCall: f.UnexpectedCall + other.UnexpectedCall,
		ToolsRefused: f.ToolsRefused + other.ToolsRefused,
	}
}

func (f ToolCallFailures) within(cases int) bool {
	for _, count := range []int{f.MissingCall, f.MalformedArguments, f.WrongFunction, f.SchemaInvalid, f.ExtraCalls, f.UnexpectedCall, f.ToolsRefused} {
		if count < 0 || count > cases {
			return false
		}
	}
	return true
}

const DistributionSchema = "infercrane.dev/quality-distribution/v1"

// Result is the content-free interchange boundary between a customer-owned
// evaluator and InferCrane. It deliberately excludes deployment identity,
// prompt bodies, and generated outputs. The CLI binds a validated result to an
// immutable deployment revision before signing the existing Payload format.
type Result struct {
	Schema           string        `json:"schema"`
	Suite            string        `json:"suite"`
	SuiteVersion     string        `json:"suite_version"`
	Evaluator        string        `json:"evaluator"`
	EvaluatorVersion string        `json:"evaluator_version"`
	Score            float64       `json:"score"`
	Passed           bool          `json:"passed"`
	SampleCount      int           `json:"sample_count"`
	Distribution     *Distribution `json:"distribution,omitempty"`
	ArtifactDigest   string        `json:"artifact_digest"`
	EvaluatedAt      time.Time     `json:"evaluated_at"`
}

func DecodeResult(body []byte) (Result, error) {
	if len(body) > MaxFileSize {
		return Result{}, errors.New("evaluator result exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var result Result
	if err := decoder.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("decode evaluator result: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Result{}, errors.New("evaluator result contains trailing JSON")
		}
		return Result{}, fmt.Errorf("decode trailing evaluator result: %w", err)
	}
	if err := result.Validate(); err != nil {
		return Result{}, err
	}
	return result, nil
}

func (r Result) Validate() error {
	if r.Schema != ResultSchema {
		return fmt.Errorf("evaluator result schema must be %q", ResultSchema)
	}
	if r.Distribution != nil && r.Distribution.ToolCalls != nil {
		return errors.New("distribution.tool_calls is recorded only by InferCrane's built-in answer check")
	}
	payload := Payload{
		Schema: Schema, Deployment: "binding-validated-by-cli", RevisionID: "binding-validated-by-cli",
		Suite: r.Suite, SuiteVersion: r.SuiteVersion, Evaluator: r.Evaluator,
		EvaluatorVersion: r.EvaluatorVersion, Score: r.Score, Passed: r.Passed,
		SampleCount: r.SampleCount, Distribution: r.Distribution, ArtifactDigest: r.ArtifactDigest, EvaluatedAt: r.EvaluatedAt,
	}
	return payload.Validate()
}

func (r Result) Bind(deployment, revision string) Payload {
	return Payload{
		Schema: Schema, Deployment: deployment, RevisionID: revision, Suite: r.Suite,
		SuiteVersion: r.SuiteVersion, Evaluator: r.Evaluator,
		EvaluatorVersion: r.EvaluatorVersion, Score: r.Score, Passed: r.Passed,
		SampleCount: r.SampleCount, Distribution: r.Distribution, ArtifactDigest: r.ArtifactDigest, EvaluatedAt: r.EvaluatedAt,
	}
}

func Decode(envelope passport.Envelope) (Payload, error) {
	if err := passport.Verify(envelope); err != nil {
		return Payload{}, fmt.Errorf("verify quality evidence: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(envelope.PayloadJSON)))
	decoder.DisallowUnknownFields()
	var payload Payload
	if err := decoder.Decode(&payload); err != nil {
		return Payload{}, fmt.Errorf("decode quality evidence payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Payload{}, errors.New("quality evidence payload contains trailing JSON")
		}
		return Payload{}, fmt.Errorf("decode trailing quality evidence payload: %w", err)
	}
	if err := payload.Validate(); err != nil {
		return Payload{}, err
	}
	return payload, nil
}

func (p Payload) Validate() error {
	if p.Schema != Schema {
		return fmt.Errorf("quality evidence schema must be %q", Schema)
	}
	for name, value := range map[string]string{"deployment": p.Deployment, "revision_id": p.RevisionID, "suite": p.Suite, "suite_version": p.SuiteVersion, "evaluator": p.Evaluator, "evaluator_version": p.EvaluatorVersion} {
		if strings.TrimSpace(value) == "" || len(value) > 256 {
			return fmt.Errorf("%s must be non-empty and at most 256 characters", name)
		}
	}
	if p.Score < 0 || p.Score > 1 {
		return errors.New("quality score must be between 0 and 1")
	}
	if p.SampleCount < 1 || p.SampleCount > 10_000_000 {
		return errors.New("quality sample_count must be between 1 and 10000000")
	}
	if p.Distribution != nil {
		if p.Distribution.Schema != DistributionSchema || p.Distribution.Kind != "paired_scores" || !sha256Pattern.MatchString(p.Distribution.PairingDigest) {
			return errors.New("quality distribution requires the v1 paired_scores schema and a SHA-256 pairing digest")
		}
		if len(p.Distribution.Scores) != p.SampleCount || len(p.Distribution.Scores) > 100_000 {
			return errors.New("quality distribution scores must match sample_count and contain at most 100000 values")
		}
		for _, score := range p.Distribution.Scores {
			if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
				return errors.New("quality distribution scores must be finite values between 0 and 1")
			}
		}
		if calls := p.Distribution.ToolCalls; calls != nil {
			if calls.Cases < 0 || calls.Cases > p.SampleCount || calls.NotChecked < 0 || calls.NotChecked > 100_000 || calls.Cases+calls.NotChecked < 1 || math.IsNaN(calls.Score) || calls.Score < 0 || calls.Score > 1 || calls.Cases == 0 && calls.Score != 0 || calls.Broken < 0 || calls.Broken > calls.Cases || calls.ReferenceInvalid < 0 || calls.ReferenceInvalid > calls.Cases || !calls.Candidate.within(calls.Cases) || !calls.Reference.within(calls.Cases) || calls.Reference.MissingCall+calls.Reference.ExtraCalls+calls.Reference.UnexpectedCall+calls.Reference.ToolsRefused > 0 {
				return errors.New("quality distribution tool_calls must count 0 to sample_count scored cases and at least one case in all, at most that many of each kind, with a score between 0 and 1 (0 when no case was scored); the reference has no missing, extra, unexpected, or refused calls")
			}
		}
	}
	if !sha256Pattern.MatchString(p.ArtifactDigest) {
		return errors.New("quality artifact_digest must be sha256:<64 lowercase hex characters>")
	}
	if p.EvaluatedAt.IsZero() || p.EvaluatedAt.After(time.Now().UTC().Add(5*time.Minute)) {
		return errors.New("quality evaluated_at must be set and cannot be in the future")
	}
	return nil
}

// Record converts a verified envelope and its decoded payload into the stored
// evidence row. The caller sets Source; the attach API never sets it to
// InferCrane.
func Record(payload Payload, envelope passport.Envelope) (domain.QualityEvidence, error) {
	distributionJSON := "{}"
	if payload.Distribution != nil {
		encoded, err := json.Marshal(payload.Distribution)
		if err != nil {
			return domain.QualityEvidence{}, errors.New("quality distribution could not be encoded")
		}
		distributionJSON = string(encoded)
	}
	return domain.QualityEvidence{RevisionID: payload.RevisionID, Suite: payload.Suite, SuiteVersion: payload.SuiteVersion, Evaluator: payload.Evaluator, EvaluatorVersion: payload.EvaluatorVersion, Score: payload.Score, Passed: payload.Passed, SampleCount: payload.SampleCount, DistributionJSON: distributionJSON, ArtifactDigest: payload.ArtifactDigest, PayloadDigest: envelope.Digest, Signature: envelope.Signature, PublicKey: envelope.PublicKey, Algorithm: envelope.Algorithm, KeyID: envelope.KeyID, EvaluatedAt: payload.EvaluatedAt}, nil
}

func Comparable(a, b Payload) bool {
	return a.Suite == b.Suite && a.SuiteVersion == b.SuiteVersion && a.Evaluator == b.Evaluator && a.EvaluatorVersion == b.EvaluatorVersion
}
