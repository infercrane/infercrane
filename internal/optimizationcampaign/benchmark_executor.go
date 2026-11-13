package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

// BenchmarkExecutor measures the exact candidate revision directly. It never
// relies on a mutable production route and never copies production prompts.
// A customer-observed campaign replays its bound content-free Replay trace;
// other campaigns send the versioned synthetic profile. Each server is warmed,
// every arm runs measurementRunsPerArm times, and when the campaign control is
// live it is measured in the same window, interleaved with the candidate.
// Persisted evidence includes exact artifact, runtime, provider, workload,
// control comparison, and sourced-cost identities.
type BenchmarkExecutor struct {
	Store        BenchmarkStore
	Runner       AIPerfRunner
	Costs        CostAuthority
	Replays      ReplayTraceSource
	Backends     map[string]BenchmarkBackend
	AIPerfBinary string
	Now          func() time.Time
	// Entropy supplies the per-measurement seed base; nil uses crypto/rand.
	Entropy io.Reader
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
	target, provider, err := e.directTarget(ctx, candidate, resolved.Deployment.ID, draft)
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	plan, err := e.loadPlan(ctx, campaign, artifact, profile)
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	base, err := invocationBase(e.Entropy)
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	runs := &invocations{runner: e.Runner, binary: e.AIPerfBinary, plan: plan, base: base}
	if plan.trace != nil {
		if runs.dir, err = os.MkdirTemp("", "infercrane-campaign-replay-"); err != nil {
			return domain.BenchmarkResult{}, fmt.Errorf("create replay staging directory: %w", err)
		}
		defer os.RemoveAll(runs.dir)
	}
	control, controlEvidence, err := e.controlTarget(ctx, campaign, candidate)
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	started := e.now()
	measured, err := measureInterleaved(ctx, runs, target, control, controlEvidence)
	ended := e.now()
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	if ended.Before(started) {
		return domain.BenchmarkResult{}, errors.New("candidate benchmark clock moved backwards")
	}
	pooled, confidence := aggregate(measured.candidate)
	quote, err := e.Costs.Quote(ctx, candidate.TenantID, draft, ended)
	if err != nil {
		return domain.BenchmarkResult{}, fmt.Errorf("bind sourced candidate cost: %w", err)
	}
	workload := map[string]any{
		"endpoint_type": "chat", "streaming": plan.streaming, "request_count": plan.requests, "request_count_per_run": plan.requests,
		"profile_runs": measurementRunsPerArm, "warmup_requests": plan.warmup, "concurrency": plan.concurrency,
		"random_seed": measurementSeed, "seed_policy": "unique_per_invocation", "measurement_method": MeasurementMethod,
		"profile": profile.Name, "profile_version": performanceprofile.Version, "ttft_slo_ms": plan.ttftSLO, "tpot_slo_ms": plan.tpotSLO,
		"server_token_count": true, "revision_selector": revision.ID, "direct_revision_validation": true,
		"confidence_95": confidence, "measured": measured.evidence,
	}
	if plan.trace != nil {
		workload["workload_source"], workload["replay"] = "customer_replay", plan.trace.Properties
	} else {
		workload["workload_source"], workload["input_tokens"], workload["output_tokens"] = "synthetic_profile", plan.inputTokens, plan.outputTokens
	}
	workloadJSON, err := json.Marshal(workload)
	if err != nil {
		return domain.BenchmarkResult{}, fmt.Errorf("encode campaign workload evidence: %w", err)
	}
	runtimeConfig, _ := json.Marshal(map[string]any{"args": draft.Runtime.Args})
	costMetadata, _ := json.Marshal(map[string]any{"available": true, "hourly": quote.HourlyUSD, "currency": "USD", "billing_unit": "hour", "source": quote.Source, "evidence_class": "provider_reported", "observed_at": quote.ObservedAt, "valid_until": quote.ValidUntil, "rate_locked": quote.Locked, "revision_id": revision.ID})
	gpuCount := draft.Resources.GPUCount
	if gpuCount == 0 {
		gpuCount = 1
	}
	row := domain.BenchmarkResult{TenantID: candidate.TenantID, DeploymentID: resolved.Deployment.ID, DeploymentName: candidate.DeploymentName, RevisionID: revision.ID, ModelArtifactID: artifact.ID, ModelIdentity: artifact.ModelIdentity, Runtime: draft.Runtime.Engine, RuntimeVersion: draft.Runtime.Version, RuntimeConfigJSON: string(runtimeConfig), Provider: draft.Provider.Cloud, Region: draft.Provider.Region, GPU: draft.Resources.GPU, GPUCount: &gpuCount, ComputeMode: draft.Compute.Mode, Tool: pooled.Tool, ToolVersion: pooled.ToolVersion, WorkloadJSON: string(workloadJSON), ReproductionCommand: pooled.Command, RequestCount: pooled.Requests, Succeeded: pooled.Succeeded, Failed: pooled.Failed, DurationSeconds: pooled.DurationSeconds, RequestThroughput: pooled.RequestThroughput, OutputTokenThroughput: pooled.OutputTokenThroughput, TTFTP50MS: pooled.TTFTP50MS, TTFTP95MS: pooled.TTFTP95MS, TPOTP50MS: pooled.TPOTP50MS, TPOTP95MS: pooled.TPOTP95MS, LatencyP50MS: pooled.LatencyP50MS, LatencyP95MS: pooled.LatencyP95MS, Goodput: pooled.Goodput, CostMetadataJSON: string(costMetadata), CreatedAt: ended}
	if row.Provider == "" {
		row.Provider = provider
	}
	if strings.TrimSpace(row.ComputeMode) == "" {
		row.ComputeMode = "elastic"
	}
	return e.Store.RecordBenchmark(ctx, row)
}

