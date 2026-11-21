package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/performanceprofile"
	"github.com/infercrane/infercrane/internal/workflows"
)

func (f *compositeStoreFixture) RecordOptimizationMeasurementCheckpoint(_ context.Context, _, _, candidateID, checkpointJSON string) (bool, error) {
	for index := range f.campaign.Candidates {
		if f.campaign.Candidates[index].ID == candidateID && f.campaign.Candidates[index].State == CandidateMeasuring {
			f.campaign.Candidates[index].MeasurementCheckpointJSON = checkpointJSON
			return true, nil
		}
	}
	return false, nil
}

// RecordOptimizationMachineReplacement mirrors the store: entries are keyed
// by lost replica, and a new entry is refused at the limit, stop markers not
// counted.
func (f *compositeStoreFixture) RecordOptimizationMachineReplacement(_ context.Context, _, _, candidateID, entryJSON string, limit int) (string, error) {
	var entry MachineReplacement
	if err := json.Unmarshal([]byte(entryJSON), &entry); err != nil {
		return "", err
	}
	for index := range f.campaign.Candidates {
		row := &f.campaign.Candidates[index]
		if row.ID != candidateID {
			continue
		}
		entries := DecodeMachineReplacements(row.MachineReplacementsJSON)
		replaced := false
		for position := range entries {
			if entries[position].LostReplicaID == entry.LostReplicaID {
				entries[position], replaced = entry, true
			}
		}
		if !replaced {
			if MachineReplacementsUsed(entries) >= limit {
				return "", domain.ErrConflict
			}
			entries = append(entries, entry)
		}
		encoded, _ := json.Marshal(entries)
		row.MachineReplacementsJSON = string(encoded)
		return row.MachineReplacementsJSON, nil
	}
	return "", domain.ErrNotFound
}

type capturingBenchmark struct {
	row        domain.BenchmarkResult
	calls      int
	checkpoint string
}

func (f *capturingBenchmark) Run(_ context.Context, _ domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, _ performanceprofile.Profile) (domain.BenchmarkResult, error) {
	f.calls++
	f.checkpoint = candidate.MeasurementCheckpointJSON
	return f.row, nil
}

type recoveryHarness struct {
	store     *compositeStoreFixture
	driver    CompositeDriver
	benchmark *capturingBenchmark
	budget    Budget
	now       time.Time
}

