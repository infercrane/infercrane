package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
)

type checkpointSinkFixture struct {
	mu     sync.Mutex
	writes int
	last   string
	err    error
}

func (s *checkpointSinkFixture) RecordOptimizationMeasurementCheckpoint(_ context.Context, tenant, campaign, candidate, checkpointJSON string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tenant != "tenant" || campaign != "campaign" || candidate == "" {
		return false, errors.New("checkpoint written for the wrong candidate")
	}
	if s.err != nil {
		return false, s.err
	}
	s.writes++
	s.last = checkpointJSON
	return true, nil
}

// resolvedCheckpoint marks the stored checkpoint's pending interruption as
// recovered, as the driver does before the next segment.
func resolvedCheckpoint(t *testing.T, raw, recovery string) string {
	t.Helper()
	checkpoint, ok := DecodeMeasurementCheckpoint(raw)
	if !ok || checkpoint.Pending() == nil {
		t.Fatalf("no pending interruption in %s", raw)
	}
	pending := checkpoint.Pending()
	resolved := pending.At
	pending.Recovery, pending.ResolvedAt = recovery, &resolved
	encoded, _ := json.Marshal(checkpoint)
	return string(encoded)
}

// sequencedAIPerf answers each endpoint's invocations, warmups included, from
// an ordered script. A zero throughput step fails.
type sequencedAIPerf struct {
	mu      sync.Mutex
	steps   map[string][]float64
	configs []benchmark.Config
}

func (r *sequencedAIPerf) Run(_ context.Context, config benchmark.Config) (benchmark.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configs = append(r.configs, config)
	steps := r.steps[config.Endpoint]
	if len(steps) == 0 {
		return benchmark.Result{}, errors.New("unscripted invocation")
	}
	throughput := steps[0]
	r.steps[config.Endpoint] = steps[1:]
	if throughput == 0 {
		return benchmark.Result{}, errors.New("endpoint stopped answering")
	}
	ttft, tpot := 200.0, 20.0
	return benchmark.Result{Tool: "aiperf", ToolVersion: benchmark.AIPerfVersion, Command: "aiperf profile", Requests: config.Requests, Succeeded: config.Requests, DurationSeconds: 10, OutputTokenThroughput: &throughput, TTFTP95MS: &ttft, TPOTP95MS: &tpot, ProfileRuns: 1}, nil
}

func sequencedFixture(t *testing.T, steps map[string][]float64) (executorFixture, *sequencedAIPerf, *checkpointSinkFixture) {
	t.Helper()
	fixture := newExecutorFixture(t, optimizer.Request{})
	runner := &sequencedAIPerf{steps: steps}
	sink := &checkpointSinkFixture{}
	fixture.executor.Runner, fixture.executor.Checkpoints = runner, sink
	return fixture, runner, sink
}

func interruptedRun(t *testing.T, fixture executorFixture) *MeasurementInterruptedError {
	t.Helper()
	_, err := fixture.executor.Run(t.Context(), fixture.campaign, fixture.candidate, fixture.profile)
	var interrupted *MeasurementInterruptedError
	if !errors.As(err, &interrupted) {
		t.Fatalf("err=%v, want an interruption", err)
	}
	return interrupted
}

