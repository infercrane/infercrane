package optimizationcampaign

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/optimizer"
)

// retiredCheckpoint resolves the pending confirmation interruption the way
// the driver does after it retired and replaced that arm's server.
func retiredCheckpoint(t *testing.T, raw string) string {
	t.Helper()
	checkpoint, ok := DecodeMeasurementCheckpoint(raw)
	pending := checkpoint.Pending()
	if !ok || pending == nil || pending.Kind != RunKindConfirmation {
		t.Fatalf("no pending confirmation in %s", raw)
	}
	resolved := pending.At
	pending.Recovery, pending.ResolvedAt = RecoveryMachineRetired, &resolved
	advanceConfirmation(&checkpoint, pending.Arm)
	encoded, _ := json.Marshal(checkpoint)
	return string(encoded)
}

func confirmationFixture(t *testing.T, objective string, steps map[string][]float64) (executorFixture, *checkpointSinkFixture) {
	t.Helper()
	fixture, _, sink := sequencedFixture(t, steps)
	proposal, _ := json.Marshal(optimizer.Proposal{Input: optimizer.Request{Objective: objective}, StartingPoint: &optimizer.StartingPoint{CandidateID: "proposal-control"}})
	fixture.campaign.ProposalJSON = string(proposal)
	return fixture, sink
}

// expectRetirement runs the measurement and checks it stopped to retire arm's
// server, then hands the driver's resolution back for the next attempt.
func expectRetirement(t *testing.T, fixture *executorFixture, sink *checkpointSinkFixture, arm, replica string) {
	t.Helper()
	interrupted := interruptedRun(t, *fixture)
	if interrupted.Interruption.Kind != RunKindConfirmation || interrupted.Interruption.Arm != arm || interrupted.Interruption.ReplicaID != replica || !interrupted.Persisted {
		t.Fatalf("interruption = %+v, want the %s server %s retired", interrupted.Interruption, arm, replica)
	}
	fixture.candidate.MeasurementCheckpointJSON = retiredCheckpoint(t, sink.last)
}

func TestAGainIsMeasuredAgainOnFreshServersBeforeItCounts(t *testing.T) {
	const warm = 1.0
	fixture, sink := confirmationFixture(t, "throughput", map[string][]float64{
		// First window, then the confirmation window on fresh servers.
		controlEndpoint:   {warm, 100, 101, 100, 101, 100, warm, 98, 99, 98, 99, 98},
		candidateEndpoint: {warm, 130, 131, 129, 130, warm, 127, 128, 127, 128},
	})
	expectRetirement(t, &fixture, sink, ArmCandidate, "replica-candidate")
	fixture.store.replicas[0].ID = "replica-candidate-fresh"
	expectRetirement(t, &fixture, sink, ArmControl, "replica-control")
	fixture.store.replicas[1].ID = "replica-control-fresh"

	row, workload := fixture.run(t)
	evidence := measuredEvidence(t, workload)
	first := evidence.Comparison["output_token_throughput"]
	if evidence.Method != MeasurementMethod || first.Decision != DecisionImproved || len(evidence.Runs) != 9 || row.RequestCount != 4*fixture.profile.Requests {
		t.Fatalf("first window = %+v runs=%d row requests=%d", first, len(evidence.Runs), row.RequestCount)
	}
	confirmation := evidence.Confirmation
	if confirmation == nil || confirmation.Control.Role != ControlRoleBracketed || len(confirmation.Runs) != 9 || !confirmation.FreshCandidate || !confirmation.FreshControl {
		t.Fatalf("confirmation = %+v", confirmation)
	}
	second := confirmation.Comparison["output_token_throughput"]
	if second.Decision != DecisionImproved || second.Comparisons != 1 || second.Delta < 0.28 || second.Delta > 0.32 {
		t.Fatalf("confirmation comparison = %+v", second)
	}
	order := strings.Join(evidence.Order, ",")
	if !strings.Contains(order, "retire:candidate,retire:control,resume,warmup:control,warmup:candidate,control,candidate") {
		t.Fatalf("order = %s", order)
	}
	decision, reported, reason := confirmedDecision(&evidence, first, "output_token_throughput", "throughput", false, 1)
	if decision != DecisionImproved || reported.Delta != second.Delta || !strings.Contains(reason, "fresh servers") {
		t.Fatalf("decision=%s reported=%+v reason=%q", decision, reported, reason)
	}
}

