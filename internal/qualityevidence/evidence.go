// Package qualityevidence validates externally produced semantic-evaluation
// evidence. InferCrane verifies provenance and applies deterministic policy;
// it does not run an LLM judge or inspect prompt/output bodies.
package qualityevidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/passport"
)

const (
	Schema       = "infercrane.dev/quality-evidence/v1"
	ResultSchema = "infercrane.dev/evaluator-result/v1"
	MaxFileSize  = 1 << 20
)

var sha256Pattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type Payload struct {
	Schema           string    `json:"schema"`
	Deployment       string    `json:"deployment"`
	RevisionID       string    `json:"revision_id"`
	Suite            string    `json:"suite"`
	SuiteVersion     string    `json:"suite_version"`
	Evaluator        string    `json:"evaluator"`
	EvaluatorVersion string    `json:"evaluator_version"`
	Score            float64   `json:"score"`
	Passed           bool      `json:"passed"`
	SampleCount      int       `json:"sample_count"`
	ArtifactDigest   string    `json:"artifact_digest"`
	EvaluatedAt      time.Time `json:"evaluated_at"`
}

// Result is the content-free interchange boundary between a customer-owned
// evaluator and InferCrane. It deliberately excludes deployment identity,
// prompt bodies, and generated outputs. The CLI binds a validated result to an
// immutable deployment revision before signing the existing Payload format.
type Result struct {
	Schema           string    `json:"schema"`
	Suite            string    `json:"suite"`
	SuiteVersion     string    `json:"suite_version"`
	Evaluator        string    `json:"evaluator"`
	EvaluatorVersion string    `json:"evaluator_version"`
	Score            float64   `json:"score"`
	Passed           bool      `json:"passed"`
	SampleCount      int       `json:"sample_count"`
	ArtifactDigest   string    `json:"artifact_digest"`
	EvaluatedAt      time.Time `json:"evaluated_at"`
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
	payload := Payload{
		Schema: Schema, Deployment: "binding-validated-by-cli", RevisionID: "binding-validated-by-cli",
		Suite: r.Suite, SuiteVersion: r.SuiteVersion, Evaluator: r.Evaluator,
		EvaluatorVersion: r.EvaluatorVersion, Score: r.Score, Passed: r.Passed,
		SampleCount: r.SampleCount, ArtifactDigest: r.ArtifactDigest, EvaluatedAt: r.EvaluatedAt,
	}
	return payload.Validate()
}

func (r Result) Bind(deployment, revision string) Payload {
	return Payload{
		Schema: Schema, Deployment: deployment, RevisionID: revision, Suite: r.Suite,
		SuiteVersion: r.SuiteVersion, Evaluator: r.Evaluator,
		EvaluatorVersion: r.EvaluatorVersion, Score: r.Score, Passed: r.Passed,
		SampleCount: r.SampleCount, ArtifactDigest: r.ArtifactDigest, EvaluatedAt: r.EvaluatedAt,
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
	if !sha256Pattern.MatchString(p.ArtifactDigest) {
		return errors.New("quality artifact_digest must be sha256:<64 lowercase hex characters>")
	}
	if p.EvaluatedAt.IsZero() || p.EvaluatedAt.After(time.Now().UTC().Add(5*time.Minute)) {
		return errors.New("quality evaluated_at must be set and cannot be in the future")
	}
	return nil
}

func Comparable(a, b Payload) bool {
	return a.Suite == b.Suite && a.SuiteVersion == b.SuiteVersion && a.Evaluator == b.Evaluator && a.EvaluatorVersion == b.EvaluatorVersion
}
