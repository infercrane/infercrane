package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

// migrationStoreFixture adds the closed API, its evidence, and verdicts to
// the quality fixture.
type migrationStoreFixture struct {
	*qualityStoreFixture
	target   domain.Target
	binding  domain.BackendBinding
	closed   []domain.BindingQualityEvidence
	verdicts []domain.MigrationQualityVerdict
}

func (f *migrationStoreFixture) ExternalBindingTarget(_ context.Context, _, endpoint, binding string) (domain.ResolvedEndpoint, domain.BackendBinding, domain.Target, error) {
	if endpoint != "support-bot" || binding != f.binding.Name {
		return domain.ResolvedEndpoint{}, domain.BackendBinding{}, domain.Target{}, domain.ErrNotFound
	}
	return domain.ResolvedEndpoint{Endpoint: domain.Endpoint{ID: "endpoint-1", Name: "support-bot"}}, f.binding, f.target, nil
}

func (f *migrationStoreFixture) RecordBindingQualityEvidence(_ context.Context, _, _, _ string, row domain.BindingQualityEvidence) (domain.BindingQualityEvidence, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row.ID, row.BindingID = fmt.Sprintf("closed-%d", len(f.closed)+1), f.binding.ID
	f.closed = append([]domain.BindingQualityEvidence{row}, f.closed...)
	return row, true, nil
}

func (f *migrationStoreFixture) BindingQualityEvidence(context.Context, string, string, int) ([]domain.BindingQualityEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.BindingQualityEvidence(nil), f.closed...), nil
}

func (f *migrationStoreFixture) RecordMigrationVerdict(_ context.Context, _, _ string, row domain.MigrationQualityVerdict) (domain.MigrationQualityVerdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row.ID = fmt.Sprintf("verdict-%d", len(f.verdicts)+1)
	f.verdicts = append([]domain.MigrationQualityVerdict{row}, f.verdicts...)
	return row, nil
}

func (f *migrationStoreFixture) MigrationVerdictsForCampaign(context.Context, string, string) ([]domain.MigrationQualityVerdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.MigrationQualityVerdict(nil), f.verdicts...), nil
}

type sealedCredential struct {
	token string
	err   error
}

func (c sealedCredential) Resolve(context.Context, string, string) (string, error) {
	return c.token, c.err
}
func (sealedCredential) Unreadable(err error) bool { return errors.Is(err, errUnreadableFixture) }

var errUnreadableFixture = errors.New("unreadable")

// keyedCompleter answers by subject and records the API key and dialect each
// subject was asked with.
type keyedCompleter struct {
	answers  map[string]func(qualitycheck.Prompt) string
	keys     map[string]string
	dialects map[string]qualitycheck.Dialect
	calls    int
	fail     string
}

func (c *keyedCompleter) Complete(_ context.Context, target qualitycheck.Target, _ qualitycheck.EvalSet, prompt qualitycheck.Prompt) (qualitycheck.Answer, error) {
	c.calls++
	c.keys[target.SubjectID], c.dialects[target.SubjectID] = target.APIKey, target.Dialect
	if c.fail == target.SubjectID {
		return qualitycheck.Answer{}, errors.New("429 rate limited")
	}
	return qualitycheck.Answer{Content: c.answers[target.SubjectID](prompt)}, nil
}

type migrationCampaign struct {
	qualityCampaign
	store     *migrationStoreFixture
	completer *keyedCompleter
	set       qualitycheck.EvalSet
}

func gradedEvalSet(t *testing.T, prompts int) (qualitycheck.EvalSet, domain.QualityEvalSet) {
	t.Helper()
	rows := make([]map[string]any, 0, prompts)
	for i := range prompts {
		rows = append(rows, map[string]any{"id": fmt.Sprintf("p%02d", i), "prompt": fmt.Sprintf("Say %d", i), "expected": []string{fmt.Sprint(i)}})
	}
	body, _ := json.Marshal(map[string]any{"schema": qualitycheck.EvalSetSchemaV1, "name": "support-graded", "version": "1", "grading": qualitycheck.GradingExactMatch, "prompts": rows})
	set, err := qualitycheck.DecodeEvalSet(body)
	if err != nil {
		t.Fatal(err)
	}
	return set, domain.QualityEvalSet{Digest: set.Digest, Body: string(body)}
}