func TestNoGainIsNotConfirmed(t *testing.T) {
	const warm = 1.0
	fixture, _ := confirmationFixture(t, "throughput", map[string][]float64{
		controlEndpoint:   {warm, 100, 101, 100, 101, 100},
		candidateEndpoint: {warm, 100, 101, 100, 101},
	})
	_, workload := fixture.run(t)
	if evidence := measuredEvidence(t, workload); evidence.Confirmation != nil || evidence.Comparison["output_token_throughput"].Decision != DecisionNoMeasurableDifference {
		t.Fatalf("evidence = %+v", evidence)
	}
}

// The control of a run that evolves a live endpoint serves production, so
// only the candidate's server is retired.
func TestAProductionControlKeepsItsServer(t *testing.T) {
	const warm = 1.0
	fixture, sink := confirmationFixture(t, "throughput", map[string][]float64{
		controlEndpoint:   {warm, 100, 101, 100, 101, 100, warm, 100, 101, 100, 101, 100},
		candidateEndpoint: {warm, 130, 131, 129, 130, warm, 130, 131, 129, 130},
	})
	fixture.campaign.Intent = IntentEvolveEndpoint
	expectRetirement(t, &fixture, sink, ArmCandidate, "replica-candidate")
	_, workload := fixture.run(t)
	confirmation := measuredEvidence(t, workload).Confirmation
	if confirmation == nil || !confirmation.FreshCandidate || confirmation.FreshControl {
		t.Fatalf("confirmation = %+v", confirmation)
	}
}

// A gain that does not hold on fresh servers is not a win, and the report
// names both measurements.
func TestAGainThatDoesNotHoldOnFreshServersIsNotConfirmed(t *testing.T) {
	const warm = 1.0
	fixture, sink := confirmationFixture(t, "throughput", map[string][]float64{
		controlEndpoint:   {warm, 100, 101, 100, 101, 100, warm, 100, 101, 100, 101, 100},
		candidateEndpoint: {warm, 104, 105, 104, 105, warm, 100, 101, 101, 100},
	})
	expectRetirement(t, &fixture, sink, ArmCandidate, "replica-candidate")
	expectRetirement(t, &fixture, sink, ArmControl, "replica-control")
	_, workload := fixture.run(t)
	evidence := measuredEvidence(t, workload)
	first := evidence.Comparison["output_token_throughput"]
	decision, reported, reason := confirmedDecision(&evidence, first, "output_token_throughput", "throughput", false, 1)
	if first.Decision != DecisionImproved || decision != DecisionNotConfirmed || reported.Decision == DecisionImproved || !strings.Contains(reason, "measured again on fresh servers") {
		t.Fatalf("first=%+v decision=%s reported=%+v reason=%q", first, decision, reported, reason)
	}
}

func TestAWinWithoutAConfirmationIsNotConfirmed(t *testing.T) {
	first := MetricComparison{Delta: 0.2, NoiseFloor: 0.01, Decision: DecisionImproved}
	if decision, _, reason := confirmedDecision(&MeasuredEvidence{Method: MeasurementMethod}, first, "output_token_throughput", "throughput", false, 1); decision != DecisionNotConfirmed || !strings.Contains(reason, "not measured again") {
		t.Fatalf("decision=%s reason=%q", decision, reason)
	}
	// Evidence measured before confirmations existed keeps its decision.
	if decision, _, _ := confirmedDecision(&MeasuredEvidence{Method: "campaign-interleaved-control-v2"}, first, "output_token_throughput", "throughput", false, 1); decision != DecisionImproved {
		t.Fatalf("older evidence decision = %s", decision)
	}
}

