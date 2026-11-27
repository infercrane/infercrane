package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"math"
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

func (c *answerCompleter) Complete(_ context.Context, target qualitycheck.Target, _ qualitycheck.EvalSet, prompt qualitycheck.Prompt) (qualitycheck.Answer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[target.Endpoint]++
	if answer, ok := c.answers[target.Endpoint][prompt.ID]; ok {
		return qualitycheck.Answer{Content: answer}, nil
	}
	return qualitycheck.Answer{Content: "answer to " + prompt.ID}, nil
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

// forcedCompleter also reads forced sequences: the candidate's distribution
// moves on the answer's positions by shift.
type forcedCompleter struct {
	*answerCompleter
	shift  float64
	forced int
}

func (c *forcedCompleter) Forced(_ context.Context, target qualitycheck.Target, _ qualitycheck.EvalSet, _ qualitycheck.Prompt, _ string, _ int) (qualitycheck.Forced, error) {
	c.mu.Lock()
	c.forced++
	c.mu.Unlock()
	positions := make([]map[string]float64, 0, 6)
	for index := 1; index <= 6; index++ {
		p := 0.8
		if index >= 3 && target.Endpoint == "http://fast" {
			p -= c.shift
		}
		positions = append(positions, map[string]float64{"7": math.Log(p), "8": math.Log(0.1)})
	}
	return qualitycheck.Forced{PrefixTokens: 3, Positions: positions}, nil
}

func withArgs(t *testing.T, run *qualityCampaign, args ...string) {
	t.Helper()
	var draft optimizer.DeploymentDraft
	if err := json.Unmarshal([]byte(run.candidate.DeploymentSpecJSON), &draft); err != nil {
		t.Fatal(err)
	}
	draft.Runtime.Args = args
	encoded, _ := json.Marshal(draft)
	run.candidate.DeploymentSpecJSON = string(encoded)
	run.composite.campaign.Candidates[1] = run.candidate
}

// A candidate that changes numeric precision also records how far its
// next-token distribution moved on the reference's own answers; one that
// changes nothing else is not read that way.
func TestBuiltinQualityCheckRecordsTheDivergenceOfAPrecisionChange(t *testing.T) {
	for name, test := range map[string]struct {
		args []string
		want bool
	}{
		"fp8-kv-cache": {args: []string{"--kv-cache-dtype", "fp8"}, want: true},
		"copy":         {args: []string{"--seed", "1"}},
	} {
		t.Run(name, func(t *testing.T) {
			run := newQualityCampaign(t, optimizer.Request{})
			withArgs(t, &run, test.args...)
			completer := &forcedCompleter{answerCompleter: run.completer, shift: 0.3}
			check := run.driver.Quality.(BuiltinQualityCheck)
			check.Completer = completer
			run.driver.Quality = check
			validated, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
			if err != nil || !validated.Passed {
				t.Fatalf("validation = %+v err=%v", validated, err)
			}
			row := run.store.evidence["qwen-opt-fast"][0]
			var distribution qualityevidence.Distribution
			if err = json.Unmarshal([]byte(row.DistributionJSON), &distribution); err != nil {
				t.Fatal(err)
			}
			if !test.want {
				if distribution.Divergence != nil || completer.forced != 0 {
					t.Fatalf("a change of no precision was read: %+v forced=%d", distribution.Divergence, completer.forced)
				}
				return
			}
			divergence := distribution.Divergence
			if divergence == nil || divergence.Method != qualityevidence.DivergenceMethod || divergence.PrefillMeanKL != 0 || divergence.GenerationMeanKL <= 0 || divergence.Prompts == 0 {
				t.Fatalf("divergence = %+v", divergence)
			}
			// The reference's own row carries none.
			var reference qualityevidence.Distribution
			_ = json.Unmarshal([]byte(run.store.evidence["qwen-opt-control"][0].DistributionJSON), &reference)
			if reference.Divergence != nil {
				t.Fatalf("reference row divergence = %+v", reference.Divergence)
			}
		})
	}
}

// withModels gives the control and the candidate their own model and
// revision, as a published checkpoint candidate has.
func withModels(t *testing.T, run *qualityCampaign, control, candidate [2]string) {
	t.Helper()
	set := func(row *domain.OptimizationCandidateRun, model [2]string) {
		var draft optimizer.DeploymentDraft
		if err := json.Unmarshal([]byte(row.DeploymentSpecJSON), &draft); err != nil {
			t.Fatal(err)
		}
		draft.Model.ID, draft.Model.Revision = model[0], model[1]
		encoded, _ := json.Marshal(draft)
		row.DeploymentSpecJSON = string(encoded)
	}
	set(&run.control, control)
	set(&run.candidate, candidate)
	run.composite.campaign.Candidates[0], run.composite.campaign.Candidates[1] = run.control, run.candidate
}

// A candidate serving another repository is read for divergence only when
// the proposal recorded both models' tokenizer digests and they are equal;
// otherwise the divergence is unknown, no server is asked for it, and the
// answers still decide.
func TestBuiltinQualityCheckReadsDivergenceAcrossModelsOnlyWithTheSameTokenizer(t *testing.T) {
	base := [2]string{"Qwen/Qwen3-8B", strings.Repeat("a", 40)}
	checkpoint := [2]string{"Qwen/Qwen3-8B-FP8", strings.Repeat("b", 40)}
	digest := func(fill string) string { return "sha256:" + strings.Repeat(fill, 64) }
	key := func(model [2]string) string { return optimizer.TokenizerKey(model[0], model[1]) }
	for name, test := range map[string]struct {
		tokenizers map[string]string
		want       bool
	}{
		"the same tokenizer files":   {map[string]string{key(base): digest("c"), key(checkpoint): digest("c")}, true},
		"another tokenizer":          {map[string]string{key(base): digest("c"), key(checkpoint): digest("d")}, false},
		"the checkpoint's not known": {map[string]string{key(base): digest("c")}, false},
		"nothing recorded":           {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			run := newQualityCampaign(t, optimizer.Request{ModelTokenizers: test.tokenizers})
			withModels(t, &run, base, checkpoint)
			completer := &forcedCompleter{answerCompleter: run.completer, shift: 0.3}
			check := run.driver.Quality.(BuiltinQualityCheck)
			check.Completer = completer
			run.driver.Quality = check
			validated, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
			if err != nil || !validated.Passed {
				t.Fatalf("validation = %+v err=%v", validated, err)
			}
			var distribution qualityevidence.Distribution
			if err = json.Unmarshal([]byte(run.store.evidence["qwen-opt-fast"][0].DistributionJSON), &distribution); err != nil {
				t.Fatal(err)
			}
			if test.want != (distribution.Divergence != nil) || test.want != (completer.forced > 0) {
				t.Fatalf("divergence = %+v forced = %d, want read = %v", distribution.Divergence, completer.forced, test.want)
			}
		})
	}
}

