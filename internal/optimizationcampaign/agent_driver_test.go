package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/qualitycheck"
	"github.com/infercrane/infercrane/internal/qualityevidence"
	"github.com/infercrane/infercrane/internal/support"
)

// agentStore is an in-memory AgentDriverStore holding one campaign.
type agentStore struct {
	mu         sync.Mutex
	campaign   domain.OptimizationCampaign
	benchmarks map[string]domain.BenchmarkResult
	evidence   map[string]domain.QualityEvidence
	guards     []domain.AgentReleaseGuardEvaluation
}

func (s *agentStore) RecordAgentReleaseGuardEvaluation(_ context.Context, value domain.AgentReleaseGuardEvaluation) (domain.AgentReleaseGuardEvaluation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.guards {
		if existing.CandidateID == value.CandidateID && existing.BenchmarkID == value.BenchmarkID {
			if existing.Decision != value.Decision {
				return existing, domain.ErrConflict
			}
			return existing, nil
		}
	}
	value.ID = "guard-" + value.CandidateID + "-" + value.BenchmarkID
	s.guards = append(s.guards, value)
	return value, nil
}

func (s *agentStore) OptimizationCampaign(context.Context, string, string) (domain.OptimizationCampaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	campaign := s.campaign
	campaign.Candidates = append([]domain.OptimizationCandidateRun(nil), s.campaign.Candidates...)
	return campaign, nil
}
func (s *agentStore) TransitionOptimizationCandidate(context.Context, string, string, string, string, string, domain.OptimizationCandidateRun) (domain.OptimizationCandidateRun, error) {
	return domain.OptimizationCandidateRun{}, errors.New("the driver never transitions")
}
func (s *agentStore) RecordOptimizationMeasurementCheckpoint(_ context.Context, _, _, candidate, checkpoint string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.campaign.Candidates {
		if s.campaign.Candidates[index].ID == candidate {
			s.campaign.Candidates[index].MeasurementCheckpointJSON = checkpoint
			return true, nil
		}
	}
	return false, nil
}
func (s *agentStore) RecordBenchmark(_ context.Context, row domain.BenchmarkResult) (domain.BenchmarkResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row.AgentID == "" || row.DeploymentID != "" || !strings.HasPrefix(row.RevisionID, "agent") {
		return row, errors.New("an agent's row must be scoped to the agent and its setup")
	}
	if _, exists := s.benchmarks[row.ID]; exists {
		return row, errors.New("duplicate benchmark id")
	}
	s.benchmarks[row.ID] = row
	return row, nil
}
func (s *agentStore) BenchmarksByIDs(_ context.Context, _ string, ids []string) (map[string]domain.BenchmarkResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]domain.BenchmarkResult{}
	for _, id := range ids {
		if row, ok := s.benchmarks[id]; ok {
			out[id] = row
		}
	}
	return out, nil
}
func (s *agentStore) RecordAgentQualityEvidence(_ context.Context, _, agentID string, row domain.QualityEvidence) (domain.QualityEvidence, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.evidence {
		if existing.PayloadDigest == row.PayloadDigest {
			return existing, false, nil
		}
	}
	row.ID, row.AgentID = "quality-"+row.RevisionID, agentID
	s.evidence[row.ID] = row
	return row, true, nil
}
func (s *agentStore) QualityEvidenceByIDs(_ context.Context, _ string, ids []string) (map[string]domain.QualityEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]domain.QualityEvidence{}
	for _, id := range ids {
		if row, ok := s.evidence[id]; ok {
			out[id] = row
		}
	}
	return out, nil
}

func (s *agentStore) candidate(id string) domain.OptimizationCandidateRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, candidate := range s.campaign.Candidates {
		if candidate.ID == id {
			return candidate
		}
	}
	return domain.OptimizationCandidateRun{}
}

// standInAgent answers windows at once from a speed model: the live server's
// output throughput, and the candidate's in each stage's window. It records
// every spec it was sent.
type standInAgent struct {
	mu        sync.Mutex
	control   float64
	candidate map[bool]float64 // fresh -> throughput
	nodes     map[bool]string  // fresh -> the setup's node digest
	answers   *agentprotocol.WindowAnswers
	// freshAnswers and freshFailure answer the fresh windows' check when
	// set; otherwise fresh windows answer as the first does. firstFailure is
	// the first window's answer-check failure.
	freshAnswers               *agentprotocol.WindowAnswers
	freshFailure, firstFailure string
	failWith                   []string // failure codes for the first windows, in order
	pending                    bool
	specs                      []agentprotocol.WindowSpec
	keys                       map[string]AgentWindow
	cancelled                  []string
}