// newRecoveryHarness is a run whose control is serving and whose second setup
// stopped measuring at an interruption of arm, still pending.
func newRecoveryHarness(t *testing.T, intent, arm string, sequence int) recoveryHarness {
	t.Helper()
	now := time.Now().UTC()
	draft := func(name string) string {
		value := optimizer.DeploymentDraft{APIVersion: "infercrane.dev/v1", Kind: "Deployment", Name: name}
		value.Model.ID, value.Model.Revision = "Qwen/Qwen3-8B", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		value.Runtime.Engine, value.Runtime.Version = "vllm", "0.8.5.post1"
		value.Compute.Mode, value.Resources.GPU = "elastic", "L40S"
		value.Provider.Cloud, value.Provider.Adapter, value.Provider.Region = "aws", "aws-ec2", "eu-central-1"
		value.Scaling.MinReplicas, value.Scaling.MaxReplicas = 1, 1
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
	proposal, _ := json.Marshal(optimizer.Proposal{StartingPoint: &optimizer.StartingPoint{CandidateID: "proposal-control"}, Candidates: []optimizer.Candidate{{ID: "proposal-control", BenchmarkProfile: "interactive"}, {ID: "proposal-candidate", BenchmarkProfile: "interactive"}, {ID: "proposal-later", BenchmarkProfile: "interactive"}}})
	approved := now.Add(-time.Minute)
	campaign := domain.OptimizationCampaign{ID: "campaign-1", TenantID: "tenant", Intent: intent, ProposalJSON: string(proposal), ApprovedAt: &approved}
	if intent == IntentEvolveEndpoint {
		campaign.TargetDeployment = "production"
	}
	deployment := func(name string) string {
		if intent == IntentEvolveEndpoint {
			return "production"
		}
		return name
	}
	control := domain.OptimizationCandidateRun{ID: "control-12345678", TenantID: "tenant", CampaignID: campaign.ID, ProposalCandidateID: "proposal-control", State: CandidateRanked, DeploymentName: deployment("qwen-opt-control"), RevisionID: "revision-control", DeploymentSpecJSON: draft("qwen"), CreatedAt: now.Add(-time.Hour), MachineReplacementsJSON: "[]"}
	measured := domain.OptimizationCandidateRun{ID: "candidate-12345678", TenantID: "tenant", CampaignID: campaign.ID, ProposalCandidateID: "proposal-candidate", State: CandidateMeasuring, DeploymentName: deployment("qwen-opt-candidate"), RevisionID: "revision-candidate", DeploymentSpecJSON: draft("qwen"), CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour), MachineReplacementsJSON: "[]"}
	later := domain.OptimizationCandidateRun{ID: "later-12345678", TenantID: "tenant", CampaignID: campaign.ID, ProposalCandidateID: "proposal-later", State: CandidateProvisioning, DeploymentSpecJSON: draft("qwen"), CreatedAt: now.Add(-time.Hour), MachineReplacementsJSON: "[]"}
	owner, replica := measured, "replica-lost-candidate"
	if arm == ArmControl {
		owner, replica = control, "replica-lost-control"
	}
	checkpoint := MeasurementCheckpoint{Version: measurementCheckpointVersion, Method: MeasurementMethod, Identity: "sha256:identity", Segments: []MeasurementSegment{{StartedAt: now.Add(-10 * time.Minute), Ended: SegmentInterrupted}}}
	for index := 1; index < sequence; index++ {
		resolved := now.Add(-5 * time.Minute)
		checkpoint.Interruptions = append(checkpoint.Interruptions, MeasurementInterruption{Sequence: index, Arm: ArmCandidate, Kind: RunKindTurn, OwnerCandidateID: measured.ID, Recovery: RecoveryMachineServing, ResolvedAt: &resolved})
	}
	checkpoint.Interruptions = append(checkpoint.Interruptions, MeasurementInterruption{Sequence: sequence, At: now.Add(-time.Minute), Arm: arm, Kind: RunKindTurn, OwnerCandidateID: owner.ID, ReplicaID: replica, Reason: "endpoint stopped answering"})
	encoded, _ := json.Marshal(checkpoint)
	measured.MeasurementCheckpointJSON = string(encoded)
	campaign.Candidates = []domain.OptimizationCandidateRun{control, measured, later}
	store := &compositeStoreFixture{campaign: campaign, resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment-1", Name: owner.DeploymentName}}}
	benchmark := &capturingBenchmark{row: domain.BenchmarkResult{ID: "benchmark-1", RevisionID: measured.RevisionID}}
	driver := CompositeDriver{Store: store, Costs: freshCost(now), Benchmark: benchmark, Now: func() time.Time { return now }}
	return recoveryHarness{store: store, driver: driver, benchmark: benchmark, budget: Budget{MaxCostUSD: 10, ExpiresAt: now.Add(time.Hour)}, now: now}
}

func (h recoveryHarness) candidate(id string) domain.OptimizationCandidateRun {
	candidate, _ := candidateByID(h.store.campaign.Candidates, id)
	return candidate
}

func (h recoveryHarness) measure(t *testing.T) (MeasurementResult, error) {
	t.Helper()
	measured := h.candidate("candidate-12345678")
	return h.driver.Measure(t.Context(), measured.ID, measured, h.budget)
}

func (h recoveryHarness) finish(key, result string) {
	operation := h.store.operations[key]
	operation.Status, operation.ResultJSON = "succeeded", result
	h.store.operations[key] = operation
}

func (h recoveryHarness) checkpoint(t *testing.T) MeasurementCheckpoint {
	t.Helper()
	checkpoint, ok := DecodeMeasurementCheckpoint(h.candidate("candidate-12345678").MeasurementCheckpointJSON)
	if !ok {
		t.Fatal("measured candidate has no checkpoint")
	}
	return checkpoint
}

func assertPermanentCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Retryable || failure.Code != code {
		t.Fatalf("error=%v, want permanent %s", err, code)
	}
}

