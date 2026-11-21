package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/benchmark"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/performanceprofile"
	"github.com/infercrane/infercrane/internal/support"
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
	// Progress records each invocation while the candidate measures; nil
	// records nothing. It is display evidence and never affects the result.
	Progress MeasurementProgressSink
	// Checkpoints stores the resumable measurement after every complete pair
	// and at an interruption; nil keeps nothing, so a stopped measurement
	// starts over.
	Checkpoints MeasurementCheckpointSink
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
	control, controlEvidence, err := e.controlTarget(ctx, campaign, candidate)
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	if control != nil {
		controlEvidence.ServerMetrics = control.serverMetrics
	}
	state, err := e.checkpoint(campaign, candidate, profile, target, controlEvidence, plan)
	if err != nil {
		return domain.BenchmarkResult{}, err
	}
	if state.Control.Role != ControlRoleBracketed {
		// The control's role is fixed when the measurement starts. A control
		// that comes up later is not added mid-measurement.
		control = nil
	}
	runs := &invocations{runner: e.Runner, binary: e.AIPerfBinary, plan: plan}
	if plan.trace != nil {
		if runs.dir, err = os.MkdirTemp("", "infercrane-campaign-replay-"); err != nil {
			return domain.BenchmarkResult{}, fmt.Errorf("create replay staging directory: %w", err)
		}
		defer os.RemoveAll(runs.dir)
	}
	started := e.now()
	var tracker *progressTracker
	if e.Progress != nil {
		tracker = newProgressTracker(e.Progress, candidate, e.now)
	}
	session := measurementSession{runs: runs, candidate: target, control: control, state: &state, sink: e.Checkpoints, owner: candidate, tracker: tracker, now: e.now,
		confirm: e.confirmation(ctx, campaign, candidate, draft), keepControl: campaign.Intent == IntentEvolveEndpoint}
	measured, err := session.run(ctx)
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
	runtimeConfig, _ := json.Marshal(support.BenchmarkRuntimeConfig(draft.Runtime.Args, draft.Runtime.Env))
	costMetadata, _ := json.Marshal(map[string]any{"available": true, "hourly": quote.HourlyUSD, "currency": "USD", "billing_unit": "hour", "source": quote.Source, "evidence_class": "provider_reported", "observed_at": quote.ObservedAt, "valid_until": quote.ValidUntil, "rate_locked": quote.Locked, "revision_id": revision.ID})
	gpuCount := draft.Resources.GPUCount
	if gpuCount == 0 {
		gpuCount = 1
	}
	row := domain.BenchmarkResult{TenantID: candidate.TenantID, DeploymentID: resolved.Deployment.ID, DeploymentName: candidate.DeploymentName, RevisionID: revision.ID, ModelArtifactID: artifact.ID, ModelIdentity: artifact.ModelIdentity, Runtime: draft.Runtime.Engine, RuntimeVersion: draft.Runtime.Version, RuntimeConfigJSON: string(runtimeConfig), Provider: draft.Provider.Cloud, Region: draft.Provider.Region, GPU: draft.Resources.GPU, GPUCount: &gpuCount, ComputeMode: draft.Compute.Mode, Tool: pooled.Tool, ToolVersion: pooled.ToolVersion, WorkloadJSON: string(workloadJSON), ReproductionCommand: pooled.Command, RequestCount: pooled.Requests, Succeeded: pooled.Succeeded, Failed: pooled.Failed, DurationSeconds: pooled.DurationSeconds, RequestThroughput: pooled.RequestThroughput, OutputTokenThroughput: pooled.OutputTokenThroughput, TTFTP50MS: pooled.TTFTP50MS, TTFTP95MS: pooled.TTFTP95MS, TPOTP50MS: pooled.TPOTP50MS, TPOTP95MS: pooled.TPOTP95MS, LatencyP50MS: pooled.LatencyP50MS, LatencyP95MS: pooled.LatencyP95MS, TTFTP99MS: pooled.TTFTP99MS, TPOTP99MS: pooled.TPOTP99MS, LatencyP99MS: pooled.LatencyP99MS, StreamGapP99MS: pooled.StreamGapP99MS, StreamGapMaxMS: pooled.StreamGapMaxMS, Goodput: pooled.Goodput, CostMetadataJSON: string(costMetadata), CreatedAt: ended}
	if row.Provider == "" {
		row.Provider = provider
	}
	if strings.TrimSpace(row.ComputeMode) == "" {
		row.ComputeMode = "elastic"
	}
	return e.Store.RecordBenchmark(ctx, row)
}

