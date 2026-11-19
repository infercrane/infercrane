package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualitycheck"
)

// qualityStoreFixture is the built-in check's view of persistence: deployments
// by name, replicas by deployment ID, eval sets, and evidence by deployment.
type qualityStoreFixture struct {
	mu          sync.Mutex
	deployments map[string]domain.Deployment
	replicas    map[string][]domain.Replica
	evalSets    map[string]domain.QualityEvalSet
	evidence    map[string][]domain.QualityEvidence
	recorded    int
}

func (f *qualityStoreFixture) ResolveForTenant(_ context.Context, _, name string) (domain.ResolvedDeployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	deployment, ok := f.deployments[name]
	if !ok {
		return domain.ResolvedDeployment{}, domain.ErrNotFound
	}
	return domain.ResolvedDeployment{Deployment: deployment}, nil
}

func (f *qualityStoreFixture) ReplicasForDeployment(_ context.Context, _, deploymentID string) ([]domain.Replica, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Replica(nil), f.replicas[deploymentID]...), nil
}

func (f *qualityStoreFixture) QualityEvalSet(_ context.Context, _, digest string) (domain.QualityEvalSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	set, ok := f.evalSets[digest]
	if !ok {
		return domain.QualityEvalSet{}, domain.ErrNotFound
	}
	return set, nil
}

func (f *qualityStoreFixture) QualityEvidenceForDeployment(_ context.Context, _, name string, _ int) ([]domain.QualityEvidence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.QualityEvidence(nil), f.evidence[name]...), nil
}

func (f *qualityStoreFixture) RecordQualityEvidence(_ context.Context, _, name string, row domain.QualityEvidence) (domain.QualityEvidence, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recorded++
	row.ID = fmt.Sprintf("quality-%d", f.recorded)
	if f.evidence == nil {
		f.evidence = map[string][]domain.QualityEvidence{}
	}
	f.evidence[name] = append([]domain.QualityEvidence{row}, f.evidence[name]...)
	return row, true, nil
}

func (f *qualityStoreFixture) addServing(name, deploymentID, revision, endpoint string) {
	if f.deployments == nil {
		f.deployments, f.replicas = map[string]domain.Deployment{}, map[string][]domain.Replica{}
	}
	deployment := f.deployments[name]
	deployment.ID, deployment.Name = deploymentID, name
	if deployment.ActiveRevisionID == "" {
		deployment.ActiveRevisionID = revision
	}
	f.deployments[name] = deployment
	f.replicas[deploymentID] = append(f.replicas[deploymentID], domain.Replica{RevisionID: revision, Endpoint: endpoint, Provider: "kubernetes", Health: "healthy", LifecycleState: "ready"})
}

// answerCompleter answers by endpoint. Unknown prompts get the default.
type answerCompleter struct {
	mu      sync.Mutex
	answers map[string]map[string]string
	calls   map[string]int
}

func (c *answerCompleter) Complete(_ context.Context, target qualitycheck.Target, _ qualitycheck.EvalSet, prompt qualitycheck.Prompt) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[target.Endpoint]++
	if answer, ok := c.answers[target.Endpoint][prompt.ID]; ok {
		return answer, nil
	}
	return "answer to " + prompt.ID, nil
}

func (c *answerCompleter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, count := range c.calls {
		total += count
	}
	return total
}

type qualityCampaign struct {
	store     *qualityStoreFixture
	composite *compositeStoreFixture
	completer *answerCompleter
	driver    CompositeDriver
	control   domain.OptimizationCandidateRun
	candidate domain.OptimizationCandidateRun
}