func removalKey(owner string, sequence int) string {
	return childKey(owner, fmt.Sprintf("remove-lost-candidat-%d", sequence))
}

func lostResult(replica string, lost bool) string {
	encoded, _ := json.Marshal(workflows.RemoveLostReplicaResult{ReplicaID: replica, Lost: lost})
	return string(encoded)
}

func TestLostCandidateMachineIsReplacedBeforeMeasuringResumes(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmCandidate, 1)
	_, err := h.measure(t)
	assertRetryableCode(t, err, "optimization_child_pending")
	removal := h.store.operations[removalKey("candidate-12345678", 1)]
	var removeRequest workflows.RemoveLostReplicaRequest
	_ = json.Unmarshal([]byte(removal.RequestJSON), &removeRequest)
	stopped := h.checkpoint(t)
	if removal.Kind != workflows.ReplicaRemoveLostKind || removal.ResourceName != "qwen-opt-candidate" || removeRequest.ReplicaID != "replica-lost-candidate" || stopped.Pending().Stage != stageRemoving || h.benchmark.calls != 0 {
		t.Fatalf("removal=%+v request=%+v checkpoint=%+v", removal, removeRequest, h.checkpoint(t))
	}

	h.finish(removalKey("candidate-12345678", 1), lostResult("replica-lost-candidate", true))
	_, err = h.measure(t)
	assertRetryableCode(t, err, "optimization_child_pending")
	replaceKey := childKey("candidate-12345678", "replace-1")
	replace := h.store.operations[replaceKey]
	var converge workflows.CloudRequest
	_ = json.Unmarshal([]byte(replace.RequestJSON), &converge)
	if replace.Kind != workflows.ConvergeKind || converge.Name != "qwen-opt-candidate" || converge.DeploymentID != "deployment-1" || converge.RevisionID != "revision-candidate" || !converge.NoEndpoint || converge.Validate() != nil {
		t.Fatalf("replacement must converge the same revision without an endpoint: %+v %+v", replace, converge)
	}
	entries := DecodeMachineReplacements(h.candidate("candidate-12345678").MachineReplacementsJSON)
	if len(entries) != 1 || entries[0].Number != 1 || entries[0].State != ReplacementProvisioning || entries[0].LostReplicaID != "replica-lost-candidate" {
		t.Fatalf("entries = %+v", entries)
	}

	h.finish(replaceKey, `{"replicas":1}`)
	result, err := h.measure(t)
	if err != nil || result.BenchmarkID != "benchmark-1" || h.benchmark.calls != 1 {
		t.Fatalf("measuring must resume after the replacement: result=%+v err=%v calls=%d", result, err, h.benchmark.calls)
	}
	resumed, _ := DecodeMeasurementCheckpoint(h.benchmark.checkpoint)
	interruption := resumed.Interruptions[0]
	if resumed.Pending() != nil || interruption.Recovery != RecoveryMachineReplaced || interruption.ReplacementNumber != 1 || interruption.ReplaceOperationID == "" || interruption.RemovalOperationID == "" {
		t.Fatalf("resumed checkpoint = %+v", resumed)
	}
	if entries = DecodeMachineReplacements(h.candidate("candidate-12345678").MachineReplacementsJSON); entries[0].State != ReplacementReplaced || entries[0].OperationID != replaceKey {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestServingMachineIsKeptAndMeasuringResumes(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmControl, 1)
	_, err := h.measure(t)
	assertRetryableCode(t, err, "optimization_child_pending")
	h.finish(removalKey("control-12345678", 1), lostResult("replica-lost-control", false))
	if _, err = h.measure(t); err != nil || h.benchmark.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, h.benchmark.calls)
	}
	resumed, _ := DecodeMeasurementCheckpoint(h.benchmark.checkpoint)
	if resumed.Interruptions[0].Recovery != RecoveryMachineServing || len(h.store.operations) != 1 || len(DecodeMachineReplacements(h.candidate("control-12345678").MachineReplacementsJSON)) != 0 {
		t.Fatalf("a serving machine must not be replaced: checkpoint=%+v operations=%d", resumed, len(h.store.operations))
	}
}

