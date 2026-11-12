package optimizationcampaign

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/lab"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/performanceprofile"
	"github.com/infercrane/infercrane/internal/replay"
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

const (
	candidateEndpoint = "https://candidate.internal"
	controlEndpoint   = "https://control.internal"
)

// scriptedAIPerf answers each AIPerf invocation from a per-endpoint script.
// The first call to an endpoint is its warmup; later calls consume the
// scripted measured values in order. Trace files are read during the call
// because the executor deletes its staging directory afterwards.
type scriptedAIPerf struct {
	configs    []benchmark.Config
	traces     [][][]int
	throughput map[string][]float64
	ttft       map[string][]float64
	failAt     map[string]int
	calls      map[string]int
}

func (r *scriptedAIPerf) Run(_ context.Context, config benchmark.Config) (benchmark.Result, error) {
	r.configs = append(r.configs, config)
	var hashes [][]int
	if config.InputFile != "" {
		file, err := os.Open(config.InputFile)
		if err != nil {
			return benchmark.Result{}, err
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var row struct {
				HashIDs []int `json:"hash_ids"`
			}
			if err = json.Unmarshal(scanner.Bytes(), &row); err != nil {
				file.Close()
				return benchmark.Result{}, err
			}
			hashes = append(hashes, row.HashIDs)
		}
		file.Close()
	}
	r.traces = append(r.traces, hashes)
	if r.calls == nil {
		r.calls = map[string]int{}
	}
	call := r.calls[config.Endpoint]
	r.calls[config.Endpoint]++
	if call > 0 && r.failAt[config.Endpoint] == call {
		return benchmark.Result{}, errors.New("endpoint stopped answering")
	}
	throughput, ttft := 100.0, 200.0
	if call > 0 {
		if values := r.throughput[config.Endpoint]; len(values) >= call {
			throughput = values[call-1]
		}
		if values := r.ttft[config.Endpoint]; len(values) >= call {
			ttft = values[call-1]
		}
	}
	tpot := 20.0
	return benchmark.Result{Tool: "aiperf", ToolVersion: benchmark.AIPerfVersion, Command: "aiperf profile --input-file " + config.InputFile, Requests: config.Requests, Succeeded: config.Requests, DurationSeconds: 10, OutputTokenThroughput: &throughput, TTFTP95MS: &ttft, TPOTP95MS: &tpot, ProfileRuns: 1}, nil
}

type replaySourceFixture struct {
	trace domain.ReplayTrace
	err   error
}

func (f replaySourceFixture) ReplayTraceByDigest(_ context.Context, _ string, digest string) (domain.ReplayTrace, error) {
	if f.err != nil {
		return domain.ReplayTrace{}, f.err
	}
	if digest != f.trace.ShapeDigest {
		return domain.ReplayTrace{}, domain.ErrNotFound
	}
	return f.trace, nil
}

type executorFixture struct {
	executor  BenchmarkExecutor
	store     *benchmarkStoreFixture
	runner    *scriptedAIPerf
	campaign  domain.OptimizationCampaign
	candidate domain.OptimizationCandidateRun
	control   domain.OptimizationCandidateRun
	profile   performanceprofile.Profile
}

func measurementDraft(name string, metrics bool) string {
	draft := optimizer.DeploymentDraft{APIVersion: "infercrane.dev/v1", Kind: "Deployment", Name: name}
	draft.Model.ID, draft.Model.Revision = "Qwen/Qwen3-8B", strings.Repeat("a", 40)
	draft.Runtime.Engine, draft.Runtime.Version = "custom-oci", "1"
	if metrics {
		draft.Runtime.Workload = runtimecontract.Workload{MetricsPath: "/metrics"}
	}
	draft.Compute.Mode, draft.Provider.Cloud, draft.Provider.Region = "elastic", "aws", "eu-central-1"
	draft.Resources.GPU, draft.Resources.GPUCount = "H100", 1
	encoded, _ := json.Marshal(draft)
	return string(encoded)
}

