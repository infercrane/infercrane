package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/performanceprofile"
)

type compositeStoreFixture struct {
	campaign       domain.OptimizationCampaign
	operations     map[string]domain.Operation
	resolved       domain.ResolvedDeployment
	benchmarks     []domain.BenchmarkResult
	quality        []domain.QualityEvidence
	guard          domain.ReleaseGuardEvaluation
	cloudSubmits   int
	rolloutSubmits int
	deleteSubmits  int
}

func (f *compositeStoreFixture) OptimizationCampaign(_ context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	if f.campaign.TenantID != tenant || f.campaign.ID != id {
		return domain.OptimizationCampaign{}, domain.ErrNotFound
	}
	return f.campaign, nil
}
func (f *compositeStoreFixture) TransitionOptimizationCandidate(context.Context, string, string, string, string, string, domain.OptimizationCandidateRun) (domain.OptimizationCandidateRun, error) {
	return domain.OptimizationCandidateRun{}, nil
}
func (f *compositeStoreFixture) SubmitCloudDeployment(_ context.Context, deployment domain.Deployment, operation domain.Operation) (domain.Deployment, domain.Operation, bool, error) {
	f.cloudSubmits++
	return deployment, f.operation(operation), f.cloudSubmits == 1, nil
}
func (f *compositeStoreFixture) SubmitDeploymentDelete(_ context.Context, _, _, _ string, operation domain.Operation) (domain.Operation, bool, error) {
	f.deleteSubmits++
	return f.operation(operation), f.deleteSubmits == 1, nil
}
func (f *compositeStoreFixture) EnqueueOperation(_ context.Context, operation domain.Operation) (domain.Operation, bool, error) {
	f.rolloutSubmits++
	return f.operation(operation), f.rolloutSubmits == 1, nil
}
func (f *compositeStoreFixture) operation(operation domain.Operation) domain.Operation {
	if f.operations == nil {
		f.operations = map[string]domain.Operation{}
	}
	if existing, ok := f.operations[operation.IdempotencyKey]; ok {
		return existing
	}
	operation.ID, operation.Status = operation.IdempotencyKey, "pending"
	f.operations[operation.IdempotencyKey] = operation
	return operation
}
func (f *compositeStoreFixture) ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error) {
	if f.resolved.Deployment.ID == "" {
		return domain.ResolvedDeployment{}, domain.ErrNotFound
	}
	return f.resolved, nil
}
func (f *compositeStoreFixture) BenchmarksForDeployment(context.Context, string, string, int) ([]domain.BenchmarkResult, error) {
	return f.benchmarks, nil
}
func (f *compositeStoreFixture) QualityEvidenceForDeployment(context.Context, string, string, int) ([]domain.QualityEvidence, error) {
	return f.quality, nil
}
func (f *compositeStoreFixture) ReleaseGuardEvaluations(context.Context, string, string, int) ([]domain.ReleaseGuardEvaluation, error) {
	if f.guard.ID == "" {
		return nil, nil
	}
	return []domain.ReleaseGuardEvaluation{f.guard}, nil
}
func (f *compositeStoreFixture) EvaluateReleaseGuard(context.Context, string, string, time.Duration) (domain.ReleaseGuardEvaluation, error) {
	if f.guard.ID == "" {
		return domain.ReleaseGuardEvaluation{}, errors.New("guard unavailable")
	}
	return f.guard, nil
}

type costAuthorityFixture struct{ quote CostQuote }

func (f costAuthorityFixture) Quote(context.Context, optimizer.DeploymentDraft, time.Time) (CostQuote, error) {
	return f.quote, nil
}

type benchmarkFixture struct {
	row   domain.BenchmarkResult
	calls int
}

func (f *benchmarkFixture) Run(context.Context, domain.OptimizationCampaign, domain.OptimizationCandidateRun, performanceprofile.Profile) (domain.BenchmarkResult, error) {
	f.calls++
	return f.row, nil
}

func TestCompositeDriverRequiresCostAuthorityBeforeAnyMutation(t *testing.T) {
	now := time.Now().UTC()
	store, candidate := compositeFixture(t, IntentNewEndpoint, now)
	driver := CompositeDriver{Store: store, Now: func() time.Time { return now }}
	_, err := driver.Provision(t.Context(), candidate.ID, candidate, Budget{MaxCostUSD: 10, ExpiresAt: now.Add(time.Hour)})
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Code != "optimization_cost_authority_unavailable" || store.cloudSubmits != 0 {
		t.Fatalf("missing cost authority reached mutation: err=%v submits=%d", err, store.cloudSubmits)
	}
}