func TestEvolveReplacementReprovisionsTheCandidateRevisionOnTheTarget(t *testing.T) {
	h := newRecoveryHarness(t, IntentEvolveEndpoint, ArmCandidate, 1)
	_, _ = h.measure(t)
	h.finish(removalKey("candidate-12345678", 1), lostResult("replica-lost-candidate", true))
	_, err := h.measure(t)
	assertRetryableCode(t, err, "optimization_child_pending")
	replace := h.store.operations[childKey("candidate-12345678", "replace-1")]
	var rollout workflows.RolloutRequest
	_ = json.Unmarshal([]byte(replace.RequestJSON), &rollout)
	if replace.Kind != workflows.RolloutProvisionKind || replace.ResourceName != "production" || rollout.Name != "production" || rollout.CandidateID != "revision-candidate" {
		t.Fatalf("replace=%+v rollout=%+v", replace, rollout)
	}
}

func TestThirdLostMachineOfASetupStopsItInsideTheCap(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmCandidate, 1)
	used, _ := json.Marshal([]MachineReplacement{{LostReplicaID: "first", Number: 1, State: ReplacementReplaced}, {LostReplicaID: "second", Number: 2, State: ReplacementReplaced}})
	h.store.campaign.Candidates[1].MachineReplacementsJSON = string(used)
	_, _ = h.measure(t)
	h.finish(removalKey("candidate-12345678", 1), lostResult("replica-lost-candidate", true))
	_, err := h.measure(t)
	assertPermanentCode(t, err, FailureMachineLostRepeatedly)
	if _, replaced := h.store.operations[childKey("candidate-12345678", "replace-3")]; replaced || h.checkpoint(t).Interruptions[0].GaveUp != FailureMachineLostRepeatedly || ControlLost(h.store.campaign) {
		t.Fatalf("operations=%v checkpoint=%+v", h.store.operations, h.checkpoint(t))
	}
}

func TestLostControlThatCannotBeReplacedStopsTheWholeRun(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmControl, 1)
	used, _ := json.Marshal([]MachineReplacement{{LostReplicaID: "first", Number: 1, State: ReplacementReplaced}, {LostReplicaID: "second", Number: 2, State: ReplacementReplaced}})
	h.store.campaign.Candidates[0].MachineReplacementsJSON = string(used)
	_, _ = h.measure(t)
	h.finish(removalKey("control-12345678", 1), lostResult("replica-lost-control", true))
	_, err := h.measure(t)
	assertPermanentCode(t, err, FailureControlLost)
	if !ControlLost(h.store.campaign) || h.checkpoint(t).Interruptions[0].GaveUp != FailureMachineLostRepeatedly {
		t.Fatalf("control=%s checkpoint=%+v", h.candidate("control-12345678").MachineReplacementsJSON, h.checkpoint(t))
	}
	// No later setup spends: it fails before any price check or provider
	// mutation.
	h.driver.Costs = nil
	submitted := len(h.store.operations)
	later := h.candidate("later-12345678")
	_, err = h.driver.Provision(t.Context(), later.ID, later, h.budget)
	assertPermanentCode(t, err, FailureControlLost)
	if len(h.store.operations) != submitted || h.store.cloudSubmits != 0 {
		t.Fatalf("a later setup was provisioned after the control was lost: operations=%d submits=%d", len(h.store.operations), h.store.cloudSubmits)
	}
}

func TestReplacementOutsideTheApprovedCapStopsTheSetup(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmCandidate, 1)
	// One hour at $1.50 fits a $2 share; the replacement's reserve does not.
	h.budget.MaxCostUSD = 2
	_, _ = h.measure(t)
	h.finish(removalKey("candidate-12345678", 1), lostResult("replica-lost-candidate", true))
	_, err := h.measure(t)
	assertPermanentCode(t, err, FailureReplacementNotCovered)
	if _, replaced := h.store.operations[childKey("candidate-12345678", "replace-1")]; replaced || len(DecodeMachineReplacements(h.candidate("candidate-12345678").MachineReplacementsJSON)) != 0 {
		t.Fatal("a replacement the cap cannot cover was requested")
	}
}