// directTarget selects the lowest-ordinal healthy ready replica of one exact
// revision and the server-side credential for its provider.
func (e BenchmarkExecutor) directTarget(ctx context.Context, candidate domain.OptimizationCandidateRun, deploymentID string, draft optimizer.DeploymentDraft) (armTarget, string, error) {
	replicas, err := e.Store.ReplicasForDeployment(ctx, candidate.TenantID, deploymentID)
	if err != nil {
		return armTarget{}, "", err
	}
	endpoint, provider, ordinal := "", "", int(^uint(0)>>1)
	for _, replica := range replicas {
		if replica.RevisionID == candidate.RevisionID && replica.Endpoint != "" && replica.Health == "healthy" && (replica.LifecycleState == "ready" || replica.LifecycleState == "active") && replica.Ordinal < ordinal {
			endpoint, provider, ordinal = replica.Endpoint, replica.Provider, replica.Ordinal
		}
	}
	if endpoint == "" || provider == "" {
		return armTarget{}, "", fmt.Errorf("candidate %w", errNoDirectEndpoint)
	}
	backend, ok := e.Backends[provider]
	if !ok || backend.APIKey == "" {
		return armTarget{}, "", fmt.Errorf("%w: benchmark credential is unavailable for provider %s", errNoDirectEndpoint, provider)
	}
	// The runtime contract declares a Prometheus path only when the server
	// exposes one; without it, runtime metrics stay unknown.
	return armTarget{candidateID: candidate.ID, revisionID: candidate.RevisionID, model: draft.Model.ID, endpoint: endpoint, apiKey: backend.APIKey, apiKeyEnv: backend.APIKeyEnv, serverMetrics: draft.Runtime.Workload.MetricsPath != ""}, provider, nil
}

// controlTarget resolves the campaign control's live endpoint. The control is
// optional evidence: when it is this candidate, not yet provisioned, or no
// longer serving, the candidate is measured alone and the reason is kept. A
// store failure is returned instead, so a transient error is retried rather
// than persisted as a weaker measurement that adoption would then reuse.
func (e BenchmarkExecutor) controlTarget(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) (*armTarget, ControlEvidence, error) {
	control, found := CampaignControl(campaign)
	if !found {
		return nil, ControlEvidence{Role: ControlRoleUnavailable, Reason: "the campaign has no control candidate"}, nil
	}
	evidence := ControlEvidence{Role: ControlRoleUnavailable, CandidateID: control.ID, RevisionID: control.RevisionID}
	if control.ID == candidate.ID {
		evidence.Role = ControlRoleCandidate
		return nil, evidence, nil
	}
	if control.DeploymentName == "" || control.RevisionID == "" || !liveControlState(control.State) {
		evidence.Reason = "the control candidate is not serving (state " + control.State + ")"
		return nil, evidence, nil
	}
	draft, err := candidateDraft(control)
	if err != nil {
		evidence.Reason = "the control candidate spec is invalid"
		return nil, evidence, nil
	}
	resolved, err := e.Store.ResolveForTenant(ctx, control.TenantID, control.DeploymentName)
	if errors.Is(err, domain.ErrNotFound) {
		evidence.Reason = "the control deployment no longer exists"
		return nil, evidence, nil
	}
	if err != nil {
		return nil, evidence, fmt.Errorf("resolve campaign control deployment: %w", err)
	}
	target, _, err := e.directTarget(ctx, control, resolved.Deployment.ID, draft)
	if errors.Is(err, errNoDirectEndpoint) {
		evidence.Reason = boundedReason(err)
		return nil, evidence, nil
	}
	if err != nil {
		return nil, evidence, fmt.Errorf("resolve campaign control endpoint: %w", err)
	}
	evidence.Role = ControlRoleBracketed
	return &target, evidence, nil
}

// errNoDirectEndpoint means the revision has no healthy ready replica or no
// server-side credential, a state rather than a transient failure.
var errNoDirectEndpoint = errors.New("revision has no healthy ready direct endpoint")

func liveControlState(state string) bool {
	switch state {
	case CandidateReady, CandidateMeasuring, CandidateValidating, CandidateRanked, CandidateGuarding, CandidateGuardPassed, CandidateQualified:
		return true
	default:
		return false
	}
}

// loadPlan fixes the load every arm receives. A bound customer replay that
// cannot be resolved or verified fails closed: synthetic load would silently
// stand in for the customer's prefix and session structure.
func (e BenchmarkExecutor) loadPlan(ctx context.Context, campaign domain.OptimizationCampaign, artifact domain.ModelArtifact, profile performanceprofile.Profile) (loadPlan, error) {
	request := campaignRequest(campaign)
	plan := loadPlan{tokenizer: artifact.Repository, requests: profile.Requests, concurrency: profile.Concurrency, inputTokens: profile.InputTokens, outputTokens: profile.OutputTokens, streaming: profile.Streaming, ttftSLO: campaignTTFTSLO(campaign), tpotSLO: campaignTPOTSLO(campaign)}
	plan.warmup = warmupRequests(plan.concurrency)
	rendered, err := resolveBoundReplay(ctx, e.Replays, campaign.TenantID, request)
	if err != nil {
		return loadPlan{}, err
	}
	if rendered == nil {
		return plan, nil
	}
	plan.trace, plan.requests = rendered, rendered.Requests()
	plan.inputTokens, plan.outputTokens = 0, 0
	return plan, nil
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
