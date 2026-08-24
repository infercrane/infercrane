package optimizationcampaign

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/performanceprofile"
)

type benchmarkStoreFixture struct {
	resolved  domain.ResolvedDeployment
	revisions []domain.DeploymentRevision
	replicas  []domain.Replica
	artifact  domain.ModelArtifact
	recorded  domain.BenchmarkResult
}

func (f *benchmarkStoreFixture) ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error) {
	return f.resolved, nil
}
func (f *benchmarkStoreFixture) Revisions(context.Context, string, string) ([]domain.DeploymentRevision, error) {
	return f.revisions, nil
}
func (f *benchmarkStoreFixture) ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error) {
	return f.replicas, nil
}
func (f *benchmarkStoreFixture) ModelArtifactForRevision(context.Context, string, string) (domain.ModelArtifact, error) {
	return f.artifact, nil
}
func (f *benchmarkStoreFixture) RecordBenchmark(_ context.Context, row domain.BenchmarkResult) (domain.BenchmarkResult, error) {
	row.ID = "benchmark-1"
	f.recorded = row
	return row, nil
}

type aiperfFixture struct{ config benchmark.Config }

func (f *aiperfFixture) Run(_ context.Context, config benchmark.Config) (benchmark.Result, error) {
	f.config = config
	throughput, ttft := 420.0, 180.0
	return benchmark.Result{Tool: "aiperf", ToolVersion: "0.9", Command: "aiperf profile ...", Requests: config.Requests, Succeeded: config.Requests, OutputTokenThroughput: &throughput, TTFTP95MS: &ttft}, nil
}

func TestBenchmarkExecutorMeasuresExactCandidateRevisionAndPersistsProvenance(t *testing.T) {
	now := time.Now().UTC()
	draft := optimizer.DeploymentDraft{APIVersion: "infercrane.dev/v1", Kind: "Deployment", Name: "coder"}
	draft.Model.ID, draft.Model.Revision = "Qwen/Qwen3-32B", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	draft.Runtime.Engine, draft.Runtime.Version, draft.Runtime.Args = "vllm", "0.8.5.post1", []string{"--enable-prefix-caching"}
	draft.Compute.Mode, draft.Provider.Cloud, draft.Provider.Region, draft.Provider.Adapter = "elastic", "aws", "eu-central-1", "aws-ec2"
	draft.Resources.GPU, draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = "L40S", 1, 1
	draftJSON, _ := json.Marshal(draft)
	maxTTFT, maxTPOT := 250.0, 25.0
	proposalJSON, _ := json.Marshal(optimizer.Proposal{Input: optimizer.Request{MaxTTFTP95MS: &maxTTFT, MaxTPOTP95MS: &maxTPOT}})
	campaign := domain.OptimizationCampaign{ID: "campaign", TenantID: "tenant", ProposalJSON: string(proposalJSON)}
	candidate := domain.OptimizationCandidateRun{ID: "candidate", TenantID: "tenant", CampaignID: campaign.ID, DeploymentName: "coder-opt", RevisionID: "revision-candidate", DeploymentSpecJSON: string(draftJSON)}
	store := &benchmarkStoreFixture{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment", Name: candidate.DeploymentName}}, revisions: []domain.DeploymentRevision{{ID: candidate.RevisionID}}, replicas: []domain.Replica{{RevisionID: candidate.RevisionID, Endpoint: "https://candidate.internal/v1", Provider: "aws-ec2", Health: "healthy", LifecycleState: "ready"}}, artifact: domain.ModelArtifact{ID: "artifact", Repository: draft.Model.ID, ModelIdentity: draft.Model.ID + "@" + draft.Model.Revision}}
	runner := &aiperfFixture{}
	executor := BenchmarkExecutor{Store: store, Runner: runner, Costs: costAuthorityFixture{quote: CostQuote{HourlyUSD: 2.10, Source: "aws-price-list", ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour)}}, Backends: map[string]BenchmarkBackend{"aws-ec2": {APIKey: "worker-secret", APIKeyEnv: "INFERCRANE_WORKER_API_KEY"}}, AIPerfBinary: "aiperf", Now: func() time.Time { return now }}
	profile, _ := performanceprofile.Get("interactive")
	row, err := executor.Run(t.Context(), campaign, candidate, profile)
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != "benchmark-1" || store.recorded.RevisionID != candidate.RevisionID || store.recorded.ModelArtifactID != "artifact" || store.recorded.Provider != "aws" {
		t.Fatalf("wrong persisted identity: %+v", store.recorded)
	}
	if runner.config.Endpoint != "https://candidate.internal/v1" || runner.config.Model != draft.Model.ID || runner.config.TTFTSLOMS != maxTTFT || runner.config.TPOTSLOMS != maxTPOT || runner.config.APIKey != "worker-secret" {
		t.Fatalf("wrong AIPerf config: %+v", runner.config)
	}
	var cost map[string]any
	if json.Unmarshal([]byte(row.CostMetadataJSON), &cost) != nil || cost["source"] != "aws-price-list" || cost["revision_id"] != candidate.RevisionID {
		t.Fatalf("cost provenance missing: %s", row.CostMetadataJSON)
	}
	if string(store.recorded.WorkloadJSON) == "" || string(store.recorded.RuntimeConfigJSON) == "" {
		t.Fatal("workload/runtime provenance missing")
	}
}

func TestBenchmarkExecutorRejectsMutableRouteAndMissingDirectCandidate(t *testing.T) {
	now := time.Now().UTC()
	store, candidate := compositeFixture(t, IntentNewEndpoint, now)
	candidate.DeploymentName, candidate.RevisionID = "candidate", "revision"
	_ = store
	executorStore := &benchmarkStoreFixture{resolved: domain.ResolvedDeployment{Deployment: domain.Deployment{ID: "deployment", Name: candidate.DeploymentName, ActiveRevisionID: candidate.RevisionID}}, revisions: []domain.DeploymentRevision{{ID: candidate.RevisionID}}, artifact: domain.ModelArtifact{ID: "artifact", Repository: "model"}}
	profile, _ := performanceprofile.Get("interactive")
	executor := BenchmarkExecutor{Store: executorStore, Runner: &aiperfFixture{}, Costs: freshCost(now), Backends: map[string]BenchmarkBackend{"aws-ec2": {APIKey: "secret"}}, Now: func() time.Time { return now }}
	if _, err := executor.Run(t.Context(), store.campaign, candidate, profile); err == nil {
		t.Fatal("mutable deployment route was accepted without an exact healthy candidate endpoint")
	}
}
