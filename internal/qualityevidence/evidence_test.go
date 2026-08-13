package qualityevidence

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/passport"
)

func TestSignedEvidenceRoundTripAndTamperRejection(t *testing.T) {
	_, privateKey, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload := Payload{Schema: Schema, Deployment: "coder-prod", RevisionID: "rev-2", Suite: "acme-support", SuiteVersion: "2026-08-13", Evaluator: "ragas", EvaluatorVersion: "0.3.1", Score: .91, Passed: true, SampleCount: 200, ArtifactDigest: "sha256:" + strings.Repeat("a", 64), EvaluatedAt: time.Now().UTC()}
	envelope, err := passport.Sign(payload, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(envelope)
	if err != nil || decoded != payload {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	var mutated map[string]any
	_ = json.Unmarshal([]byte(envelope.PayloadJSON), &mutated)
	mutated["score"] = .1
	body, _ := json.Marshal(mutated)
	envelope.PayloadJSON = string(body)
	if _, err = Decode(envelope); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("expected tamper rejection, got %v", err)
	}
}

func TestEvidenceValidationAndComparability(t *testing.T) {
	base := Payload{Schema: Schema, Deployment: "prod", RevisionID: "rev-1", Suite: "suite", SuiteVersion: "v1", Evaluator: "eval", EvaluatorVersion: "v1", Score: .8, Passed: true, SampleCount: 1, ArtifactDigest: "sha256:" + strings.Repeat("b", 64), EvaluatedAt: time.Now().UTC()}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	other := base
	other.RevisionID = "rev-2"
	if !Comparable(base, other) {
		t.Fatal("revision-bound evidence from the same versioned suite should compare")
	}
	other.EvaluatorVersion = "v2"
	if Comparable(base, other) {
		t.Fatal("different evaluator versions must not compare")
	}
	bad := base
	bad.Score = 1.1
	if err := bad.Validate(); err == nil {
		t.Fatal("out-of-range score accepted")
	}
}

func TestSignedEvidenceRejectsTrailingJSON(t *testing.T) {
	_, privateKey, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	payload := Payload{Schema: Schema, Deployment: "prod", RevisionID: "rev-1", Suite: "suite", SuiteVersion: "v1", Evaluator: "eval", EvaluatorVersion: "v1", Score: .8, Passed: true, SampleCount: 1, ArtifactDigest: "sha256:" + strings.Repeat("c", 64), EvaluatedAt: time.Now().UTC()}
	envelope, err := passport.Sign(payload, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	envelope.PayloadJSON += ` {"unexpected":true}`
	if _, err = Decode(envelope); err == nil {
		t.Fatalf("expected trailing JSON rejection, got %v", err)
	}
}
