package optimizationreport

import (
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/releaseguard"
)

const (
	fixtureModel    = "Qwen/Qwen3-8B"
	fixtureRevision = "0123456789abcdef0123456789abcdef01234567"
	traceDigest     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type armFixture struct {
	id, proposalID, state, failure string
	rank                           int
	engine                         string
	args                           []string
	gpu                            string
	gpus, maxReplicas              int
	hourly                         *float64
	tps, rps, ttft, tpot           float64
	measured                       *optimizationcampaign.MeasuredEvidence
	quality                        *domain.QualityEvidence
	labID, guardID                 string
}

type fixture struct {
	intent, state, objective string
	source                   string
	arms                     []armFixture
	replay                   *domain.ReplayTrace
	guard                    map[string]domain.ReleaseGuardEvaluation
}

func hourly(value float64) *float64 { return &value }

func draftJSON(a armFixture) string {
	var draft optimizer.DeploymentDraft
	draft.Name, draft.Model.ID, draft.Model.Revision = "qwen-chat", fixtureModel, fixtureRevision
	draft.Runtime.Engine, draft.Runtime.Version, draft.Runtime.Args = a.engine, "0.11.0", a.args
	draft.Resources.GPU, draft.Resources.GPUCount = a.gpu, a.gpus
	draft.Provider.Cloud, draft.Provider.Region, draft.Compute.Mode = "aws", "eu-central-1", "elastic"
	draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = 1, a.maxReplicas
	encoded, _ := json.Marshal(draft)
	return string(encoded)
}

func workloadJSON(source string, measured *optimizationcampaign.MeasuredEvidence) string {
	document := map[string]any{"profile": "buffered", "profile_version": "v1", "concurrency": 8, "request_count_per_run": 60, "workload_source": source, "measurement_method": optimizationcampaign.MeasurementMethod, "confidence_95": map[string]any{}}
	if measured != nil {
		document["measured"] = measured
	}
	encoded, _ := json.Marshal(document)
	return string(encoded)
}

func (f fixture) evidence() Evidence {
	proposal := optimizer.Proposal{SchemaVersion: optimizer.SchemaVersion, AlgorithmVersion: "catalog-v1", Input: optimizer.Request{ModelIdentity: fixtureModel, Objective: f.objective, WorkloadSource: f.source}, StartingPoint: &optimizer.StartingPoint{CandidateID: "proposal-control"}}
	if f.source == optimizer.WorkloadSourceCustomerObserved {
		proposal.Input.WorkloadFingerprint = "sha256:" + traceDigest
	}
	proposalJSON, _ := json.Marshal(proposal)
	campaign := domain.OptimizationCampaign{ID: "campaign-1", TenantID: "tenant-a", InputDigest: strings.Repeat("c", 64), ModelIdentity: fixtureModel, Objective: f.objective, Intent: f.intent, State: f.state, ProposalJSON: string(proposalJSON)}
	evidence := Evidence{Benchmarks: map[string]domain.BenchmarkResult{}, Quality: map[string]domain.QualityEvidence{}, ReleaseGuard: f.guard, Replay: f.replay}
	workloadSource := "synthetic_profile"
	if f.source == optimizer.WorkloadSourceCustomerObserved {
		workloadSource = "customer_replay"
	}
	for _, a := range f.arms {
		run := domain.OptimizationCandidateRun{ID: a.id, TenantID: "tenant-a", CampaignID: campaign.ID, ProposalCandidateID: a.proposalID, State: a.state, FailureCode: a.failure, Rank: a.rank, DeploymentSpecJSON: draftJSON(a), DeploymentName: "qwen-chat-opt-" + a.id, RevisionID: "rev-" + a.id, LabEvaluationID: a.labID, ReleaseGuardEvaluationID: a.guardID}
		if a.tps > 0 {
			run.BenchmarkID = "bench-" + a.id
			cost := map[string]any{"available": false}
			if a.hourly != nil {
				cost = map[string]any{"available": true, "hourly": *a.hourly, "currency": "USD", "source": "aws-price-list/2026-11-12"}
			}
			costJSON, _ := json.Marshal(cost)
			gpus := a.gpus
			row := domain.BenchmarkResult{ID: run.BenchmarkID, TenantID: "tenant-a", DeploymentName: run.DeploymentName, RevisionID: run.RevisionID, ModelIdentity: fixtureModel + "@" + fixtureRevision, Runtime: a.engine, GPU: a.gpu, GPUCount: &gpus, WorkloadJSON: workloadJSON(workloadSource, a.measured), OutputTokenThroughput: hourly(a.tps), RequestThroughput: hourly(a.rps), TTFTP95MS: hourly(a.ttft), CostMetadataJSON: string(costJSON)}
			if a.tpot > 0 {
				row.TPOTP95MS = hourly(a.tpot)
			}
			evidence.Benchmarks[row.ID] = row
		}
		if a.quality != nil {
			run.QualityEvidenceID = a.quality.ID
			evidence.Quality[a.quality.ID] = *a.quality
		}
		campaign.Candidates = append(campaign.Candidates, run)
	}
	evidence.Campaign = campaign
	return evidence
}

func serverMetrics(avg, peak float64) *benchmark.ServerMetrics {
	return &benchmark.ServerMetrics{Collected: true, Metrics: map[string]benchmark.ServerMetricSummary{"vllm:kv_cache_usage_perc": {Type: "gauge", Unit: "ratio", Series: 1, Stats: map[string]float64{"avg": avg, "max": peak}}}}
}

// interleaved is a winner measured in turns with the control "control".
func interleaved() *optimizationcampaign.MeasuredEvidence {
	runs := []optimizationcampaign.RunSummary{}
	for index := range 7 {
		if index%2 == 0 {
			runs = append(runs, optimizationcampaign.RunSummary{Arm: "control", ServerMetrics: serverMetrics(0.4, 0.7)})
		} else {
			runs = append(runs, optimizationcampaign.RunSummary{Arm: "candidate", ServerMetrics: serverMetrics(0.6, 0.93)})
		}
	}
	return &optimizationcampaign.MeasuredEvidence{
		Method:  optimizationcampaign.MeasurementMethod,
		Runs:    runs,
		Control: optimizationcampaign.ControlEvidence{Role: optimizationcampaign.ControlRoleBracketed, CandidateID: "control", Runs: 4, MaxDrift: 0.012, DriftLimit: 0.05},
		Comparison: map[string]optimizationcampaign.MetricComparison{
			"output_token_throughput": {HigherIsBetter: true, ControlMean: 1000, CandidateMean: 1300, Delta: 0.3, NoiseFloor: 0.03, Decision: optimizationcampaign.DecisionImproved},
			"request_throughput":      {HigherIsBetter: true, ControlMean: 4, CandidateMean: 5.2, Delta: 0.3, NoiseFloor: 0.04, Decision: optimizationcampaign.DecisionImproved},
			"ttft_p95_ms":             {ControlMean: 400, CandidateMean: 380, Delta: -0.05, NoiseFloor: 0.08, Decision: optimizationcampaign.DecisionNoMeasurableDifference},
			"tpot_p95_ms":             {ControlMean: 25, CandidateMean: 20, Delta: -0.2, NoiseFloor: 0.05, Decision: optimizationcampaign.DecisionImproved},
		},
		GPUTelemetry: "not_collected",
	}
}

func signedQuality(id, revision, version string, score float64, passed bool) *domain.QualityEvidence {
	return &domain.QualityEvidence{ID: id, TenantID: "tenant-a", RevisionID: revision, Suite: "gsm8k", SuiteVersion: version, Evaluator: "lm-eval", EvaluatorVersion: "0.4.9", Score: score, Passed: passed, PayloadDigest: strings.Repeat("d", 64), KeyID: "key-1", EvaluatedAt: time.Unix(1, 0)}
}

func replayTrace(outputs ...*int) *domain.ReplayTrace {
	shapes := []map[string]any{{"operation": "embeddings", "arrival_ms": 0}}
	for _, output := range outputs {
		row := map[string]any{"operation": "chat", "arrival_ms": 10, "input_tokens": 100}
		if output != nil {
			row["output_tokens"] = *output
		}
		shapes = append(shapes, row)
	}
	encoded, _ := json.Marshal(shapes)
	start := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	return &domain.ReplayTrace{ID: "trace-1", ShapeJSON: string(encoded), ShapeDigest: traceDigest, RequestCount: len(shapes), WindowStart: start, WindowEnd: start.Add(time.Hour)}
}

func tokensOf(value int) *int { return &value }

// winnerFixture is a finished new-endpoint campaign: the winner was measured
// in turns with the control on replayed customer traffic.
func winnerFixture() fixture {
	return fixture{intent: optimizationcampaign.IntentNewEndpoint, state: optimizationcampaign.CampaignQualified, objective: "throughput", source: optimizer.WorkloadSourceCustomerObserved,
		replay: replayTrace(tokensOf(400), tokensOf(600)),
		arms: []armFixture{
			{id: "control", proposalID: "proposal-control", state: optimizationcampaign.CandidateCleaned, failure: "measured_candidate_superseded", rank: 1, engine: "vllm", gpu: "H100", gpus: 1, maxReplicas: 2, hourly: hourly(4), tps: 990, rps: 3.9, ttft: 410, tpot: 26,
				measured: &optimizationcampaign.MeasuredEvidence{Method: optimizationcampaign.MeasurementMethod, Control: optimizationcampaign.ControlEvidence{Role: optimizationcampaign.ControlRoleCandidate, CandidateID: "control"}}, quality: signedQuality("quality-control", "rev-control", "1", 0.80, true), labID: "optlab-" + strings.Repeat("e", 64)},
			{id: "winner", proposalID: "proposal-winner", state: optimizationcampaign.CandidateQualified, rank: 2, engine: "vllm", args: []string{"--max-num-seqs", "256"}, gpu: "H100", gpus: 1, maxReplicas: 2, hourly: hourly(4), tps: 1300, rps: 5.2, ttft: 380, tpot: 20,
				measured: interleaved(), quality: signedQuality("quality-winner", "rev-winner", "1", 0.82, true), labID: "optlab-" + strings.Repeat("e", 64)},
			{id: "same", proposalID: "proposal-same", state: optimizationcampaign.CandidateCleaned, failure: "measured_no_measurable_difference", rank: 3, engine: "vllm", args: []string{"--enable-prefix-caching"}, gpu: "H100", gpus: 1, maxReplicas: 2, hourly: hourly(4), tps: 1010, rps: 4, ttft: 400, tpot: 25,
				measured: &optimizationcampaign.MeasuredEvidence{Method: optimizationcampaign.MeasurementMethod, Control: optimizationcampaign.ControlEvidence{Role: optimizationcampaign.ControlRoleBracketed, CandidateID: "control", Runs: 4, MaxDrift: 0.02, DriftLimit: 0.05}, Comparison: map[string]optimizationcampaign.MetricComparison{"output_token_throughput": {HigherIsBetter: true, ControlMean: 1000, CandidateMean: 1010, Delta: 0.01, NoiseFloor: 0.03, Decision: optimizationcampaign.DecisionNoMeasurableDifference}}}},
		}}
}

func metricByKey(t *testing.T, report Report, key string) Metric {
	t.Helper()
	for _, metric := range report.Metrics {
		if metric.Key == key {
			return metric
		}
	}
	t.Fatalf("report has no metric %s", key)
	return Metric{}
}

func near(value *float64, want float64) bool {
	return value != nil && math.Abs(*value-want) < 1e-6*math.Max(1, math.Abs(want))
}

func TestAssembleComparesWinnerWithBaselineMeasuredInTheSameWindow(t *testing.T) {
	report := Assemble(winnerFixture().evidence())
	if report.Outcome.Kind != OutcomeWinnerSelected || report.Outcome.WinnerCandidateID != "winner" || report.Outcome.BaselineCandidateID != "control" || !report.Finished {
		t.Fatalf("outcome = %+v finished=%v", report.Outcome, report.Finished)
	}
	if report.Outcome.Headline != "Roll out “vllm, --max-num-seqs 256”: it measured better than your current setup on the same traffic." {
		t.Fatalf("headline = %q", report.Outcome.Headline)
	}
	if report.SimulatedAccelerator || report.Banner != "" {
		t.Fatal("a GPU campaign was flagged as simulated")
	}
	// $4/h covers two replicas; one replica was measured: $2/h per replica.
	cost := metricByKey(t, report, MetricCostPerMillion)
	if !near(cost.Baseline, 2.0/(1000*3600)*1e6) || !near(cost.Winner, 2.0/(1300*3600)*1e6) || !near(cost.ChangePercent, (1000.0/1300-1)*100) || cost.Decision != DecisionImproved || !near(cost.NoiseFloorPercent, 3) || cost.Basis != BasisInterleaved {
		t.Fatalf("cost metric = %+v", cost)
	}
	ttft := metricByKey(t, report, MetricTTFTP95)
	if !near(ttft.Baseline, 400) || !near(ttft.Winner, 380) || !near(ttft.ChangePercent, -5) || ttft.Decision != DecisionNoDifference || !near(ttft.NoiseFloorPercent, 8) {
		t.Fatalf("ttft metric = %+v", ttft)
	}
	perUser := metricByKey(t, report, MetricTokensPerUser)
	if !near(perUser.Baseline, 40) || !near(perUser.Winner, 50) || !near(perUser.ChangePercent, 25) || perUser.Decision != DecisionImproved {
		t.Fatalf("per-user metric = %+v", perUser)
	}
	perGPU := metricByKey(t, report, MetricTokensPerAccel)
	if perGPU.Label != "Output tokens/s per GPU" || !near(perGPU.Baseline, 1000) || !near(perGPU.Winner, 1300) || perGPU.Decision != DecisionImproved {
		t.Fatalf("per-GPU metric = %+v", perGPU)
	}
	rps := metricByKey(t, report, MetricRequestsPerSec)
	if !near(rps.Baseline, 4) || !near(rps.Winner, 5.2) || rps.Decision != DecisionImproved {
		t.Fatalf("requests metric = %+v", rps)
	}
	quality := report.Quality
	if quality.Verdict != releaseguard.QualityPass || quality.Basis != BasisCampaignGate || !near(quality.Delta, 0.82-0.80) || !near(quality.BaselineScore, 0.80) || quality.Eval != "gsm8k@1" {
		t.Fatalf("quality = %+v", quality)
	}
	noise := report.Noise
	if noise.Basis != BasisInterleaved || noise.CandidateID != "winner" || noise.ControlRuns != 4 || noise.CandidateRuns != 3 || !near(noise.ControlDriftPercent, 1.2) || !near(noise.DriftLimitPercent, 5) || noise.ObjectiveMetric != "output_token_throughput" || !near(noise.ObjectiveNoiseFloorPercent, 3) {
		t.Fatalf("noise = %+v", noise)
	}
	// 1,000 output tokens in a one-hour window; the embeddings request is not
	// generation traffic.
	monthly := report.Monthly
	if monthly.Basis != BasisReplayVolume || *monthly.RequestsInWindow != 2 || *monthly.OutputTokensInWindow != 1000 || !near(monthly.MonthlyOutputTokens, 730_000) {
		t.Fatalf("monthly volume = %+v", monthly)
	}
	if !near(monthly.BaselineUSD, 0.73**cost.Baseline) || !near(monthly.WinnerUSD, 0.73**cost.Winner) || !near(monthly.ChangeUSD, 0.73*(*cost.Winner-*cost.Baseline)) || monthly.Decision != DecisionImproved || monthly.TraceDigest != "sha256:"+traceDigest {
		t.Fatalf("monthly cost = %+v", monthly)
	}
	if report.Bottleneck.State != "measured" || report.Bottleneck.GPUTelemetry != "not_collected" || len(report.Bottleneck.Signals) != 2 {
		t.Fatalf("bottleneck = %+v", report.Bottleneck)
	}
	for _, signal := range report.Bottleneck.Signals {
		wantBase, wantWin := 0.4, 0.6
		if signal.Stat == "max" {
			wantBase, wantWin = 0.7, 0.93
		}
		if signal.Name != "vllm:kv_cache_usage_perc" || !near(signal.Baseline, wantBase) || !near(signal.Winner, wantWin) {
			t.Fatalf("signal = %+v", signal)
		}
	}
	winner := report.Appendix.Winner
	if winner == nil || winner.Engine != "vllm" || winner.EngineVersion != "0.11.0" || !slices.Equal(winner.Args, []string{"--max-num-seqs", "256"}) || winner.Accelerator != "H100" || *winner.AcceleratorCount != 1 || winner.AcceleratorClass != AcceleratorGPU || winner.Model != fixtureModel+"@"+fixtureRevision || !near(winner.HourlyUSD, 2) {
		t.Fatalf("winning setup = %+v", winner)
	}
	kinds := []string{}
	for _, entry := range report.Appendix.Evidence {
		kinds = append(kinds, entry.Kind)
		if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(entry.Digest) {
			t.Fatalf("evidence %s digest %q is not a sha256 digest", entry.Kind, entry.Digest)
		}
	}
	for _, want := range []string{"campaign_input", "proposal", "winner_benchmark", "winner_quality", "ranking", "baseline_benchmark", "baseline_quality", "replayed_traffic"} {
		if !slices.Contains(kinds, want) {
			t.Fatalf("evidence kinds %v lack %s", kinds, want)
		}
	}
	if len(report.Unknown) != 0 {
		t.Fatalf("a complete campaign reported unknown fields: %v", report.Unknown)
	}
	roles := map[string]string{}
	for _, candidate := range report.Candidates {
		roles[candidate.ID] = candidate.Role
	}
	if roles["control"] != "baseline" || roles["winner"] != "winner" || roles["same"] != "candidate" || report.Candidates[2].VersusBaseline == nil || report.Candidates[2].VersusBaseline.Decision != DecisionNoDifference {
		t.Fatalf("candidates = %+v", report.Candidates)
	}
	if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(report.ReportDigest) || Assemble(winnerFixture().evidence()).ReportDigest != report.ReportDigest {
		t.Fatalf("report digest %q is not stable", report.ReportDigest)
	}
}

func TestActivatedWinnerHeadlineAndRankingDigest(t *testing.T) {
	f := winnerFixture()
	f.state, f.arms[1].state = optimizationcampaign.CampaignPromoted, optimizationcampaign.CandidatePromoted
	f.arms[0].labID, f.arms[1].labID = "lab-row-7", "lab-row-7"
	report := Assemble(f.evidence())
	if !report.Outcome.Activated || report.Outcome.Headline != "“vllm, --max-num-seqs 256” is live: it measured better than your previous setup on the same traffic." {
		t.Fatalf("outcome = %+v", report.Outcome)
	}
	for _, entry := range report.Appendix.Evidence {
		if entry.Kind == "ranking" {
			t.Fatalf("a ranking without a content address was cited with digest %q", entry.Digest)
		}
	}
}

func TestAssembleRescalesCostAndPerAcceleratorDecisions(t *testing.T) {
	f := winnerFixture()
	// The winner doubles the accelerators and the price: 30% more throughput
	// is 35% less per GPU and per dollar.
	f.arms[1].gpus, f.arms[1].hourly = 2, hourly(8)
	report := Assemble(f.evidence())
	cost := metricByKey(t, report, MetricCostPerMillion)
	if cost.Decision != DecisionRegressed || !near(cost.ChangePercent, (4.0/(1300*3600))/(2.0/(1000*3600))*100-100) {
		t.Fatalf("cost metric = %+v", cost)
	}
	perGPU := metricByKey(t, report, MetricTokensPerAccel)
	if perGPU.Decision != DecisionRegressed || !near(perGPU.Winner, 650) || !near(perGPU.ChangePercent, -35) {
		t.Fatalf("per-GPU metric = %+v", perGPU)
	}
	if !strings.Contains(report.Outcome.Headline, "better than your current setup on some outcomes and worse on others") {
		t.Fatalf("headline hid a regression: %q", report.Outcome.Headline)
	}
}

func assertUnknown(t *testing.T, report Report, path string) {
	t.Helper()
	if !slices.Contains(report.Unknown, path) {
		t.Fatalf("unknown = %v, want %s", report.Unknown, path)
	}
}

func TestAssembleKeepsMissingAndIncomparableInputsUnknown(t *testing.T) {
	t.Run("no sourced price", func(t *testing.T) {
		f := winnerFixture()
		f.arms[1].hourly = nil
		report := Assemble(f.evidence())
		cost := metricByKey(t, report, MetricCostPerMillion)
		if cost.Winner != nil || cost.ChangePercent != nil || cost.Decision != DecisionUnknown || !strings.Contains(cost.Reason, "no sourced hourly price") {
			t.Fatalf("cost was estimated without a sourced price: %+v", cost)
		}
		if report.Monthly.WinnerUSD != nil || report.Monthly.ChangeUSD != nil || report.Monthly.Decision != DecisionUnknown {
			t.Fatalf("monthly cost was estimated without a sourced price: %+v", report.Monthly)
		}
		assertUnknown(t, report, "metrics."+MetricCostPerMillion)
		assertUnknown(t, report, "monthly.change")
		if metricByKey(t, report, MetricTTFTP95).Decision != DecisionNoDifference {
			t.Fatal("an unpriced winner lost its measured speed comparison")
		}
	})
	t.Run("control not measured alongside", func(t *testing.T) {
		f := winnerFixture()
		measured := interleaved()
		measured.Control = optimizationcampaign.ControlEvidence{Role: optimizationcampaign.ControlRoleUnavailable, CandidateID: "control", Reason: "the control candidate is not serving (state cleaned)"}
		measured.Comparison = nil
		f.arms[1].measured = measured
		report := Assemble(f.evidence())
		rps := metricByKey(t, report, MetricRequestsPerSec)
		if rps.Basis != BasisSeparateRuns || !near(rps.Baseline, 3.9) || !near(rps.Winner, 5.2) || rps.ChangePercent == nil || rps.Decision != DecisionUnknown || rps.NoiseFloorPercent != nil || !strings.Contains(rps.Reason, "not serving") {
			t.Fatalf("separate-run metric = %+v", rps)
		}
		if report.Noise.Basis != BasisUnknown || report.Noise.ObjectiveNoiseFloorPercent != nil {
			t.Fatalf("noise band invented without interleaved runs: %+v", report.Noise)
		}
		assertUnknown(t, report, "noise")
		assertUnknown(t, report, "metrics."+MetricRequestsPerSec)
	})
	t.Run("missing winner benchmark", func(t *testing.T) {
		evidence := winnerFixture().evidence()
		delete(evidence.Benchmarks, "bench-winner")
		report := Assemble(evidence)
		for _, metric := range report.Metrics {
			if metric.Winner != nil || metric.ChangePercent != nil || metric.Decision != DecisionUnknown {
				t.Fatalf("metric without winner evidence = %+v", metric)
			}
		}
	})
	t.Run("missing time per output token", func(t *testing.T) {
		f := winnerFixture()
		measured := interleaved()
		delete(measured.Comparison, "tpot_p95_ms")
		f.arms[1].measured, f.arms[1].tpot = measured, 0
		perUser := metricByKey(t, Assemble(f.evidence()), MetricTokensPerUser)
		if perUser.Winner != nil || perUser.Decision != DecisionUnknown {
			t.Fatalf("per-user speed = %+v", perUser)
		}
	})
	t.Run("synthetic load has no traffic volume", func(t *testing.T) {
		f := winnerFixture()
		f.source, f.replay = optimizer.WorkloadSourceReviewedDefault, nil
		report := Assemble(f.evidence())
		if report.Monthly.Basis != BasisUnknown || report.Monthly.BaselineUSD != nil || report.Monthly.MonthlyOutputTokens != nil || !strings.Contains(report.Monthly.Reason, "did not replay your traffic") {
			t.Fatalf("monthly = %+v", report.Monthly)
		}
		assertUnknown(t, report, "monthly")
	})
	t.Run("replayed request without output tokens", func(t *testing.T) {
		f := winnerFixture()
		f.replay = replayTrace(tokensOf(400), nil)
		report := Assemble(f.evidence())
		if report.Monthly.Basis != BasisUnknown || report.Monthly.OutputTokensInWindow != nil || report.Monthly.Reason != "1 of 2 replayed requests have no output-token count" {
			t.Fatalf("a partial traffic volume was projected: %+v", report.Monthly)
		}
	})
	t.Run("replayed traffic not found", func(t *testing.T) {
		evidence := winnerFixture().evidence()
		evidence.Replay, evidence.ReplayReason = nil, "the replayed traffic record was not found"
		report := Assemble(evidence)
		if report.Monthly.Basis != BasisUnknown || report.Monthly.Reason != "the replayed traffic record was not found" {
			t.Fatalf("monthly = %+v", report.Monthly)
		}
	})
	t.Run("winner has no quality evidence", func(t *testing.T) {
		f := winnerFixture()
		f.arms[1].quality = nil
		report := Assemble(f.evidence())
		if report.Quality.Verdict != releaseguard.QualityUnknown || report.Quality.WinnerScore != nil || report.Quality.Delta != nil {
			t.Fatalf("quality = %+v", report.Quality)
		}
		assertUnknown(t, report, "quality.verdict")
	})
	t.Run("baseline quality from another suite version", func(t *testing.T) {
		f := winnerFixture()
		f.arms[0].quality = signedQuality("quality-control", "rev-control", "2", 0.95, true)
		report := Assemble(f.evidence())
		if report.Quality.Verdict != releaseguard.QualityPass || report.Quality.Delta != nil || report.Quality.BaselineScore != nil {
			t.Fatalf("incomparable quality evidence was compared: %+v", report.Quality)
		}
		assertUnknown(t, report, "quality.delta")
	})
	t.Run("no engine metrics", func(t *testing.T) {
		f := winnerFixture()
		measured := interleaved()
		for index := range measured.Runs {
			measured.Runs[index].ServerMetrics = nil
		}
		f.arms[1].measured = measured
		report := Assemble(f.evidence())
		if report.Bottleneck.State != BasisUnknown || len(report.Bottleneck.Signals) != 0 {
			t.Fatalf("bottleneck = %+v", report.Bottleneck)
		}
		assertUnknown(t, report, "bottleneck")
	})
}

func TestAssembleFlagsSimulatedAcceleratorRuns(t *testing.T) {
	f := winnerFixture()
	for index := range f.arms {
		f.arms[index].gpu = "SIMULATED-CPU"
	}
	report := Assemble(f.evidence())
	if !report.SimulatedAccelerator || report.Banner != CPUBanner || CPUBanner != "CPU run: proves the pipeline, not GPU speed" {
		t.Fatalf("simulated run was not flagged: simulated=%v banner=%q", report.SimulatedAccelerator, report.Banner)
	}
	if label := metricByKey(t, report, MetricTokensPerAccel).Label; strings.Contains(label, "GPU") {
		t.Fatalf("a CPU number was labelled as a GPU result: %q", label)
	}
	if report.Appendix.Winner.AcceleratorClass != AcceleratorSimulated {
		t.Fatalf("winning setup class = %s", report.Appendix.Winner.AcceleratorClass)
	}
	text := Text(report)
	if !strings.Contains(text, "!! "+CPUBanner+" !!") || strings.Index(text, CPUBanner) > strings.Index(text, report.Outcome.Headline) {
		t.Fatalf("text output lacks the fixed banner above the outcome:\n%s", text)
	}
	if strings.Contains(text, "per GPU") {
		t.Fatalf("text output presents CPU numbers as GPU results:\n%s", text)
	}
	page, err := HTML(report)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), `<div class="banner" role="alert">`+CPUBanner+`</div>`) || strings.Contains(string(page), "per GPU") {
		t.Fatal("HTML output lacks the fixed banner or presents CPU numbers as GPU results")
	}

	// One simulated setup is enough to flag the campaign.
	mixed := winnerFixture()
	mixed.arms[2].gpu = "cpu"
	if !Assemble(mixed.evidence()).SimulatedAccelerator {
		t.Fatal("a campaign with one CPU setup was not flagged")
	}
	gpu := Assemble(winnerFixture().evidence())
	if strings.Contains(Text(gpu), CPUBanner) {
		t.Fatal("a GPU campaign carried the CPU banner")
	}
}

