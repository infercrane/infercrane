// Package trainingartifact verifies content-free, signed handoffs from
// externally owned training systems. It deliberately does not schedule jobs,
// read training data, or fetch checkpoint contents.
package trainingartifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/passport"
)

const Schema = "infercrane.dev/training-artifact-handoff/v1"

var (
	sha256Pattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	namePattern   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/@+-]{0,255}$`)
)

// Payload contains provenance and immutable identity only. Dataset contents,
// prompts, outputs, credentials, and training logs are intentionally absent.
type Payload struct {
	Schema             string    `json:"schema"`
	Deployment         string    `json:"deployment"`
	RevisionID         string    `json:"revision_id"`
	Provider           string    `json:"provider"`
	ExternalRunID      string    `json:"external_run_id"`
	Repository         string    `json:"repository"`
	ImmutableRevision  string    `json:"immutable_revision"`
	ArtifactDigest     string    `json:"artifact_digest"`
	BaseModelIdentity  string    `json:"base_model_identity,omitempty"`
	Method             string    `json:"method,omitempty"`
	Framework          string    `json:"framework,omitempty"`
	FrameworkVersion   string    `json:"framework_version,omitempty"`
	DatasetFingerprint string    `json:"dataset_fingerprint,omitempty"`
	ProducedAt         time.Time `json:"produced_at"`
}

func (p Payload) Validate() error {
	if p.Schema != Schema {
		return fmt.Errorf("training handoff schema must be %q", Schema)
	}
	for field, value := range map[string]string{
		"deployment": p.Deployment, "revision_id": p.RevisionID, "provider": p.Provider,
		"external_run_id": p.ExternalRunID, "immutable_revision": p.ImmutableRevision,
	} {
		if !namePattern.MatchString(value) {
			return fmt.Errorf("%s must be non-empty, bounded, and contain only safe identifier characters", field)
		}
	}
	if err := validateRepository(p.Repository); err != nil {
		return err
	}
	if !sha256Pattern.MatchString(p.ArtifactDigest) {
		return errors.New("artifact_digest must be sha256:<64 lowercase hex characters>")
	}
	for field, value := range map[string]string{
		"base_model_identity": p.BaseModelIdentity, "method": p.Method, "framework": p.Framework,
		"framework_version": p.FrameworkVersion, "dataset_fingerprint": p.DatasetFingerprint,
	} {
		if len(value) > 512 || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("%s must be at most 512 characters and single-line", field)
		}
	}
	if p.ProducedAt.IsZero() || p.ProducedAt.After(time.Now().UTC().Add(5*time.Minute)) {
		return errors.New("produced_at must be set and cannot be in the future")
	}
	return nil
}

func Decode(envelope passport.Envelope) (Payload, error) {
	if err := passport.Verify(envelope); err != nil {
		return Payload{}, fmt.Errorf("verify training handoff: %w", err)
	}
	if len(envelope.PayloadJSON) > 64<<10 {
		return Payload{}, errors.New("training handoff payload exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(envelope.PayloadJSON)))
	decoder.DisallowUnknownFields()
	var payload Payload
	if err := decoder.Decode(&payload); err != nil {
		return Payload{}, fmt.Errorf("decode training handoff payload: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Payload{}, errors.New("training handoff contains trailing JSON")
		}
		return Payload{}, fmt.Errorf("decode trailing training handoff: %w", err)
	}
	if err := payload.Validate(); err != nil {
		return Payload{}, err
	}
	return payload, nil
}

func validateRepository(value string) error {
	if value == "" || len(value) > 2048 || strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("repository must be a non-empty, bounded, single-line immutable artifact location")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return errors.New("repository must be a valid artifact location")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("repository must not contain credentials, query parameters, or fragments")
	}
	if parsed.Scheme == "" {
		// Hugging Face-style repository identities such as org/model remain
		// valid. Paths that could escape a workspace do not.
		if strings.HasPrefix(value, "/") || strings.Contains(value, "..") || !strings.Contains(value, "/") {
			return errors.New("repository must be an org/model identity or an approved artifact URI")
		}
		return nil
	}
	switch parsed.Scheme {
	case "hf", "mlflow", "s3", "gs", "https":
	default:
		return fmt.Errorf("repository scheme %q is not supported", parsed.Scheme)
	}
	if parsed.Host == "" && parsed.Scheme != "s3" && parsed.Scheme != "gs" {
		return errors.New("repository URI must include a host")
	}
	return nil
}