func TestCompositeDriverAdoptsNewEndpointChildAfterLostResponse(t *testing.T) {
	now := time.Now().UTC()
	store, candidate := compositeFixture(t, IntentNewEndpoint, now)
	driver := CompositeDriver{Store: store, Costs: freshCost(now), Now: func() time.Time { return now }}
	_, err := driver.Provision(t.Context(), candidate.ID, candidate, Budget{MaxCostUSD: 5, ExpiresAt: now.Add(time.Hour)})
	assertRetryableCode(t, err, "optimization_child_pending")
	key := childKey(candidate.ID, "provision")
	completed := store.operations[key]
	completed.Status, completed.ResultJSON = "succeeded", `{"ready":true}`
	store.operations[key] = completed
	name := candidateDeploymentName("qwen-interactive", candidate.ID)
	store.resolved = domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment-1", Name: name, ActiveRevisionID: "revision-1"}}
	result, err := driver.Provision(t.Context(), candidate.ID, candidate, Budget{MaxCostUSD: 5, ExpiresAt: now.Add(time.Hour)})
	if err != nil || result.DeploymentName != name || result.RevisionID != "revision-1" || store.cloudSubmits != 2 {
		t.Fatalf("lost response was not adopted: result=%+v err=%v submits=%d", result, err, store.cloudSubmits)
	}
}

func TestCompositeDriverEvolutionUsesDurableCreateThenProvision(t *testing.T) {
	now := time.Now().UTC()
	store, candidate := compositeFixture(t, IntentEvolveEndpoint, now)
	driver := CompositeDriver{Store: store, Costs: freshCost(now), Now: func() time.Time { return now }}
	_, err := driver.Provision(t.Context(), candidate.ID, candidate, Budget{MaxCostUSD: 5, ExpiresAt: now.Add(time.Hour)})
	assertRetryableCode(t, err, "optimization_child_pending")
	createKey := childKey(candidate.ID, "create-revision")
	create := store.operations[createKey]
	create.Status, create.ResultJSON = "succeeded", `{"candidate_id":"revision-candidate"}`
	store.operations[createKey] = create
	_, err = driver.Provision(t.Context(), candidate.ID, candidate, Budget{MaxCostUSD: 5, ExpiresAt: now.Add(time.Hour)})
	assertRetryableCode(t, err, "optimization_child_pending")
	provisionKey := childKey(candidate.ID, "provision")
	provision := store.operations[provisionKey]
	provision.Status = "succeeded"
	store.operations[provisionKey] = provision
	result, err := driver.Provision(t.Context(), candidate.ID, candidate, Budget{MaxCostUSD: 5, ExpiresAt: now.Add(time.Hour)})
	if err != nil || result.DeploymentName != "production" || result.RevisionID != "revision-candidate" {
		t.Fatalf("evolution child operations did not converge: %+v err=%v", result, err)
	}
}

func TestCompositeDriverAdoptsExactBenchmarkAndQualityEvidence(t *testing.T) {
	now := time.Now().UTC()
	store, candidate := compositeFixture(t, IntentNewEndpoint, now)
	candidate.DeploymentName, candidate.RevisionID = "candidate", "revision-1"
	workload, _ := json.Marshal(map[string]any{"profile": "interactive", "profile_version": performanceprofile.Version})
	store.benchmarks = []domain.BenchmarkResult{{ID: "benchmark-1", RevisionID: candidate.RevisionID, WorkloadJSON: string(workload), CostMetadataJSON: `{"available":true}`, RequestCount: 20, CreatedAt: now.Add(time.Second)}}
	benchmark := &benchmarkFixture{row: domain.BenchmarkResult{ID: "unexpected"}}
	driver := CompositeDriver{Store: store, Benchmark: benchmark, Costs: freshCost(now), Now: func() time.Time { return now }}
	measured, err := driver.Measure(t.Context(), candidate.ID, candidate, Budget{MaxCostUSD: 5, ExpiresAt: now.Add(time.Hour)})
	if err != nil || measured.BenchmarkID != "benchmark-1" || benchmark.calls != 0 {
		t.Fatalf("exact persisted benchmark was not adopted: %+v err=%v calls=%d", measured, err, benchmark.calls)
	}
	store.quality = []domain.QualityEvidence{{ID: "quality-old", RevisionID: candidate.RevisionID, Passed: true, EvaluatedAt: now.Add(-time.Second)}, {ID: "quality-1", RevisionID: candidate.RevisionID, Passed: true, EvaluatedAt: now.Add(time.Second)}}
	validated, err := driver.Validate(t.Context(), candidate.ID, candidate)
	if err != nil || !validated.Passed || validated.QualityEvidenceID != "quality-1" {
		t.Fatalf("exact fresh quality evidence was not selected: %+v err=%v", validated, err)
	}
}