func TestTooManyInterruptionsStopTheMeasurement(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmCandidate, maxMeasurementInterruptions+1)
	_, err := h.measure(t)
	assertPermanentCode(t, err, FailureMeasurementInterrupted)
	if len(h.store.operations) != 0 || h.benchmark.calls != 0 {
		t.Fatalf("operations=%d calls=%d", len(h.store.operations), h.benchmark.calls)
	}
}

func TestFailedReplacementIsRecordedAndStopsTheSetup(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmCandidate, 1)
	_, _ = h.measure(t)
	h.finish(removalKey("candidate-12345678", 1), lostResult("replica-lost-candidate", true))
	_, _ = h.measure(t)
	key := childKey("candidate-12345678", "replace-1")
	failed := h.store.operations[key]
	failed.Status, failed.ErrorCode, failed.Message = "failed", "runtime_bootstrap_failed", "runtime exited"
	h.store.operations[key] = failed
	_, err := h.measure(t)
	assertPermanentCode(t, err, FailureMachineReplacementFailed)
	if entries := DecodeMachineReplacements(h.candidate("candidate-12345678").MachineReplacementsJSON); len(entries) != 1 || entries[0].State != ReplacementFailed || entries[0].FailureCode != "runtime_bootstrap_failed" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestServerlessWorkersAreLeftToTheirProvider(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmCandidate, 1)
	var draft optimizer.DeploymentDraft
	_ = json.Unmarshal([]byte(h.store.campaign.Candidates[1].DeploymentSpecJSON), &draft)
	draft.Compute.Mode = "serverless"
	encoded, _ := json.Marshal(draft)
	h.store.campaign.Candidates[1].DeploymentSpecJSON = string(encoded)
	if _, err := h.measure(t); err != nil || len(h.store.operations) != 0 || h.benchmark.calls != 1 {
		t.Fatalf("err=%v operations=%d calls=%d", err, len(h.store.operations), h.benchmark.calls)
	}
}

func TestReplacementWorstCaseCountsTheWindowAndOneReservePerReplacement(t *testing.T) {
	now := time.Now().UTC()
	quote := CostQuote{HourlyUSD: 2}
	since, expires := now, now.Add(90*time.Minute)
	if worst := ReplacementWorstCase(quote, since, expires, 0); worst != 3 {
		t.Fatalf("no replacement = %v", worst)
	}
	if worst := ReplacementWorstCase(quote, since, expires, 2); worst != 7 {
		t.Fatalf("two replacements = %v", worst)
	}
	for share, want := range map[float64]int{2.99: 0, 5: 1, 7: 2, 100: MaxMachineReplacements} {
		if got := ReplacementsCovered(quote, since, expires, share); got != want {
			t.Fatalf("share $%.2f covers %d, want %d", share, got, want)
		}
	}
	capped := CostQuote{HourlyUSD: 2, HardMaximumCostUSD: 4}
	if got := ReplacementsCovered(capped, since, expires, 4); got != MaxMachineReplacements {
		t.Fatalf("a provider hard maximum bounds every replacement: %d", got)
	}
}

func TestInterruptionLimitOnAControlRunStopsOnlyTheMeasuredSetup(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmControl, maxMeasurementInterruptions+1)
	_, err := h.measure(t)
	assertPermanentCode(t, err, FailureMeasurementInterrupted)
	if ControlLost(h.store.campaign) || len(h.store.operations) != 0 {
		t.Fatalf("a control never found lost was marked lost: %s", h.candidate("control-12345678").MachineReplacementsJSON)
	}
}

func TestReplacementIsPricedAtTheBoundRateWhenTheFreshQuoteIsLower(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmCandidate, 1)
	// The setup's servers were bound at $3/h; the fresh quote is $1.50/h. A
	// $5 share covers the window plus one reserve at $1.50 but not at $3.
	h.store.campaign.Candidates[1].CostQuoteJSON = `{"hourly_usd":3,"max_replicas":1,"source":"provider-price-list"}`
	h.budget.MaxCostUSD = 5
	_, _ = h.measure(t)
	h.finish(removalKey("candidate-12345678", 1), lostResult("replica-lost-candidate", true))
	_, err := h.measure(t)
	assertPermanentCode(t, err, FailureReplacementNotCovered)
}