// newQualityCampaign builds a new-endpoint campaign with a serving control and
// one serving candidate, both on the kubernetes provider.
func newQualityCampaign(t *testing.T, input optimizer.Request) qualityCampaign {
	t.Helper()
	now := time.Now().UTC()
	draft := optimizer.DeploymentDraft{APIVersion: "infercrane.dev/v1", Kind: "Deployment", Name: "qwen"}
	draft.Model.ID = "Qwen/Qwen3-0.6B"
	encoded, _ := json.Marshal(draft)
	proposal, _ := json.Marshal(optimizer.Proposal{Input: input, StartingPoint: &optimizer.StartingPoint{CandidateID: "proposal-control"}, Candidates: []optimizer.Candidate{{ID: "proposal-control", BenchmarkProfile: "interactive"}, {ID: "proposal-fast", BenchmarkProfile: "interactive"}}})
	expiry := now.Add(time.Hour)
	control := domain.OptimizationCandidateRun{ID: "control-1", TenantID: "tenant", CampaignID: "campaign-1", ProposalCandidateID: "proposal-control", State: CandidateRanked, DeploymentName: "qwen-opt-control", RevisionID: "rev-control", DeploymentSpecJSON: string(encoded), QualityEvidenceID: "quality-control"}
	candidate := domain.OptimizationCandidateRun{ID: "fast-1", TenantID: "tenant", CampaignID: "campaign-1", ProposalCandidateID: "proposal-fast", State: CandidateValidating, DeploymentName: "qwen-opt-fast", RevisionID: "rev-fast", DeploymentSpecJSON: string(encoded)}
	campaign := domain.OptimizationCampaign{ID: "campaign-1", TenantID: "tenant", Intent: IntentNewEndpoint, ProposalJSON: string(proposal), ApprovalExpiresAt: &expiry, Candidates: []domain.OptimizationCandidateRun{control, candidate}}
	store := &qualityStoreFixture{}
	store.addServing("qwen-opt-control", "dep-control", "rev-control", "http://control")
	store.addServing("qwen-opt-fast", "dep-fast", "rev-fast", "http://fast")
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	completer := &answerCompleter{}
	composite := &compositeStoreFixture{campaign: campaign}
	check := BuiltinQualityCheck{Store: store, Backends: map[string]BenchmarkBackend{"kubernetes": {APIKey: "worker-key"}}, Completer: completer, Signer: key, Now: func() time.Time { return now }}
	return qualityCampaign{store: store, composite: composite, completer: completer, control: control, candidate: candidate, driver: CompositeDriver{Store: qualityCompositeStore{composite, store}, Quality: check}}
}

// qualityCompositeStore answers evidence lookups from the quality fixture so
// the driver and the check see the same rows.
type qualityCompositeStore struct {
	*compositeStoreFixture
	quality *qualityStoreFixture
}

func (s qualityCompositeStore) QualityEvidenceForDeployment(ctx context.Context, tenant, name string, limit int) ([]domain.QualityEvidence, error) {
	return s.quality.QualityEvidenceForDeployment(ctx, tenant, name, limit)
}

func TestBuiltinQualityCheckScoresCandidateAgainstControlAndSignsEvidence(t *testing.T) {
	run := newQualityCampaign(t, optimizer.Request{})
	validated, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || !validated.Passed || validated.QualityEvidenceID == "" || validated.Inconclusive {
		t.Fatalf("validation = %+v err=%v", validated, err)
	}
	set := qualitycheck.Default()
	if run.completer.calls["http://control"] != len(set.Prompts) || run.completer.calls["http://fast"] != len(set.Prompts) {
		t.Fatalf("calls = %v", run.completer.calls)
	}
	rows := run.store.evidence["qwen-opt-fast"]
	if len(rows) != 1 {
		t.Fatalf("candidate rows = %+v", rows)
	}
	row := rows[0]
	if row.Source != domain.QualityEvidenceSourceInferCrane || row.ReferenceRevisionID != "rev-control" || row.Evaluator != qualitycheck.Evaluator || row.Suite != "infercrane-default" || row.SuiteVersion != set.SuiteVersion() || row.Score != 1 || !row.Passed || row.SampleCount != len(set.Prompts) || row.ID != validated.QualityEvidenceID {
		t.Fatalf("candidate evidence = %+v", row)
	}
	envelope := passport.Envelope{Digest: row.PayloadDigest, Signature: row.Signature, PublicKey: row.PublicKey, Algorithm: row.Algorithm, KeyID: row.KeyID}
	if envelope.Algorithm != passport.Algorithm || !strings.HasPrefix(envelope.KeyID, "sha256:") {
		t.Fatalf("evidence is not signed: %+v", envelope)
	}
	reference := run.store.evidence["qwen-opt-control"]
	if len(reference) != 1 || reference[0].RevisionID != "rev-control" || reference[0].Score != 1 || reference[0].ReferenceRevisionID != "rev-control" || reference[0].Source != domain.QualityEvidenceSourceInferCrane {
		t.Fatalf("reference evidence = %+v", reference)
	}

	// A retry adopts the recorded check instead of asking again.
	before := run.completer.total()
	again, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || again.QualityEvidenceID != validated.QualityEvidenceID || run.completer.total() != before {
		t.Fatalf("retry = %+v err=%v calls %d -> %d", again, err, before, run.completer.total())
	}
}