// A setup that changes answers must show them held within a point on the
// task set; the same answers from a setup that changes nothing pass by the
// ±3-point rule. Not shown to hold ends the setup inconclusive, never
// qualified, and its evidence says why.
func TestASetupThatChangesAnswersIsHeldBackWhenItCannotShowThemHeld(t *testing.T) {
	prompts := make([]map[string]any, 0, 1000)
	control, candidate := map[string]string{}, map[string]string{}
	for index := range 1000 {
		id, gold := fmt.Sprintf("gsm8k.test.%d", index), fmt.Sprint(index+1)
		prompts = append(prompts, map[string]any{"id": id, "prompt": "item with gold " + gold, "expected": []string{gold}})
		control[id], candidate[id] = "Some working.\nAnswer: "+gold, "Some working.\nAnswer: "+gold
		if index < 30 {
			control[id] = "Some working.\nAnswer: 999"
		}
		// One point lost: ten items gained, twenty lost.
		if (index >= 10 && index < 30) || (index >= 100 && index < 120) {
			candidate[id] = "Some working.\nAnswer: 999"
		}
	}
	body, err := json.Marshal(map[string]any{"schema": qualitycheck.EvalSetSchemaV1, "name": "task-set", "version": "1", "max_tokens": 1024, "grading": qualitycheck.GradingAnswerLine, "prompts": prompts})
	if err != nil {
		t.Fatal(err)
	}
	digest := qualitycheck.Digest(body)
	for name, test := range map[string]struct {
		args         []string
		passed       bool
		inconclusive bool
		code         string
	}{
		"FP8 KV cache": {args: []string{"--kv-cache-dtype", "fp8"}, inconclusive: true, code: FailureQualityAnswersNotShownHeld},
		"a copy":       {args: []string{"--seed", "1"}, passed: true},
	} {
		t.Run(name, func(t *testing.T) {
			run := newQualityCampaign(t, optimizer.Request{QualityEvalSet: digest})
			run.store.evalSets = map[string]domain.QualityEvalSet{digest: {Digest: digest, Body: string(body)}}
			run.completer.answers = map[string]map[string]string{"http://control": control, "http://fast": candidate}
			withArgs(t, &run, test.args...)
			validated, err := run.driver.Validate(t.Context(), run.candidate.ID, run.candidate)
			if err != nil || validated.Passed != test.passed || validated.Inconclusive != test.inconclusive || validated.FailureCode != test.code {
				t.Fatalf("validation = %+v err=%v", validated, err)
			}
			var distribution qualityevidence.Distribution
			if err = json.Unmarshal([]byte(run.store.evidence["qwen-opt-fast"][0].DistributionJSON), &distribution); err != nil {
				t.Fatal(err)
			}
			wantRule := qualitycheck.RuleWithinMargin
			if test.inconclusive {
				wantRule = qualitycheck.RuleHeld
			}
			if distribution.TaskRule != wantRule || distribution.TaskServers != 1 || distribution.ItemsAtOnce != 32 {
				t.Fatalf("distribution = %+v", distribution)
			}
		})
	}
}