// A confirmation retires the candidate's serving machine on purpose: the
// removal is a retirement, the replacement is recorded as a confirmation,
// and the confirmation moves on to the control's server.
func TestConfirmationRetiresAServingMachineAndReplacesIt(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmCandidate, 1)
	measured := h.candidate("candidate-12345678")
	checkpoint, _ := DecodeMeasurementCheckpoint(measured.MeasurementCheckpointJSON)
	checkpoint.Interruptions[0].Kind, checkpoint.Interruptions[0].ReplicaID = RunKindConfirmation, "replica-serving-candidate"
	checkpoint.Confirmation = &ConfirmationState{Phase: ConfirmRetireCandidate}
	encoded, _ := json.Marshal(checkpoint)
	for index := range h.store.campaign.Candidates {
		if h.store.campaign.Candidates[index].ID == measured.ID {
			h.store.campaign.Candidates[index].MeasurementCheckpointJSON = string(encoded)
		}
	}

	_, err := h.measure(t)
	assertRetryableCode(t, err, "optimization_child_pending")
	retireKey := childKey("candidate-12345678", "retire-candidat-1")
	var request workflows.RemoveLostReplicaRequest
	_ = json.Unmarshal([]byte(h.store.operations[retireKey].RequestJSON), &request)
	if !request.Retire || request.ReplicaID != "replica-serving-candidate" || h.benchmark.calls != 0 {
		t.Fatalf("retire request = %+v operations=%v", request, h.store.operations)
	}

	retired, _ := json.Marshal(workflows.RemoveLostReplicaResult{ReplicaID: "replica-serving-candidate", Retired: true})
	h.finish(retireKey, string(retired))
	_, err = h.measure(t)
	assertRetryableCode(t, err, "optimization_child_pending")
	entries := DecodeMachineReplacements(h.candidate("candidate-12345678").MachineReplacementsJSON)
	if len(entries) != 1 || entries[0].Purpose != ReplacementPurposeConfirmation || entries[0].Number != 1 || lostMachineReplacements(entries) != 0 {
		t.Fatalf("entries = %+v", entries)
	}

	h.finish(childKey("candidate-12345678", "replace-1"), `{"replicas":1}`)
	if _, err = h.measure(t); err != nil || h.benchmark.calls != 1 {
		t.Fatalf("measuring must continue after the retirement: err=%v calls=%d", err, h.benchmark.calls)
	}
	resumed, _ := DecodeMeasurementCheckpoint(h.benchmark.checkpoint)
	if resumed.Pending() != nil || resumed.Interruptions[0].Recovery != RecoveryMachineRetired || resumed.Confirmation.Phase != ConfirmRetireControl {
		t.Fatalf("resumed checkpoint = %+v confirmation=%+v", resumed.Interruptions, resumed.Confirmation)
	}
}

// A retirement the approved cap could not replace never retires anything.
func TestConfirmationOutsideTheCapRetiresNothing(t *testing.T) {
	h := newRecoveryHarness(t, IntentNewEndpoint, ArmCandidate, 1)
	measured := h.candidate("candidate-12345678")
	checkpoint, _ := DecodeMeasurementCheckpoint(measured.MeasurementCheckpointJSON)
	checkpoint.Interruptions[0].Kind = RunKindConfirmation
	checkpoint.Confirmation = &ConfirmationState{Phase: ConfirmRetireCandidate}
	encoded, _ := json.Marshal(checkpoint)
	for index := range h.store.campaign.Candidates {
		if h.store.campaign.Candidates[index].ID == measured.ID {
			h.store.campaign.Candidates[index].MeasurementCheckpointJSON = string(encoded)
		}
	}
	h.budget.MaxCostUSD = 0.01
	_, err := h.measure(t)
	assertPermanentCode(t, err, FailureConfirmationNotCovered)
	if len(h.store.operations) != 0 {
		t.Fatalf("a retirement started outside the cap: %v", h.store.operations)
	}
}