func newExecutorFixture(t *testing.T, request optimizer.Request) executorFixture {
	t.Helper()
	now := time.Now().UTC()
	proposal, _ := json.Marshal(optimizer.Proposal{Input: request, StartingPoint: &optimizer.StartingPoint{CandidateID: "proposal-control"}})
	control := domain.OptimizationCandidateRun{ID: "control", TenantID: "tenant", CampaignID: "campaign", ProposalCandidateID: "proposal-control", State: CandidateValidating, DeploymentName: "control-opt", RevisionID: "revision-control", DeploymentSpecJSON: measurementDraft("control", false)}
	candidate := domain.OptimizationCandidateRun{ID: "candidate", TenantID: "tenant", CampaignID: "campaign", ProposalCandidateID: "proposal-candidate", State: CandidateMeasuring, DeploymentName: "candidate-opt", RevisionID: "revision-candidate", DeploymentSpecJSON: measurementDraft("candidate", true)}
	campaign := domain.OptimizationCampaign{ID: "campaign", TenantID: "tenant", ProposalJSON: string(proposal), Candidates: []domain.OptimizationCandidateRun{control, candidate}}
	store := &benchmarkStoreFixture{
		resolved:  domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment"}},
		revisions: []domain.DeploymentRevision{{ID: candidate.RevisionID}, {ID: control.RevisionID}},
		replicas: []domain.Replica{
			{RevisionID: candidate.RevisionID, Endpoint: candidateEndpoint, Provider: "aws-ec2", Health: "healthy", LifecycleState: "ready"},
			{RevisionID: control.RevisionID, Endpoint: controlEndpoint, Provider: "aws-ec2", Health: "healthy", LifecycleState: "ready"},
		},
		artifact: domain.ModelArtifact{ID: "artifact", Repository: "Qwen/Qwen3-8B", ModelIdentity: "Qwen/Qwen3-8B@" + strings.Repeat("a", 40)},
	}
	runner := &scriptedAIPerf{throughput: map[string][]float64{}, ttft: map[string][]float64{}, failAt: map[string]int{}}
	executor := BenchmarkExecutor{Store: store, Runner: runner, Costs: freshCost(now), Backends: map[string]BenchmarkBackend{"aws-ec2": {APIKey: "worker-secret"}}, AIPerfBinary: "aiperf", Now: func() time.Time { return now }}
	profile, err := performanceprofile.Get("throughput")
	if err != nil {
		t.Fatal(err)
	}
	return executorFixture{executor: executor, store: store, runner: runner, campaign: campaign, candidate: candidate, control: control, profile: profile}
}

func (f executorFixture) run(t *testing.T) (domain.BenchmarkResult, map[string]any) {
	t.Helper()
	row, err := f.executor.Run(t.Context(), f.campaign, f.candidate, f.profile)
	if err != nil {
		t.Fatal(err)
	}
	var workload map[string]any
	if err = json.Unmarshal([]byte(row.WorkloadJSON), &workload); err != nil {
		t.Fatal(err)
	}
	return row, workload
}

func measuredEvidence(t *testing.T, workload map[string]any) MeasuredEvidence {
	t.Helper()
	encoded, _ := json.Marshal(workload["measured"])
	var evidence MeasuredEvidence
	if err := json.Unmarshal(encoded, &evidence); err != nil {
		t.Fatal(err)
	}
	return evidence
}