func TestBuiltinQualityCheckBlocksCandidateWhoseAnswersDrift(t *testing.T) {
	run := newQualityCampaign(t, optimizer.Request{})
	drift := map[string]string{}
	for index, prompt := range qualitycheck.Default().Prompts {
		if index%4 == 0 {
			drift[prompt.ID] = "something else entirely"
		}
	}
	run.completer.answers = map[string]map[string]string{"http://fast": drift}
	validated, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || validated.Passed || validated.FailureCode != FailureQualityRejected || validated.QualityEvidenceID == "" {
		t.Fatalf("validation = %+v err=%v", validated, err)
	}
	if row := run.store.evidence["qwen-opt-fast"][0]; row.Passed || row.Score >= 0.9 {
		t.Fatalf("drifting candidate evidence = %+v", row)
	}
}

func TestBuiltinQualityCheckControlIsItsOwnReference(t *testing.T) {
	run := newQualityCampaign(t, optimizer.Request{})
	control := run.control
	control.State, control.QualityEvidenceID = CandidateValidating, ""
	run.composite.campaign.Candidates[0] = control
	validated, err := run.driver.Validate(t.Context(), control.ID, control)
	if err != nil || !validated.Passed {
		t.Fatalf("control validation = %+v err=%v", validated, err)
	}
	if run.completer.calls["http://fast"] != 0 || run.completer.calls["http://control"] != len(qualitycheck.Default().Prompts) {
		t.Fatalf("control asked the wrong revisions: %v", run.completer.calls)
	}
	rows := run.store.evidence["qwen-opt-control"]
	if len(rows) != 1 || rows[0].Score != 1 || rows[0].ReferenceRevisionID != "rev-control" {
		t.Fatalf("control evidence = %+v", rows)
	}
}

func TestCustomerEvidenceWinsOverTheBuiltinCheck(t *testing.T) {
	run := newQualityCampaign(t, optimizer.Request{})
	run.store.evidence = map[string][]domain.QualityEvidence{"qwen-opt-fast": {{ID: "customer-1", RevisionID: "rev-fast", Source: domain.QualityEvidenceSourceCustomer, Passed: false, EvaluatedAt: time.Now()}}}
	validated, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || validated.Passed || validated.QualityEvidenceID != "customer-1" || run.completer.total() != 0 {
		t.Fatalf("customer evidence did not win: %+v err=%v calls=%d", validated, err, run.completer.total())
	}
	// Built-in evidence for the same revision never outranks customer evidence.
	run.store.evidence["qwen-opt-fast"] = append([]domain.QualityEvidence{{ID: "builtin-newer", RevisionID: "rev-fast", Source: domain.QualityEvidenceSourceInferCrane, Passed: true, EvaluatedAt: time.Now().Add(time.Hour)}}, run.store.evidence["qwen-opt-fast"]...)
	validated, err = run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || validated.QualityEvidenceID != "customer-1" {
		t.Fatalf("newer built-in evidence outranked customer evidence: %+v err=%v", validated, err)
	}
}

func TestCampaignCanWaitForCustomerEvidenceInstead(t *testing.T) {
	run := newQualityCampaign(t, optimizer.Request{QualityCheck: optimizer.QualityCheckCustomer})
	_, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	assertRetryableCode(t, err, retryQualityEvidencePending)
	if run.completer.total() != 0 || run.store.recorded != 0 {
		t.Fatalf("customer-evidence campaign ran the built-in check: calls=%d recorded=%d", run.completer.total(), run.store.recorded)
	}
	withoutChecker := run.driver
	withoutChecker.Quality = nil
	_, err = withoutChecker.Validate(t.Context(), run.candidate.ID, run.candidate)
	assertRetryableCode(t, err, retryQualityEvidencePending)
}

func TestBuiltinQualityCheckWaitsForTheCustomerEvalSet(t *testing.T) {
	body := []byte(`{"schema":"infercrane.dev/quality-eval-set/v1","name":"support","version":"7","prompts":[{"id":"refund","prompt":"How do I get a refund?"},{"id":"hours","prompt":"When are you open?"}]}`)
	digest := qualitycheck.Digest(body)
	run := newQualityCampaign(t, optimizer.Request{QualityEvalSet: digest})
	_, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	assertRetryableCode(t, err, retryQualityEvalSetPending)
	if !strings.Contains(err.Error(), digest) || run.completer.total() != 0 {
		t.Fatalf("missing eval set: err=%v calls=%d", err, run.completer.total())
	}
	run.store.evalSets = map[string]domain.QualityEvalSet{digest: {Digest: digest, Body: string(body)}}
	validated, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || !validated.Passed {
		t.Fatalf("validation = %+v err=%v", validated, err)
	}
	row := run.store.evidence["qwen-opt-fast"][0]
	if row.Suite != "support" || row.SuiteVersion != "7@"+digest || row.SampleCount != 2 || run.completer.total() != 4 {
		t.Fatalf("eval set evidence = %+v calls=%d", row, run.completer.total())
	}
	// A stored body that no longer matches its digest is never used.
	run.store.evalSets[digest] = domain.QualityEvalSet{Digest: digest, Body: strings.Replace(string(body), "7", "8", 1)}
	run.store.evidence = nil
	if _, err = run.driver.Validate(t.Context(), run.candidate.ID, run.candidate); err == nil || errors.Is(err, ErrQualityEvalSetMissing) {
		t.Fatalf("tampered eval set err=%v", err)
	}
}

