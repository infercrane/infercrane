package optimizationcampaign

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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

// A fresh server can serve its first measured run slower than the rest, and
// that can make the control drift past its limit. An invalid window measured
// nothing, so it is measured once more on the same servers: a valid second
// try decides, and the invalid one is kept for audit. Invalid twice, whether
// the gain holds stays unknown, never "did not hold".
func TestAnInvalidWindowOnFreshServersIsMeasuredOnceMore(t *testing.T) {
	const warm = 1.0
	firstControl := []float64{warm, 100, 101, 100, 101, 100}
	firstCandidate := []float64{warm, 125, 126, 125, 126}
	drifting, drifted := []float64{warm, 80, 99, 100, 99, 100}, []float64{warm, 110, 111, 110, 111}
	for name, test := range map[string]struct {
		control, candidate []float64
		decision           string
		says               string
	}{
		"invalid, then valid": {control: append(append(append([]float64{}, firstControl...), drifting...), warm, 100, 101, 100, 101, 100),
			candidate: append(append(append([]float64{}, firstCandidate...), drifted...), warm, 120, 121, 120, 121), decision: DecisionImproved, says: "measured once more"},
		"invalid twice": {control: append(append(append([]float64{}, firstControl...), drifting...), drifting...),
			candidate: append(append(append([]float64{}, firstCandidate...), drifted...), drifted...), decision: controlDecisionUnknown, says: "again when measured once more"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture, sink := confirmationFixture(t, "throughput", map[string][]float64{controlEndpoint: test.control, candidateEndpoint: test.candidate})
			expectRetirement(t, &fixture, sink, ArmCandidate, "replica-candidate")
			expectRetirement(t, &fixture, sink, ArmControl, "replica-control")
			_, workload := fixture.run(t)
			evidence := measuredEvidence(t, workload)
			first := evidence.Comparison["output_token_throughput"]
			confirmation := evidence.Confirmation
			if first.Decision != DecisionImproved || confirmation == nil || confirmation.Discarded == nil || confirmation.Discarded.Control.Role != ControlRoleInvalid || len(confirmation.Discarded.Runs) != 9 || len(confirmation.Runs) != 9 {
				t.Fatalf("first=%+v confirmation=%+v", first, confirmation)
			}
			decision, _, reason := confirmedDecision(&evidence, first, "output_token_throughput", "throughput", false, 1)
			if decision != test.decision || !strings.Contains(reason, test.says) || strings.Contains(reason, "did not hold") {
				t.Fatalf("decision=%s reason=%q", decision, reason)
			}
		})
	}
}

