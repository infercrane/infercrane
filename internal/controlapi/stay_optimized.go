package controlapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/continualoptimizer"
	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/modelcheck"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/store"
	"github.com/infercrane/infercrane/internal/workloadprofile"
)

const (
	// StayOptimizedInterval is how often the background loop looks for due
	// checks.
	StayOptimizedInterval = 5 * time.Minute
	// stayOptimizedPoll is how often an endpoint is looked at for engine
	// releases, model revisions, price changes, and traffic drift. The full
	// scheduled check runs at the policy's interval (weekly by default).
	stayOptimizedPoll  = time.Hour
	stayOptimizedLease = 10 * time.Minute
	// stayOptimizedWindow is the request window a check reads.
	stayOptimizedWindow  = 7 * 24 * time.Hour
	stayOptimizedBatch   = 20
	stayOptimizedBatches = 10
	// A recorded traffic window older than this is not reused for a run; a
	// fresh one is captured from the request records.
	stayOptimizedReplayAge      = 30 * 24 * time.Hour
	stayOptimizedReplayWindow   = 24 * time.Hour
	stayOptimizedReplayRequests = 2000
	stayOptimizedPastRuns       = 5
)

var errStayOptimizedPolicyRequired = errors.New("persist a bounded continual optimization policy before checking")

// stayOptimizedStore is the storage a Stay optimized check needs.
type stayOptimizedStore interface {
	continualOptimizationStore
	optimizationCampaignStore
	ClaimDueStayOptimizedChecks(context.Context, string, time.Duration, int) ([]domain.StayOptimizedCheck, error)
	ClaimStayOptimizedCheck(context.Context, string, string, string, time.Duration) (domain.StayOptimizedCheck, error)
	CompleteStayOptimizedCheck(context.Context, domain.StayOptimizedCheck, string, time.Time, string) error
	StayOptimizedProposals(context.Context, string, string, int) ([]domain.OptimizationCampaign, error)
	SetOptimizationCampaignOrigin(context.Context, string, string, string) error
	StayOptimizedAutoRunSpend(context.Context, string, string, time.Time) (float64, error)
	EndpointCustomKernels(context.Context, string, string) ([]domain.EndpointCustomKernel, error)
	SetEndpointCustomKernelState(context.Context, string, string, string, string, string) error
	Revision(context.Context, string, string, string) (domain.DeploymentRevision, error)
}

// StayOptimized runs Stay optimized checks on their own, off the inference
// path. Every control-plane instance may run it: a lease on each endpoint's
// check state keeps one check per endpoint at a time. A check proposes a run
// that waits for approval; it never deploys, and it spends only when the
// policy's opt-in auto-run approves a run under its monthly cap.
type StayOptimized struct {
	API      API
	Owner    string
	Interval time.Duration
	Logger   *slog.Logger
}