func TestBenchmarkExecutorInterleavesControlWithWarmupRepeatsAndUniqueSeeds(t *testing.T) {
	fixture := newExecutorFixture(t, optimizer.Request{})
	fixture.runner.throughput[controlEndpoint] = []float64{100, 101, 100, 101}
	fixture.runner.throughput[candidateEndpoint] = []float64{130, 131, 129}
	fixture.runner.ttft[controlEndpoint] = []float64{200, 202, 201, 200}
	fixture.runner.ttft[candidateEndpoint] = []float64{150, 151, 149}
	row, workload := fixture.run(t)

	var endpoints []string
	seeds := map[int64]struct{}{}
	for index, config := range fixture.runner.configs {
		endpoints = append(endpoints, strings.TrimPrefix(config.Endpoint, "https://"))
		seeds[config.RandomSeed] = struct{}{}
		wantRequests := fixture.profile.Requests
		if index < 2 {
			wantRequests = warmupRequests(fixture.profile.Concurrency)
		}
		if config.Requests != wantRequests || config.Concurrency != fixture.profile.Concurrency || config.InputTokens != fixture.profile.InputTokens || config.InputFile != "" {
			t.Fatalf("invocation %d load=%+v", index, config)
		}
		if wantMetrics := config.Endpoint == candidateEndpoint; config.ServerMetrics != wantMetrics {
			t.Fatalf("server metrics must follow each runtime contract: %+v", config)
		}
	}
	want := "control.internal,candidate.internal,control.internal,candidate.internal,control.internal,candidate.internal,control.internal,candidate.internal,control.internal"
	if strings.Join(endpoints, ",") != want || len(seeds) != len(fixture.runner.configs) {
		t.Fatalf("order=%v seeds=%v", endpoints, seeds)
	}
	if row.RequestCount != 3*fixture.profile.Requests || row.OutputTokenThroughput == nil || *row.OutputTokenThroughput != 130 || row.TTFTP95MS == nil || *row.TTFTP95MS != 150 || row.RevisionID != fixture.candidate.RevisionID {
		t.Fatalf("candidate arm was not pooled from its own runs only: %+v", row)
	}
	if workload["measurement_method"] != MeasurementMethod || workload["profile_runs"] != float64(3) || workload["seed_policy"] != "unique_per_invocation" || workload["workload_source"] != "synthetic_profile" || workload["input_tokens"] != float64(fixture.profile.InputTokens) {
		t.Fatalf("workload identity=%v", workload)
	}
	confidence := workload["confidence_95"].(map[string]any)["output_token_throughput"].(map[string]any)
	if confidence["samples"] != float64(3) || confidence["mean"] != float64(130) {
		t.Fatalf("candidate confidence=%v", confidence)
	}
	evidence := measuredEvidence(t, workload)
	if evidence.Control.Role != ControlRoleBracketed || evidence.Control.Runs != 4 || evidence.Control.CandidateID != "control" || evidence.Control.MaxDrift > maxControlDrift || evidence.Control.ServerMetrics || !evidence.ServerMetricsRequested || evidence.GPUTelemetry != "not_collected" {
		t.Fatalf("control evidence=%+v", evidence)
	}
	if throughput := evidence.Comparison["output_token_throughput"]; throughput.Decision != DecisionImproved || throughput.ControlMean != 100.5 || throughput.Delta <= throughput.NoiseFloor {
		t.Fatalf("throughput comparison=%+v", throughput)
	}
	if ttft := evidence.Comparison["ttft_p95_ms"]; ttft.Decision != DecisionImproved || ttft.Delta >= 0 {
		t.Fatalf("latency comparison=%+v", ttft)
	}
	if len(evidence.Runs) != 7 || strings.Join(evidence.Order[:2], ",") != "warmup:control,warmup:candidate" {
		t.Fatalf("run audit=%+v order=%v", evidence.Runs, evidence.Order)
	}
}

func TestBenchmarkExecutorReportsGainInsideControlSpreadAsNoMeasurableDifference(t *testing.T) {
	fixture := newExecutorFixture(t, optimizer.Request{})
	fixture.runner.throughput[controlEndpoint] = []float64{100, 103, 98.5, 101}
	fixture.runner.throughput[candidateEndpoint] = []float64{102, 101.5, 102.5}
	_, workload := fixture.run(t)
	evidence := measuredEvidence(t, workload)
	comparison := evidence.Comparison["output_token_throughput"]
	if evidence.Control.Role != ControlRoleBracketed || comparison.Decision != DecisionNoMeasurableDifference || comparison.Delta <= 0 || comparison.Delta > comparison.ControlSpread {
		t.Fatalf("a gain inside the control-to-control spread was not reported as no measurable difference: %+v", comparison)
	}
}

