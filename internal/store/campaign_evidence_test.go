package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// evidenceDeployment is one deployment with a benchmark and signed quality
// evidence recorded against its active revision, as a measured candidate has.
type evidenceDeployment struct {
	deployment domain.Deployment
	benchmark  domain.BenchmarkResult
	quality    domain.QualityEvidence
}

func recordEvidenceDeployment(t *testing.T, ctx context.Context, s *Store, name string, outputTPS, ttftP95 float64, digestSeed string) evidenceDeployment {
	t.Helper()
	// Targets outlive a deleted deployment, so each deployment gets its own.
	target, err := s.AddTarget(ctx, domain.Target{Name: name + "-target-" + digestSeed, URL: "http://" + name + "-" + digestSeed, Provider: "existing", Runtime: "vllm", UpstreamModel: "Qwen/Qwen3-0.6B"})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := s.ApplyDeployment(ctx, domain.Deployment{Name: name, Model: "Qwen/Qwen3-0.6B"}, []string{target.Name})
	if err != nil {
		t.Fatal(err)
	}
	gpus := 1
	benchmark, err := s.RecordBenchmark(ctx, domain.BenchmarkResult{TenantID: "global", DeploymentID: deployment.ID, DeploymentName: name, RevisionID: deployment.ActiveRevisionID, ModelIdentity: "Qwen/Qwen3-0.6B", Runtime: "vllm", RuntimeVersion: "0.22.1", RuntimeConfigJSON: `{"args":[]}`, Provider: "vast", GPU: "RTX4090", GPUCount: &gpus, ComputeMode: "elastic", Tool: "aiperf", ToolVersion: "0.9.0", WorkloadJSON: `{"workload_source":"customer_observed","concurrency":2}`, ReproductionCommand: "aiperf profile", RequestCount: 10, Succeeded: 10, DurationSeconds: 2, OutputTokenThroughput: &outputTPS, TTFTP95MS: &ttftP95, CostMetadataJSON: `{"available":false}`})
	if err != nil {
		t.Fatal(err)
	}
	quality, _, err := s.RecordQualityEvidence(ctx, "global", name, domain.QualityEvidence{RevisionID: deployment.ActiveRevisionID, Suite: "answers", SuiteVersion: "git:abc", Evaluator: "infercrane", EvaluatorVersion: "1.0.0", Score: .97, Passed: true, SampleCount: 50, ArtifactDigest: "sha256:" + strings.Repeat(digestSeed, 64), PayloadDigest: "sha256:" + strings.Repeat(digestSeed+"f", 32), Signature: "signature", PublicKey: "public-key", Algorithm: "Ed25519-SHA256", KeyID: "sha256:key", EvaluatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return evidenceDeployment{deployment: deployment, benchmark: benchmark, quality: quality}
}

// TestEvidenceStaysReadableByIDAfterItsDeploymentIsDeleted pins the boundary
// that finished campaigns rely on: once a deployment is fully deleted, and
// even after its name is deployed again, the evidence it recorded is still
// readable by ID for its own tenant and for no other tenant. Live by-name
// lookups keep returning only the live deployment's evidence.
func TestEvidenceStaysReadableByIDAfterItsDeploymentIsDeleted(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	const name = "evidence-by-id"
	first := recordEvidenceDeployment(t, ctx, s, name, 246, 315, "a")
	candidate, err := s.CreateCandidateRevision(ctx, "global", name, `{"model":"Qwen/Qwen3-0.6B-FP8"}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SetReleaseGuardPolicy(ctx, "global", name, domain.ReleaseGuardPolicy{Enabled: true, MinimumRequests: 5, MaxTTFTRegressionPercent: 10, MaxLatencyRegressionPercent: 12, MaxErrorRateIncrease: .005, MaxOutputThroughputDropPercent: 15}); err != nil {
		t.Fatal(err)
	}
	guard, err := s.EvaluateReleaseGuard(ctx, "global", name, 5*time.Minute)
	if err != nil || guard.ID == "" || guard.CandidateRevisionID != candidate.ID {
		t.Fatalf("guard=%+v err=%v", guard, err)
	}

	if err = s.DeleteDeploymentForTenant(ctx, "global", name); err != nil {
		t.Fatal(err)
	}
	var desired, observed string
	if err = s.QueryRowContext(ctx, `SELECT desired_state,observed_state FROM deployments WHERE id=?`, first.deployment.ID).Scan(&desired, &observed); err != nil || desired != "deleted" || observed != "deleted" {
		t.Fatalf("deployment after delete=(%s,%s,%v), want fully deleted", desired, observed, err)
	}
	// The name is free again; deploying it creates a new deployment.
	second := recordEvidenceDeployment(t, ctx, s, name, 100, 900, "b")
	if second.deployment.ID == first.deployment.ID {
		t.Fatal("redeploy reused the deleted deployment")
	}

	// Live by-name lookups see only the live deployment's evidence.
	benchmarks, err := s.BenchmarksForDeployment(ctx, "global", name, 10)
	if err != nil || len(benchmarks) != 1 || benchmarks[0].ID != second.benchmark.ID {
		t.Fatalf("live benchmarks=%+v err=%v", benchmarks, err)
	}
	quality, err := s.QualityEvidenceForDeployment(ctx, "global", name, 10)
	if err != nil || len(quality) != 1 || quality[0].ID != second.quality.ID {
		t.Fatalf("live quality evidence=%+v err=%v", quality, err)
	}
	if guards, guardErr := s.ReleaseGuardEvaluations(ctx, "global", name, 10); guardErr != nil || len(guards) != 0 {
		t.Fatalf("live Release Guard evaluations=%+v err=%v", guards, guardErr)
	}

	// By ID, the deleted deployment's evidence stays readable, unchanged and
	// still attached to its own deployment.
	byBenchmark, err := s.BenchmarksByIDs(ctx, "global", []string{first.benchmark.ID, "", first.benchmark.ID, second.benchmark.ID, "missing"})
	if err != nil || len(byBenchmark) != 2 {
		t.Fatalf("benchmarks by ID=%+v err=%v", byBenchmark, err)
	}
	if row := byBenchmark[first.benchmark.ID]; row.DeploymentID != first.deployment.ID || row.OutputTokenThroughput == nil || *row.OutputTokenThroughput != 246 || row.TTFTP95MS == nil || *row.TTFTP95MS != 315 || row.GPUCount == nil || *row.GPUCount != 1 || !strings.Contains(row.WorkloadJSON, "customer_observed") {
		t.Fatalf("deleted deployment's benchmark=%+v", row)
	}
	byQuality, err := s.QualityEvidenceByIDs(ctx, "global", []string{first.quality.ID})
	if err != nil || len(byQuality) != 1 || byQuality[first.quality.ID].DeploymentID != first.deployment.ID || byQuality[first.quality.ID].PayloadDigest != first.quality.PayloadDigest {
		t.Fatalf("quality by ID=%+v err=%v", byQuality, err)
	}
	byGuard, err := s.ReleaseGuardEvaluationsByIDs(ctx, "global", []string{guard.ID, guard.ID, ""})
	if err != nil || len(byGuard) != 1 || byGuard[guard.ID].DeploymentID != first.deployment.ID || byGuard[guard.ID].Decision != guard.Decision || byGuard[guard.ID].PolicyJSON == "" {
		t.Fatalf("Release Guard evaluations by ID=%+v err=%v", byGuard, err)
	}

	// Another tenant reads none of it, live or deleted.
	otherBenchmarks, err := s.BenchmarksByIDs(ctx, "other-tenant", []string{first.benchmark.ID, second.benchmark.ID})
	if err != nil || len(otherBenchmarks) != 0 {
		t.Fatalf("cross-tenant benchmarks=%+v err=%v", otherBenchmarks, err)
	}
	otherQuality, err := s.QualityEvidenceByIDs(ctx, "other-tenant", []string{first.quality.ID, second.quality.ID})
	if err != nil || len(otherQuality) != 0 {
		t.Fatalf("cross-tenant quality evidence=%+v err=%v", otherQuality, err)
	}
	otherGuard, err := s.ReleaseGuardEvaluationsByIDs(ctx, "other-tenant", []string{guard.ID})
	if err != nil || len(otherGuard) != 0 {
		t.Fatalf("cross-tenant Release Guard evaluations=%+v err=%v", otherGuard, err)
	}

	empty, err := s.BenchmarksByIDs(ctx, "global", []string{"", ""})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("empty benchmark read=%+v err=%v", empty, err)
	}
	tooMany := make([]string, maxEvidenceIDs+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("id-%d", i)
	}
	if _, err = s.BenchmarksByIDs(ctx, "global", tooMany); err == nil {
		t.Fatal("an unbounded benchmark ID list was accepted")
	}
	if _, err = s.ReleaseGuardEvaluationsByIDs(ctx, "global", tooMany); err == nil {
		t.Fatal("an unbounded Release Guard evaluation ID list was accepted")
	}
}

// TestCleanedCampaignReportKeepsMeasuredEvidence is the regression for a
// report that turned every metric unknown once cleanup deleted the candidate
// deployments: the report must read the evidence the candidates recorded,
// whatever happened to their deployments afterwards.
func TestCleanedCampaignReportKeepsMeasuredEvidence(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, ctx)
	controlProposalID, challengerProposalID := strings.Repeat("1", 64), strings.Repeat("2", 64)
	proposal, err := json.Marshal(optimizer.Proposal{SchemaVersion: "infercrane.optimizer.proposal/v1", Input: optimizer.Request{ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive"}, StartingPoint: &optimizer.StartingPoint{CandidateID: controlProposalID}})
	if err != nil {
		t.Fatal(err)
	}
	spec := func(args ...string) string {
		var draft optimizer.DeploymentDraft
		draft.Name, draft.Model.ID, draft.Runtime.Engine, draft.Runtime.Args = "qwen", "Qwen/Qwen3-0.6B", "vllm", args
		draft.Resources.GPU, draft.Resources.GPUCount, draft.Scaling.MaxReplicas = "RTX4090", 1, 1
		encoded, _ := json.Marshal(draft)
		return string(encoded)
	}
	campaign, _, err := s.CreateOptimizationCampaign(ctx, domain.OptimizationCampaign{TenantID: "global", IdempotencyKey: "cleaned-report", InputDigest: strings.Repeat("e", 64), ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive", Source: "fixture", Intent: optimizationcampaign.IntentNewEndpoint, ProposalJSON: string(proposal)}, []domain.OptimizationCandidateRun{
		{ProposalCandidateID: controlProposalID, Rank: 1, EvidenceState: "unmeasured", DeploymentSpecJSON: spec(), PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
		{ProposalCandidateID: challengerProposalID, Rank: 2, EvidenceState: "unmeasured", DeploymentSpecJSON: spec("--kv-cache-dtype", "fp8"), PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if campaign, err = s.ApproveOptimizationCampaign(ctx, "global", campaign.ID, "operator", 5, time.Now().Add(4*time.Hour)); err != nil {
		t.Fatal(err)
	}
	lab, err := s.RecordLabEvaluation(ctx, "global", domain.LabEvaluation{ModelIdentity: "Qwen/Qwen3-0.6B", AlgorithmVersion: "inference-lab-v3", InputJSON: `{"objective":"interactive"}`, ResultsJSON: `[]`, InputDigest: strings.Repeat("5", 64)})
	if err != nil {
		t.Fatal(err)
	}
	measured := map[string]evidenceDeployment{}
	for _, candidate := range campaign.Candidates {
		outputTPS, ttft, seed := 246.0, 315.0, "c"
		if candidate.ProposalCandidateID == challengerProposalID {
			outputTPS, ttft, seed = 193.0, 339.0, "d"
		}
		current := recordEvidenceDeployment(t, ctx, s, "qwen-opt-"+candidate.ID[:8], outputTPS, ttft, seed)
		measured[candidate.ID] = current
		state := optimizationcampaign.CandidateProposed
		for _, step := range []struct {
			to      string
			updates domain.OptimizationCandidateRun
		}{
			{optimizationcampaign.CandidateProvisioning, domain.OptimizationCandidateRun{}},
			{optimizationcampaign.CandidateReady, domain.OptimizationCandidateRun{DeploymentName: current.deployment.Name, RevisionID: current.deployment.ActiveRevisionID}},
			{optimizationcampaign.CandidateMeasuring, domain.OptimizationCandidateRun{}},
			{optimizationcampaign.CandidateValidating, domain.OptimizationCandidateRun{BenchmarkID: current.benchmark.ID}},
			{optimizationcampaign.CandidateRanked, domain.OptimizationCandidateRun{BenchmarkID: current.benchmark.ID, QualityEvidenceID: current.quality.ID}},
			{optimizationcampaign.CandidateRejected, domain.OptimizationCandidateRun{LabEvaluationID: lab.ID, FailureCode: "measured_candidate_superseded"}},
		} {
			if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, candidate.ID, state, step.to, step.updates); err != nil {
				t.Fatalf("transition %s -> %s: %v", state, step.to, err)
			}
			state = step.to
		}
		// Cleanup deletes the candidate deployment, then records the candidate cleaned.
		if err = s.DeleteDeploymentForTenant(ctx, "global", current.deployment.Name); err != nil {
			t.Fatal(err)
		}
		if _, err = s.TransitionOptimizationCandidate(ctx, "global", campaign.ID, candidate.ID, state, optimizationcampaign.CandidateCleaned, domain.OptimizationCandidateRun{}); err != nil {
			t.Fatal(err)
		}
	}

	report, err := optimizationreport.Load(ctx, s, "global", campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.CampaignState != optimizationcampaign.CampaignCleaned || report.Outcome.Kind != optimizationreport.OutcomeNoWinner {
		t.Fatalf("outcome=%+v state=%s", report.Outcome, report.CampaignState)
	}
	control := campaign.Candidates[0]
	if control.ProposalCandidateID != controlProposalID {
		control = campaign.Candidates[1]
	}
	for _, metric := range report.Metrics {
		switch metric.Key {
		case optimizationreport.MetricTTFTP95:
			if metric.Baseline == nil || *metric.Baseline != 315 {
				t.Fatalf("baseline p95 TTFT after cleanup=%v (%s), want 315", metric.Baseline, metric.Reason)
			}
		case optimizationreport.MetricTokensPerAccel:
			if metric.Baseline == nil || *metric.Baseline != 246 {
				t.Fatalf("baseline output tokens/s per GPU after cleanup=%v (%s), want 246", metric.Baseline, metric.Reason)
			}
		}
	}
	if len(report.Candidates) != 2 {
		t.Fatalf("candidates=%+v", report.Candidates)
	}
	for _, result := range report.Candidates {
		want := measured[result.ID].benchmark.OutputTokenThroughput
		if result.OutputTokensPerSecond == nil || *result.OutputTokensPerSecond != *want || strings.Contains(result.Reason, "could not be found") {
			t.Fatalf("candidate %s after cleanup: output tokens/s=%v reason=%q, want %v", result.ID, result.OutputTokensPerSecond, result.Reason, *want)
		}
	}
	cited := map[string]string{}
	for _, entry := range report.Appendix.Evidence {
		cited[entry.Kind] = entry.ID
	}
	if cited["baseline_benchmark"] != measured[control.ID].benchmark.ID || cited["baseline_quality"] != measured[control.ID].quality.ID {
		t.Fatalf("report evidence after cleanup=%+v", report.Appendix.Evidence)
	}
	if other, otherErr := optimizationreport.Load(ctx, s, "other-tenant", campaign.ID); !errors.Is(otherErr, domain.ErrNotFound) {
		t.Fatalf("another tenant loaded the report: outcome=%+v err=%v", other.Outcome, otherErr)
	}
}