// checkpoint returns the measurement to continue: the stored one when it
// belongs to these exact arms and load, otherwise a fresh one with a new
// invocation base. A stored checkpoint whose interruption is still pending is
// never measured from; the driver resolves it first.
func (e BenchmarkExecutor) checkpoint(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, profile performanceprofile.Profile, target armTarget, control ControlEvidence, plan loadPlan) (MeasurementCheckpoint, error) {
	identity := measurementIdentity(target, control, plan, profile.Name+"@"+performanceprofile.Version, campaignRequest(campaign).WorkloadFingerprint)
	if stored, ok := DecodeMeasurementCheckpoint(candidate.MeasurementCheckpointJSON); ok {
		if stored.Pending() != nil {
			return MeasurementCheckpoint{}, errCheckpointPending
		}
		resumable := stored.Identity == identity
		if owner, found := CampaignControl(campaign); !resumable && found && stored.Control.Role == ControlRoleBracketed && control.Role == ControlRoleUnavailable && control.CandidateID == stored.Control.CandidateID && liveControlState(owner.State) {
			// The recovered control is still coming up. Measuring without it
			// would throw away every bracketed pair, so wait for it.
			return MeasurementCheckpoint{}, operations.Retryable("optimization_control_not_serving", errors.New("the recovered control has no healthy endpoint yet"))
		}
		if !resumable && len(stored.Segments) > 0 {
			// A recovered control keeps its candidate and revision, so the
			// identity matches. A role change means the control is gone; the
			// identity then differs and the stored control runs would
			// misdescribe the comparison.
			slog.Warn("optimization measurement checkpoint does not match the current arms or load; measuring from the start", "campaign_id", candidate.CampaignID, "candidate_id", candidate.ID)
		}
		if resumable {
			return stored, nil
		}
	}
	base, err := invocationBase(e.Entropy)
	if err != nil {
		return MeasurementCheckpoint{}, err
	}
	return MeasurementCheckpoint{Version: measurementCheckpointVersion, Method: MeasurementMethod, Identity: identity, InvocationBase: base, Control: control, Comparisons: campaignComparisons(campaign)}, nil
}

// confirmation decides, from a finished first window, whether the candidate
// gained on the campaign's objective and must be measured again on fresh
// servers before the gain counts. It applies the ranking's own objective
// rule. A cost objective also needs both setups' sourced hourly rates;
// without them the ranking cannot decide on cost either, so nothing is
// confirmed.
func (e BenchmarkExecutor) confirmation(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft) func(MeasuredEvidence) bool {
	metric, _, costScale := objectiveMetric(campaignRequest(campaign).Objective)
	if metric == "" {
		return nil
	}
	return func(evidence MeasuredEvidence) bool {
		ratio := 1.0
		if costScale {
			control, found := CampaignControl(campaign)
			if !found {
				return false
			}
			controlDraft, err := candidateDraft(control)
			if err != nil {
				return false
			}
			controlQuote, controlErr := e.Costs.Quote(ctx, control.TenantID, controlDraft, e.now())
			candidateQuote, candidateErr := e.Costs.Quote(ctx, candidate.TenantID, draft, e.now())
			if controlErr != nil || candidateErr != nil || controlQuote.HourlyUSD <= 0 || candidateQuote.HourlyUSD <= 0 {
				return false
			}
			ratio = controlQuote.HourlyUSD / candidateQuote.HourlyUSD
		}
		comparison, found := objectiveComparison(evidence.Comparison, metric, costScale, ratio)
		return found && comparison.Decision == DecisionImproved
	}
}

// campaignComparisons counts the candidates a campaign compares with its
// control. The count comes from the proposal, not from how many candidates
// survived to measurement, so a candidate that failed to start still widens
// every other candidate's interval: the family of comparisons is fixed before
// any result is seen.
func campaignComparisons(campaign domain.OptimizationCampaign) int {
	count := len(campaign.Candidates)
	if _, found := CampaignControl(campaign); found {
		count--
	}
	return max(1, count)
}

// directTarget selects the lowest-ordinal healthy ready replica of one exact
// revision and the server-side credential for its provider.
func (e BenchmarkExecutor) directTarget(ctx context.Context, candidate domain.OptimizationCandidateRun, deploymentID string, draft optimizer.DeploymentDraft) (armTarget, string, error) {
	return directReplica(ctx, e.Store, e.Backends, candidate, deploymentID, draft)
}

type replicaLister interface {
	ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error)
}

// directReplica is shared by measurement and the built-in answer check, so
// both address the same exact revision with the same credential.
func directReplica(ctx context.Context, store replicaLister, backends map[string]BenchmarkBackend, candidate domain.OptimizationCandidateRun, deploymentID string, draft optimizer.DeploymentDraft) (armTarget, string, error) {
	replicas, err := store.ReplicasForDeployment(ctx, candidate.TenantID, deploymentID)
	if err != nil {
		return armTarget{}, "", err
	}
	endpoint, provider, replicaID, ordinal := "", "", "", int(^uint(0)>>1)
	for _, replica := range replicas {
		if replica.RevisionID == candidate.RevisionID && replica.Endpoint != "" && replica.Health == "healthy" && (replica.LifecycleState == "ready" || replica.LifecycleState == "active") && replica.Ordinal < ordinal {
			endpoint, provider, replicaID, ordinal = replica.Endpoint, replica.Provider, replica.ID, replica.Ordinal
		}
	}
	if endpoint == "" || provider == "" {
		return armTarget{}, "", fmt.Errorf("candidate %w", errNoDirectEndpoint)
	}
	backend, ok := backends[provider]
	if !ok || backend.APIKey == "" {
		return armTarget{}, "", fmt.Errorf("%w: benchmark credential is unavailable for provider %s", errNoDirectEndpoint, provider)
	}
	// The runtime contract declares a Prometheus path only when the server
	// exposes one; without it, runtime metrics stay unknown. A built-in
	// runtime's draft carries no workload, so read the contract the replica
	// was provisioned with, not the empty draft.
	served := support.ServedWorkload(draft.Runtime.Engine, draft.Resources.GPU, draft.Runtime.Workload)
	return armTarget{candidateID: candidate.ID, revisionID: candidate.RevisionID, model: draft.ServedModel(), replicaID: replicaID, endpoint: endpoint, apiKey: backend.APIKey, apiKeyEnv: backend.APIKeyEnv, serverMetrics: served.MetricsPath != ""}, provider, nil
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