func (s StayOptimized) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = StayOptimizedInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
			s.logger().Warn("stay optimized pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce checks every endpoint that is due, in bounded batches. It returns
// how many checks ran.
func (s StayOptimized) RunOnce(ctx context.Context) (int, error) {
	checks, ok := s.API.Store.(stayOptimizedStore)
	if !ok {
		return 0, nil
	}
	owner := strings.TrimSpace(s.Owner)
	if owner == "" {
		owner = "stay-optimized"
	}
	ran := 0
	for range stayOptimizedBatches {
		claims, err := checks.ClaimDueStayOptimizedChecks(ctx, owner, stayOptimizedLease, stayOptimizedBatch)
		if err != nil {
			return ran, err
		}
		for _, claim := range claims {
			pass, cancel := context.WithTimeout(ctx, stayOptimizedLease-time.Minute)
			result, checkErr := s.API.runStayOptimizedCheck(pass, checks, claim, nil)
			cancel()
			ran++
			if checkErr != nil {
				s.logger().Warn("stay optimized check failed", "tenant", claim.TenantID, "deployment", claim.DeploymentName, "error", checkErr)
			} else if result.Proposal != nil {
				s.logger().Info("stay optimized proposed a run", "tenant", claim.TenantID, "deployment", claim.DeploymentName, "campaign", result.Proposal.ID)
			}
		}
		if len(claims) < stayOptimizedBatch {
			break
		}
	}
	return ran, nil
}

func (s StayOptimized) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// stayOptimizedResult is one check's outcome. Evaluated is false when no
// trigger fired or a run is already proposed or running; nothing is recorded
// then.
type stayOptimizedResult struct {
	Evaluated bool
	Note      string
	Decision  continualoptimizer.Decision
	RecordID  string
	Created   bool
	Proposal  *domain.OptimizationCampaign
}

// runStayOptimizedCheck runs one leased check and always completes the
// lease, recording what it saw and when the next poll is due.
func (a API) runStayOptimizedCheck(ctx context.Context, checks stayOptimizedStore, claim domain.StayOptimizedCheck, manual *continualoptimizer.Trigger) (stayOptimizedResult, error) {
	observed := decodeObserved(claim.ObservedJSON)
	result, err := a.stayOptimizedCheck(ctx, checks, claim, manual, &observed)
	encoded, _ := json.Marshal(observed)
	message := ""
	if err != nil {
		message = err.Error()
	}
	if completeErr := checks.CompleteStayOptimizedCheck(context.WithoutCancel(ctx), claim, string(encoded), time.Now().UTC().Add(stayOptimizedPoll), message); completeErr != nil && err == nil {
		err = fmt.Errorf("check lease could not be completed: %w", completeErr)
	}
	return result, err
}

func decodeObserved(raw string) continualoptimizer.Observed {
	var observed continualoptimizer.Observed
	_ = json.Unmarshal([]byte(raw), &observed)
	return observed
}

// stayOptimizedEvidence is what a check read about the endpoint.
type stayOptimizedEvidence struct {
	resolved     domain.ResolvedDeployment
	endpoint     string
	spec         domain.DeploymentRevisionSpec
	setup        continualoptimizer.Setup
	hourly       float64
	priceSource  string
	snapshot     *domain.EndpointMonitoringSnapshot
	current      continualoptimizer.Workload
	baseline     *continualoptimizer.Workload
	lastRun      *time.Time
	memory       stayOptimizedMemory
	latestCommit string
}

func (a API) stayOptimizedCheck(ctx context.Context, checks stayOptimizedStore, claim domain.StayOptimizedCheck, manual *continualoptimizer.Trigger, observed *continualoptimizer.Observed) (stayOptimizedResult, error) {
	now := time.Now().UTC()
	tenant, name := claim.TenantID, claim.DeploymentName
	policyRow, err := checks.ContinualOptimizationPolicy(ctx, tenant, name)
	if errors.Is(err, domain.ErrNotFound) {
		return stayOptimizedResult{}, errStayOptimizedPolicyRequired
	}
	if err != nil {
		return stayOptimizedResult{}, fmt.Errorf("policy could not be loaded: %w", err)
	}
	var policy continualoptimizer.Policy
	if json.Unmarshal([]byte(policyRow.PolicyJSON), &policy) != nil || continualoptimizer.ValidatePolicy(policy) != nil {
		return stayOptimizedResult{}, errors.New("persisted continual optimization policy is invalid")
	}
	if !policy.Enabled && manual == nil {
		return stayOptimizedResult{Note: "Stay optimized is off"}, nil
	}
	evidence, err := a.readStayOptimizedEvidence(ctx, checks, tenant, name, policy, observed, now)
	if err != nil {
		return stayOptimizedResult{}, err
	}
	a.applyKernelUpkeep(ctx, checks, tenant, evidence)

	var triggers []continualoptimizer.Trigger
	if manual != nil {
		triggers = append(triggers, *manual)
	}
	scheduled := continualoptimizer.ScheduleDue(*observed, policy, now)
	if scheduled {
		triggers = append(triggers, continualoptimizer.ScheduleTrigger(policy, now))
	}
	engine, engineFired := continualoptimizer.DetectEngineRelease(evidence.setup, *observed)
	if engineFired {
		triggers = append(triggers, engine)
	}
	model, modelFired := continualoptimizer.DetectModelRevision(evidence.setup, evidence.latestCommit, *observed)
	if modelFired {
		triggers = append(triggers, model)
	}
	price, priceFired := continualoptimizer.DetectPriceChange(observed.HourlyUSDPerReplica, evidence.hourly)
	if priceFired {
		triggers = append(triggers, price)
	}
	drifted := evidence.baseline != nil && evidence.current.Source == continualoptimizer.SourceCustomerObserved && continualoptimizer.WorkloadDrift(*evidence.baseline, evidence.current) >= policy.DriftThreshold
	if len(triggers) == 0 && !drifted {
		if observed.HourlyUSDPerReplica <= 0 && evidence.hourly > 0 {
			observed.HourlyUSDPerReplica, observed.PriceSource = evidence.hourly, evidence.priceSource
		}
		return stayOptimizedResult{Note: "nothing changed since the last check"}, nil
	}
	// A run already proposed, running, or just finished keeps every trigger
	// pending: nothing is recorded, and the trigger fires again once the run
	// is decided. A person's Check now still records a decision.
	inCooldown := evidence.lastRun != nil && now.Sub(*evidence.lastRun) < time.Duration(policy.CooldownSeconds)*time.Second
	if manual == nil && (len(evidence.memory.active) > 0 || len(evidence.memory.pending) > 0 || inCooldown) {
		return stayOptimizedResult{Note: "a run is already proposed, running, or cooling down"}, nil
	}

	service := continualoptimizer.ServiceEvidence{RequestCount: evidence.current.RequestCount}
	if evidence.snapshot != nil {
		service = continualoptimizer.ServiceFromMonitoring(*evidence.snapshot, continualoptimizer.CostEvidence{HourlyUSDPerReplica: evidence.hourly, AverageReplicas: a.averageReplicas(ctx, tenant, evidence), PriceSource: evidence.priceSource})
	}
	input := continualoptimizer.Input{
		Now: now, Policy: policy, Baseline: evidence.baseline, Current: evidence.current, ActiveService: service,
		History: evidence.memory.history, ActiveCampaigns: evidence.memory.active, PendingProposals: evidence.memory.pending,
		LastExperimentAt: evidence.lastRun, Triggers: triggers,
	}
	var proposal optimizer.Proposal
	hourly := map[string]float64{}
	if policy.Enabled && policy.AllowAutomaticExperimentation && len(input.ActiveCampaigns) == 0 && len(input.PendingProposals) == 0 && !inCooldown && evidence.current.Source == continualoptimizer.SourceCustomerObserved {
		planned, issue := a.planStayOptimized(ctx, tenant, evidence, policy)
		if issue != "" {
			input.PlanningIssue = issue
		} else {
			proposal = planned
			hourly = a.quoteSetups(ctx, tenant, proposal, now)
			perHour := map[string]float64{}
			for id, price := range hourly {
				perHour[id] = price * continualoptimizer.RunCostHours
			}
			input.Hypotheses = continualoptimizer.HypothesesFromProposal(proposal, perHour)
		}
	}
	decision, err := continualoptimizer.Evaluate(input)
	if err != nil {
		return stayOptimizedResult{}, fmt.Errorf("evaluation failed: %w", err)
	}
	record, created, err := checks.RecordContinualOptimizationDecision(ctx, tenant, name, evidence.current, decision)
	if err != nil {
		return stayOptimizedResult{}, fmt.Errorf("decision could not be recorded: %w", err)
	}
	_ = a.Store.Audit(context.WithoutCancel(ctx), domain.AuditEvent{TenantID: tenant, Actor: store.StayOptimizedActor, Action: "continual_optimization.evaluate", ResourceType: "deployment", ResourceName: name, Outcome: "succeeded", Payload: record.ID})
	// The check acted on these changes; they do not fire again.
	if engineFired {
		observed.EngineRelease = engine.Key
	}
	if modelFired {
		observed.ModelRevision = model.Key
	}
	if evidence.hourly > 0 {
		observed.HourlyUSDPerReplica, observed.PriceSource = evidence.hourly, evidence.priceSource
	}
	if scheduled {
		observed.ScheduledAt = &now
	}
	result := stayOptimizedResult{Evaluated: true, Decision: decision, RecordID: record.ID, Created: created}
	if decision.Action != continualoptimizer.ActionStartExperiment {
		return result, nil
	}
	campaign, err := a.proposeStayOptimizedRun(ctx, checks, tenant, evidence, policy, proposal, hourly, decision, record.ID, now)
	if err != nil {
		return result, fmt.Errorf("proposed run could not be created: %w", err)
	}
	result.Proposal = &campaign
	return result, nil
}

func (a API) readStayOptimizedEvidence(ctx context.Context, checks stayOptimizedStore, tenant, name string, policy continualoptimizer.Policy, observed *continualoptimizer.Observed, now time.Time) (stayOptimizedEvidence, error) {
	var evidence stayOptimizedEvidence
	resolved, err := a.Store.ResolveForTenant(ctx, tenant, name)
	if err != nil {
		return evidence, fmt.Errorf("deployment could not be resolved: %w", err)
	}
	evidence.resolved, evidence.endpoint = resolved, name
	if len(resolved.EndpointNames) > 0 {
		evidence.endpoint = resolved.EndpointNames[0]
	}
	evidence.setup = continualoptimizer.Setup{Model: resolved.Deployment.Model, Runtime: resolved.Deployment.Runtime}
	if resolved.Deployment.ActiveRevisionID != "" {
		if revision, revisionErr := checks.Revision(ctx, tenant, name, resolved.Deployment.ActiveRevisionID); revisionErr == nil && json.Unmarshal([]byte(revision.SpecJSON), &evidence.spec) == nil {
			evidence.setup = continualoptimizer.Setup{Model: firstNonEmpty(evidence.spec.Model, resolved.Deployment.Model), ModelRevision: evidence.spec.ModelRevision, Runtime: firstNonEmpty(evidence.spec.Runtime, resolved.Deployment.Runtime), RuntimeVersion: evidence.spec.RuntimeVersion, RuntimeImage: evidence.spec.Workload.Image, GPU: evidence.spec.GPU}
		}
	}
	evidence.hourly, evidence.priceSource = a.setupHourly(ctx, tenant, evidence.spec, resolved.Deployment.ComputeConnectionID, now)
	evidence.latestCommit = a.latestModelRevision(ctx, evidence.setup, observed, now)

	if monitoring, ok := a.Store.(monitoringStore); ok {
		snapshot, monitoringErr := monitoring.EndpointMonitoring(ctx, tenant, evidence.endpoint, stayOptimizedWindow, time.Duration(defaultMonitoringBucket(int(stayOptimizedWindow.Seconds())))*time.Second)
		if monitoringErr == nil {
			evidence.snapshot = &snapshot
			evidence.current, err = continualoptimizer.WorkloadFromMonitoring(snapshot)
		}
		if monitoringErr != nil || err != nil {
			evidence.snapshot = nil
		}
	}
	if evidence.snapshot == nil {
		if trace, found := a.recentReplay(ctx, tenant, name, now); found {
			evidence.current, err = continualoptimizer.FromReplayTrace(trace)
		}
		if evidence.current.Source == "" || err != nil {
			document, documentErr := workloadprofile.PublicChutesPrior()
			if documentErr != nil {
				return evidence, errors.New("the embedded public workload profile failed integrity validation")
			}
			profile, lookupErr := workloadprofile.Lookup(document, "public-interactive")
			if lookupErr != nil {
				return evidence, lookupErr
			}
			if evidence.current, err = continualoptimizer.FromPublicProfile(document, profile); err != nil {
				return evidence, err
			}
		}
	}
	state, stateErr := checks.ContinualOptimizationState(ctx, tenant, name)
	if stateErr == nil {
		evidence.lastRun = state.LastExperimentAt
		if state.BaselineJSON != "" {
			var decoded continualoptimizer.Workload
			if json.Unmarshal([]byte(state.BaselineJSON), &decoded) == nil {
				evidence.baseline = &decoded
			}
		}
	} else if !errors.Is(stateErr, domain.ErrNotFound) {
		return evidence, fmt.Errorf("check state could not be loaded: %w", stateErr)
	}
	evidence.memory = stayOptimizedCampaignMemory(ctx, checks, tenant, name, policy)
	return evidence, nil
}

// setupHourly is the sourced hourly price of one replica of the running
// setup, or zero when it is unknown.
func (a API) setupHourly(ctx context.Context, tenant string, spec domain.DeploymentRevisionSpec, connection string, now time.Time) (float64, string) {
	if a.OptimizationCosts == nil || spec.GPU == "" || spec.Cloud == "" {
		return 0, ""
	}
	var draft optimizer.DeploymentDraft
	draft.Provider.Cloud, draft.Provider.Adapter, draft.Provider.Region = spec.Cloud, spec.ProviderAdapter, spec.Region
	draft.Resources.GPU, draft.Resources.GPUCount = spec.GPU, max(spec.GPUCount, 1)
	draft.Scaling.MinReplicas, draft.Scaling.MaxReplicas = 1, 1
	draft.ComputeConnectionID = firstNonEmpty(spec.ComputeConnectionID, connection)
	quote, err := a.OptimizationCosts.Quote(ctx, tenant, draft, now.Add(time.Hour))
	if err != nil || quote.HourlyUSD <= 0 {
		return 0, ""
	}
	return quote.HourlyUSD, quote.Source
}

// latestModelRevision asks the model host for the newest commit of a pinned
// public model, at most once a day. Unknown is empty.
func (a API) latestModelRevision(ctx context.Context, setup continualoptimizer.Setup, observed *continualoptimizer.Observed, now time.Time) string {
	if a.ModelCheck == nil || setup.ModelRevision == "" {
		return ""
	}
	repository, _, _ := strings.Cut(setup.Model, "@")
	if !modelcheck.ValidRepository(repository) {
		return ""
	}
	if observed.ModelCheckedAt != nil && now.Sub(*observed.ModelCheckedAt) < continualoptimizer.ModelRevisionCheckInterval {
		return ""
	}
	model, err := a.ModelCheck.Resolve(ctx, modelcheck.Request{Repository: repository, Revision: "main"})
	if err != nil {
		return ""
	}
	observed.ModelCheckedAt = &now
	return model.Commit
}

func (a API) recentReplay(ctx context.Context, tenant, name string, now time.Time) (domain.ReplayTrace, bool) {
	intelligence, ok := a.Store.(intelligenceStore)
	if !ok {
		return domain.ReplayTrace{}, false
	}
	traces, err := intelligence.ReplayTraces(ctx, tenant, name, 5)
	if err != nil {
		return domain.ReplayTrace{}, false
	}
	for _, trace := range traces {
		if now.Sub(trace.WindowEnd) <= stayOptimizedReplayAge && trace.RequestCount > 0 {
			return trace, true
		}
	}
	return domain.ReplayTrace{}, false
}

func (a API) averageReplicas(ctx context.Context, tenant string, evidence stayOptimizedEvidence) float64 {
	spans, ok := a.Store.(campaignSpendStore)
	if !ok || evidence.snapshot == nil || evidence.resolved.Deployment.ActiveRevisionID == "" {
		return 0
	}
	rows, err := spans.OptimizationReplicaSpans(ctx, tenant, evidence.resolved.Deployment.Name, evidence.resolved.Deployment.ActiveRevisionID, evidence.snapshot.WindowStart)
	if err != nil {
		return 0
	}
	return continualoptimizer.AverageReplicas(rows, evidence.snapshot.WindowStart, evidence.snapshot.WindowEnd)
}

// applyKernelUpkeep moves custom kernels whose engine pin no longer matches.
func (a API) applyKernelUpkeep(ctx context.Context, checks stayOptimizedStore, tenant string, evidence stayOptimizedEvidence) []continualoptimizer.KernelUpkeep {
	kernels, err := checks.EndpointCustomKernels(ctx, tenant, evidence.resolved.Deployment.ID)
	if err != nil || len(kernels) == 0 {
		return nil
	}
	var upkeep []continualoptimizer.KernelUpkeep
	for _, change := range continualoptimizer.KernelUpkeepFor(kernels, evidence.setup) {
		if change.Change {
			if setErr := checks.SetEndpointCustomKernelState(ctx, tenant, change.Kernel.ID, change.Kernel.State, change.Upkeep.State, change.Upkeep.Reason); setErr != nil {
				continue
			}
			_ = a.Store.Audit(context.WithoutCancel(ctx), domain.AuditEvent{TenantID: tenant, Actor: store.StayOptimizedActor, Action: "custom_kernel." + change.Upkeep.State, ResourceType: "deployment", ResourceName: evidence.resolved.Deployment.Name, Outcome: "succeeded", Payload: change.Kernel.ID})
		}
		upkeep = append(upkeep, change.Upkeep)
	}
	return upkeep
}

// planStayOptimized asks the planner for setups for the endpoint's running
// deployment, the way a person's Optimize does: the same model, provider,
// GPU, and compute connection, the endpoint's recorded traffic, and its
// measured bottleneck. A non-empty issue says why it could not plan.
func (a API) planStayOptimized(ctx context.Context, tenant string, evidence stayOptimizedEvidence, policy continualoptimizer.Policy) (optimizer.Proposal, string) {
	spec := evidence.spec
	if spec.GPU == "" || spec.Cloud == "" {
		return optimizer.Proposal{}, "the running revision records no provider or GPU"
	}
	request := optimizer.Request{
		ModelIdentity: evidence.resolved.Deployment.Model, ModelRevision: spec.ModelRevision,
		Provider: spec.Cloud, ComputeConnectionID: firstNonEmpty(spec.ComputeConnectionID, evidence.resolved.Deployment.ComputeConnectionID),
		Region: spec.Region, GPU: spec.GPU, GPUCount: spec.GPUCount, Objective: evidence.memory.objective,
		MaxCandidates: min(policy.MaximumCandidatesPerCampaign+1, 10),
	}
	if trace, found := a.workloadForRun(ctx, tenant, evidence); found {
		request.WorkloadSource, request.WorkloadFingerprint = optimizer.WorkloadSourceCustomerObserved, "sha256:"+strings.TrimPrefix(trace.ShapeDigest, "sha256:")
	} else if policy.RequireCustomerReplay {
		return optimizer.Proposal{}, "no recorded traffic could be captured, and this policy measures runs only on your own traffic"
	} else {
		request.WorkloadProfile, request.WorkloadSource = "public-interactive", optimizer.WorkloadSourcePublicPrior
	}
	if request.ComputeConnectionID != "" {
		if a.ComputeConnections == nil {
			return optimizer.Proposal{}, "compute connection storage is not configured"
		}
		connection, err := a.ComputeConnections.Get(ctx, tenant, request.ComputeConnectionID)
		if err != nil || connection.Status != "verified" || connection.Provider != strings.ToLower(strings.TrimSpace(request.Provider)) {
			return optimizer.Proposal{}, "the endpoint's compute connection is not verified; reconnect it"
		}
		request.ProviderAdapter = connection.Adapter
	}
	if failure := a.checkUncatalogedProposal(ctx, tenant, &request, spec.ModelSecretReferenceID); failure != nil {
		return optimizer.Proposal{}, failure.Message
	}
	source := optimizer.NewCatalogSource(curatedrecipe.All(), a.Integrations)
	if diagnosis, err := a.endpointDiagnosis(ctx, tenant, evidence.endpoint); err == nil {
		diagnosed := request
		diagnosed.Diagnosis = diagnosis
		if proposal, proposeErr := source.Propose(ctx, diagnosed); proposeErr == nil && proposal.Diagnosis != nil && proposal.Diagnosis.State == "applied" && len(proposal.Candidates) > 1 {
			return proposal, ""
		}
	}
	proposal, err := source.Propose(ctx, request)
	if err != nil {
		return optimizer.Proposal{}, err.Error()
	}
	if len(proposal.Candidates) == 0 {
		return optimizer.Proposal{}, "no reviewed setup fits this model, provider, and GPU"
	}
	return proposal, ""
}

// workloadForRun is the recorded traffic a proposed run replays: a recent
// recording, else one captured now from the gateway's content-free request
// records.
func (a API) workloadForRun(ctx context.Context, tenant string, evidence stayOptimizedEvidence) (domain.ReplayTrace, bool) {
	now := time.Now().UTC()
	if trace, found := a.recentReplay(ctx, tenant, evidence.resolved.Deployment.Name, now); found {
		return trace, true
	}
	intelligence, ok := a.Store.(intelligenceStore)
	if !ok {
		return domain.ReplayTrace{}, false
	}
	trace, err := intelligence.CaptureReplayTrace(ctx, tenant, evidence.resolved.Deployment.Name, stayOptimizedReplayWindow, stayOptimizedReplayRequests)
	if err != nil || trace.RequestCount < 1 || trace.ShapeDigest == "" {
		return domain.ReplayTrace{}, false
	}
	return trace, true
}

// quoteSetups is each setup's sourced hourly price for one replica. A setup
// without a current price is absent.
func (a API) quoteSetups(ctx context.Context, tenant string, proposal optimizer.Proposal, now time.Time) map[string]float64 {
	prices := map[string]float64{}
	if a.OptimizationCosts == nil {
		return prices
	}
	for _, candidate := range proposal.Candidates {
		draft := candidate.Deployment
		draft.Scaling.MaxReplicas = max(draft.Scaling.MinReplicas, 1)
		quote, err := a.OptimizationCosts.Quote(ctx, tenant, draft, now.Add(time.Hour))
		if err == nil && quote.HourlyUSD > 0 {
			prices[candidate.ID] = quote.HourlyUSD
		}
	}
	return prices
}

func (a API) proposeStayOptimizedRun(ctx context.Context, checks stayOptimizedStore, tenant string, evidence stayOptimizedEvidence, policy continualoptimizer.Policy, proposal optimizer.Proposal, hourly map[string]float64, decision continualoptimizer.Decision, recordID string, now time.Time) (domain.OptimizationCampaign, error) {
	narrowed, err := continualoptimizer.NarrowProposal(proposal, decision.Selected)
	if err != nil {
		return domain.OptimizationCampaign{}, err
	}
	if err = optimizer.ValidateProposal(narrowed); err != nil {
		return domain.OptimizationCampaign{}, err
	}
	ids := make([]string, 0, len(narrowed.Candidates))
	for _, candidate := range narrowed.Candidates {
		ids = append(ids, candidate.ID)
	}
	name := evidence.resolved.Deployment.Name
	origin := continualoptimizer.Origin{
		Kind: continualoptimizer.OriginStayOptimized, Deployment: name, DecisionID: recordID, DecisionDigest: decision.DecisionDigest,
		Triggers: decision.Triggers, Reason: continualoptimizer.ProposalReason(decision.Triggers, len(decision.Selected)),
		ExpectedChange: a.pastRunChange(ctx, tenant, evidence.memory.decided), RunCost: continualoptimizer.EstimateRunCost(len(ids), hourly, ids),
		UnknownEvidence: decision.UnknownEvidence, AutoRun: continualoptimizer.AutoRunResult{Reason: "off: a person approves every run"},
		Kernels: a.kernelWarnings(ctx, checks, tenant, evidence),
	}
	proposalJSON, err := json.Marshal(narrowed)
	if err != nil {
		return domain.OptimizationCampaign{}, err
	}
	originJSON, err := json.Marshal(origin)
	if err != nil {
		return domain.OptimizationCampaign{}, err
	}
	candidates, invalid := campaignCandidateRuns(narrowed)
	if invalid != "" {
		return domain.OptimizationCampaign{}, errors.New(invalid)
	}
	campaign := domain.OptimizationCampaign{TenantID: tenant, IdempotencyKey: "stay-optimized-" + strings.TrimPrefix(decision.DecisionDigest, "sha256:"), InputDigest: narrowed.InputDigest, ModelIdentity: narrowed.Input.ModelIdentity, Objective: narrowed.Input.Objective, Source: narrowed.AlgorithmVersion, Intent: optimizationcampaign.IntentEvolveEndpoint, TargetDeployment: name, ProposalJSON: string(proposalJSON), OriginJSON: string(originJSON)}
	created, _, err := checks.CreateOptimizationCampaign(ctx, campaign, candidates)
	if err != nil {
		return domain.OptimizationCampaign{}, err
	}
	_ = a.Store.Audit(context.WithoutCancel(ctx), domain.AuditEvent{TenantID: tenant, Actor: store.StayOptimizedActor, Action: "continual_optimization.propose", ResourceType: "optimization_campaign", ResourceName: created.ID, Outcome: "succeeded", Payload: recordID})
	if !policy.AutoRun {
		return created, nil
	}
	origin.AutoRun = a.autoRun(ctx, checks, tenant, name, policy, created.ID, origin.RunCost, now)
	if originJSON, err = json.Marshal(origin); err == nil && checks.SetOptimizationCampaignOrigin(ctx, tenant, created.ID, string(originJSON)) == nil {
		created.OriginJSON = string(originJSON)
	}
	if origin.AutoRun.Approved {
		if refreshed, lookupErr := checks.OptimizationCampaign(ctx, tenant, created.ID); lookupErr == nil {
			created = refreshed
		}
	}
	return created, nil
}

// autoRun approves a proposed run without a person only when the policy
// opted in, the run's cost is known, and it fits what is left of this
// calendar month's cap. Rollout still needs Release Guard and a person.
func (a API) autoRun(ctx context.Context, checks stayOptimizedStore, tenant, name string, policy continualoptimizer.Policy, campaignID string, cost continualoptimizer.RunCostEstimate, now time.Time) continualoptimizer.AutoRunResult {
	if cost.USD == nil {
		return continualoptimizer.AutoRunResult{Reason: "not approved: the run's cost is unknown, so a person sets the spend cap"}
	}
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	spent, err := checks.StayOptimizedAutoRunSpend(ctx, tenant, name, monthStart)
	if err != nil {
		return continualoptimizer.AutoRunResult{Reason: "not approved: this month's auto-run spend could not be read"}
	}
	if spent+*cost.USD > policy.MonthlyAutoRunCapUSD {
		return continualoptimizer.AutoRunResult{Reason: fmt.Sprintf("not approved: $%.2f would pass this month's auto-run cap ($%.2f of $%.2f already approved)", *cost.USD, spent, policy.MonthlyAutoRunCapUSD)}
	}
	actor := domain.Principal{ID: store.StayOptimizedActor, TenantID: tenant, Name: store.StayOptimizedActor}
	expires := now.Add(time.Duration(cost.Hours * float64(time.Hour))).Truncate(time.Second)
	if _, failure := a.approveCampaign(ctx, checks, actor, campaignID, *cost.USD, expires); failure != nil {
		return continualoptimizer.AutoRunResult{Reason: "not approved: " + failure.message}
	}
	capUSD := *cost.USD
	return continualoptimizer.AutoRunResult{Approved: true, CapUSD: &capUSD, Reason: fmt.Sprintf("approved within this month's auto-run cap ($%.2f of $%.2f now approved)", spent+capUSD, policy.MonthlyAutoRunCapUSD)}
}

// kernelWarnings lists the endpoint's kernels the engine pin affects, for
// the proposed run's origin. The state changes were applied at check start.
func (a API) kernelWarnings(ctx context.Context, checks stayOptimizedStore, tenant string, evidence stayOptimizedEvidence) []continualoptimizer.KernelUpkeep {
	kernels, err := checks.EndpointCustomKernels(ctx, tenant, evidence.resolved.Deployment.ID)
	if err != nil {
		return nil
	}
	var out []continualoptimizer.KernelUpkeep
	pinned, hasPinned := continualoptimizer.PinnedEngine(evidence.setup)
	for _, kernel := range kernels {
		if !hasPinned || !strings.EqualFold(kernel.Engine, pinned.Runtime) || kernel.EngineVersion == pinned.EngineVersion {
			continue
		}
		reason := kernel.StateReason
		if reason == "" {
			reason = fmt.Sprintf("pinned to %s %s; setups on %s run without it until it is re-qualified", kernel.Engine, kernel.EngineVersion, pinned.EngineVersion)
		}
		out = append(out, continualoptimizer.KernelUpkeep{ID: kernel.ID, Operator: kernel.Operator, EngineVersion: kernel.EngineVersion, State: kernel.State, Reason: reason})
	}
	return out
}

// pastRunChange is the cost-per-token change earlier decided runs on the
// endpoint measured for their winner. It reads at most a few reports.
func (a API) pastRunChange(ctx context.Context, tenant string, decided []string) continualoptimizer.ExpectedChange {
	reports, ok := a.Store.(optimizationreport.Store)
	if !ok {
		return continualoptimizer.ExpectedChangeFrom(nil)
	}
	var history []continualoptimizer.PastRunChange
	for _, id := range decided {
		report, err := optimizationreport.Load(ctx, reports, tenant, id)
		if err != nil || report.SimulatedAccelerator || report.Outcome.Kind != optimizationreport.OutcomeWinnerSelected {
			continue
		}
		for _, metric := range report.Metrics {
			if metric.Key == optimizationreport.MetricCostPerMillion && metric.ChangePercent != nil {
				history = append(history, continualoptimizer.PastRunChange{CampaignID: id, Change: *metric.ChangePercent / 100})
			}
		}
	}
	return continualoptimizer.ExpectedChangeFrom(history)
}

// stayOptimizedMemory is what earlier runs on the endpoint say: setups
// already tried, runs still active or waiting for approval, the objective
// the endpoint was last optimized for, and decided runs to learn from.
type stayOptimizedMemory struct {
	history   []continualoptimizer.Outcome
	active    []continualoptimizer.Active
	pending   []continualoptimizer.Active
	objective string
	decided   []string
}

func stayOptimizedCampaignMemory(ctx context.Context, campaigns optimizationCampaignStore, tenant, deployment string, policy continualoptimizer.Policy) stayOptimizedMemory {
	memory := stayOptimizedMemory{objective: "interactive"}
	rows, err := campaigns.OptimizationCampaigns(ctx, tenant, 100)
	if err != nil {
		return memory
	}
	objectiveSet := false
	for _, campaign := range rows {
		if campaign.Intent != optimizationcampaign.IntentEvolveEndpoint || campaign.TargetDeployment != deployment {
			continue
		}
		if !objectiveSet && campaign.Objective != "" {
			memory.objective, objectiveSet = campaign.Objective, true
		}
		_, proposed := continualoptimizer.DecodeOrigin(campaign.OriginJSON)
		switch campaign.State {
		case optimizationcampaign.CampaignAwaitingApproval:
			if proposed {
				memory.pending = append(memory.pending, continualoptimizer.Active{CampaignID: campaign.ID, Phase: campaign.State, Count: len(campaign.Candidates)})
			}
		case optimizationcampaign.CampaignApproved, optimizationcampaign.CampaignRunning, optimizationcampaign.CampaignRanked, optimizationcampaign.CampaignQualified, optimizationcampaign.CampaignGuardPassed:
			if len(memory.active) < policy.MaximumConcurrentCampaigns {
				memory.active = append(memory.active, continualoptimizer.Active{CampaignID: campaign.ID, Phase: campaign.State, Count: len(campaign.Candidates)})
			}
		case optimizationcampaign.CampaignPromoted, optimizationcampaign.CampaignObserved, optimizationcampaign.CampaignRejected, optimizationcampaign.CampaignInconclusive, optimizationcampaign.CampaignCleaned:
			if len(memory.decided) < stayOptimizedPastRuns {
				memory.decided = append(memory.decided, campaign.ID)
			}
		}
		for _, candidate := range campaign.Candidates {
			decision := ""
			switch candidate.State {
			case optimizationcampaign.CandidateRejected, optimizationcampaign.CandidateInconclusive:
				decision = "rejected"
			case optimizationcampaign.CandidateFailed:
				decision = "failed"
			case optimizationcampaign.CandidateCancelled:
				// A dismissed proposal is not proposed again inside the
				// rejection memory.
				if proposed {
					decision = "rejected"
				}
			case optimizationcampaign.CandidateCleaned:
				if candidate.FailureCode != "" {
					decision = "failed"
				} else if proposed && campaign.ApprovedAt == nil {
					decision = "rejected"
				}
			}
			if decision != "" && len(candidate.ProposalCandidateID) == 64 {
				memory.history = append(memory.history, continualoptimizer.Outcome{Fingerprint: continualoptimizer.HypothesisFingerprint(candidate.ProposalCandidateID), Decision: decision, FailureCode: candidate.FailureCode, CompletedAt: candidate.UpdatedAt})
			}
		}
	}
	return memory
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// evaluateContinualOptimization is Check now: one Stay optimized check run
// immediately, from the control plane's own records. Legacy request fields
// (public_profile, replay_trace_id, hypotheses, economics) are accepted and
// ignored: the control plane reads evidence itself.
func (a API) evaluateContinualOptimization(w http.ResponseWriter, r *http.Request) {
	checks, ok := a.Store.(stayOptimizedStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "continual optimization storage is not configured")
		return
	}
	if r.ContentLength != 0 {
		var ignored struct {
			PublicProfile string          `json:"public_profile,omitempty"`
			ReplayTraceID string          `json:"replay_trace_id,omitempty"`
			Hypotheses    json.RawMessage `json:"hypotheses,omitempty"`
			Economics     json.RawMessage `json:"economics,omitempty"`
		}
		if !decodeMutationBody(w, r, &ignored) {
			return
		}
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	name := r.PathValue("name")
	if _, err := checks.ContinualOptimizationPolicy(r.Context(), actor.TenantID, name); errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusConflict, "continual_optimization_policy_required", errStayOptimizedPolicyRequired.Error())
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "continual optimization policy could not be loaded")
		return
	}
	claim, err := checks.ClaimStayOptimizedCheck(r.Context(), actor.TenantID, name, "check-now:"+randomSuffix(), stayOptimizedLease)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	if errors.Is(err, domain.ErrConflict) {
		writeError(w, http.StatusConflict, "check_in_progress", "a check of this deployment is already running; try again in a minute")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "the check could not be started")
		return
	}
	trigger := continualoptimizer.ManualTrigger(actor.Name)
	result, err := a.runStayOptimizedCheck(r.Context(), checks, claim, &trigger)
	if errors.Is(err, errStayOptimizedPolicyRequired) {
		writeError(w, http.StatusConflict, "continual_optimization_policy_required", err.Error())
		return
	}
	if err != nil && !result.Evaluated {
		writeError(w, http.StatusUnprocessableEntity, "continual_optimization_check_failed", err.Error())
		return
	}
	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	response := map[string]any{"decision": result.Decision, "record_id": result.RecordID, "created": result.Created, "provider_mutation": false, "mutation": "decision_record_only"}
	if result.Proposal != nil {
		response["mutation"] = "proposed_run_created"
		response["proposal"] = stayOptimizedSuggestion(*result.Proposal)
	}
	if err != nil {
		response["warning"] = err.Error()
	}
	writeJSON(w, status, response)
}

