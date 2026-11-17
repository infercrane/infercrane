package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
)

type progressSinkFixture struct {
	mu      sync.Mutex
	records []MeasurementProgress
	err     error
}

func (s *progressSinkFixture) RecordOptimizationMeasurementProgress(_ context.Context, tenant, campaign, candidate, progressJSON string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if tenant != "tenant" || campaign != "campaign" || candidate == "" {
		return false, errors.New("progress written for the wrong candidate")
	}
	var record MeasurementProgress
	if err := json.Unmarshal([]byte(progressJSON), &record); err != nil {
		return false, err
	}
	s.records = append(s.records, record)
	return s.err == nil, s.err
}

func tickingClock(start time.Time, step time.Duration) func() time.Time {
	var mu sync.Mutex
	current := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		current = current.Add(step)
		return current
	}
}

func TestMeasurementProgressRecordsEveryTurnAsItRuns(t *testing.T) {
	fixture := newExecutorFixture(t, optimizer.Request{})
	fixture.runner.throughput[controlEndpoint] = []float64{246, 247, 245, 246}
	fixture.runner.throughput[candidateEndpoint] = []float64{301, 300, 302}
	sink := &progressSinkFixture{}
	fixture.executor.Progress = sink
	fixture.executor.Now = tickingClock(time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC), 30*time.Second)
	fixture.run(t)

	final := sink.records[len(sink.records)-1]
	if final.Method != MeasurementMethod || final.TurnsPlanned != 7 || final.WarmupsPlanned != 2 || final.TurnsDone() != 7 || len(final.Runs) != 9 {
		t.Fatalf("final record = %+v", final)
	}
	if _, running := final.Current(); running {
		t.Fatal("a finished measurement still reports a running invocation")
	}
	wantArms := []string{"control", "candidate", "control", "candidate", "control", "candidate", "control", "candidate", "control"}
	wantTPS := []float64{0, 0, 246, 301, 247, 300, 245, 302, 246}
	for index, run := range final.Runs {
		kind := RunKindTurn
		if index < 2 {
			kind = RunKindWarmup
		}
		if run.Kind != kind || run.Arm != wantArms[index] || run.EndedAt == nil || run.DurationSeconds == nil || *run.DurationSeconds <= 0 || run.Failed == nil {
			t.Fatalf("run %d = %+v", index, run)
		}
		if kind == RunKindTurn && (run.Turn != index-1 || run.OutputTokenThroughput == nil || *run.OutputTokenThroughput != wantTPS[index]) {
			t.Fatalf("turn %d did not record its own measured value: %+v", index-1, run)
		}
	}
	// Every invocation is written when it starts, so a reader sees turn 4 of
	// 7 running after three finished turns.
	seen := false
	for _, record := range sink.records {
		if current, running := record.Current(); running && current.Turn == 4 {
			seen = record.TurnsDone() == 3 && record.TurnsPlanned == 7
		}
	}
	if !seen {
		t.Fatal("no record showed turn 4 running after three finished turns")
	}
}

func TestMeasurementProgressKeepsTheFailedTurnAndPlansTheResumedSegment(t *testing.T) {
	fixture := newExecutorFixture(t, optimizer.Request{})
	fixture.runner.failAt[controlEndpoint] = 2
	sink := &progressSinkFixture{err: errors.New("database unavailable")}
	checkpoints := &checkpointSinkFixture{}
	fixture.executor.Progress, fixture.executor.Checkpoints = sink, checkpoints
	if _, err := fixture.executor.Run(t.Context(), fixture.campaign, fixture.candidate, fixture.profile); err == nil {
		t.Fatal("a failed control turn must stop the measurement")
	}
	stopped := sink.records[len(sink.records)-1]
	// Two warmups, then C X C(failed): three turns ended, the last with an error.
	if stopped.TurnsPlanned != 7 || stopped.WarmupsPlanned != 2 || stopped.TurnsDone() != 3 || len(stopped.Runs) != 5 || stopped.Runs[4].Error == "" || stopped.Runs[4].Arm != ArmControl {
		t.Fatalf("stopped record = %+v", stopped)
	}

	// The driver resolved the interruption; the next segment continues the
	// same record.
	fixture.candidate.MeasurementCheckpointJSON = resolvedCheckpoint(t, checkpoints.last, RecoveryMachineReplaced)
	encoded, _ := json.Marshal(stopped)
	fixture.candidate.MeasurementProgressJSON = string(encoded)
	fixture.runner.failAt = map[string]int{}
	fixture.run(t)
	final := sink.records[len(sink.records)-1]
	// The failed control turn dropped the open pair, so all three pairs are
	// measured again: two more warmups and C X C X C X C.
	if final.TurnsPlanned != 3+7 || final.WarmupsPlanned != 4 || final.TurnsDone() != 10 || len(final.Runs) != 14 || !final.StartedAt.Equal(stopped.StartedAt) {
		t.Fatalf("resumed record = %+v", final)
	}
}

