package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/performanceprofile"
)

type BenchmarkStore interface {
	ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error)
	Revisions(context.Context, string, string) ([]domain.DeploymentRevision, error)
	ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error)
	ModelArtifactForRevision(context.Context, string, string) (domain.ModelArtifact, error)
	RecordBenchmark(context.Context, domain.BenchmarkResult) (domain.BenchmarkResult, error)
}

type BenchmarkBackend struct {
	APIKey, APIKeyEnv string
}

type AIPerfRunner interface {
	Run(context.Context, benchmark.Config) (benchmark.Result, error)
}

// BenchmarkExecutor sends a versioned synthetic workload directly to the
// exact candidate revision. It never relies on a mutable production route and
// never copies production prompts. Persisted evidence includes exact artifact,
// runtime, provider, workload, and sourced-cost identities.
type BenchmarkExecutor struct {
	Store        BenchmarkStore
	Runner       AIPerfRunner
	Costs        CostAuthority
	Backends     map[string]BenchmarkBackend
	AIPerfBinary string
	Now          func() time.Time
}

func (e BenchmarkExecutor) Run(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, profile performanceprofile.Profile) (domain.BenchmarkResult, error) {
	if e.Store == nil || e.Runner == nil || e.Costs == nil || candidate.DeploymentName == "" || candidate.RevisionID == "" {
		return domain.BenchmarkResult{}, errors.New("candidate benchmark requires store, runner, pricing, deployment, and revision")
	}
	draft, err := candidateDraft(candidate)
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	resolved, err := e.Store.ResolveForTenant(ctx, candidate.TenantID, candidate.DeploymentName)
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	revisions, err := e.Store.Revisions(ctx, candidate.TenantID, candidate.DeploymentName)
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	var revision domain.DeploymentRevision
	for _, row := range revisions {
		if row.ID == candidate.RevisionID {
			revision = row
			break
		}
	}
	if revision.ID == "" {
		return domain.BenchmarkResult{}, errors.New("candidate revision metadata is unavailable")
	}
	artifact, err := e.Store.ModelArtifactForRevision(ctx, candidate.TenantID, revision.ID)
	if err != nil {
		return domain.BenchmarkResult{}, fmt.Errorf("resolve immutable candidate artifact: %w", err)
	}
	replicas, err := e.Store.ReplicasForDeployment(ctx, candidate.TenantID, resolved.Deployment.ID)
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	endpoint, provider, ordinal := "", "", int(^uint(0)>>1)
	for _, replica := range replicas {
		if replica.RevisionID == revision.ID && replica.Endpoint != "" && replica.Health == "healthy" && (replica.LifecycleState == "ready" || replica.LifecycleState == "active") && replica.Ordinal < ordinal {
			endpoint, provider, ordinal = replica.Endpoint, replica.Provider, replica.Ordinal
		}
	}
	if endpoint == "" || provider == "" {
		return domain.BenchmarkResult{}, errors.New("candidate revision has no healthy ready direct endpoint")
	}
	backend, ok := e.Backends[provider]
	if !ok || backend.APIKey == "" {
		return domain.BenchmarkResult{}, fmt.Errorf("candidate benchmark credential is unavailable for provider %s", provider)
	}
	streaming := profile.Streaming
	started := e.now()
	measured, err := e.Runner.Run(ctx, benchmark.Config{Binary: e.AIPerfBinary, Endpoint: endpoint, APIKey: backend.APIKey, APIKeyEnv: backend.APIKeyEnv, Model: draft.Model.ID, Tokenizer: artifact.Repository, Requests: profile.Requests, Concurrency: profile.Concurrency, InputTokens: profile.InputTokens, OutputTokens: profile.OutputTokens, RandomSeed: 17, Streaming: &streaming, TTFTSLOMS: campaignTTFTSLO(campaign), TPOTSLOMS: campaignTPOTSLO(campaign)})
	ended := e.now()
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	quote, err := e.Costs.Quote(ctx, draft, ended)
	if err != nil {
		return domain.BenchmarkResult{}, fmt.Errorf("bind sourced candidate cost: %w", err)
	}
	workload, _ := json.Marshal(map[string]any{"endpoint_type": "chat", "streaming": profile.Streaming, "request_count": profile.Requests, "concurrency": profile.Concurrency, "random_seed": 17, "input_tokens": profile.InputTokens, "output_tokens": profile.OutputTokens, "profile": profile.Name, "profile_version": performanceprofile.Version, "ttft_slo_ms": campaignTTFTSLO(campaign), "tpot_slo_ms": campaignTPOTSLO(campaign), "server_token_count": true, "revision_selector": revision.ID, "direct_revision_validation": true})
	runtimeConfig, _ := json.Marshal(map[string]any{"args": draft.Runtime.Args})
	costMetadata, _ := json.Marshal(map[string]any{"available": true, "hourly": quote.HourlyUSD, "currency": "USD", "billing_unit": "hour", "source": quote.Source, "evidence_class": "provider_reported", "observed_at": quote.ObservedAt, "valid_until": quote.ValidUntil, "revision_id": revision.ID})
	gpuCount := draft.Resources.GPUCount
	if gpuCount == 0 {
		gpuCount = 1
	}
	row := domain.BenchmarkResult{TenantID: candidate.TenantID, DeploymentID: resolved.Deployment.ID, DeploymentName: candidate.DeploymentName, RevisionID: revision.ID, ModelArtifactID: artifact.ID, ModelIdentity: artifact.ModelIdentity, Runtime: draft.Runtime.Engine, RuntimeVersion: draft.Runtime.Version, RuntimeConfigJSON: string(runtimeConfig), Provider: draft.Provider.Cloud, Region: draft.Provider.Region, GPU: draft.Resources.GPU, GPUCount: &gpuCount, ComputeMode: draft.Compute.Mode, Tool: measured.Tool, ToolVersion: measured.ToolVersion, WorkloadJSON: string(workload), ReproductionCommand: measured.Command, RequestCount: measured.Requests, Succeeded: measured.Succeeded, Failed: measured.Failed, DurationSeconds: measured.DurationSeconds, RequestThroughput: measured.RequestThroughput, OutputTokenThroughput: measured.OutputTokenThroughput, TTFTP50MS: measured.TTFTP50MS, TTFTP95MS: measured.TTFTP95MS, TPOTP50MS: measured.TPOTP50MS, TPOTP95MS: measured.TPOTP95MS, LatencyP50MS: measured.LatencyP50MS, LatencyP95MS: measured.LatencyP95MS, Goodput: measured.Goodput, CostMetadataJSON: string(costMetadata), CreatedAt: ended}
	if row.Provider == "" {
		row.Provider = provider
	}
	if strings.TrimSpace(row.ComputeMode) == "" {
		row.ComputeMode = "elastic"
	}
	if ended.Before(started) {
		return domain.BenchmarkResult{}, errors.New("candidate benchmark clock moved backwards")
	}
	return e.Store.RecordBenchmark(ctx, row)
}

func (e BenchmarkExecutor) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func campaignTTFTSLO(campaign domain.OptimizationCampaign) float64 {
	request := campaignRequest(campaign)
	if request.MaxTTFTP95MS != nil {
		return *request.MaxTTFTP95MS
	}
	return 0
}

func campaignTPOTSLO(campaign domain.OptimizationCampaign) float64 {
	request := campaignRequest(campaign)
	if request.MaxTPOTP95MS != nil {
		return *request.MaxTPOTP95MS
	}
	return 0
}

func campaignRequest(campaign domain.OptimizationCampaign) optimizer.Request {
	var proposal optimizer.Proposal
	if json.Unmarshal([]byte(campaign.ProposalJSON), &proposal) != nil {
		return optimizer.Request{}
	}
	return proposal.Input
}
