package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualityevidence"
)

func TestEvaluationIngestSignsAndOptionallyAttachesStrictResult(t *testing.T) {
	directory := t.TempDir()
	_, privateKey, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(directory, "evaluator.key")
	if err = os.WriteFile(keyPath, []byte(passport.EncodePrivateKey(privateKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(directory, "result.json")
	resultJSON := `{"schema":"infercrane.dev/evaluator-result/v1","suite":"support-answers","suite_version":"git:8a91d7c","evaluator":"custom-ci","evaluator_version":"1.4.0","score":0.93,"passed":true,"sample_count":250,"artifact_digest":"sha256:` + strings.Repeat("a", 64) + `","evaluated_at":"2026-08-13T20:00:00Z"}`
	if err = os.WriteFile(resultPath, []byte(resultJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	var attached passport.Envelope
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/deployments/coder-prod/quality-evidence" {
			t.Fatalf("request=%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&attached); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "test-secret")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(directory, "missing-client.json"))
	evidencePath := filepath.Join(directory, "evidence.json")
	output, err := captureStdout(t, func() error {
		return evaluationIngestCommand(context.Background(), []string{"coder-prod", "rev-19", "--result", resultPath, "--key", keyPath, "--file", evidencePath, "--attach"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "Attached    true") || attached.Digest == "" {
		t.Fatalf("output=%q attached=%+v", output, attached)
	}
	payload, err := qualityevidence.Decode(attached)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Deployment != "coder-prod" || payload.RevisionID != "rev-19" || payload.Evaluator != "custom-ci" {
		t.Fatalf("payload=%+v", payload)
	}
	if _, _, err = readQualityEnvelope(evidencePath); err != nil {
		t.Fatalf("written evidence did not verify: %v", err)
	}
}

func TestEvaluationIngestRejectsPromptContentBeforeSigning(t *testing.T) {
	directory := t.TempDir()
	resultPath := filepath.Join(directory, "result.json")
	result := `{"schema":"infercrane.dev/evaluator-result/v1","suite":"s","suite_version":"v1","evaluator":"e","evaluator_version":"v1","score":0.8,"passed":true,"sample_count":1,"artifact_digest":"sha256:` + strings.Repeat("b", 64) + `","evaluated_at":"2026-08-13T20:00:00Z","prompt":"do not persist"}`
	if err := os.WriteFile(resultPath, []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
	err := evaluationIngestCommand(context.Background(), []string{"prod", "rev-2", "--result", resultPath, "--key", filepath.Join(directory, "missing"), "--file", filepath.Join(directory, "evidence.json")})
	if err == nil || !strings.Contains(err.Error(), "unknown field \"prompt\"") {
		t.Fatalf("err=%v", err)
	}
}

func TestEvaluationIngestPreservesSignedEvidenceWhenAttachmentFails(t *testing.T) {
	directory := t.TempDir()
	_, privateKey, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(directory, "evaluator.key")
	if err = os.WriteFile(keyPath, []byte(passport.EncodePrivateKey(privateKey)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(directory, "result.json")
	result := `{"schema":"infercrane.dev/evaluator-result/v1","suite":"s","suite_version":"v1","evaluator":"e","evaluator_version":"v1","score":0.8,"passed":true,"sample_count":1,"artifact_digest":"sha256:` + strings.Repeat("c", 64) + `","evaluated_at":"2026-08-13T20:00:00Z"}`
	if err = os.WriteFile(resultPath, []byte(result), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"unavailable","message":"try later"}}`)
	}))
	defer server.Close()
	t.Setenv("INFERCRANE_URL", server.URL)
	t.Setenv("INFERCRANE_API_KEY", "test-secret")
	t.Setenv("INFERCRANE_CONFIG", filepath.Join(directory, "missing-client.json"))
	evidencePath := filepath.Join(directory, "retryable-evidence.json")
	err = evaluationIngestCommand(context.Background(), []string{"prod", "rev-2", "--result", resultPath, "--key", keyPath, "--file", evidencePath, "--attach"})
	if err == nil || !strings.Contains(err.Error(), "signed evidence was written") {
		t.Fatalf("err=%v", err)
	}
	if _, _, verifyErr := readQualityEnvelope(evidencePath); verifyErr != nil {
		t.Fatalf("retryable evidence missing or invalid: %v", verifyErr)
	}
}