func TestAcceleratorClass(t *testing.T) {
	for accelerator, want := range map[string]string{
		"SIMULATED-CPU": AcceleratorSimulated, "simulated-accelerator": AcceleratorSimulated, "cpu": AcceleratorSimulated, "x86-CPU": AcceleratorSimulated,
		"H100": AcceleratorGPU, "NVIDIA-L40S": AcceleratorGPU, "MI300X": AcceleratorGPU, "": AcceleratorUnknown, "  ": AcceleratorUnknown,
	} {
		if got := acceleratorClass(accelerator); got != want {
			t.Errorf("acceleratorClass(%q) = %s, want %s", accelerator, got, want)
		}
	}
}

func TestAssembleBaselineKept(t *testing.T) {
	f := winnerFixture()
	f.arms[0].state, f.arms[0].failure = optimizationcampaign.CandidateQualified, ""
	f.arms[1].state, f.arms[1].failure = optimizationcampaign.CandidateCleaned, "measured_no_measurable_difference"
	report := Assemble(f.evidence())
	if report.Outcome.Kind != OutcomeBaselineKept || report.Outcome.Headline != "Keep your current setup: nothing beat it by more than the measurement noise." {
		t.Fatalf("outcome = %+v", report.Outcome)
	}
	for _, metric := range report.Metrics {
		if metric.Decision != DecisionUnchanged || metric.Baseline == nil || *metric.Baseline != *metric.Winner || metric.ChangePercent != nil {
			t.Fatalf("kept baseline metric = %+v", metric)
		}
	}
	if report.Quality.Basis != BasisBaselineKept || report.Quality.Verdict != releaseguard.QualityPass {
		t.Fatalf("quality = %+v", report.Quality)
	}
	// The noise band comes from the challenger closest to beating the kept
	// setup, with its own measured decision rather than an assumed one.
	if report.Noise.Basis != BasisInterleaved || report.Noise.CandidateID != "winner" || report.Noise.ObjectiveDecision != DecisionImproved {
		t.Fatalf("noise = %+v", report.Noise)
	}
	if !strings.Contains(Text(report), "Closest challenger: output throughput +30.0% against your current setup, better.") {
		t.Fatalf("closest challenger line is missing or claims the wrong decision:\n%s", Text(report))
	}
	if report.Monthly.Decision != DecisionUnchanged || report.Monthly.BaselineUSD == nil || report.Monthly.ChangeUSD != nil {
		t.Fatalf("monthly = %+v", report.Monthly)
	}
	if report.Candidates[0].Role != "baseline_kept" {
		t.Fatalf("candidates = %+v", report.Candidates)
	}
}