func randomSuffix() string {
	buffer := make([]byte, 8)
	_, _ = rand.Read(buffer)
	return hex.EncodeToString(buffer)
}

// continualOptimizationSuggestions lists the runs Stay optimized proposed for
// a deployment with why, what they may change, and what they cost, plus when
// the deployment is checked next and its custom kernels' engine pins.
func (a API) continualOptimizationSuggestions(w http.ResponseWriter, r *http.Request) {
	checks, ok := a.Store.(stayOptimizedStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "capability_unavailable", "continual optimization storage is not configured")
		return
	}
	actor := r.Context().Value(identityKey{}).(domain.Principal)
	name := r.PathValue("name")
	resolved, err := a.Store.ResolveForTenant(r.Context(), actor.TenantID, name)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "deployment was not found")
		return
	}
	proposals, err := checks.StayOptimizedProposals(r.Context(), actor.TenantID, name, 10)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "proposed runs could not be listed")
		return
	}
	data := make([]map[string]any, 0, len(proposals))
	for _, campaign := range proposals {
		data = append(data, stayOptimizedSuggestion(campaign))
	}
	check := map[string]any{"last_check_at": nil, "next_check_at": nil, "last_check_error": nil, "scheduled_every_seconds": nil, "observed": continualoptimizer.Observed{}}
	if state, stateErr := checks.ContinualOptimizationState(r.Context(), actor.TenantID, name); stateErr == nil {
		check["last_check_at"], check["next_check_at"], check["observed"] = state.LastCheckAt, state.NextCheckAt, decodeObserved(state.ObservedJSON)
		if state.LastCheckError != "" {
			check["last_check_error"] = state.LastCheckError
		}
	}
	autoRun := map[string]any{"enabled": false, "monthly_cap_usd": nil, "approved_this_month_usd": nil}
	if policyRow, policyErr := checks.ContinualOptimizationPolicy(r.Context(), actor.TenantID, name); policyErr == nil {
		var policy continualoptimizer.Policy
		if json.Unmarshal([]byte(policyRow.PolicyJSON), &policy) == nil {
			check["scheduled_every_seconds"] = int(policy.CheckInterval().Seconds())
			autoRun["enabled"] = policy.AutoRun
			if policy.AutoRun {
				now := time.Now().UTC()
				autoRun["monthly_cap_usd"] = policy.MonthlyAutoRunCapUSD
				if spent, spendErr := checks.StayOptimizedAutoRunSpend(r.Context(), actor.TenantID, name, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)); spendErr == nil {
					autoRun["approved_this_month_usd"] = spent
				}
			}
		}
	}
	kernels := []map[string]any{}
	if rows, kernelErr := checks.EndpointCustomKernels(r.Context(), actor.TenantID, resolved.Deployment.ID); kernelErr == nil {
		for _, kernel := range rows {
			kernels = append(kernels, map[string]any{"id": kernel.ID, "operator": kernel.Operator, "family": kernel.Family, "engine": kernel.Engine, "engine_version": kernel.EngineVersion, "state": kernel.State, "state_reason": kernel.StateReason, "updated_at": kernel.UpdatedAt})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "check": check, "auto_run": autoRun, "kernels": kernels})
}

// stayOptimizedSuggestion is one proposed run as the endpoint page shows it.
func stayOptimizedSuggestion(campaign domain.OptimizationCampaign) map[string]any {
	origin, _ := continualoptimizer.DecodeOrigin(campaign.OriginJSON)
	return map[string]any{
		"campaign_id": campaign.ID, "state": campaign.State, "created_at": campaign.CreatedAt, "setups": len(campaign.Candidates),
		"reason": origin.Reason, "triggers": origin.Triggers, "expected_change": origin.ExpectedChange, "run_cost": origin.RunCost,
		"unknown_evidence": origin.UnknownEvidence, "auto_run": origin.AutoRun, "kernels": origin.Kernels,
		"approved_by": campaign.ApprovedBy, "approved_max_cost_usd": campaign.ApprovedMaxCostUSD, "approval_expires_at": campaign.ApprovalExpiresAt,
	}
}
