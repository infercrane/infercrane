package proofreport

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/technique"
)

// The fixtures below are made-up records, shaped like a finished proof run.
// No number in them comes from a real measurement.
const (
	fixtureModel    = "Qwen/Qwen3-8B"
	fixtureRevision = "0123456789abcdef0123456789abcdef01234567"
	fixtureTrace    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixtureImage    = "vllm/vllm-openai@sha256:1111111111111111111111111111111111111111111111111111111111111111"
	fixtureCommit   = "0123abc"
)

// runFixture describes one finished run: the current setup against one
// challenger at one load.
type runFixture struct {
	id        string
	model     string
	gpu       string
	load      int
	gain      float64 // the challenger's output change against the current setup
	confirmed bool    // a fresh-server window measured the gain again
	kept      bool    // the current setup was kept instead
	price     *float64
	replay    bool
	passed    bool
	unstamped bool // a binary without a build commit, as `go test` builds
}

func goodRun(id string, load int, gain float64) runFixture {
	price := 0.80
	return runFixture{id: id, model: fixtureModel, gpu: "L4", load: load, gain: gain, confirmed: true, price: &price, replay: true, passed: true}
}

func (f runFixture) draft(args []string) string {
	encoded, _ := json.Marshal(f.deployment(args))
	return string(encoded)
}

func (f runFixture) deployment(args []string) optimizer.DeploymentDraft {
	var draft optimizer.DeploymentDraft
	draft.Name, draft.Model.ID, draft.Model.Revision = "chat", f.model, fixtureRevision
	draft.Runtime.Engine, draft.Runtime.Version, draft.Runtime.Args, draft.Runtime.Workload.Image = "vllm", "0.34.0", args, fixtureImage
	draft.Resources.GPU, draft.Resources.GPUCount = f.gpu, 1
	draft.Provider.Cloud, draft.Provider.Region = "aws", "eu-central-1"
	draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = 1, 1
	return draft
}

func (f runFixture) measured() *optimizationcampaign.MeasuredEvidence {
	comparison := func(delta float64) map[string]optimizationcampaign.MetricComparison {
		return map[string]optimizationcampaign.MetricComparison{
			"output_token_throughput": {HigherIsBetter: true, ControlMean: 1000, CandidateMean: 1000 * (1 + delta), Delta: delta, NoiseFloor: 0.03, Decision: optimizationcampaign.DecisionImproved},
			"ttft_p95_ms":             {ControlMean: 420, CandidateMean: 400, Delta: -0.048, NoiseFloor: 0.08, Decision: optimizationcampaign.DecisionNoMeasurableDifference},
			"tpot_p95_ms":             {ControlMean: 25, CandidateMean: 21, Delta: -0.16, NoiseFloor: 0.05, Decision: optimizationcampaign.DecisionImproved},
		}
	}
	runs := []optimizationcampaign.RunSummary{{Arm: "control"}, {Arm: "candidate"}, {Arm: "control"}, {Arm: "candidate"}, {Arm: "control"}}
	measured := &optimizationcampaign.MeasuredEvidence{Method: optimizationcampaign.MeasurementMethod, Runs: runs, GPUTelemetry: "not_collected",
		Control:    optimizationcampaign.ControlEvidence{Role: optimizationcampaign.ControlRoleBracketed, CandidateID: f.id + "-control", Runs: 3, MaxDrift: 0.01, DriftLimit: 0.05},
		Comparison: comparison(f.gain)}
	if f.confirmed {
		measured.Confirmation = &optimizationcampaign.ConfirmationEvidence{Comparison: comparison(f.gain * 0.9), FreshCandidate: true, FreshControl: true}
	}
	return measured
}

func (f runFixture) workload(measured *optimizationcampaign.MeasuredEvidence) string {
	document := map[string]any{"profile": "chat", "concurrency": f.load, "request_count_per_run": 480, "workload_source": "customer_replay", "measurement_method": optimizationcampaign.MeasurementMethod}
	if measured != nil {
		document["measured"] = measured
	}
	encoded, _ := json.Marshal(document)
	return string(encoded)
}

func (f runFixture) quality(id string, score float64, passed bool) domain.QualityEvidence {
	return domain.QualityEvidence{ID: id, TenantID: "tenant-a", Suite: "support-tasks", SuiteVersion: "3", Evaluator: "lm-eval", EvaluatorVersion: "0.4.9", Score: score, Passed: passed,
		SampleCount: 1000, PayloadDigest: strings.Repeat("d", 64), KeyID: "customer-key", Source: domain.QualityEvidenceSourceCustomer, EvaluatedAt: time.Unix(1, 0)}
}