func TestMeasurementProgressPlansTheControlAlone(t *testing.T) {
	fixture := newExecutorFixture(t, optimizer.Request{})
	fixture.candidate = fixture.control
	sink := &progressSinkFixture{}
	fixture.executor.Progress = sink
	fixture.run(t)
	final := sink.records[len(sink.records)-1]
	if final.TurnsPlanned != 3 || final.WarmupsPlanned != 1 || final.TurnsDone() != 3 {
		t.Fatalf("control-alone record = %+v", final)
	}
}

// progressCandidate builds a candidate with state entry times and runs, each
// given in seconds after base.
type progressRun struct {
	kind, arm  string
	start, end float64
}

func progressCandidate(t *testing.T, base time.Time, id, proposalID, state string, entered map[string]float64, planned, warmups int, runs ...progressRun) domain.OptimizationCandidateRun {
	t.Helper()
	at := func(seconds float64) time.Time { return base.Add(time.Duration(seconds * float64(time.Second))) }
	stamps := map[string]string{}
	for name, seconds := range entered {
		stamps[name] = at(seconds).Format(time.RFC3339Nano)
	}
	enteredJSON, _ := json.Marshal(stamps)
	candidate := domain.OptimizationCandidateRun{ID: id, ProposalCandidateID: proposalID, State: state, StateEnteredAtJSON: string(enteredJSON), MeasurementProgressJSON: "{}"}
	if planned > 0 {
		record := MeasurementProgress{Method: MeasurementMethod, StartedAt: at(entered[CandidateMeasuring]), TurnsPlanned: planned, WarmupsPlanned: warmups}
		turn := 0
		for _, run := range runs {
			entry := MeasurementRun{Kind: run.kind, Arm: run.arm, StartedAt: at(run.start)}
			if run.kind == RunKindTurn {
				turn++
				entry.Turn = turn
			}
			if run.end > 0 {
				ended, duration := at(run.end), run.end-run.start
				entry.EndedAt, entry.DurationSeconds = &ended, &duration
			}
			record.Runs = append(record.Runs, entry)
		}
		encoded, _ := json.Marshal(record)
		candidate.MeasurementProgressJSON = string(encoded)
	}
	if state != CandidateProposed && state != CandidateProvisioning {
		candidate.BenchmarkID = map[bool]string{true: "bench-" + id, false: ""}[state != CandidateReady && state != CandidateMeasuring]
	}
	return candidate
}

func progressCampaign(base time.Time, candidates ...domain.OptimizationCandidateRun) domain.OptimizationCampaign {
	proposal, _ := json.Marshal(optimizer.Proposal{StartingPoint: &optimizer.StartingPoint{CandidateID: "proposal-control"}})
	approved := base
	return domain.OptimizationCampaign{ID: "campaign", State: CampaignRanked, ProposalJSON: string(proposal), ApprovedAt: &approved, Candidates: candidates}
}