func TestRetirementsDoNotUseUpTheLostMachineLimits(t *testing.T) {
	entries := []MachineReplacement{{LostReplicaID: "a", Purpose: ReplacementPurposeConfirmation}, {LostReplicaID: "b"}, {LostReplicaID: "c", Purpose: ReplacementPurposeConfirmation}, {LostReplicaID: "stopped", State: ReplacementGivenUp}}
	if lostMachineReplacements(entries) != 1 || MachineReplacementsUsed(entries) != 3 {
		t.Fatalf("lost=%d used=%d", lostMachineReplacements(entries), MachineReplacementsUsed(entries))
	}
	checkpoint := MeasurementCheckpoint{Interruptions: []MeasurementInterruption{{Kind: RunKindConfirmation}, {Kind: RunKindTurn}, {Kind: RunKindConfirmation}}}
	if lostInterruptions(&checkpoint) != 1 {
		t.Fatalf("lost interruptions = %d", lostInterruptions(&checkpoint))
	}
}

func TestConfirmationNeedsAServingControl(t *testing.T) {
	fixture, _ := confirmationFixture(t, "throughput", map[string][]float64{})
	checkpoint := MeasurementCheckpoint{Confirmation: &ConfirmationState{Phase: ConfirmMeasure}, Segments: []MeasurementSegment{{CandidateRuns: measurementRunsPerArm, ControlRuns: measurementRunsPerArm + 1, Ended: SegmentCompleted}}}
	session := measurementSession{state: &checkpoint, candidate: armTarget{candidateID: fixture.candidate.ID}, now: fixture.executor.now}
	if _, err := session.run(t.Context()); err == nil || !strings.Contains(err.Error(), "control is no longer serving") {
		t.Fatalf("err = %v", err)
	}
}

// A candidate that won its first window but not again on fresh servers keeps
// the control, with its own code; one that won both is selected and reports
// the second window's change.
func TestRankingSelectsOnlyAConfirmedGain(t *testing.T) {
	won := MetricComparison{ControlMean: 220, CandidateMean: 140, Delta: -80.0 / 220, NoiseFloor: 0.04, Decision: DecisionImproved}
	for name, test := range map[string]struct {
		second   MetricComparison
		decision string
		code     string
	}{
		"held":         {second: MetricComparison{ControlMean: 222, CandidateMean: 150, Delta: -72.0 / 222, NoiseFloor: 0.05, Decision: DecisionImproved}, decision: RankSelect},
		"did-not-hold": {second: MetricComparison{ControlMean: 222, CandidateMean: 219, Delta: -3.0 / 222, NoiseFloor: 0.05, Decision: DecisionNoMeasurableDifference}, decision: RankSupersede, code: FailureNotConfirmed},
	} {
		t.Run(name, func(t *testing.T) {
			campaign, benchmarks := rankingFixture(t, "interactive", nil)
			control := ControlEvidence{Role: ControlRoleBracketed, CandidateID: "candidate-a", Runs: 5}
			benchmarks[1].WorkloadJSON = measuredWorkload(t, `{"profile":"interactive","request_count":100,"random_seed":17}`, MeasuredEvidence{
				Method: MeasurementMethod, Control: control, Comparison: map[string]MetricComparison{"ttft_p95_ms": won},
				Confirmation: &ConfirmationEvidence{Control: control, Comparison: map[string]MetricComparison{"ttft_p95_ms": test.second}, FreshCandidate: true, FreshControl: true},
			}, nil)
			result, err := RankMeasuredCampaign(campaign, benchmarks)
			if err != nil {
				t.Fatal(err)
			}
			if result.Decisions["candidate-b"] != test.decision || result.FailureCodes["candidate-b"] != test.code || !strings.Contains(result.Reasons["candidate-b"], "fresh servers") {
				t.Fatalf("decisions=%v codes=%v reasons=%v", result.Decisions, result.FailureCodes, result.Reasons)
			}
		})
	}
}