func TestLostCandidateMachineKeepsCompletedPairsAndResumesAtTheNextPair(t *testing.T) {
	const warm = 1.0
	fixture, runner, sink := sequencedFixture(t, map[string][]float64{
		// Segment one: warmup, C1, C2 | segment two: warmup, C3, C4, C5.
		controlEndpoint: {warm, 100, 101, warm, 100, 101, 100},
		// Segment one: warmup, X1, X2 fails | segment two: warmup, X2, X3.
		candidateEndpoint: {warm, 130, 0, warm, 131, 129},
	})
	interrupted := interruptedRun(t, fixture)
	if !interrupted.Persisted || interrupted.Interruption.Arm != ArmCandidate || interrupted.Interruption.OwnerCandidateID != fixture.candidate.ID || interrupted.Interruption.ReplicaID != "replica-candidate" || interrupted.Interruption.DiscardedRuns != 0 || interrupted.Interruption.Sequence != 1 {
		t.Fatalf("interruption = %+v", interrupted.Interruption)
	}
	stopped, _ := DecodeMeasurementCheckpoint(sink.last)
	if len(stopped.ControlRuns) != 2 || len(stopped.CandidateRuns) != 1 || stopped.Segments[0].Ended != SegmentInterrupted || stopped.Pending() == nil {
		t.Fatalf("completed pair was not kept: %+v", stopped)
	}
	// Nothing measures while the interruption is unresolved.
	fixture.candidate.MeasurementCheckpointJSON = sink.last
	if _, err := fixture.executor.Run(t.Context(), fixture.campaign, fixture.candidate, fixture.profile); !errors.Is(err, errCheckpointPending) {
		t.Fatalf("measured past a pending interruption: %v", err)
	}

	fixture.candidate.MeasurementCheckpointJSON = resolvedCheckpoint(t, sink.last, RecoveryMachineReplaced)
	row, workload := fixture.run(t)
	evidence := measuredEvidence(t, workload)
	if evidence.Control.Role != ControlRoleBracketed || evidence.Control.Runs != 5 || len(evidence.Segments) != 2 || len(evidence.Interruptions) != 1 || evidence.Interruptions[0].Recovery != RecoveryMachineReplaced {
		t.Fatalf("resumed evidence = %+v", evidence)
	}
	if row.RequestCount != 3*fixture.profile.Requests || row.OutputTokenThroughput == nil || *row.OutputTokenThroughput != 130 {
		t.Fatalf("candidate arm must pool exactly its three accepted runs: %+v", row)
	}
	if comparison := evidence.Comparison["output_token_throughput"]; comparison.Decision != DecisionImproved {
		t.Fatalf("comparison across the restart = %+v", evidence.Comparison)
	}
	var arms []string
	for _, run := range evidence.Runs {
		arms = append(arms, run.Arm)
	}
	if got := strings.Join(arms, ","); got != "control,candidate,control,control,candidate,control,candidate,control" {
		t.Fatalf("accepted runs must be listed as they ran: %s", got)
	}
	order := strings.Join(evidence.Order, ",")
	if order != "warmup:control,warmup:candidate,control,candidate,control,interrupted:candidate,resume,warmup:control,warmup:candidate,control,candidate,control,candidate,control" {
		t.Fatalf("order = %s", order)
	}
	// Every invocation got its own seed, and the resumed segment starts past
	// a gap so a lost checkpoint write can never repeat one.
	seeds := map[int64]struct{}{}
	for _, config := range runner.configs {
		seeds[config.RandomSeed] = struct{}{}
	}
	if len(seeds) != len(runner.configs) || evidence.Segments[1].FirstInvocation-evidence.Segments[0].FirstInvocation < resumeInvocationGap {
		t.Fatalf("seeds=%d invocations=%d segments=%+v", len(seeds), len(runner.configs), evidence.Segments)
	}
}

func TestReplacedControlThatServesDifferentlyInvalidatesTheComparison(t *testing.T) {
	const warm = 1.0
	fixture, _, sink := sequencedFixture(t, map[string][]float64{
		// The replacement control runs 10% slower than the lost one.
		controlEndpoint:   {warm, 100, 101, 0, warm, 90, 91, 90},
		candidateEndpoint: {warm, 130, 131, warm, 129, 130},
	})
	interrupted := interruptedRun(t, fixture)
	// C1 X1 C2 X2 then the closing control failed: X2 had no closing bracket.
	if interrupted.Interruption.Arm != ArmControl || interrupted.Interruption.OwnerCandidateID != fixture.control.ID || interrupted.Interruption.ReplicaID != "replica-control" || interrupted.Interruption.DiscardedRuns != 1 {
		t.Fatalf("interruption = %+v", interrupted.Interruption)
	}
	fixture.candidate.MeasurementCheckpointJSON = resolvedCheckpoint(t, sink.last, RecoveryMachineReplaced)
	_, workload := fixture.run(t)
	evidence := measuredEvidence(t, workload)
	if evidence.Control.Role != ControlRoleInvalid || evidence.Comparison != nil || evidence.Control.MaxDrift < 0.09 || !strings.Contains(evidence.Control.Reason, "drifted") {
		t.Fatalf("drift across the restart must invalidate the control: %+v", evidence.Control)
	}
}

func TestOpeningControlIsDroppedWhenItsFirstPairNeverFinished(t *testing.T) {
	const warm = 1.0
	fixture, _, sink := sequencedFixture(t, map[string][]float64{
		controlEndpoint:   {warm, 100},
		candidateEndpoint: {warm, 0},
	})
	interrupted := interruptedRun(t, fixture)
	stopped, _ := DecodeMeasurementCheckpoint(sink.last)
	if interrupted.Interruption.DiscardedRuns != 1 || len(stopped.ControlRuns) != 0 || len(stopped.CandidateRuns) != 0 {
		t.Fatalf("an opening control without its pair must be dropped: %+v %+v", interrupted.Interruption, stopped)
	}
}