// The real run on 2026-11-17: the control was ranked, so the aggregate state
// read "ranked" and the console marked measuring and answer checks done while
// setup 2 was measuring and setup 3 waited.
func TestProgressSaysRunningWhileALaterSetupStillMeasures(t *testing.T) {
	base := time.Date(2026, 11, 17, 1, 15, 0, 0, time.UTC)
	control := progressCandidate(t, base, "control", "proposal-control", CandidateRanked,
		map[string]float64{CandidateProposed: 0, CandidateProvisioning: 60, CandidateReady: 960, CandidateMeasuring: 960, CandidateValidating: 1320, CandidateRanked: 1380}, 3, 1,
		progressRun{RunKindWarmup, ArmCandidate, 960, 1020}, progressRun{RunKindTurn, ArmCandidate, 1020, 1140}, progressRun{RunKindTurn, ArmCandidate, 1140, 1260}, progressRun{RunKindTurn, ArmCandidate, 1260, 1380})
	measuring := progressCandidate(t, base, "ngram", "proposal-ngram", CandidateMeasuring,
		map[string]float64{CandidateProposed: 0, CandidateProvisioning: 1380, CandidateReady: 2280, CandidateMeasuring: 2280}, 7, 2,
		progressRun{RunKindWarmup, ArmControl, 2280, 2340}, progressRun{RunKindWarmup, ArmCandidate, 2340, 2400}, progressRun{RunKindTurn, ArmControl, 2400, 2520}, progressRun{RunKindTurn, ArmCandidate, 2520, 0})
	waiting := progressCandidate(t, base, "fp8", "proposal-fp8", CandidateProposed, map[string]float64{CandidateProposed: 0}, 0, 0)
	campaign := progressCampaign(base, control, measuring, waiting)
	now := base.Add(2580 * time.Second)

	progress := Progress(campaign, now)
	if progress.Stage != StageRunning || progress.SetupsMeasured != 1 || progress.SetupsTotal != 3 || progress.TurnsDone != 4 || progress.TurnsTotal == nil || *progress.TurnsTotal != 17 {
		t.Fatalf("progress = %+v", progress)
	}
	if progress.ElapsedSeconds == nil || *progress.ElapsedSeconds != 2580 || progress.EndedAt != nil {
		t.Fatalf("elapsed = %v ended = %v", progress.ElapsedSeconds, progress.EndedAt)
	}
	// Observed: provisioning 900 s and 900 s, warmups 60 s, turns 120 s, one
	// answer check 60 s. Setup 2: 60 s left of its running turn, five turns,
	// its answer check. Setup 3: provisioning, two warmups, seven turns, its
	// answer check.
	want := (60.0 + 5*120 + 60) + (900 + 2*60 + 7*120 + 60)
	if progress.ETA.State != ETAEstimated || progress.ETA.Basis != ETABasis || progress.ETA.RemainingSeconds == nil || math.Abs(*progress.ETA.RemainingSeconds-want) > 1e-6 {
		t.Fatalf("eta = %+v, want %v seconds", progress.ETA, want)
	}
	if progress.ETA.CompletesAt == nil || !progress.ETA.CompletesAt.Equal(now.Add(time.Duration(want)*time.Second)) {
		t.Fatalf("completes at = %v", progress.ETA.CompletesAt)
	}
	if progress.ETA.Observed["turns"] != 4 || progress.ETA.Observed["answer_checks"] != 1 || progress.ETA.Observed["provisioning"] != 2 {
		t.Fatalf("observed = %v", progress.ETA.Observed)
	}
}