func newMigrationCampaign(t *testing.T, check string, candidateAnswer func(qualitycheck.Prompt) string) migrationCampaign {
	t.Helper()
	set, stored := gradedEvalSet(t, 40)
	input := optimizer.Request{WorkloadSource: optimizer.WorkloadSourceCustomerObserved, WorkloadFingerprint: "sha256:" + strings.Repeat("a", 64), QualityCheck: check, Migration: &optimizer.MigrationBaseline{Endpoint: "support-bot", Binding: "primary", Dialect: optimizer.MigrationDialectOpenAI, MaxQualityRegressionPercent: 5}}
	if check == "" {
		input.QualityEvalSet = set.Digest
	}
	base := newQualityCampaign(t, input)
	base.store.evalSets = map[string]domain.QualityEvalSet{set.Digest: stored}
	store := &migrationStoreFixture{qualityStoreFixture: base.store, target: domain.Target{ID: "target-1", URL: "https://api.openai.invalid/v1", UpstreamModel: "gpt-4.1-mini"}, binding: domain.BackendBinding{ID: "binding-1", Name: "primary", Kind: "external", TargetID: "target-1", OwnershipMode: "traffic-managed"}}
	closedAPI := qualitycheck.BindingSubject("binding-1")
	completer := &keyedCompleter{keys: map[string]string{}, dialects: map[string]qualitycheck.Dialect{}, answers: map[string]func(qualitycheck.Prompt) string{
		closedAPI:     func(p qualitycheck.Prompt) string { return p.Expected[0] },
		"rev-fast":    candidateAnswer,
		"rev-control": candidateAnswer,
	}}
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	base.driver.Migration = MigrationQualityCheck{Store: store, Backends: map[string]BenchmarkBackend{"kubernetes": {APIKey: "worker-key"}}, Completer: completer, Credentials: sealedCredential{token: "sk-sealed"}, Signer: key}
	base.driver.Store = qualityCompositeStore{base.composite, base.store}
	return migrationCampaign{qualityCampaign: base, store: store, completer: completer, set: set}
}

func TestMigrationCheckPassesACandidateThatAnswersLikeTheClosedAPI(t *testing.T) {
	run := newMigrationCampaign(t, "", func(p qualitycheck.Prompt) string { return p.Expected[0] + "." })
	validated, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || !validated.Passed || validated.QualityEvidenceID == "" {
		t.Fatalf("validation = %+v err=%v", validated, err)
	}
	closedAPI := qualitycheck.BindingSubject("binding-1")
	if run.completer.keys[closedAPI] != "sk-sealed" || run.completer.dialects[closedAPI] != qualitycheck.DialectOpenAI || run.completer.keys["rev-fast"] != "worker-key" || run.completer.dialects["rev-fast"] != qualitycheck.DialectModelServer {
		t.Fatalf("keys=%v dialects=%v", run.completer.keys, run.completer.dialects)
	}
	candidate := run.store.evidence["qwen-opt-fast"][0]
	if candidate.Evaluator != qualitycheck.GradedEvaluator || candidate.ReferenceRevisionID != closedAPI || !candidate.Passed || candidate.Score != 1 {
		t.Fatalf("candidate evidence = %+v", candidate)
	}
	if len(run.store.closed) != 1 || run.store.closed[0].Source != domain.QualityEvidenceSourceInferCrane || run.store.closed[0].Score != 1 {
		t.Fatalf("closed API evidence = %+v", run.store.closed)
	}
	verdict := run.store.verdicts[0]
	if verdict.Verdict != releaseguard.QualityPass || verdict.RevisionID != "rev-fast" || verdict.BindingID != "binding-1" || verdict.CandidateRunID != run.candidate.ID {
		t.Fatalf("verdict = %+v", verdict)
	}
	payload, err := qualityevidence.DecodeMigrationVerdict(passport.Envelope{PayloadJSON: verdict.PayloadJSON, Digest: verdict.PayloadDigest, Signature: verdict.Signature, PublicKey: verdict.PublicKey, Algorithm: verdict.Algorithm, KeyID: verdict.KeyID})
	if err != nil || payload.CandidateEvidenceDigest != candidate.PayloadDigest || payload.ReferenceEvidenceDigest != run.store.closed[0].PayloadDigest || payload.Bootstrap == nil {
		t.Fatalf("signed verdict = %+v err=%v", payload, err)
	}

	// A retry adopts the signed verdict instead of asking again.
	calls := run.completer.calls
	again, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || again.QualityEvidenceID != validated.QualityEvidenceID || run.completer.calls != calls {
		t.Fatalf("retry = %+v err=%v calls %d -> %d", again, err, calls, run.completer.calls)
	}
}