// records are the persisted records of the run, as the store holds them.
func (f runFixture) records() (domain.OptimizationCampaign, optimizationreport.Evidence) {
	limit, load := 1000.0, float64(f.load)
	proposal := optimizer.Proposal{SchemaVersion: optimizer.SchemaVersion, AlgorithmVersion: "catalog-v1", Input: optimizer.Request{ModelIdentity: f.model, Provider: "aws", Region: "eu-central-1", GPU: f.gpu,
		Objective: "throughput", WorkloadProfile: "chat", WorkloadSource: optimizer.WorkloadSourceCustomerObserved, WorkloadFingerprint: "sha256:" + fixtureTrace,
		TargetConcurrency: &load, MaxTTFTP95MS: &limit, MaxCandidates: 4}, StartingPoint: &optimizer.StartingPoint{CandidateID: "proposal-control"},
		Candidates: []optimizer.Candidate{{ID: "proposal-control", Deployment: f.deployment(nil)}, {ID: "proposal-winner", Deployment: f.deployment([]string{"--max-num-seqs", "256"})}}}
	proposalJSON, _ := json.Marshal(proposal)
	created := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	campaign := domain.OptimizationCampaign{ID: f.id, TenantID: "tenant-a", InputDigest: strings.Repeat("c", 64), ModelIdentity: f.model, Objective: "throughput",
		Intent: optimizationcampaign.IntentNewEndpoint, State: optimizationcampaign.CampaignQualified, ProposalJSON: string(proposalJSON), CreatedAt: created, UpdatedAt: created.Add(26 * time.Hour)}
	evidence := optimizationreport.Evidence{Benchmarks: map[string]domain.BenchmarkResult{}, Quality: map[string]domain.QualityEvidence{}}
	cost := `{"available":false}`
	if f.price != nil {
		encoded, _ := json.Marshal(map[string]any{"available": true, "hourly": *f.price, "currency": "USD", "source": "your-aws-price-list/2026-11-01"})
		cost = string(encoded)
	}
	gpus := 1
	bench := func(id string, tps, ttft, tpot float64, measured *optimizationcampaign.MeasuredEvidence) domain.BenchmarkResult {
		return domain.BenchmarkResult{ID: "bench-" + id, TenantID: "tenant-a", ModelIdentity: f.model + "@" + fixtureRevision, Runtime: "vllm", GPU: f.gpu, GPUCount: &gpus, RevisionID: "rev-" + id,
			WorkloadJSON: f.workload(measured), OutputTokenThroughput: &tps, RequestThroughput: ptr(tps / 250), TTFTP95MS: &ttft, TPOTP95MS: &tpot, CostMetadataJSON: cost}
	}
	controlID, winnerID := f.id+"-control", f.id+"-winner"
	control := domain.OptimizationCandidateRun{ID: controlID, CampaignID: f.id, ProposalCandidateID: "proposal-control", Rank: 1, State: optimizationcampaign.CandidateCleaned,
		FailureCode: "measured_candidate_superseded", DeploymentSpecJSON: f.draft(nil), BenchmarkID: "bench-" + controlID, QualityEvidenceID: "q-" + controlID}
	evidence.Benchmarks[control.BenchmarkID] = bench(controlID, 1000, 420, 25, &optimizationcampaign.MeasuredEvidence{Method: optimizationcampaign.MeasurementMethod, Control: optimizationcampaign.ControlEvidence{Role: optimizationcampaign.ControlRoleCandidate, CandidateID: controlID}})
	evidence.Quality[control.QualityEvidenceID] = f.quality(control.QualityEvidenceID, 0.81, true)
	measured := f.measured()
	winner := domain.OptimizationCandidateRun{ID: winnerID, CampaignID: f.id, ProposalCandidateID: "proposal-winner", Rank: 2, State: optimizationcampaign.CandidateQualified,
		DeploymentSpecJSON: f.draft([]string{"--max-num-seqs", "256"}), BenchmarkID: "bench-" + winnerID, QualityEvidenceID: "q-" + winnerID}
	if f.kept {
		winner.State, winner.FailureCode = optimizationcampaign.CandidateCleaned, "measured_no_measurable_difference"
		control.State, control.FailureCode = optimizationcampaign.CandidateQualified, ""
	}
	actual, _ := json.Marshal(map[string]any{"workload": json.RawMessage(f.workload(measured)), "request_count": 480})
	winner.ActualEvidenceJSON = string(actual)
	evidence.Benchmarks[winner.BenchmarkID] = bench(winnerID, 1000*(1+f.gain), 400, 21, measured)
	evidence.Quality[winner.QualityEvidenceID] = f.quality(winner.QualityEvidenceID, 0.808, f.passed)
	campaign.Candidates = []domain.OptimizationCandidateRun{control, winner}
	if f.replay {
		evidence.Replay = trace()
	}
	evidence.Campaign = campaign
	return campaign, evidence
}

// trace is a captured traffic shape: 100 requests of 600 output tokens in
// one minute, 1,000 output tokens a second.
func trace() *domain.ReplayTrace {
	shapes := make([]map[string]any, 0, 100)
	for index := range 100 {
		shapes = append(shapes, map[string]any{"operation": "chat", "arrival_ms": index * 600, "input_tokens": 300, "output_tokens": 600})
	}
	encoded, _ := json.Marshal(shapes)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	complete := false
	return &domain.ReplayTrace{ID: "trace-1", ShapeJSON: string(encoded), ShapeDigest: fixtureTrace, RequestCount: len(shapes), WindowStart: start, WindowEnd: start.Add(time.Minute), RequestLimitReached: &complete}
}