func TestProgressLeavesTheETAUnknownUntilEveryStepWasTimed(t *testing.T) {
	base := time.Date(2026, 11, 17, 1, 15, 0, 0, time.UTC)
	provisioning := progressCandidate(t, base, "control", "proposal-control", CandidateProvisioning, map[string]float64{CandidateProposed: 0, CandidateProvisioning: 10}, 0, 0)
	waiting := progressCandidate(t, base, "ngram", "proposal-ngram", CandidateProposed, map[string]float64{CandidateProposed: 0}, 0, 0)
	progress := Progress(progressCampaign(base, provisioning, waiting), base.Add(5*time.Minute))
	if progress.Stage != StageRunning || progress.ETA.State != ETAUnknown || progress.ETA.RemainingSeconds != nil || progress.ETA.CompletesAt != nil || progress.ETA.Reason != "waiting for the first setup to start serving" {
		t.Fatalf("eta before any observation = %+v", progress.ETA)
	}
	if progress.TurnsTotal == nil || *progress.TurnsTotal != 10 || progress.TurnsDone != 0 {
		t.Fatalf("turns before measuring = %v of %v", progress.TurnsDone, progress.TurnsTotal)
	}

	measuring := progressCandidate(t, base, "control", "proposal-control", CandidateMeasuring, map[string]float64{CandidateProposed: 0, CandidateProvisioning: 10, CandidateReady: 600, CandidateMeasuring: 600}, 3, 1,
		progressRun{RunKindWarmup, ArmCandidate, 600, 660}, progressRun{RunKindTurn, ArmCandidate, 660, 780})
	progress = Progress(progressCampaign(base, measuring, waiting), base.Add(800*time.Second))
	if progress.ETA.State != ETAUnknown || progress.ETA.Reason != "waiting for the first answer check to finish" {
		t.Fatalf("eta before any answer check = %+v", progress.ETA)
	}
}

func TestProgressOfADecidedRunStopsTheClockAtTheDecision(t *testing.T) {
	base := time.Date(2026, 11, 17, 1, 15, 0, 0, time.UTC)
	control := progressCandidate(t, base, "control", "proposal-control", CandidateCleaned, map[string]float64{CandidateProposed: 0, CandidateRanked: 1380, CandidateRejected: 5400, CandidateCleaned: 5600}, 0, 0)
	winner := progressCandidate(t, base, "ngram", "proposal-ngram", CandidateQualified, map[string]float64{CandidateProposed: 0, CandidateRanked: 5300, CandidateQualified: 5390}, 0, 0)
	campaign := progressCampaign(base, control, winner)
	campaign.State = CampaignQualified
	progress := Progress(campaign, base.Add(10*time.Hour))
	if progress.Stage != StageDecided || progress.EndedAt == nil || !progress.EndedAt.Equal(base.Add(5400*time.Second)) || progress.ElapsedSeconds == nil || *progress.ElapsedSeconds != 5400 {
		t.Fatalf("decided progress = %+v", progress)
	}
	if progress.ETA.State != ETAUnknown || progress.ETA.RemainingSeconds != nil {
		t.Fatalf("a decided run has no ETA: %+v", progress.ETA)
	}
	// A run decided before decision times were recorded has no elapsed time.
	legacy := progressCampaign(base, progressCandidate(t, base, "control", "proposal-control", CandidateQualified, map[string]float64{CandidateProposed: 0}, 0, 0))
	if progress := Progress(legacy, base.Add(time.Hour)); progress.Stage != StageDecided || progress.EndedAt != nil || progress.ElapsedSeconds != nil {
		t.Fatalf("legacy decided progress = %+v", progress)
	}
	awaiting := progressCampaign(base, progressCandidate(t, base, "control", "proposal-control", CandidateProposed, map[string]float64{CandidateProposed: 0}, 0, 0))
	awaiting.ApprovedAt = nil
	if progress := Progress(awaiting, base.Add(time.Hour)); progress.Stage != StageAwaitingApproval || progress.StartedAt != nil || progress.ElapsedSeconds != nil {
		t.Fatalf("awaiting progress = %+v", progress)
	}
}

func quoteJSON(hourly float64, maxReplicas int) string {
	encoded, _ := json.Marshal(CostQuoteRecord{HourlyUSD: hourly, MaxReplicas: maxReplicas, Source: "vast-offers/2026-11-17", ObservedAt: time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)})
	return string(encoded)
}