func TestBenchmarkExecutorInvalidatesComparisonWhenControlDriftsOrFails(t *testing.T) {
	drifting := newExecutorFixture(t, optimizer.Request{})
	drifting.runner.throughput[controlEndpoint] = []float64{100, 110, 100, 110}
	drifting.runner.throughput[candidateEndpoint] = []float64{150, 150, 150}
	_, workload := drifting.run(t)
	evidence := measuredEvidence(t, workload)
	if evidence.Control.Role != ControlRoleInvalid || evidence.Comparison != nil || evidence.Control.MaxDrift < 0.09 || !strings.Contains(evidence.Control.Reason, "drifted") {
		t.Fatalf("drifting control was used for a comparison: %+v", evidence)
	}

	failing := newExecutorFixture(t, optimizer.Request{})
	failing.runner.failAt[controlEndpoint] = 2
	row, workload := failing.run(t)
	evidence = measuredEvidence(t, workload)
	if evidence.Control.Role != ControlRoleInvalid || evidence.Comparison != nil || !strings.Contains(evidence.Control.Reason, "control run failed") || failing.runner.calls[candidateEndpoint] != 4 || row.RequestCount != 3*failing.profile.Requests {
		t.Fatalf("a failed control must invalidate the comparison but keep the candidate measurement: evidence=%+v calls=%v", evidence, failing.runner.calls)
	}
}

func TestBenchmarkExecutorMeasuresControlAloneOrWithoutAServingControl(t *testing.T) {
	self := newExecutorFixture(t, optimizer.Request{})
	self.candidate = self.control
	_, workload := self.run(t)
	if evidence := measuredEvidence(t, workload); evidence.Control.Role != ControlRoleCandidate || self.runner.calls[controlEndpoint] != 4 || self.runner.calls[candidateEndpoint] != 0 {
		t.Fatalf("the control must be measured alone with warmup and three runs: %+v calls=%v", evidence, self.runner.calls)
	}

	cleaned := newExecutorFixture(t, optimizer.Request{})
	cleaned.campaign.Candidates[0].State = CandidateCleaned
	_, workload = cleaned.run(t)
	if evidence := measuredEvidence(t, workload); evidence.Control.Role != ControlRoleUnavailable || !strings.Contains(evidence.Control.Reason, CandidateCleaned) || cleaned.runner.calls[controlEndpoint] != 0 {
		t.Fatalf("a cleaned control was measured: %+v", evidence)
	}
}