func (a *standInAgent) QueueWindow(_ context.Context, _, _, key string, spec agentprotocol.WindowSpec) (AgentWindow, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if window, ok := a.keys[key]; ok {
		return window, nil
	}
	if err := spec.Validate(); err != nil {
		return AgentWindow{}, err
	}
	a.specs = append(a.specs, spec)
	window := AgentWindow{ID: "window-" + itoaTest(len(a.specs)), State: agentprotocol.CommandSucceeded}
	switch {
	case a.pending:
		window.State = agentprotocol.CommandRunning
	case len(a.failWith) > 0:
		window.State, window.FailureCode = agentprotocol.CommandFailed, a.failWith[0]
		a.failWith = a.failWith[1:]
	default:
		window.Result = a.result(spec)
	}
	if a.keys == nil {
		a.keys = map[string]AgentWindow{}
	}
	a.keys[key] = window
	return window, nil
}

func (a *standInAgent) CancelWindow(_ context.Context, _, _, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancelled = append(a.cancelled, id)
	return nil
}

func (a *standInAgent) result(spec agentprotocol.WindowSpec) *agentprotocol.WindowResult {
	result := &agentprotocol.WindowResult{Tool: "aiperf", ToolVersion: "0.12.0", CleanedUp: true}
	// A little drift both arms share, which the bracketing cancels.
	drift := []float64{1, 1.004, 0.997, 1.002, 0.999, 1.003, 0.998, 1.001, 1}
	run := func(index int, arm string, throughput float64) agentprotocol.WindowRun {
		value, ttft, latency := throughput*drift[index%len(drift)], 120.0, 4000.0
		node := "node-cccccccccccccccc"
		if arm == agentprotocol.ArmCandidate {
			node = a.nodes[spec.Fresh]
		}
		return agentprotocol.WindowRun{Arm: arm, Seed: spec.SeedBase + int64(index+2), Requests: spec.Load.Requests, Succeeded: spec.Load.Requests, DurationSeconds: 60,
			OutputTokenThroughput: &value, TTFTP95MS: &ttft, LatencyP95MS: &latency, Node: node, GPU: "unknown"}
	}
	if spec.Candidate == nil {
		for index := range spec.Rounds {
			result.Runs = append(result.Runs, run(index, agentprotocol.ArmControl, a.control))
		}
	} else {
		result.Runs = append(result.Runs, run(0, agentprotocol.ArmControl, a.control))
		for round := range spec.Rounds {
			result.Runs = append(result.Runs, run(2*round+1, agentprotocol.ArmCandidate, a.candidate[spec.Fresh]), run(2*round+2, agentprotocol.ArmControl, a.control))
		}
	}
	switch {
	case spec.Quality.Skip:
	case spec.Fresh && (a.freshAnswers != nil || a.freshFailure != ""):
		result.Quality, result.QualityFailure = a.freshAnswers, a.freshFailure
	case spec.Fresh:
		result.Quality = a.answers
	default:
		result.Quality, result.QualityFailure = a.answers, a.firstFailure
	}
	return result
}

func itoaTest(value int) string {
	return string(rune('0' + value%10))
}

type agentRun struct {
	store          *agentStore
	agent          *standInAgent
	driver         AgentDriver
	control, setup domain.OptimizationCandidateRun
	budget         Budget
}

