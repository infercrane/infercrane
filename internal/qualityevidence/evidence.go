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

const Schema = "infercrane.dev/quality-evidence/v1"

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
