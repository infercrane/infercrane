package optimizationcampaign

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizer"
)

func rankingFixture(t *testing.T, objective string, maxCost *float64) (domain.OptimizationCampaign, []domain.BenchmarkResult) {
	t.Helper()
	request := optimizer.Request{ModelIdentity: "qwen3-8b", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Runtimes: []string{"vllm"}, Objective: objective, WorkloadProfile: "interactive", MaxHourlyCost: maxCost, MaxCandidates: 2}
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := optimizer.NewCatalogSource(curatedrecipe.All(), registry.Snapshot()).Propose(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	// The catalog fixture can expose one candidate; duplicate the immutable
	// shape with a distinct identity to exercise measured ranking independently
	// from proposal ordering.
	if len(proposal.Candidates) == 1 {
		second := proposal.Candidates[0]
		second.ID = strings.Repeat("b", 64)
		second.Rank = 2
		second.Deployment.Name += "-second"
		proposal.Candidates = append(proposal.Candidates, second)
	}
	encoded, _ := json.Marshal(proposal)
	campaign := domain.OptimizationCampaign{ID: "campaign", TenantID: "tenant", InputDigest: proposal.InputDigest, ModelIdentity: proposal.Input.ModelIdentity, Objective: proposal.Input.Objective, ProposalJSON: string(encoded)}
	for index, candidate := range proposal.Candidates[:2] {
		campaign.Candidates = append(campaign.Candidates, domain.OptimizationCandidateRun{ID: "candidate-" + string(rune('a'+index)), TenantID: campaign.TenantID, CampaignID: campaign.ID, ProposalCandidateID: candidate.ID, State: CandidateRanked, RevisionID: "revision-" + string(rune('a'+index)), BenchmarkID: "benchmark-" + string(rune('a'+index)), QualityEvidenceID: "quality-" + string(rune('a'+index))})
	}
	ttftA, ttftB, throughputA, throughputB := 220.0, 140.0, 60.0, 80.0
	costA, costB := 2.0, 3.0
	workload := `{"profile":"interactive","request_count":100,"random_seed":17}`
	benchmarks := []domain.BenchmarkResult{
		{ID: "benchmark-a", RevisionID: "revision-a", ModelIdentity: campaign.ModelIdentity, Provider: "aws", Region: "eu-central-1", GPU: "L40S", Runtime: "vllm", ComputeMode: "elastic", RequestCount: 100, TTFTP95MS: &ttftA, OutputTokenThroughput: &throughputA, WorkloadJSON: workload, CostMetadataJSON: costJSON(costA)},
		{ID: "benchmark-b", RevisionID: "revision-b", ModelIdentity: campaign.ModelIdentity, Provider: "aws", Region: "eu-central-1", GPU: "L40S", Runtime: "vllm", ComputeMode: "elastic", RequestCount: 100, TTFTP95MS: &ttftB, OutputTokenThroughput: &throughputB, WorkloadJSON: workload, CostMetadataJSON: costJSON(costB)},
	}
	return campaign, benchmarks
}

func costJSON(hourly float64) string {
	encoded, _ := json.Marshal(map[string]any{"available": true, "hourly": hourly, "source": "aws-price-list"})
	return string(encoded)
}

func TestRankMeasuredCampaignIgnoresProposalOrder(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-b"] != RankSelect || result.Decisions["candidate-a"] != RankSupersede {
		t.Fatalf("unexpected measured rank: %+v", result.Decisions)
	}
	if result.Evaluation.AlgorithmVersion == "" || result.Evaluation.InputDigest == "" {
		t.Fatalf("ranking did not retain immutable Lab evidence: %+v", result.Evaluation)
	}
}

func TestRankMeasuredCampaignFailsClosedForUnlikeWorkload(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	benchmarks[1].WorkloadJSON = `{"profile":"interactive","request_count":32,"random_seed":17}`
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-a"] != RankInconclusive || result.Decisions["candidate-b"] != RankInconclusive {
		t.Fatalf("unlike workloads were ranked: %+v", result.Decisions)
	}
}

func TestRankMeasuredCampaignRejectsAllCandidatesOutsideCostBoundary(t *testing.T) {
	maxCost := 1.0
	campaign, benchmarks := rankingFixture(t, "cost-efficiency", &maxCost)
	result, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decisions["candidate-a"] != RankReject || result.Decisions["candidate-b"] != RankReject {
		t.Fatalf("cost violations were not rejected: %+v", result.Decisions)
	}
}

func TestRankMeasuredCampaignRejectsMismatchedBenchmarkIdentity(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	benchmarks[0].RevisionID = "another-revision"
	if _, err := RankMeasuredCampaign(campaign, benchmarks); err == nil {
		t.Fatal("mismatched benchmark identity was accepted")
	}
}