func TestMigrationCheckBlocksWorseAnswersAndLeavesCloseCallsInconclusive(t *testing.T) {
	worse := newMigrationCampaign(t, "", func(p qualitycheck.Prompt) string {
		if strings.HasSuffix(p.ID, "0") || strings.HasSuffix(p.ID, "5") || strings.HasSuffix(p.ID, "7") {
			return "I don't know"
		}
		return p.Expected[0]
	})
	validated, err := worse.driver.Validate(t.Context(), worse.candidate.ID, worse.candidate)
	if err != nil || validated.Passed || validated.FailureCode != FailureQualityRejected {
		t.Fatalf("worse validation = %+v err=%v", validated, err)
	}
	if worse.store.evidence["qwen-opt-fast"][0].Passed || worse.store.verdicts[0].Verdict != releaseguard.QualityBlock {
		t.Fatalf("worse evidence/verdict = %+v %+v", worse.store.evidence["qwen-opt-fast"][0], worse.store.verdicts[0])
	}
	close := newMigrationCampaign(t, "", func(p qualitycheck.Prompt) string {
		if p.ID == "p03" {
			return ""
		}
		return p.Expected[0]
	})
	validated, err = close.driver.Validate(t.Context(), close.candidate.ID, close.candidate)
	if err != nil || validated.Passed || !validated.Inconclusive || validated.FailureCode != FailureMigrationQualityInconclusive {
		t.Fatalf("close-call validation = %+v err=%v", validated, err)
	}
}

func TestMigrationCheckNeverScoresAFailedOrUnauthenticatedClosedAPIRequest(t *testing.T) {
	run := newMigrationCampaign(t, "", func(p qualitycheck.Prompt) string { return p.Expected[0] })
	run.completer.fail = qualitycheck.BindingSubject("binding-1")
	if _, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate); err == nil || len(run.store.verdicts) != 0 {
		t.Fatalf("a rate-limited closed API produced a verdict: err=%v verdicts=%v", err, run.store.verdicts)
	}
	unreadable := newMigrationCampaign(t, "", func(p qualitycheck.Prompt) string { return p.Expected[0] })
	check := unreadable.driver.Migration.(MigrationQualityCheck)
	check.Credentials = sealedCredential{err: errUnreadableFixture}
	unreadable.driver.Migration = check
	validated, err := unreadable.driver.Validate(t.Context(), unreadable.candidate.ID, unreadable.candidate)
	if err != nil || !validated.Inconclusive || validated.FailureCode != FailureQualityReferenceUnavailable || unreadable.completer.calls != 0 {
		t.Fatalf("unreadable credential = %+v err=%v calls=%d", validated, err, unreadable.completer.calls)
	}
}

func signedPayload(t *testing.T, key []byte, payload qualityevidence.Payload) passport.Envelope {
	t.Helper()
	envelope, err := passport.Sign(payload, key)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func TestMigrationCustomerEvidenceComparesBothSignedEvaluations(t *testing.T) {
	run := newMigrationCampaign(t, optimizer.QualityCheckCustomer, nil)
	// The candidate's own passing evaluation is not enough on its own.
	scores := make([]float64, 40)
	for i := range scores {
		scores[i] = 1
	}
	pairing := "sha256:" + strings.Repeat("d", 64)
	_, key, _ := passport.GenerateKey()
	candidatePayload := qualityevidence.Payload{Schema: qualityevidence.Schema, Deployment: "qwen-opt-fast", RevisionID: "rev-fast", Suite: "support", SuiteVersion: "7", Evaluator: "acme-eval", EvaluatorVersion: "3", Score: 1, Passed: true, SampleCount: 40, ArtifactDigest: "sha256:" + strings.Repeat("e", 64), EvaluatedAt: time.Now().UTC().Truncate(time.Second), Distribution: &qualityevidence.Distribution{Schema: qualityevidence.DistributionSchema, Kind: "paired_scores", PairingDigest: pairing, Scores: scores}}
	envelope := signedPayload(t, key, candidatePayload)
	row, _ := qualityevidence.Record(candidatePayload, envelope)
	row.ID, row.Source = "customer-1", domain.QualityEvidenceSourceCustomer
	run.store.evidence = map[string][]domain.QualityEvidence{"qwen-opt-fast": {row}}
	if _, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate); err == nil || !strings.Contains(err.Error(), "binding:") {
		t.Fatalf("validation without the closed API's evidence: %v", err)
	}
	referencePayload := candidatePayload
	referencePayload.Deployment, referencePayload.RevisionID = "support-bot", qualitycheck.BindingSubject("binding-1")
	referenceEnvelope := signedPayload(t, key, referencePayload)
	run.store.closed = []domain.BindingQualityEvidence{bindingEvidence(referencePayload, referenceEnvelope, domain.QualityEvidenceSourceCustomer)}
	validated, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || !validated.Passed || validated.QualityEvidenceID != "customer-1" || run.completer.calls != 0 {
		t.Fatalf("customer migration validation = %+v err=%v calls=%d", validated, err, run.completer.calls)
	}
	if run.store.verdicts[0].Verdict != releaseguard.QualityPass {
		t.Fatalf("verdict = %+v", run.store.verdicts[0])
	}
}