func TestAssembleUsesReleaseGuardQualityVerdict(t *testing.T) {
	f := winnerFixture()
	f.intent, f.state = optimizationcampaign.IntentEvolveEndpoint, optimizationcampaign.CampaignGuardPassed
	f.arms[1].state, f.arms[1].guardID = optimizationcampaign.CandidateGuardPassed, "guard-1"
	reference, delta, regression, watch, block := 0.84, -0.02, 2.38, 1.0, 5.0
	quality := releaseguard.QualityReport{Verdict: releaseguard.QualityWatch, WatchRegressionPercent: &watch, MaxRegressionPercent: &block, Evals: []releaseguard.QualityEval{
		{Suite: "mmlu", SuiteVersion: "1", Evaluator: "lm-eval", EvaluatorVersion: "0.4.9", Verdict: releaseguard.QualityPass, CandidateScore: 0.7, CandidatePassed: true},
		{Suite: "gsm8k", SuiteVersion: "1", Evaluator: "lm-eval", EvaluatorVersion: "0.4.9", Verdict: releaseguard.QualityWatch, CandidateScore: 0.82, CandidatePassed: true, ReferenceScore: &reference, Delta: &delta, RegressionPercent: &regression, FollowUp: "Re-run gsm8k@1"},
	}}
	metrics, _ := json.Marshal(map[string]any{"quality": quality})
	f.guard = map[string]domain.ReleaseGuardEvaluation{"guard-1": {ID: "guard-1", Decision: "ACCEPT", MetricsJSON: string(metrics)}}
	report := Assemble(f.evidence())
	got := report.Quality
	if got.Verdict != releaseguard.QualityWatch || got.Basis != BasisReleaseGuard || got.Eval != "gsm8k@1" || !near(got.Delta, -0.02) || !near(got.RegressionPercent, 2.38) || !near(got.WatchThresholdPercent, 1) || got.ReleaseGuardEvaluationID != "guard-1" || len(got.Evals) != 2 || got.Evals[1].FollowUp == "" {
		t.Fatalf("quality = %+v", got)
	}
	if !strings.Contains(Text(report), "WATCH") {
		t.Fatal("text output lacks the WATCH verdict")
	}
}