// evidence is what Load would read for the run. Unless unstamped, the
// result carries a build commit, as a release build records one.
func (f runFixture) evidence(t *testing.T) RunEvidence {
	t.Helper()
	campaign, records := f.records()
	report := optimizationreport.Assemble(records)
	out := RunEvidence{Campaign: campaign, Report: report, Claim: "not shown"}
	if report.Outcome.Kind != optimizationreport.OutcomeWinnerSelected {
		return out
	}
	for _, run := range campaign.Candidates {
		if run.ID != report.Outcome.WinnerCandidateID {
			continue
		}
		answers := records.Quality[run.QualityEvidenceID]
		out.Answers = &answers
		out.Result, out.Claim = optimizationreport.SetupResult(campaign, run, out.Answers)
		if out.Result != nil && !f.unstamped {
			out.Result.Reproduction.Commit = fixtureCommit
			out.Claim = out.Result.Claim()
		}
	}
	return out
}

// asTechnique is the run's chosen setup as the one-engine adaptive speculation
// technique's, the way optimizationreport.SetupResult returns it once the
// technique's own evidence held (that path is tested in optimizationreport):
// the manifest's id, version and evidence, carrying the given disclosures, and
// judged by the manifest (Manifest.ClaimOf).
func asTechnique(t *testing.T, evidence RunEvidence, disclosures []string) RunEvidence {
	t.Helper()
	manifest, known := optimizer.TechniqueManifest(optimizer.TechniqueAdaptiveSpeculationOneEngine)
	if !known || len(manifest.Disclosures) == 0 || evidence.Result == nil {
		t.Fatalf("manifest %v %+v, result %v", known, manifest, evidence.Result)
	}
	result := *evidence.Result
	result.Technique, result.TechniqueVersion = manifest.ID, manifest.Version
	result.RequiredEvidence = append(slices.Clone(result.RequiredEvidence), manifest.Evidence...)
	result.Evidence = slices.Clone(result.Evidence)
	for _, name := range manifest.Evidence {
		result.Evidence = append(result.Evidence, technique.EvidenceItem{Name: name, Digest: "sha256:" + strings.Repeat("e", 64)})
	}
	result.Disclosures = slices.Clone(disclosures)
	evidence.Result, evidence.Claim = &result, manifest.ClaimOf(result)
	return evidence
}

func assemble(t *testing.T, fixtures ...runFixture) Report {
	t.Helper()
	in := Input{Title: "Proof Report", PreparedFor: "Example Labs"}
	for _, f := range fixtures {
		in.Runs = append(in.Runs, f.evidence(t))
	}
	return Assemble(in)
}

// store serves a tenant's records the way the control plane's store does:
// a record of another tenant is not found.
type store struct {
	tenant  string
	records map[string]optimizationreport.Evidence
}

func newStore(tenant string, fixtures ...runFixture) store {
	out := store{tenant: tenant, records: map[string]optimizationreport.Evidence{}}
	for _, f := range fixtures {
		_, records := f.records()
		out.records[f.id] = records
	}
	return out
}

func (s store) OptimizationCampaign(_ context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	records, found := s.records[id]
	if tenant != s.tenant || !found {
		return domain.OptimizationCampaign{}, domain.ErrNotFound
	}
	return records.Campaign, nil
}

func (s store) BenchmarksByIDs(_ context.Context, tenant string, ids []string) (map[string]domain.BenchmarkResult, error) {
	out := map[string]domain.BenchmarkResult{}
	for _, records := range s.records {
		for _, id := range ids {
			if row, found := records.Benchmarks[id]; found && tenant == s.tenant {
				out[id] = row
			}
		}
	}
	return out, nil
}

func (s store) QualityEvidenceByIDs(_ context.Context, tenant string, ids []string) (map[string]domain.QualityEvidence, error) {
	out := map[string]domain.QualityEvidence{}
	for _, records := range s.records {
		for _, id := range ids {
			if row, found := records.Quality[id]; found && tenant == s.tenant {
				out[id] = row
			}
		}
	}
	return out, nil
}

func (store) ReleaseGuardEvaluationsByIDs(context.Context, string, []string) (map[string]domain.ReleaseGuardEvaluation, error) {
	return map[string]domain.ReleaseGuardEvaluation{}, nil
}

func (store) LabEvaluationsByIDs(context.Context, string, []string) (map[string]domain.LabEvaluation, error) {
	return map[string]domain.LabEvaluation{}, nil
}

func (s store) ReplayTraceByDigest(_ context.Context, tenant, digest string) (domain.ReplayTrace, error) {
	if tenant != s.tenant || digest != fixtureTrace {
		return domain.ReplayTrace{}, domain.ErrNotFound
	}
	return *trace(), nil
}