func boundReplayTrace(t *testing.T) domain.ReplayTrace {
	t.Helper()
	start := time.Unix(2000, 0).UTC()
	length := func(value int) *int { return &value }
	trace, err := replay.Build("production", "prod", "revision", start, start.Add(time.Minute), []replay.Observation{
		{StartedAt: start.Add(time.Second), DurationMS: 800, InputTokens: length(1200), OutputTokens: length(64), Operation: "chat", Streaming: true, SessionIDHash: "session", SharedPrefixHash: "system"},
		{StartedAt: start.Add(2 * time.Second), DurationMS: 800, InputTokens: length(1800), OutputTokens: length(64), Operation: "chat", Streaming: true, SessionIDHash: "session", SharedPrefixHash: "system"},
		{StartedAt: start.Add(3 * time.Second), DurationMS: 800, InputTokens: length(900), OutputTokens: length(32), Operation: "chat", Streaming: true, SharedPrefixHash: "system"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return trace
}

func TestBenchmarkExecutorReplaysBoundTraceWithFreshPrefixNamespacePerInvocation(t *testing.T) {
	trace := boundReplayTrace(t)
	fixture := newExecutorFixture(t, optimizer.Request{WorkloadSource: optimizer.WorkloadSourceCustomerObserved, WorkloadFingerprint: "sha256:" + trace.ShapeDigest})
	fixture.executor.Replays = replaySourceFixture{trace: trace}
	row, workload := fixture.run(t)
	if len(fixture.runner.configs) != 9 {
		t.Fatalf("invocations=%d", len(fixture.runner.configs))
	}
	seen := map[int]int{}
	for index, config := range fixture.runner.configs {
		if config.DatasetType != replay.AIPerfDatasetType || config.InputFile == "" || config.InputTokens != 0 || config.OutputTokens != 0 {
			t.Fatalf("invocation %d did not replay the bound trace: %+v", index, config)
		}
		if index >= 2 && config.Requests != 3 {
			t.Fatalf("a measured run must send exactly one pass over the trace: %+v", config)
		}
		rows := fixture.runner.traces[index]
		if len(rows) != 3 || rows[0][0] != rows[1][0] || rows[1][1] != rows[0][1] || rows[2][0] != rows[0][0] {
			t.Fatalf("invocation %d lost the trace's prefix and session structure: %v", index, rows)
		}
		for _, hashes := range rows {
			for _, id := range hashes {
				if previous, reused := seen[id]; reused && previous != index {
					t.Fatalf("invocations %d and %d shared prefix block %d", previous, index, id)
				}
				seen[id] = index
			}
		}
	}
	if _, err := os.Stat(filepath.Dir(fixture.runner.configs[0].InputFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replay staging directory was left behind: %v", err)
	}
	if strings.Contains(row.ReproductionCommand, filepath.Dir(fixture.runner.configs[0].InputFile)) || !strings.Contains(row.ReproductionCommand, "./infercrane-replay/") {
		t.Fatalf("reproduction command kept a deleted temporary path: %s", row.ReproductionCommand)
	}
	replayIdentity, _ := workload["replay"].(map[string]any)
	if workload["workload_source"] != "customer_replay" || replayIdentity["trace_digest"] != "sha256:"+trace.ShapeDigest || replayIdentity["dataset_type"] != replay.AIPerfDatasetType || workload["request_count"] != float64(3) {
		t.Fatalf("replay identity=%v", workload)
	}
	if _, synthetic := workload["input_tokens"]; synthetic {
		t.Fatalf("replay evidence claimed synthetic token lengths: %v", workload)
	}
	// Two replays of the same bound trace are one workload for Lab.
	second := newExecutorFixture(t, optimizer.Request{WorkloadSource: optimizer.WorkloadSourceCustomerObserved, WorkloadFingerprint: trace.ShapeDigest})
	second.executor.Replays = replaySourceFixture{trace: trace}
	second.runner.throughput[candidateEndpoint] = []float64{140, 141, 139}
	other, _ := second.run(t)
	if lab.WorkloadDigest(row.WorkloadJSON) != lab.WorkloadDigest(other.WorkloadJSON) {
		t.Fatal("two measurements of one bound trace produced different workload digests")
	}
}

func TestBenchmarkExecutorFailsClosedWhenBoundReplayCannotBeReplayed(t *testing.T) {
	trace := boundReplayTrace(t)
	tampered := trace
	tampered.ShapeJSON = strings.Replace(tampered.ShapeJSON, `"input_tokens":1200`, `"input_tokens":1300`, 1)
	for name, test := range map[string]struct {
		fingerprint string
		source      ReplayTraceSource
		code        string
	}{
		"not-a-replay-digest": {fingerprint: "customer-dashboard-export", source: replaySourceFixture{trace: trace}, code: "optimization_replay_unavailable"},
		"no-source":           {fingerprint: trace.ShapeDigest, code: "optimization_replay_unavailable"},
		"missing":             {fingerprint: strings.Repeat("f", 64), source: replaySourceFixture{trace: trace}, code: "optimization_replay_unavailable"},
		"tampered":            {fingerprint: trace.ShapeDigest, source: replaySourceFixture{trace: tampered}, code: "optimization_replay_invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newExecutorFixture(t, optimizer.Request{WorkloadSource: optimizer.WorkloadSourceCustomerObserved, WorkloadFingerprint: test.fingerprint})
			fixture.executor.Replays = test.source
			_, err := fixture.executor.Run(t.Context(), fixture.campaign, fixture.candidate, fixture.profile)
			var failure operations.Failure
			if !errors.As(err, &failure) || failure.Retryable || failure.Code != test.code || len(fixture.runner.configs) != 0 || fixture.store.recorded.ID != "" {
				t.Fatalf("err=%v configs=%d", err, len(fixture.runner.configs))
			}
		})
	}
	transient := newExecutorFixture(t, optimizer.Request{WorkloadSource: optimizer.WorkloadSourceCustomerObserved, WorkloadFingerprint: trace.ShapeDigest})
	transient.executor.Replays = replaySourceFixture{err: errors.New("database unavailable")}
	_, err := transient.executor.Run(t.Context(), transient.campaign, transient.candidate, transient.profile)
	var failure operations.Failure
	if err == nil || errors.As(err, &failure) {
		t.Fatalf("a transient replay lookup failure must stay retryable: %v", err)
	}
}

// The control server is measured inside every other candidate's measurement,
// and a failed measurement is retried against the same servers. No two
// measurements may send a server the same seed or prompt blocks.
func TestBenchmarkExecutorNeverRepeatsSeedsOrPromptBlocksAcrossMeasurements(t *testing.T) {
	trace := boundReplayTrace(t)
	fixture := newExecutorFixture(t, optimizer.Request{WorkloadSource: optimizer.WorkloadSourceCustomerObserved, WorkloadFingerprint: trace.ShapeDigest})
	fixture.executor.Replays = replaySourceFixture{trace: trace}
	other := fixture.candidate
	other.ID, other.ProposalCandidateID = "candidate-2", "proposal-candidate-2"
	fixture.campaign.Candidates = append(fixture.campaign.Candidates, other)
	for _, candidate := range []domain.OptimizationCandidateRun{fixture.control, fixture.candidate, other, fixture.candidate} {
		if _, err := fixture.executor.Run(t.Context(), fixture.campaign, candidate, fixture.profile); err != nil {
			t.Fatal(err)
		}
	}
	seeds, blocks := map[string]map[int64]int{}, map[string]map[int]int{}
	for index, config := range fixture.runner.configs {
		if seeds[config.Endpoint] == nil {
			seeds[config.Endpoint], blocks[config.Endpoint] = map[int64]int{}, map[int]int{}
		}
		if previous, repeated := seeds[config.Endpoint][config.RandomSeed]; repeated {
			t.Fatalf("invocations %d and %d sent %s the same seed", previous, index, config.Endpoint)
		}
		seeds[config.Endpoint][config.RandomSeed] = index
		for _, row := range fixture.runner.traces[index] {
			for _, id := range row {
				if previous, repeated := blocks[config.Endpoint][id]; repeated && previous != index {
					t.Fatalf("invocations %d and %d sent %s prompt block %d", previous, index, config.Endpoint, id)
				}
				blocks[config.Endpoint][id] = index
			}
		}
	}
	if len(fixture.runner.configs) != 4+9+9+9 {
		t.Fatalf("invocations=%d", len(fixture.runner.configs))
	}
}

func TestBenchmarkExecutorRecordsItsInvocationBase(t *testing.T) {
	fixture := newExecutorFixture(t, optimizer.Request{})
	fixture.executor.Entropy = strings.NewReader("\x00\x00\x01\x00")
	_, workload := fixture.run(t)
	if evidence := measuredEvidence(t, workload); evidence.InvocationBase != 64 || fixture.runner.configs[0].RandomSeed != 64 || fixture.runner.configs[8].RandomSeed != 72 {
		t.Fatalf("base=%d seeds=%d..%d", evidence.InvocationBase, fixture.runner.configs[0].RandomSeed, fixture.runner.configs[8].RandomSeed)
	}
	if workload["random_seed"] != float64(measurementSeed) {
		t.Fatalf("the per-measurement base leaked into the workload identity: %v", workload["random_seed"])
	}
	empty := newExecutorFixture(t, optimizer.Request{})
	empty.executor.Entropy = strings.NewReader("")
	if _, err := empty.executor.Run(t.Context(), empty.campaign, empty.candidate, empty.profile); err == nil || len(empty.runner.configs) != 0 {
		t.Fatalf("a measurement ran without a seed base: %v", err)
	}
}

func TestBenchmarkExecutorRetriesTransientControlLookupInsteadOfDowngrading(t *testing.T) {
	fixture := newExecutorFixture(t, optimizer.Request{})
	fixture.store.resolveErr = map[string]error{fixture.control.DeploymentName: errors.New("database unavailable")}
	_, err := fixture.executor.Run(t.Context(), fixture.campaign, fixture.candidate, fixture.profile)
	var failure operations.Failure
	if err == nil || errors.As(err, &failure) || len(fixture.runner.configs) != 0 || fixture.store.recorded.ID != "" {
		t.Fatalf("a transient control lookup error was persisted as evidence: err=%v configs=%d", err, len(fixture.runner.configs))
	}
	fixture.store.resolveErr = map[string]error{fixture.control.DeploymentName: domain.ErrNotFound}
	_, workload := fixture.run(t)
	if evidence := measuredEvidence(t, workload); evidence.Control.Role != ControlRoleUnavailable || !strings.Contains(evidence.Control.Reason, "no longer exists") {
		t.Fatalf("a deleted control was not recorded as unavailable: %+v", evidence.Control)
	}
}