func TestEstimateSpendMultipliesTheBoundQuoteByReplicaSeconds(t *testing.T) {
	base := time.Date(2026, 11, 17, 1, 0, 0, 0, time.UTC)
	capUSD := 3.3
	deleted := base.Add(time.Hour)
	campaign := domain.OptimizationCampaign{ApprovedAt: &base, ApprovedMaxCostUSD: &capUSD, Candidates: []domain.OptimizationCandidateRun{
		{ID: "control", CostQuoteJSON: quoteJSON(0.40, 1)},
		{ID: "ngram", CostQuoteJSON: quoteJSON(0.80, 2)},
		{ID: "waiting", CostQuoteJSON: "{}"},
	}}
	usage := map[string]ReplicaUsage{
		"control": {Known: true, Spans: []domain.ReplicaSpan{{CreatedAt: base, DeletedAt: &deleted}}},
		"ngram":   {Known: true, Spans: []domain.ReplicaSpan{{CreatedAt: base.Add(time.Hour)}}},
		"waiting": {Known: true},
	}
	spend := EstimateSpend(campaign, usage, base.Add(90*time.Minute))
	// $0.40/h for one hour, plus $0.80/h over two quoted replicas for the
	// half hour one replica has existed.
	if spend.State != SpendEstimated || spend.Label != "estimated from quoted rate" || spend.Basis != SpendBasis || spend.EstimatedUSD == nil || math.Abs(*spend.EstimatedUSD-0.6) > 1e-9 || *spend.ReplicaSeconds != 5400 || spend.CapUSD == nil || *spend.CapUSD != 3.3 {
		t.Fatalf("spend = %+v", spend)
	}

	unquoted := campaign
	unquoted.Candidates = append([]domain.OptimizationCandidateRun(nil), campaign.Candidates...)
	unquoted.Candidates[1].CostQuoteJSON = "{}"
	if spend := EstimateSpend(unquoted, usage, base.Add(90*time.Minute)); spend.State != SpendUnknown || spend.EstimatedUSD != nil || !strings.Contains(spend.Reason, "without a recorded quoted rate") {
		t.Fatalf("a setup with servers and no quote was not unknown: %+v", spend)
	}
	unattributed := map[string]ReplicaUsage{"control": usage["control"], "ngram": {}, "waiting": usage["waiting"]}
	if spend := EstimateSpend(campaign, unattributed, base.Add(90*time.Minute)); spend.State != SpendUnknown || spend.EstimatedUSD != nil {
		t.Fatalf("unattributed servers were not unknown: %+v", spend)
	}
	notStarted := campaign
	notStarted.ApprovedAt = nil
	if spend := EstimateSpend(notStarted, usage, base); spend.State != SpendUnknown || spend.Reason != "the run has not started" {
		t.Fatalf("spend before approval = %+v", spend)
	}
}

func TestCapacityTargetAttributesServersOnlyWhenItCan(t *testing.T) {
	draft := optimizer.DeploymentDraft{Name: "qwen-rtx-optimized"}
	draft.Model.ID = "Qwen/Qwen3-0.6B"
	spec, _ := json.Marshal(draft)
	provisioning := domain.OptimizationCandidateRun{ID: "26d5f7270000", State: CandidateProvisioning, DeploymentSpecJSON: string(spec)}
	newEndpoint := domain.OptimizationCampaign{Intent: IntentNewEndpoint}
	if name, revision, ok := CapacityTarget(newEndpoint, provisioning); !ok || name != "qwen-rtx-optimized-opt-26d5f727" || revision != "" {
		t.Fatalf("new-endpoint provisioning target = %q %q %v", name, revision, ok)
	}
	evolve := domain.OptimizationCampaign{Intent: IntentEvolveEndpoint}
	if _, _, ok := CapacityTarget(evolve, provisioning); ok {
		t.Fatal("an evolution candidate's servers were attributed before its revision was known")
	}
	provisioning.RevisionID = "revision-1"
	if name, revision, ok := CapacityTarget(evolve, provisioning); !ok || name != "" || revision != "revision-1" {
		t.Fatalf("revision target = %q %q %v", name, revision, ok)
	}
	proposed := domain.OptimizationCandidateRun{State: CandidateProposed}
	if name, revision, ok := CapacityTarget(evolve, proposed); !ok || name != "" || revision != "" {
		t.Fatal("a proposed evolution candidate has no servers yet and must count as known")
	}
}
