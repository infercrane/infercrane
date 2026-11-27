package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/qualitycheck"
)

func TestBuiltinQualityEvidenceKeepsItsSourceAndReference(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	name := "builtin-quality-" + time.Now().UTC().Format("150405.000000000")
	if _, err := s.AddTarget(ctx, domain.Target{Name: name + "-target", URL: "http://" + name, Provider: "existing", Runtime: "vllm", UpstreamModel: "model-v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "model-v1"}, []string{name + "-target"}); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveForTenant(ctx, "global", name)
	if err != nil {
		t.Fatal(err)
	}
	revision := resolved.Deployment.ActiveRevisionID
	row := func(digest int, source, reference string) domain.QualityEvidence {
		return domain.QualityEvidence{RevisionID: revision, Suite: "infercrane-default", SuiteVersion: "1@" + qualitycheck.DefaultSuiteDigest, Evaluator: qualitycheck.Evaluator, EvaluatorVersion: qualitycheck.EvaluatorVersion, Score: 1, Passed: true, SampleCount: 24, ArtifactDigest: "sha256:" + strings.Repeat("a", 64), PayloadDigest: fmt.Sprintf("sha256:%064x", digest), Signature: "signature", PublicKey: "public-key", Algorithm: "Ed25519-SHA256", KeyID: "sha256:key", Source: source, ReferenceRevisionID: reference, EvaluatedAt: time.Now().UTC()}
	}
	builtin, created, err := s.RecordQualityEvidence(ctx, "global", name, row(1, domain.QualityEvidenceSourceInferCrane, revision))
	if err != nil || !created {
		t.Fatalf("record built-in evidence: created=%t err=%v", created, err)
	}
	legacy, _, err := s.RecordQualityEvidence(ctx, "global", name, row(2, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.RecordQualityEvidence(ctx, "global", name, row(3, "someone-else", "")); err == nil {
		t.Fatal("an unknown evidence source was accepted")
	}
	// The same digest under a different source is a conflict, not an adoption.
	if _, _, err = s.RecordQualityEvidence(ctx, "global", name, row(1, domain.QualityEvidenceSourceCustomer, "")); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("source change on an existing digest: %v", err)
	}
	rows, err := s.QualityEvidenceForDeployment(ctx, "global", name, 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	byID, err := s.QualityEvidenceByIDs(ctx, "global", []string{builtin.ID, legacy.ID, builtin.ID, "", "missing"})
	if err != nil || len(byID) != 2 {
		t.Fatalf("by IDs=%+v err=%v", byID, err)
	}
	if got := byID[builtin.ID]; got.Source != domain.QualityEvidenceSourceInferCrane || got.ReferenceRevisionID != revision {
		t.Fatalf("built-in row = %+v", got)
	}
	if got := byID[legacy.ID]; got.Source != domain.QualityEvidenceSourceCustomer || got.ReferenceRevisionID != "" {
		t.Fatalf("customer row = %+v", got)
	}
	if other, err := s.QualityEvidenceByIDs(ctx, "other-tenant", []string{builtin.ID}); err != nil || len(other) != 0 {
		t.Fatalf("another tenant read the evidence: %+v err=%v", other, err)
	}
}

func TestQualityEvalSetsAreContentAddressedPerTenant(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	body := []byte(`{"schema":"infercrane.dev/quality-eval-set/v1","name":"support","version":"3","prompts":[{"id":"refund","prompt":"How do I get a refund?"},{"id":"hours","prompt":"When are you open?"}]}`)
	digest := qualitycheck.Digest(body)
	stored, created, err := s.RecordQualityEvalSet(ctx, "global", "operator", body)
	if err != nil || !created || stored.Digest != digest || stored.Name != "support" || stored.Version != "3" || stored.PromptCount != 2 || stored.Body != string(body) || stored.CreatedBy != "operator" || stored.CreatedAt.IsZero() {
		t.Fatalf("stored=%+v created=%t err=%v", stored, created, err)
	}
	again, created, err := s.RecordQualityEvalSet(ctx, "global", "someone-else", body)
	if err != nil || created || again.ID != stored.ID || again.CreatedBy != "operator" {
		t.Fatalf("repeat upload=%+v created=%t err=%v", again, created, err)
	}
	if _, _, err = s.RecordQualityEvalSet(ctx, "global", "operator", []byte(`{"schema":"infercrane.dev/quality-eval-set/v1"}`)); err == nil {
		t.Fatal("an invalid eval set was stored")
	}
	if _, err = s.QualityEvalSet(ctx, "other-tenant", digest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("another tenant read the eval set: %v", err)
	}
	loaded, err := s.QualityEvalSet(ctx, "global", digest)
	if err != nil || loaded.Body != string(body) || qualitycheck.Digest([]byte(loaded.Body)) != digest {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	if err = s.DeleteQualityEvalSet(ctx, "global", digest); err != nil {
		t.Fatal(err)
	}
	if err = s.DeleteQualityEvalSet(ctx, "global", digest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err = s.QualityEvalSet(ctx, "global", digest); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("deleted eval set: %v", err)
	}
}

// A full task set (1,000 items, more than the general eval set's 512 KiB) is
// stored like any eval set.
func TestAFullTaskSetIsStored(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	prompts := make([]map[string]any, 0, qualitycheck.MaxTaskPrompts)
	for index := range qualitycheck.MaxTaskPrompts {
		prompts = append(prompts, map[string]any{"id": fmt.Sprintf("gsm8k.test.%d", index), "prompt": strings.Repeat("A worked question with enough text. ", 18), "expected": []string{fmt.Sprint(index)}})
	}
	body, err := json.Marshal(map[string]any{"schema": qualitycheck.EvalSetSchemaV1, "name": "task-set", "version": "1", "max_tokens": 1024, "grading": qualitycheck.GradingAnswerLine, "prompts": prompts})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 512*1024 || len(body) > qualitycheck.MaxEvalSetBytes {
		t.Fatalf("the test set is %d bytes, want between 512 KiB and 1 MiB", len(body))
	}
	stored, created, err := s.RecordQualityEvalSet(ctx, "global", "operator", body)
	if err != nil || !created || stored.PromptCount != qualitycheck.MaxTaskPrompts {
		t.Fatalf("stored=%+v created=%t err=%v", stored.PromptCount, created, err)
	}
}
