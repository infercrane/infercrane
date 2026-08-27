package qualityevidence

import (
	"encoding/json"
	"reflect"
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
	if err != nil || !reflect.DeepEqual(decoded, payload) {
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

func TestEvaluatorResultStrictlyDecodesAndBinds(t *testing.T) {
	resultJSON := `{
	  "schema":"infercrane.dev/evaluator-result/v2",
  "suite":"support-answers",
  "suite_version":"git:8a91d7c",
  "evaluator":"custom-ci",
  "evaluator_version":"1.4.0",
  "score":0.93,
  "passed":true,
  "sample_count":250,
  "artifact_digest":"sha256:` + strings.Repeat("d", 64) + `",
  "evaluated_at":"2026-08-13T20:00:00Z"
}`
	result, err := DecodeResult([]byte(resultJSON))
	if err != nil {
		t.Fatal(err)
	}
	payload := result.Bind("coder-prod", "rev-19")
	if err = payload.Validate(); err != nil {
		t.Fatal(err)
	}
	if payload.Deployment != "coder-prod" || payload.RevisionID != "rev-19" || payload.Score != .93 {
		t.Fatalf("payload=%+v", payload)
	}
}

func TestEvaluatorResultRejectsContentUnknownFieldsAndOversize(t *testing.T) {
	base := `{"schema":"infercrane.dev/evaluator-result/v2","suite":"s","suite_version":"v1","evaluator":"e","evaluator_version":"v1","score":0.8,"passed":true,"sample_count":1,"artifact_digest":"sha256:` + strings.Repeat("e", 64) + `","evaluated_at":"2026-08-13T20:00:00Z"`
	for name, suffix := range map[string]string{
		"prompt content":  `,"prompt":"secret prompt"}`,
		"output content":  `,"output":"secret answer"}`,
		"trailing object": `} {"unexpected":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeResult([]byte(base + suffix)); err == nil {
				t.Fatalf("expected %s rejection", name)
			}
		})
	}
	if _, err := DecodeResult(make([]byte, MaxFileSize+1)); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("err=%v", err)
	}
}

func TestPairedDistributionIsBoundedAndContentFree(t *testing.T) {
	base := Payload{Schema: Schema, Deployment: "prod", RevisionID: "rev", Suite: "suite", SuiteVersion: "v1", Evaluator: "eval", EvaluatorVersion: "v1", Score: .8, Passed: true, SampleCount: 3, Distribution: &Distribution{Schema: DistributionSchema, Kind: "paired_scores", PairingDigest: "sha256:" + strings.Repeat("f", 64), Scores: []float64{.7, .8, .9}}, ArtifactDigest: "sha256:" + strings.Repeat("a", 64), EvaluatedAt: time.Now().UTC()}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	base.Distribution.Scores = []float64{.7}
	if err := base.Validate(); err == nil || !strings.Contains(err.Error(), "match sample_count") {
		t.Fatalf("mismatched distribution accepted: %v", err)
	}
}