func TestBuiltinQualityCheckIsInconclusiveWhenTheReferenceGivesNoAnswerText(t *testing.T) {
	run := newQualityCampaign(t, optimizer.Request{})
	// A reasoning model cut off at the token limit: the reference's answer is
	// all reasoning, and so is the candidate's. Two empty answers once scored
	// as a perfect match.
	cutOff := map[string]string{}
	for _, prompt := range qualitycheck.Default().Prompts {
		cutOff[prompt.ID] = "<think>Let me work this out step by step"
	}
	run.completer.answers = map[string]map[string]string{"http://control": cutOff, "http://fast": cutOff}
	validated, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || !validated.Inconclusive || validated.Passed || validated.FailureCode != FailureQualityReferenceUnanswered {
		t.Fatalf("validation = %+v err=%v", validated, err)
	}
	if run.store.recorded != 0 {
		t.Fatalf("recorded %d evidence rows for answers that were never given", run.store.recorded)
	}
}

func TestBuiltinQualityCheckIsInconclusiveWithoutAServingControl(t *testing.T) {
	run := newQualityCampaign(t, optimizer.Request{})
	run.composite.campaign.Candidates[0].State = CandidateFailed
	validated, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	if err != nil || !validated.Inconclusive || validated.Passed || validated.FailureCode != FailureQualityReferenceUnavailable || run.completer.total() != 0 {
		t.Fatalf("validation = %+v err=%v calls=%d", validated, err, run.completer.total())
	}
}

func TestBuiltinQualityCheckRetriesWhenAServingRevisionIsUnreachable(t *testing.T) {
	run := newQualityCampaign(t, optimizer.Request{})
	run.store.replicas["dep-fast"][0].Health = "unhealthy"
	_, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
	assertRetryableCode(t, err, retryQualityCheckFailed)
	if run.store.recorded != 0 {
		t.Fatalf("evidence recorded without an answer from the candidate: %d", run.store.recorded)
	}
}

func TestBuiltinQualityCheckUsesTheActiveRevisionForAnEvolveCampaign(t *testing.T) {
	run := newQualityCampaign(t, optimizer.Request{})
	run.composite.campaign.Intent, run.composite.campaign.TargetDeployment = IntentEvolveEndpoint, "production"
	run.store.addServing("production", "dep-production", "rev-active", "http://active")
	run.store.replicas["dep-production"] = append(run.store.replicas["dep-production"], domain.Replica{RevisionID: "rev-fast", Endpoint: "http://fast-in-place", Provider: "kubernetes", Health: "healthy", LifecycleState: "ready"})
	candidate := run.candidate
	candidate.DeploymentName = "production"
	run.composite.campaign.Candidates[1] = candidate
	validated, err := run.driver.Validate(t.Context(), candidate.ID, candidate)
	if err != nil || !validated.Passed {
		t.Fatalf("validation = %+v err=%v", validated, err)
	}
	if run.completer.calls["http://active"] == 0 || run.completer.calls["http://fast-in-place"] == 0 || run.completer.calls["http://control"] != 0 {
		t.Fatalf("calls = %v", run.completer.calls)
	}
	rows := run.store.evidence["production"]
	if len(rows) != 2 {
		t.Fatalf("production rows = %+v", rows)
	}
	for _, row := range rows {
		if row.ReferenceRevisionID != "rev-active" || row.SuiteVersion != rows[0].SuiteVersion {
			t.Fatalf("rows are not comparable for Release Guard: %+v", rows)
		}
	}
}

func TestCoordinatorEndsCandidateInconclusiveWhenAnswersCannotBeCompared(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	repository.campaign.State = CampaignRunning
	repository.campaign.Candidates[0].State, repository.campaign.Candidates[0].BenchmarkID = CandidateValidating, "benchmark-1"
	driver.validation = &ValidationResult{Inconclusive: true, FailureCode: FailureQualityReferenceUnavailable}
	result, err := coordinator.Step(t.Context(), "tenant", "campaign", "candidate-a")
	if err != nil || result.To != CandidateInconclusive || repository.campaign.Candidates[0].FailureCode != FailureQualityReferenceUnavailable {
		t.Fatalf("result = %+v err=%v candidate=%+v", result, err, repository.campaign.Candidates[0])
	}
}