func TestAWinWithoutAJudgedSecondWindowIsUnknown(t *testing.T) {
	first := MetricComparison{Delta: 0.2, NoiseFloor: 0.01, Decision: DecisionImproved}
	if decision, _, reason := confirmedDecision(&MeasuredEvidence{Method: MeasurementMethod}, first, "output_token_throughput", "throughput", false, 1); decision != controlDecisionUnknown || !strings.Contains(reason, "not measured again") || !strings.Contains(reason, "unknown") {
		t.Fatalf("decision=%s reason=%q", decision, reason)
	}
	bracketed := ControlEvidence{Role: ControlRoleBracketed}
	withoutObjective := &MeasuredEvidence{Method: MeasurementMethod, Confirmation: &ConfirmationEvidence{Control: bracketed, Comparison: map[string]MetricComparison{"ttft_p95_ms": first}}}
	if decision, _, reason := confirmedDecision(withoutObjective, first, "output_token_throughput", "throughput", false, 1); decision != controlDecisionUnknown || !strings.Contains(reason, "not comparable on fresh servers") {
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
// the second window's change. One whose fresh window was invalid keeps the
// control too, as not compared reliably: nothing measured its gain failing.
func TestRankingSelectsOnlyAConfirmedGain(t *testing.T) {
	won := MetricComparison{ControlMean: 220, CandidateMean: 140, Delta: -80.0 / 220, NoiseFloor: 0.04, Decision: DecisionImproved}
	drifted := ControlEvidence{Role: ControlRoleInvalid, CandidateID: "candidate-a", Runs: 5, Reason: "control output throughput drifted 20.0% between adjacent runs, above the 15% limit", MaxDrift: 0.2, DriftLimit: maxControlDrift}
	for name, test := range map[string]struct {
		second   MetricComparison
		invalid  bool
		decision string
		code     string
		says     string
	}{
		"held":         {second: MetricComparison{ControlMean: 222, CandidateMean: 150, Delta: -72.0 / 222, NoiseFloor: 0.05, Decision: DecisionImproved}, decision: RankSelect, says: "measurably better"},
		"did-not-hold": {second: MetricComparison{ControlMean: 222, CandidateMean: 219, Delta: -3.0 / 222, NoiseFloor: 0.05, Decision: DecisionNoMeasurableDifference}, decision: RankSupersede, code: FailureNotConfirmed, says: "did not hold"},
		"invalid":      {invalid: true, decision: RankSupersede, code: FailureComparisonUnavailable, says: "whether the gain holds is unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			campaign, benchmarks := rankingFixture(t, "interactive", nil)
			control := ControlEvidence{Role: ControlRoleBracketed, CandidateID: "candidate-a", Runs: 5}
			confirmation := &ConfirmationEvidence{Control: control, Comparison: map[string]MetricComparison{"ttft_p95_ms": test.second}, FreshCandidate: true, FreshControl: true}
			if test.invalid {
				confirmation = &ConfirmationEvidence{Control: drifted, FreshCandidate: true, FreshControl: true}
			}
			benchmarks[1].WorkloadJSON = measuredWorkload(t, `{"profile":"interactive","request_count":100,"random_seed":17}`, MeasuredEvidence{
				Method: MeasurementMethod, Control: control, Comparison: map[string]MetricComparison{"ttft_p95_ms": won}, Confirmation: confirmation,
			}, nil)
			result, err := RankMeasuredCampaign(campaign, benchmarks)
			if err != nil {
				t.Fatal(err)
			}
			reason := result.Reasons["candidate-b"]
			if result.Decisions["candidate-b"] != test.decision || result.FailureCodes["candidate-b"] != test.code || !strings.Contains(reason, "fresh servers") || !strings.Contains(reason, test.says) {
				t.Fatalf("decisions=%v codes=%v reasons=%v", result.Decisions, result.FailureCodes, result.Reasons)
			}
			if test.invalid && strings.Contains(reason, "did not hold") {
				t.Fatalf("an invalid window cannot show the gain failing: %q", reason)
			}
		})
	}
}

// A window is measured again only while the approval leaves as long as the
// invalid try took, and only once.
func TestAWindowIsMeasuredAgainOnlyWithTimeLeftAndOnce(t *testing.T) {
	now := time.Date(2026, 11, 26, 6, 0, 0, 0, time.UTC)
	ended := now.Add(-time.Minute)
	invalid := measurement{evidence: MeasuredEvidence{Confirmation: &ConfirmationEvidence{Control: ControlEvidence{Role: ControlRoleInvalid}}}}
	session := func(deadline time.Time) *measurementSession {
		state := &MeasurementCheckpoint{Confirmation: &ConfirmationState{Phase: ConfirmMeasure}, Segments: []MeasurementSegment{{StartedAt: ended.Add(-12 * time.Minute), EndedAt: &ended, Confirmation: true}}}
		return &measurementSession{state: state, now: func() time.Time { return now }, deadline: deadline}
	}
	if short := session(now.Add(5 * time.Minute)); short.measureAgain(invalid) || short.state.Confirmation.Attempt != 0 {
		t.Fatal("measured again with less time left than the try took")
	}
	roomy := session(now.Add(time.Hour))
	if !roomy.measureAgain(invalid) || roomy.state.Confirmation.Attempt != 1 || roomy.state.Order[len(roomy.state.Order)-1] != "measure-again" {
		t.Fatalf("state = %+v", roomy.state)
	}
	if roomy.measureAgain(invalid) {
		t.Fatal("measured again twice")
	}
	valid := measurement{evidence: MeasuredEvidence{Confirmation: &ConfirmationEvidence{Control: ControlEvidence{Role: ControlRoleBracketed}}}}
	if session(time.Time{}).measureAgain(valid) {
		t.Fatal("a valid window was measured again")
	}
}