// newAgentRun is a run on the agent's endpoint "support-bot": the customer's
// live server as the control and one setup, both on one L4.
func newAgentRun(t *testing.T, objective string, setupArgs ...string) agentRun {
	t.Helper()
	now := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	draft := func(args ...string) string {
		value := optimizer.DeploymentDraft{APIVersion: "infercrane.dev/v1", Kind: "Deployment", Name: "support-bot"}
		value.Model.ID, value.Model.Revision = "Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218"
		value.Runtime.Engine, value.Runtime.Version, value.Runtime.Args = "vllm", support.DefaultRuntimeVersion, args
		value.Resources.GPU, value.Resources.GPUCount = "NVIDIA-L4", 1
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	proposal, _ := json.Marshal(optimizer.Proposal{Input: optimizer.Request{Objective: objective}, StartingPoint: &optimizer.StartingPoint{CandidateID: "proposal-control"},
		Candidates: []optimizer.Candidate{{ID: "proposal-control", BenchmarkProfile: "throughput"}, {ID: "proposal-setup", BenchmarkProfile: "throughput"}}})
	expiry := now.Add(8 * time.Hour)
	control := domain.OptimizationCandidateRun{ID: "control-1", TenantID: "tenant", CampaignID: "campaign-1", ProposalCandidateID: "proposal-control", State: CandidateMeasuring, DeploymentSpecJSON: draft()}
	setup := domain.OptimizationCandidateRun{ID: "setup-1", TenantID: "tenant", CampaignID: "campaign-1", ProposalCandidateID: "proposal-setup", State: CandidateProvisioning, DeploymentSpecJSON: draft(setupArgs...)}
	campaign := domain.OptimizationCampaign{ID: "campaign-1", TenantID: "tenant", Intent: IntentEvolveEndpoint, TargetEndpoint: "support-bot", AgentID: "agent-1", ProposalJSON: string(proposal), ApprovalExpiresAt: &expiry,
		Candidates: []domain.OptimizationCandidateRun{control, setup}}
	store := &agentStore{campaign: campaign, benchmarks: map[string]domain.BenchmarkResult{}, evidence: map[string]domain.QualityEvidence{}}
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	agent := &standInAgent{control: 1000, candidate: map[bool]float64{false: 1000, true: 1000}}
	driver := AgentDriver{Store: store, Windows: agent, Signer: key, Now: func() time.Time { return now }, Entropy: strings.NewReader(strings.Repeat("\x01", 64))}
	run := agentRun{store: store, agent: agent, driver: driver, budget: Budget{MaxCostUSD: 10, ExpiresAt: expiry}}
	// Provision fixes each setup's identity; nothing starts.
	for _, candidate := range []*domain.OptimizationCandidateRun{&control, &setup} {
		provisioned, provisionErr := driver.Provision(t.Context(), candidate.ID, *candidate, run.budget)
		if provisionErr != nil {
			t.Fatal(provisionErr)
		}
		candidate.DeploymentName, candidate.RevisionID = provisioned.DeploymentName, provisioned.RevisionID
	}
	store.campaign.Candidates = []domain.OptimizationCandidateRun{control, setup}
	run.control, run.setup = control, setup
	return run
}

// declare records a declared live setup in the run's proposal, as the
// proposal API does for an agent's endpoint.
func (r agentRun) declare(t *testing.T) {
	t.Helper()
	var proposal optimizer.Proposal
	if err := json.Unmarshal([]byte(r.store.campaign.ProposalJSON), &proposal); err != nil {
		t.Fatal(err)
	}
	proposal.Input.StartingSetup = &optimizer.StartingSetup{Endpoint: "support-bot", Engine: "vllm", Version: support.DefaultRuntimeVersion, GPU: "NVIDIA-L4", GPUCount: 1}
	encoded, _ := json.Marshal(proposal)
	r.store.campaign.ProposalJSON = string(encoded)
}

// measure runs the Measure step the way the coordinator does, with the
// candidate as stored.
func (r agentRun) measure(t *testing.T, candidateID string) (MeasurementResult, error) {
	t.Helper()
	return r.driver.Measure(t.Context(), candidateID, r.store.candidate(candidateID), r.budget)
}

func measuredOf(t *testing.T, run agentRun, result MeasurementResult) MeasuredEvidence {
	t.Helper()
	row := run.store.benchmarks[result.BenchmarkID]
	var workload struct {
		Measured MeasuredEvidence `json:"measured"`
		By       string           `json:"measured_by"`
	}
	if err := json.Unmarshal([]byte(row.WorkloadJSON), &workload); err != nil || workload.By != "agent" {
		t.Fatalf("workload: %v %s", err, row.WorkloadJSON)
	}
	return workload.Measured
}

// A setup an agent measured 10% faster in its first window is started again
// on a new server; a gain that repeats there is a confirmed improvement, by
// the same judge the control plane applies to its own windows.
func TestAgentRunConfirmsAGainOnAFreshServerWithTheSameJudge(t *testing.T) {
	run := newAgentRun(t, "throughput", "--max-num-seqs", "256")
	run.agent.candidate = map[bool]float64{false: 1100, true: 1095}
	run.agent.nodes = map[bool]string{false: "node-aaaaaaaaaaaaaaaa", true: "node-bbbbbbbbbbbbbbbb"}
	result, err := run.measure(t, run.setup.ID)
	if err != nil {
		t.Fatal(err)
	}
	specs := run.agent.specs
	// Both windows check the answers: the verdict reads the fresh server's.
	if len(specs) != 2 || specs[0].Fresh || !specs[1].Fresh || specs[0].Candidate == nil || specs[1].Candidate == nil || specs[0].Quality.Skip || specs[1].Quality.Skip {
		t.Fatalf("windows: %+v", specs)
	}
	if specs[0].Quality.ReferenceSubject != AgentServerID("agent-1", "support-bot") || specs[0].Quality.CandidateSubject != run.setup.RevisionID || specs[1].Quality != specs[0].Quality || specs[1].SeedBase <= specs[0].SeedBase+int64(2*measurementRunsPerArm+2) {
		t.Fatalf("subjects or seeds: %+v", specs)
	}
	if specs[0].Candidate.Workload.Image == "" || specs[0].Candidate.RuntimeArgs[1] != "256" || AgentSetupID(*specs[0].Candidate) != run.setup.RevisionID {
		t.Fatalf("setup: %+v", specs[0].Candidate)
	}
	measured := measuredOf(t, run, result)
	if measured.Control.Role != ControlRoleBracketed || measured.Confirmation == nil || !measured.Confirmation.FreshCandidate || measured.Confirmation.FreshControl {
		t.Fatalf("evidence: control=%+v confirmation=%+v", measured.Control, measured.Confirmation)
	}
	// The window's comparison is the campaign judge's, run for run.
	judged, reason := JudgeRecordedWindow(measured.Runs, 1)
	if reason != "" || judged["output_token_throughput"].Delta != measured.Comparison["output_token_throughput"].Delta {
		t.Fatalf("judge: %q %+v vs %+v", reason, judged["output_token_throughput"], measured.Comparison["output_token_throughput"])
	}
	decision, _, why := confirmedDecision(&measured, measured.Comparison["output_token_throughput"], "output_token_throughput", "output", false, 1)
	if decision != DecisionImproved {
		t.Fatalf("decision %s: %s", decision, why)
	}
	row := run.store.benchmarks[result.BenchmarkID]
	if row.RevisionID != run.setup.RevisionID || row.AgentID != "agent-1" || row.ComputeMode != ComputeModeAgentCluster || row.ModelIdentity != "Qwen/Qwen3-8B@b968826d9c46dd6066d109eabc6255188de91218" || !strings.Contains(row.ReproductionCommand, "window-1, window-2") {
		t.Fatalf("row: %+v", row)
	}
	// A retry after the row was stored adopts it and queues nothing.
	again, err := run.measure(t, run.setup.ID)
	if err != nil || again.BenchmarkID != result.BenchmarkID || len(run.agent.specs) != 2 {
		t.Fatalf("retry: %+v %v (%d windows)", again, err, len(run.agent.specs))
	}
}

// A first-window gain that does not repeat on the new server is not
// confirmed; without a gain only one window runs.
func TestAgentRunGainThatDoesNotRepeatIsNotConfirmed(t *testing.T) {
	run := newAgentRun(t, "throughput", "--max-num-seqs", "256")
	run.agent.candidate = map[bool]float64{false: 1100, true: 1000}
	result, err := run.measure(t, run.setup.ID)
	if err != nil {
		t.Fatal(err)
	}
	measured := measuredOf(t, run, result)
	if decision, _, why := confirmedDecision(&measured, measured.Comparison["output_token_throughput"], "output_token_throughput", "output", false, 1); decision != DecisionNotConfirmed {
		t.Fatalf("decision %s: %s", decision, why)
	}

	flat := newAgentRun(t, "throughput", "--max-num-seqs", "256")
	result, err = flat.measure(t, flat.setup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if measured = measuredOf(t, flat, result); measured.Confirmation != nil || len(flat.agent.specs) != 1 || measured.Comparison["output_token_throughput"].Decision != DecisionNoMeasurableDifference {
		t.Fatalf("a run without a gain: confirmation=%+v windows=%d comparison=%+v", measured.Confirmation, len(flat.agent.specs), measured.Comparison["output_token_throughput"])
	}
}

// The control's own window measures the live server alone and starts
// nothing; its row is the live server's.
func TestAgentRunMeasuresTheLiveServerAloneAsTheControl(t *testing.T) {
	run := newAgentRun(t, "throughput")
	result, err := run.measure(t, run.control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.agent.specs) != 1 || run.agent.specs[0].Candidate != nil || run.agent.specs[0].Quality.ReferenceSubject != run.agent.specs[0].Quality.CandidateSubject {
		t.Fatalf("control window: %+v", run.agent.specs)
	}
	row := run.store.benchmarks[result.BenchmarkID]
	if row.RevisionID != AgentServerID("agent-1", "support-bot") || row.RequestCount != measurementRunsPerArm*run.agent.specs[0].Load.Requests {
		t.Fatalf("control row: %+v", row)
	}
	if measured := measuredOf(t, run, result); measured.Control.Role != ControlRoleCandidate {
		t.Fatalf("control evidence: %+v", measured.Control)
	}
}

// A window still running leaves the step pending, and cleanup cancels it;
// a failure more time can fix is measured again, a refused image stops the
// setup at once.
func TestAgentRunWaitsRetriesAndStops(t *testing.T) {
	run := newAgentRun(t, "throughput", "--max-num-seqs", "256")
	run.agent.pending = true
	_, err := run.measure(t, run.setup.ID)
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Code != retryAgentWindowPending || !failure.Retryable {
		t.Fatalf("pending: %v", err)
	}
	if err = run.driver.Cleanup(t.Context(), run.setup.ID, run.store.candidate(run.setup.ID)); err != nil || len(run.agent.cancelled) != 1 || run.agent.cancelled[0] != "window-1" {
		t.Fatalf("cleanup: %v %v", err, run.agent.cancelled)
	}

	retried := newAgentRun(t, "throughput", "--max-num-seqs", "256")
	retried.agent.failWith = []string{"candidate_measurement_failed"}
	if _, err = retried.measure(t, retried.setup.ID); err != nil {
		t.Fatalf("a failed window was not measured again: %v", err)
	}
	if len(retried.agent.specs) != 2 {
		t.Fatalf("windows: %d", len(retried.agent.specs))
	}

	refused := newAgentRun(t, "throughput", "--max-num-seqs", "256")
	refused.agent.failWith = []string{"image_not_allowed"}
	if _, err = refused.measure(t, refused.setup.ID); !errors.As(err, &failure) || failure.Retryable || failure.Code != FailureAgentWindowFailed || len(refused.agent.specs) != 1 {
		t.Fatalf("refused image: %v (%d windows)", err, len(refused.agent.specs))
	}
}

// The answers are judged by the control plane's rules: a setup that changes
// precision must show its answers held within a point.
func TestAgentRunJudgesAnswersByTheOnePointRule(t *testing.T) {
	items := 1000
	answers := func(candidateWrong int) *agentprotocol.WindowAnswers {
		reference, candidate := make([]float64, items), make([]float64, items)
		for index := range items {
			reference[index], candidate[index] = 1, 1
			if index < items/10 {
				reference[index], candidate[index] = 0, 0
			}
			if index >= items/10 && index < items/10+candidateWrong {
				candidate[index] = 0
			}
		}
		return &agentprotocol.WindowAnswers{EvalSet: "sha256:" + strings.Repeat("a", 64), EvalSetName: "support", EvalSetVersion: "3", EvalSetDigest: "sha256:" + strings.Repeat("a", 64),
			PairingDigest: "sha256:" + strings.Repeat("b", 64), Grading: qualitycheck.GradingAnswerLine, Items: items, ItemsAtOnce: 32, ReferenceScores: reference, CandidateScores: candidate,
			ResultDigest: "sha256:" + strings.Repeat("c", 64), EvaluatedAt: time.Now().UTC().Add(-time.Minute)}
	}
	for _, test := range []struct {
		name         string
		args         []string
		items, wrong int
		declared     bool
		passed       bool
		inconclusive bool
		code         string
	}{
		{"held", []string{"--kv-cache-dtype", "fp8"}, 1000, 0, true, true, false, ""},
		{"three points worse", []string{"--kv-cache-dtype", "fp8"}, 1000, 30, true, false, false, FailureQualityRejected},
		// One item in 40 lower: the interval crosses -1 point, so the
		// answers are not shown to hold and the setup is unknown.
		{"not shown held", []string{"--kv-cache-dtype", "fp8"}, 40, 1, true, false, true, FailureQualityAnswersNotShownHeld},
		// Without a declared live setup, a setup that only changes the
		// batch size must still show its answers held.
		{"undeclared live setup", []string{"--max-num-seqs", "256"}, 1000, 0, false, true, false, ""},
	} {
		items = test.items
		run := newAgentRun(t, "throughput", test.args...)
		if test.declared {
			run.declare(t)
		}
		run.agent.answers = answers(test.wrong)
		if _, err := run.measure(t, run.setup.ID); err != nil {
			t.Fatal(err)
		}
		validated, err := run.driver.Validate(t.Context(), run.setup.ID, run.store.candidate(run.setup.ID))
		if err != nil || validated.Passed != test.passed || validated.Inconclusive != test.inconclusive || validated.FailureCode != test.code {
			t.Fatalf("%s: %+v err=%v", test.name, validated, err)
		}
		row := run.store.evidence[validated.QualityEvidenceID]
		var distribution struct {
			TaskRule string `json:"task_rule"`
		}
		_ = json.Unmarshal([]byte(row.DistributionJSON), &distribution)
		if row.AgentID != "agent-1" || row.RevisionID != run.setup.RevisionID || row.ReferenceRevisionID != AgentServerID("agent-1", "support-bot") || distribution.TaskRule != qualitycheck.RuleHeld {
			t.Fatalf("%s: evidence %+v", test.name, row)
		}
		if _, ok := run.store.evidence["quality-"+AgentServerID("agent-1", "support-bot")]; !ok {
			t.Fatalf("%s: the live server's own row was not recorded", test.name)
		}
	}
	unanswered := newAgentRun(t, "throughput", "--max-num-seqs", "256")
	if _, err := unanswered.measure(t, unanswered.setup.ID); err != nil {
		t.Fatal(err)
	}
	candidate := unanswered.store.candidate(unanswered.setup.ID)
	state, _ := DecodeMeasurementCheckpoint(candidate.MeasurementCheckpointJSON)
	state.Agent.QualityFailure = "quality_reference_unanswered"
	encoded, _ := json.Marshal(state)
	candidate.MeasurementCheckpointJSON = string(encoded)
	if validated, err := unanswered.driver.Validate(t.Context(), candidate.ID, candidate); err != nil || !validated.Inconclusive || validated.FailureCode != FailureQualityReferenceUnanswered {
		t.Fatalf("unanswered: %+v %v", validated, err)
	}
}

// taskPair is a 1,000-item task set answered right by both sides except on
// the items each side's span covers.
func taskPair(evalSet string, referenceWrong, candidateWrong [2]int) *agentprotocol.WindowAnswers {
	reference, candidate := make([]float64, 1000), make([]float64, 1000)
	for index := range 1000 {
		reference[index], candidate[index] = 1, 1
		if index >= referenceWrong[0] && index < referenceWrong[1] {
			reference[index] = 0
		}
		if index >= candidateWrong[0] && index < candidateWrong[1] {
			candidate[index] = 0
		}
	}
	return &agentprotocol.WindowAnswers{EvalSet: evalSet, EvalSetName: "support", EvalSetVersion: "3", EvalSetDigest: evalSet,
		PairingDigest: "sha256:" + strings.Repeat("b", 64), Grading: qualitycheck.GradingAnswerLine, Items: 1000, ItemsAtOnce: 32, ReferenceScores: reference, CandidateScores: candidate,
		ResultDigest: "sha256:" + strings.Repeat("c", 64), EvaluatedAt: time.Now().UTC().Add(-time.Minute)}
}

// A setup that changes answers and gained in its first window answered the
// task set there and again on its fresh server. Each pair alone is too wide
// to show its answers held within a point; the verdict pools both pairs,
// exactly as a check the control plane runs after a confirmation does.
func TestAgentRunPoolsTheFirstServersAnswersWithTheFreshOnes(t *testing.T) {
	setA, setB := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("d", 64)
	confirmed := func(t *testing.T, args []string, first, fresh *agentprotocol.WindowAnswers, firstFailure, freshFailure string) (agentRun, MeasurementCheckpoint, ValidationResult, qualityevidence.Distribution) {
		t.Helper()
		run := newAgentRun(t, "throughput", args...)
		run.declare(t)
		run.agent.candidate = map[bool]float64{false: 1100, true: 1095}
		run.agent.nodes = map[bool]string{false: "node-aaaaaaaaaaaaaaaa", true: "node-bbbbbbbbbbbbbbbb"}
		run.agent.answers, run.agent.freshAnswers, run.agent.firstFailure, run.agent.freshFailure = first, fresh, firstFailure, freshFailure
		if _, err := run.measure(t, run.setup.ID); err != nil {
			t.Fatal(err)
		}
		if len(run.agent.specs) != 2 || !run.agent.specs[1].Fresh || run.agent.specs[1].Quality.Skip {
			t.Fatalf("windows: %+v", run.agent.specs)
		}
		state, _ := DecodeMeasurementCheckpoint(run.store.candidate(run.setup.ID).MeasurementCheckpointJSON)
		validated, err := run.driver.Validate(t.Context(), run.setup.ID, run.store.candidate(run.setup.ID))
		if err != nil {
			t.Fatal(err)
		}
		var distribution qualityevidence.Distribution
		if row, ok := run.store.evidence[validated.QualityEvidenceID]; ok {
			if err = json.Unmarshal([]byte(row.DistributionJSON), &distribution); err != nil {
				t.Fatal(err)
			}
		}
		return run, state, validated, distribution
	}
	fp8 := []string{"--kv-cache-dtype", "fp8"}
	firstPair, freshPair := taskPair(setA, [2]int{0, 25}, [2]int{100, 125}), taskPair(setA, [2]int{200, 225}, [2]int{300, 325})

	// The fresh pair alone is not shown to hold answers.
	_, _, alone, _ := confirmed(t, fp8, freshPair, nil, "", "")
	if !alone.Inconclusive || alone.FailureCode != FailureQualityAnswersNotShownHeld {
		t.Fatalf("one pair = %+v", alone)
	}

	// Pooled with the first window's pair, it is.
	run, state, pooled, distribution := confirmed(t, fp8, firstPair, freshPair, "", "")
	kept := state.Confirmation.FirstAnswers
	if kept == nil || kept.EvalSet != setA || kept.ReferenceID != AgentServerID("agent-1", "support-bot") || kept.CandidateID != run.setup.RevisionID || len(kept.Candidate) != 1000 || kept.Candidate[100] != 0 {
		t.Fatalf("kept first pair = %+v", kept)
	}
	if state.Agent.Answers == nil || state.Agent.Answers.CandidateScores[300] != 0 {
		t.Fatal("the verdict must read the fresh window's answers")
	}
	if !pooled.Passed || distribution.TaskRule != qualitycheck.RuleHeld || distribution.TaskVerdict != qualitycheck.CodeTaskHeld || distribution.TaskServers != 2 || distribution.TaskPoolRefused != "" {
		t.Fatalf("pooled = %+v distribution = %+v", pooled, distribution)
	}
	want, err := qualitycheck.PooledTask([]qualitycheck.ServerPair{*kept, {EvalSet: setA, ReferenceID: kept.ReferenceID, CandidateID: kept.CandidateID, Reference: freshPair.ReferenceScores, Candidate: freshPair.CandidateScores}})
	if err != nil {
		t.Fatal(err)
	}
	if row := run.store.evidence[pooled.QualityEvidenceID]; row.Score != want.CandidateScore || row.ArtifactDigest == freshPair.ResultDigest {
		t.Fatalf("evidence %+v, want candidate score %v and a digest that commits to the pooled pair", row, want.CandidateScore)
	}

	// First answers the agent could not compare: the fresh pair alone.
	_, state, skipped, distribution := confirmed(t, fp8, nil, freshPair, "answer_check_failed", "")
	if state.Confirmation.FirstAnswers != nil || state.Confirmation.FirstAnswersSkipped != "the first server's answers could not be compared: answer_check_failed" {
		t.Fatalf("confirmation = %+v", state.Confirmation)
	}
	if !skipped.Inconclusive || skipped.FailureCode != FailureQualityAnswersNotShownHeld || distribution.TaskServers != 1 {
		t.Fatalf("skipped = %+v distribution = %+v", skipped, distribution)
	}

	// Pairs on two task sets do not pool; the evidence says why.
	_, _, refused, distribution := confirmed(t, fp8, firstPair, taskPair(setB, [2]int{200, 225}, [2]int{300, 325}), "", "")
	if !refused.Inconclusive || distribution.TaskServers != 1 || !strings.Contains(distribution.TaskPoolRefused, "another task set") {
		t.Fatalf("refused = %+v distribution = %+v", refused, distribution)
	}

	// Fresh answers the agent could not compare leave the setup unknown,
	// however the first window's answers came out.
	_, _, lost, _ := confirmed(t, fp8, firstPair, nil, "", "answer_check_failed")
	if !lost.Inconclusive || lost.FailureCode != FailureAgentAnswerCheck {
		t.Fatalf("fresh answers lost = %+v", lost)
	}

	// A setup that keeps answers under a declared live setup is judged on
	// its fresh pair by the 3-point margin; nothing is kept to pool.
	_, state, _, distribution = confirmed(t, []string{"--max-num-seqs", "256"}, firstPair, freshPair, "", "")
	if state.Confirmation.FirstAnswers != nil || state.Confirmation.FirstAnswersSkipped != "" || distribution.TaskRule == qualitycheck.RuleHeld || distribution.TaskServers != 1 {
		t.Fatalf("confirmation = %+v distribution = %+v", state.Confirmation, distribution)
	}
}

// Release Guard passes a setup that serves like the live server and rejects
// one whose first token got much slower.
func TestAgentRunGuardAppliesReleaseGuard(t *testing.T) {
	run := newAgentRun(t, "throughput", "--max-num-seqs", "256")
	result, err := run.measure(t, run.setup.ID)
	if err != nil {
		t.Fatal(err)
	}
	candidate := run.store.candidate(run.setup.ID)
	candidate.BenchmarkID = result.BenchmarkID
	guarded, err := run.driver.Guard(t.Context(), candidate.ID, candidate)
	if err != nil || guarded.Decision != "PASS" {
		t.Fatalf("guard: %+v %v", guarded, err)
	}
	// The decision is stored as evidence the candidate names.
	recorded := run.store.guards[0]
	if guarded.EvaluationID != recorded.ID || recorded.CandidateSetupID != run.setup.RevisionID || recorded.ActiveSetupID != AgentServerID("agent-1", "support-bot") ||
		recorded.BenchmarkID != result.BenchmarkID || recorded.Decision != "PASS" || recorded.AgentID != "agent-1" || !strings.Contains(recorded.MetricsJSON, `"candidate"`) {
		t.Fatalf("recorded: %+v", recorded)
	}
	// A retried guard step adopts the row it recorded.
	if again, againErr := run.driver.Guard(t.Context(), candidate.ID, candidate); againErr != nil || again.EvaluationID != guarded.EvaluationID || len(run.store.guards) != 1 {
		t.Fatalf("retry: %+v %v (%d rows)", again, againErr, len(run.store.guards))
	}
	row := run.store.benchmarks[result.BenchmarkID]
	var workload map[string]any
	_ = json.Unmarshal([]byte(row.WorkloadJSON), &workload)
	measured := workload["measured"].(map[string]any)
	for _, raw := range measured["runs"].([]any) {
		if entry := raw.(map[string]any); entry["arm"] == "candidate" {
			entry["ttft_p95_ms"] = 400.0
		}
	}
	encoded, _ := json.Marshal(workload)
	row.ID, row.WorkloadJSON = "benchmark-slower", string(encoded)
	run.store.benchmarks[row.ID] = row
	candidate.BenchmarkID = row.ID
	if guarded, err = run.driver.Guard(t.Context(), candidate.ID, candidate); err != nil || guarded.Decision != "REJECT" || !strings.HasPrefix(guarded.FailureCode, "release_guard_") {
		t.Fatalf("slower first token: %+v %v", guarded, err)
	}
}

// A setup an agent cannot start yet is refused before any window.
func TestAgentRunRefusesSetupsAnAgentCannotStart(t *testing.T) {
	draft := optimizer.DeploymentDraft{Name: "support-bot"}
	draft.Model.ID, draft.Model.Revision = "Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218"
	draft.Runtime.Engine, draft.Runtime.Version = "vllm", support.DefaultRuntimeVersion
	draft.Resources.GPU = "NVIDIA-L4"
	if _, err := AgentSetup(draft); err != nil {
		t.Fatalf("a plain setup: %v", err)
	}
	withEnv := draft
	withEnv.Runtime.Env = map[string]string{"VLLM_USE_RUST_FRONTEND": "1"}
	if _, err := AgentSetup(withEnv); err == nil || !strings.Contains(err.Error(), "environment variables") {
		t.Fatalf("env: %v", err)
	}
	if got := agentCommand([]string{"python3", "-m", "sglang.launch_server", "--model-path", "${MODEL}"}); strings.Join(got, " ") != "sglang serve --model-path ${MODEL}" {
		t.Fatalf("sglang command: %v", got)
	}
}

// A divergence is asked for and kept under one rule: a declared live setup,
// a change of precision, the same weights. Without the declaration it is
// neither asked for nor kept, whatever the agent sends.
func TestAgentRunReadsDivergenceOnlyUnderTheDeclaredSetup(t *testing.T) {
	divergence := func(run agentRun) (bool, bool) {
		run.agent.answers = &agentprotocol.WindowAnswers{EvalSet: "default", EvalSetName: "infercrane-default", EvalSetVersion: "1", EvalSetDigest: "sha256:" + strings.Repeat("a", 64),
			PairingDigest: "sha256:" + strings.Repeat("b", 64), Items: 2, CandidateScores: []float64{1, 1},
			Divergence:   &qualityevidence.Divergence{Method: qualityevidence.DivergenceMethod, TopK: 16, Prompts: 2, MaxAnswerTokens: 256, PrefillTokens: 40, GenerationTokens: 10, GenerationMeanKL: 0.01, GenerationMaxKL: 0.05},
			ResultDigest: "sha256:" + strings.Repeat("c", 64), EvaluatedAt: time.Now().UTC().Add(-time.Minute)}
		if _, err := run.measure(t, run.setup.ID); err != nil {
			t.Fatal(err)
		}
		validated, err := run.driver.Validate(t.Context(), run.setup.ID, run.store.candidate(run.setup.ID))
		if err != nil {
			t.Fatal(err)
		}
		return run.agent.specs[0].Quality.Divergence, strings.Contains(run.store.evidence[validated.QualityEvidenceID].DistributionJSON, qualityevidence.DivergenceMethod)
	}
	declared := newAgentRun(t, "throughput", "--kv-cache-dtype", "fp8")
	declared.declare(t)
	if asked, kept := divergence(declared); !asked || !kept {
		t.Fatalf("declared FP8 KV cache: asked=%v kept=%v", asked, kept)
	}
	if asked, kept := divergence(newAgentRun(t, "throughput", "--kv-cache-dtype", "fp8")); asked || kept {
		t.Fatalf("undeclared live setup: asked=%v kept=%v", asked, kept)
	}
	// A summary the evidence rules refuse is left out, not fatal.
	broken := newAgentRun(t, "throughput", "--kv-cache-dtype", "fp8")
	broken.declare(t)
	broken.agent.answers = nil
	if _, err := broken.measure(t, broken.setup.ID); err != nil {
		t.Fatal(err)
	}
	candidate := broken.store.candidate(broken.setup.ID)
	state, _ := DecodeMeasurementCheckpoint(candidate.MeasurementCheckpointJSON)
	state.Agent.Answers = &agentprotocol.WindowAnswers{EvalSet: "default", EvalSetName: "infercrane-default", EvalSetVersion: "1", EvalSetDigest: "sha256:" + strings.Repeat("a", 64),
		PairingDigest: "sha256:" + strings.Repeat("b", 64), Items: 2, CandidateScores: []float64{1, 1}, Divergence: &qualityevidence.Divergence{Method: "other"},
		ResultDigest: "sha256:" + strings.Repeat("c", 64), EvaluatedAt: time.Now().UTC().Add(-time.Minute)}
	encoded, _ := json.Marshal(state)
	candidate.MeasurementCheckpointJSON = string(encoded)
	if validated, err := broken.driver.Validate(t.Context(), candidate.ID, candidate); err != nil || validated.QualityEvidenceID == "" || strings.Contains(broken.store.evidence[validated.QualityEvidenceID].DistributionJSON, "divergence") {
		t.Fatalf("refused divergence: %+v %v", validated, err)
	}
}

// A setup started again on the same node, or on a node the agent could not
// name, is not on a fresh machine: the second window still decides, but the
// evidence says the confirmation needs a second GPU node.
func TestAgentRunSameNodeIsNotAFreshMachine(t *testing.T) {
	for name, nodes := range map[string]map[bool]string{
		"same node":    {false: "node-aaaaaaaaaaaaaaaa", true: "node-aaaaaaaaaaaaaaaa"},
		"unnamed node": {false: "node-aaaaaaaaaaaaaaaa", true: "unknown"},
		"no node":      nil,
	} {
		run := newAgentRun(t, "throughput", "--max-num-seqs", "256")
		run.agent.candidate = map[bool]float64{false: 1100, true: 1095}
		run.agent.nodes = nodes
		result, err := run.measure(t, run.setup.ID)
		if err != nil {
			t.Fatal(err)
		}
		measured := measuredOf(t, run, result)
		if measured.Confirmation == nil || measured.Confirmation.FreshCandidate {
			t.Fatalf("%s: confirmation %+v", name, measured.Confirmation)
		}
		var workload struct {
			Fresh     string              `json:"fresh_machine"`
			LiveSetup string              `json:"live_setup"`
			Windows   []AgentWindowRecord `json:"agent_windows"`
		}
		_ = json.Unmarshal([]byte(run.store.benchmarks[result.BenchmarkID].WorkloadJSON), &workload)
		if !strings.Contains(workload.Fresh, "second GPU node") || workload.LiveSetup != LiveSetupUndeclared || len(workload.Windows) != 2 || workload.Windows[0].ControlNode != "node-cccccccccccccccc" {
			t.Fatalf("%s: workload %+v", name, workload)
		}
	}
}