func TestCandidateMeasuredAloneResumesWithoutAControl(t *testing.T) {
	const warm = 1.0
	fixture, _, sink := sequencedFixture(t, map[string][]float64{candidateEndpoint: {warm, 130, 0, warm, 131, 129}})
	fixture.campaign.Candidates[0].State = CandidateCleaned
	interrupted := interruptedRun(t, fixture)
	if interrupted.Interruption.Arm != ArmCandidate || interrupted.Interruption.DiscardedRuns != 0 {
		t.Fatalf("interruption = %+v", interrupted.Interruption)
	}
	fixture.candidate.MeasurementCheckpointJSON = resolvedCheckpoint(t, sink.last, RecoveryMachineServing)
	row, workload := fixture.run(t)
	if evidence := measuredEvidence(t, workload); evidence.Control.Role != ControlRoleUnavailable || len(evidence.Segments) != 2 || row.RequestCount != 3*fixture.profile.Requests {
		t.Fatalf("evidence=%+v row=%+v", evidence, row)
	}
}

func TestCheckpointFromOtherArmsOrLoadIsNeverResumed(t *testing.T) {
	const warm = 1.0
	fixture, _, sink := sequencedFixture(t, map[string][]float64{
		controlEndpoint:   {warm, 100, 101, warm, 100, 101, 100, 101},
		candidateEndpoint: {warm, 130, 0, warm, 130, 131, 129},
	})
	interruptedRun(t, fixture)
	checkpoint, _ := DecodeMeasurementCheckpoint(resolvedCheckpoint(t, sink.last, RecoveryMachineReplaced))
	checkpoint.Identity = "sha256:another-revision"
	encoded, _ := json.Marshal(checkpoint)
	fixture.candidate.MeasurementCheckpointJSON = string(encoded)
	_, workload := fixture.run(t)
	if evidence := measuredEvidence(t, workload); len(evidence.Segments) != 0 || evidence.Control.Runs != 4 {
		t.Fatalf("a foreign checkpoint was resumed: %+v", evidence)
	}
}

func TestRecoveredControlStillStartingIsWaitedFor(t *testing.T) {
	const warm = 1.0
	fixture, _, sink := sequencedFixture(t, map[string][]float64{
		controlEndpoint:   {warm, 100, 0},
		candidateEndpoint: {warm, 130},
	})
	interruptedRun(t, fixture)
	fixture.candidate.MeasurementCheckpointJSON = resolvedCheckpoint(t, sink.last, RecoveryMachineReplaced)
	fixture.store.replicas[1].Health = "starting"
	_, err := fixture.executor.Run(t.Context(), fixture.campaign, fixture.candidate, fixture.profile)
	var failure operations.Failure
	if !errors.As(err, &failure) || !failure.Retryable || failure.Code != "optimization_control_not_serving" {
		t.Fatalf("err=%v, want to wait for the recovered control", err)
	}
}

func TestLostCheckpointWriteStillReportsTheInterruption(t *testing.T) {
	const warm = 1.0
	fixture, _, sink := sequencedFixture(t, map[string][]float64{controlEndpoint: {warm, 100}, candidateEndpoint: {warm, 0}})
	sink.err = errors.New("database unavailable")
	if interrupted := interruptedRun(t, fixture); interrupted.Persisted {
		t.Fatal("an unstored checkpoint must be reported as not persisted")
	}
}

func TestFullyMeasuredCheckpointIsAnalyzedWithoutMeasuringAgain(t *testing.T) {
	const warm = 1.0
	fixture, runner, sink := sequencedFixture(t, map[string][]float64{
		controlEndpoint:   {warm, 100, 101, 100, 101},
		candidateEndpoint: {warm, 130, 131, 129},
	})
	_, first := fixture.run(t)
	invocations := len(runner.configs)
	// A later step of the same attempt failed; the retry finds every pair.
	fixture.candidate.MeasurementCheckpointJSON = sink.last
	_, again := fixture.run(t)
	if len(runner.configs) != invocations {
		t.Fatalf("a complete measurement was run again: %d invocations after %d", len(runner.configs), invocations)
	}
	if measuredEvidence(t, first).Comparison["output_token_throughput"] != measuredEvidence(t, again).Comparison["output_token_throughput"] {
		t.Fatal("the stored runs must give the same comparison")
	}
}