func TestAssembleWithoutWinnerNeverInventsResults(t *testing.T) {
	for _, tc := range []struct{ state, kind, headline string }{
		{optimizationcampaign.CampaignRunning, OutcomeInProgress, "Still measuring"},
		{optimizationcampaign.CampaignCancelled, OutcomeCancelled, "Cancelled"},
		{optimizationcampaign.CampaignRejected, OutcomeNoWinner, "No setup qualified"},
	} {
		f := winnerFixture()
		f.state = tc.state
		for index := range f.arms {
			f.arms[index].state = optimizationcampaign.CandidateRejected
		}
		if tc.state == optimizationcampaign.CampaignRunning {
			f.arms[1].state = optimizationcampaign.CandidateMeasuring
		}
		report := Assemble(f.evidence())
		if report.Outcome.Kind != tc.kind || !strings.HasPrefix(report.Outcome.Headline, tc.headline) {
			t.Fatalf("%s: outcome = %+v", tc.state, report.Outcome)
		}
		for _, metric := range report.Metrics {
			if metric.Winner != nil || metric.ChangePercent != nil || metric.Decision != DecisionUnknown {
				t.Fatalf("%s: metric = %+v", tc.state, metric)
			}
		}
		if report.Quality.Verdict != releaseguard.QualityUnknown || report.Monthly.WinnerUSD != nil {
			t.Fatalf("%s: quality=%+v monthly=%+v", tc.state, report.Quality, report.Monthly)
		}
	}
}
