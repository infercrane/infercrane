package trainingartifact

import (
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/passport"
)

func validPayload() Payload {
	return Payload{
		Schema: Schema, Deployment: "coder-production", RevisionID: "rev-19", Provider: "mlflow",
		ExternalRunID: "run-42", Repository: "mlflow://models/coder/42", ImmutableRevision: "42",
		ArtifactDigest: "sha256:" + strings.Repeat("a", 64), BaseModelIdentity: "acme/coder@base",
		Method: "lora", Framework: "transformers", FrameworkVersion: "5.0.0",
		DatasetFingerprint: "sha256:" + strings.Repeat("b", 64), ProducedAt: time.Now().UTC(),
	}
}

func TestSignedHandoffRoundTripAndTamperRejection(t *testing.T) {
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := passport.Sign(validPayload(), key)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(envelope)
	if err != nil || decoded.ExternalRunID != "run-42" {
		t.Fatalf("decoded=%#v err=%v", decoded, err)
	}
	envelope.PayloadJSON = strings.Replace(envelope.PayloadJSON, "run-42", "run-43", 1)
	if _, err = Decode(envelope); err == nil {
		t.Fatal("tampered handoff was accepted")
	}
}

func TestHandoffRejectsCredentialedOrMutableLocations(t *testing.T) {
	for _, repository := range []string{"https://user:secret@example.com/model", "https://example.com/model?token=secret", "../model", "model"} {
		payload := validPayload()
		payload.Repository = repository
		if err := payload.Validate(); err == nil {
			t.Fatalf("repository %q was accepted", repository)
		}
	}
}