func TestCompositeDriverNeverGuardsNewEndpointAndPreservesGuardDecision(t *testing.T) {
	now := time.Now().UTC()
	store, candidate := compositeFixture(t, IntentNewEndpoint, now)
	candidate.DeploymentName, candidate.RevisionID = "candidate", "revision-1"
	driver := CompositeDriver{Store: store}
	if _, err := driver.Guard(t.Context(), candidate.ID, candidate); err == nil {
		t.Fatal("new endpoint campaign fabricated a Release Guard baseline")
	}
	store.campaign.Intent, store.campaign.TargetDeployment = IntentEvolveEndpoint, "production"
	candidate.DeploymentName = "production"
	store.guard = domain.ReleaseGuardEvaluation{ID: "guard-1", CandidateRevisionID: candidate.RevisionID, Decision: "REJECT", CreatedAt: now.Add(time.Second)}
	result, err := driver.Guard(t.Context(), candidate.ID, candidate)
	if err != nil || result.EvaluationID != "guard-1" || result.Decision != "REJECT" {
		t.Fatalf("guard evidence was not preserved: %+v err=%v", result, err)
	}
}

func compositeFixture(t *testing.T, intent string, now time.Time) (*compositeStoreFixture, domain.OptimizationCandidateRun) {
	t.Helper()
	draft := optimizer.DeploymentDraft{APIVersion: "infercrane.dev/v1", Kind: "Deployment", Name: "qwen-interactive"}
	draft.Model.ID, draft.Model.Revision = "Qwen/Qwen3-8B", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	draft.Runtime.Engine, draft.Runtime.Version = "vllm", "0.8.5.post1"
	draft.Compute.Mode, draft.Resources.GPU = "elastic", "L40S"
	draft.Provider.Cloud, draft.Provider.Adapter, draft.Provider.Region = "aws", "aws-ec2", "eu-central-1"
	draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = 1, 1
	draft.Routing.Strategy = "round-robin"
	encodedDraft, _ := json.Marshal(draft)
	proposalCandidate := optimizer.Candidate{ID: "proposal-1", BenchmarkProfile: "interactive", Deployment: draft}
	proposalJSON, _ := json.Marshal(optimizer.Proposal{Candidates: []optimizer.Candidate{proposalCandidate}})
	campaign := domain.OptimizationCampaign{ID: "campaign-1", TenantID: "tenant", Intent: intent, TargetDeployment: "", ProposalJSON: string(proposalJSON)}
	if intent == IntentEvolveEndpoint {
		campaign.TargetDeployment = "production"
	}
	candidate := domain.OptimizationCandidateRun{ID: "candidate-12345678", TenantID: "tenant", CampaignID: campaign.ID, ProposalCandidateID: proposalCandidate.ID, DeploymentSpecJSON: string(encodedDraft), UpdatedAt: now}
	campaign.Candidates = []domain.OptimizationCandidateRun{candidate}
	return &compositeStoreFixture{campaign: campaign}, candidate
}

func freshCost(now time.Time) costAuthorityFixture {
	return costAuthorityFixture{quote: CostQuote{HourlyUSD: 1.5, Source: "provider-price-list", ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour)}}
}

func assertRetryableCode(t *testing.T, err error, code string) {
	t.Helper()
	var failure operations.Failure
	if !errors.As(err, &failure) || !failure.Retryable || failure.Code != code {
		t.Fatalf("error=%v, want retryable %s", err, code)
	}
}
