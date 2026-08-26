package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/performanceprofile"
	"github.com/infercrane/infercrane/internal/workflows"
)

// CompositeStore is the narrow persistence and child-operation boundary used
// by the real campaign driver. Provider mutations remain owned by the existing
// deployment and rollout handlers; this driver only submits and adopts their
// durable operations.
type CompositeStore interface {
	Repository
	SubmitCloudDeployment(context.Context, domain.Deployment, domain.Operation) (domain.Deployment, domain.Operation, bool, error)
	SubmitDeploymentDelete(context.Context, string, string, string, domain.Operation) (domain.Operation, bool, error)
	EnqueueOperation(context.Context, domain.Operation) (domain.Operation, bool, error)
	ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error)
	BenchmarksForDeployment(context.Context, string, string, int) ([]domain.BenchmarkResult, error)
	QualityEvidenceForDeployment(context.Context, string, string, int) ([]domain.QualityEvidence, error)
	ReleaseGuardEvaluations(context.Context, string, string, int) ([]domain.ReleaseGuardEvaluation, error)
	EvaluateReleaseGuard(context.Context, string, string, time.Duration) (domain.ReleaseGuardEvaluation, error)
}

type CostQuote struct {
	// HourlyUSD is the total sourced hourly rate for the exact maximum
	// replica count requested from CostAuthority, not a per-replica price.
	HourlyUSD, HardMaximumCostUSD float64
	Source                        string
	ObservedAt, ValidUntil        time.Time
}

type CostAuthority interface {
	Quote(context.Context, optimizer.DeploymentDraft, time.Time) (CostQuote, error)
}

type CandidateBenchmark interface {
	Run(context.Context, domain.OptimizationCampaign, domain.OptimizationCandidateRun, performanceprofile.Profile) (domain.BenchmarkResult, error)
}

type CandidateRanker interface {
	Rank(context.Context, domain.OptimizationCandidateRun) (RankingResult, error)
}

// CompositeDriver composes existing lifecycle and proof primitives. It never
// promotes. Every child mutation uses a stable candidate-derived idempotency
// key, so a lost response is adopted rather than replayed as new work.
type CompositeDriver struct {
	Store     CompositeStore
	Costs     CostAuthority
	Benchmark CandidateBenchmark
	Ranker    CandidateRanker
	Now       func() time.Time
}

func (d CompositeDriver) Provision(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun, budget Budget) (ProvisionResult, error) {
	campaign, draft, err := d.inputs(ctx, candidateID, candidate)
	if err != nil {
		return ProvisionResult{}, err
	}
	if err = d.authorizeCost(ctx, draft, budget); err != nil {
		return ProvisionResult{}, err
	}
	if campaign.Intent == IntentEvolveEndpoint {
		return d.provisionEvolution(ctx, campaign, candidate, draft)
	}
	return d.provisionNewEndpoint(ctx, campaign, candidate, draft)
}

func (d CompositeDriver) Measure(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun, budget Budget) (MeasurementResult, error) {
	campaign, _, err := d.inputs(ctx, candidateID, candidate)
	if err != nil {
		return MeasurementResult{}, err
	}
	if !budget.ExpiresAt.After(d.now()) {
		return MeasurementResult{}, ErrApprovalExpired
	}
	profileName, err := campaignProfile(campaign, candidate.ProposalCandidateID)
	if err != nil {
		return MeasurementResult{}, operations.Permanent("optimization_profile_invalid", err)
	}
	profile, err := performanceprofile.Get(profileName)
	if err != nil {
		return MeasurementResult{}, operations.Permanent("optimization_profile_invalid", err)
	}
	if benchmark, ok, lookupErr := d.adoptBenchmark(ctx, candidate, profile); lookupErr != nil {
		return MeasurementResult{}, operations.Retryable("optimization_benchmark_lookup_failed", lookupErr)
	} else if ok {
		return measuredResult(benchmark), nil
	}
	if d.Benchmark == nil {
		return MeasurementResult{}, operations.Permanent("optimization_benchmark_unavailable", errors.New("durable candidate benchmark executor is not configured"))
	}
	benchmark, err := d.Benchmark.Run(ctx, campaign, candidate, profile)
	if err != nil {
		var failure operations.Failure
		if errors.As(err, &failure) {
			return MeasurementResult{}, failure
		}
		return MeasurementResult{}, operations.Retryable("optimization_benchmark_failed", err)
	}
	if benchmark.ID == "" || benchmark.RevisionID != candidate.RevisionID {
		return MeasurementResult{}, operations.Permanent("optimization_benchmark_identity_mismatch", errors.New("candidate benchmark did not return exact revision-bound evidence"))
	}
	return measuredResult(benchmark), nil
}

func (d CompositeDriver) Validate(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun) (ValidationResult, error) {
	_, _, err := d.inputs(ctx, candidateID, candidate)
	if err != nil {
		return ValidationResult{}, err
	}
	rows, err := d.Store.QualityEvidenceForDeployment(ctx, candidate.TenantID, candidate.DeploymentName, 100)
	if err != nil {
		return ValidationResult{}, operations.Retryable("optimization_quality_lookup_failed", err)
	}
	var selected *domain.QualityEvidence
	for index := range rows {
		row := &rows[index]
		// A revision is immutable and signed quality evidence is bound to that
		// exact identity. Requiring it to be newer than the candidate state
		// transition incorrectly rejects evidence attached between measurement
		// and validation.
		if row.RevisionID != candidate.RevisionID || selected != nil && !row.EvaluatedAt.After(selected.EvaluatedAt) {
			continue
		}
		selected = row
	}
	if selected != nil {
		if !selected.Passed {
			return ValidationResult{QualityEvidenceID: selected.ID, Passed: false, FailureCode: "quality_gate_rejected"}, nil
		}
		return ValidationResult{QualityEvidenceID: selected.ID, Passed: true}, nil
	}
	return ValidationResult{}, operations.Retryable("optimization_quality_evidence_pending", errors.New("attach passing signed semantic quality evidence for the exact candidate revision"))
}

func (d CompositeDriver) Rank(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun) (RankingResult, error) {
	if _, _, err := d.inputs(ctx, candidateID, candidate); err != nil {
		return RankingResult{}, err
	}
	if d.Ranker == nil {
		return RankingResult{}, operations.Permanent("optimization_ranker_unavailable", errors.New("measured campaign ranker is not configured"))
	}
	return d.Ranker.Rank(ctx, candidate)
}

func (d CompositeDriver) Guard(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun) (GuardResult, error) {
	campaign, _, err := d.inputs(ctx, candidateID, candidate)
	if err != nil {
		return GuardResult{}, err
	}
	if campaign.Intent != IntentEvolveEndpoint || campaign.TargetDeployment == "" || candidate.DeploymentName != campaign.TargetDeployment {
		return GuardResult{}, operations.Permanent("optimization_guard_boundary_invalid", errors.New("Release Guard requires an evolve-endpoint campaign bound to its target deployment"))
	}
	rows, err := d.Store.ReleaseGuardEvaluations(ctx, candidate.TenantID, candidate.DeploymentName, 20)
	if err != nil {
		return GuardResult{}, operations.Retryable("optimization_release_guard_lookup_failed", err)
	}
	for _, row := range rows {
		if row.CandidateRevisionID == candidate.RevisionID && !row.CreatedAt.Before(candidate.UpdatedAt) {
			return guardResult(row)
		}
	}
	evaluation, err := d.Store.EvaluateReleaseGuard(ctx, candidate.TenantID, candidate.DeploymentName, 15*time.Minute)
	if err != nil {
		return GuardResult{}, operations.Retryable("optimization_release_guard_failed", err)
	}
	return guardResult(evaluation)
}

func guardResult(evaluation domain.ReleaseGuardEvaluation) (GuardResult, error) {
	decision := strings.ToUpper(strings.TrimSpace(evaluation.Decision))
	if decision == "WAIT" {
		return GuardResult{}, operations.Retryable("optimization_release_guard_waiting", errors.New("Release Guard requires more fresh comparable evidence"))
	}
	if decision != "PASS" && decision != "REJECT" && decision != "INCONCLUSIVE" {
		decision = "INCONCLUSIVE"
	}
	return GuardResult{EvaluationID: evaluation.ID, Decision: decision}, nil
}

func (d CompositeDriver) Cleanup(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun) error {
	campaign, _, err := d.inputs(ctx, candidateID, candidate)
	if err != nil {
		return err
	}
	if candidate.DeploymentName == "" {
		return nil
	}
	if campaign.Intent == IntentEvolveEndpoint {
		request, _ := json.Marshal(workflows.RolloutRequest{Name: campaign.TargetDeployment, CandidateID: candidate.RevisionID, Reason: "optimization campaign cleanup", TenantID: candidate.TenantID, Actor: "optimization-campaign"})
		operation, _, enqueueErr := d.Store.EnqueueOperation(ctx, domain.Operation{TenantID: candidate.TenantID, Kind: workflows.RolloutRejectKind, ResourceType: "deployment", ResourceName: campaign.TargetDeployment, IdempotencyKey: childKey(candidate.ID, "cleanup"), RequestJSON: string(request), MaxAttempts: 120})
		if enqueueErr != nil {
			return enqueueErr
		}
		return childComplete(operation, "candidate cleanup")
	}
	resolved, resolveErr := d.Store.ResolveForTenant(ctx, candidate.TenantID, candidate.DeploymentName)
	if errors.Is(resolveErr, domain.ErrNotFound) {
		return nil
	}
	if resolveErr != nil {
		return resolveErr
	}
	request, _ := json.Marshal(workflows.DeleteRequest{DeploymentID: resolved.Deployment.ID, Name: candidate.DeploymentName, Actor: "optimization-campaign", TenantID: candidate.TenantID})
	kind := workflows.DeleteKind
	if draft, draftErr := candidateDraft(candidate); draftErr == nil && draft.Compute.Mode == "serverless" {
		kind = workflows.ServerlessDeleteKind
	}
	operation, _, submitErr := d.Store.SubmitDeploymentDelete(ctx, candidate.TenantID, candidate.DeploymentName, resolved.Deployment.ID, domain.Operation{TenantID: candidate.TenantID, Kind: kind, IdempotencyKey: childKey(candidate.ID, "cleanup"), RequestJSON: string(request), MaxAttempts: 120})
	if errors.Is(submitErr, domain.ErrConflict) && resolved.Deployment.DesiredState == "deleted" {
		return nil
	}
	if submitErr != nil {
		return submitErr
	}
	return childComplete(operation, "deployment cleanup")
}

func (d CompositeDriver) provisionNewEndpoint(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft) (ProvisionResult, error) {
	request := draftCloudRequest(draft)
	request.Name = candidateDeploymentName(draft.Name, candidate.ID)
	request.TenantID, request.Actor = candidate.TenantID, "optimization-campaign"
	encoded, _ := json.Marshal(request)
	kind := workflows.ConvergeKind
	if request.ComputeMode == "serverless" {
		kind = workflows.ServerlessConvergeKind
	}
	deployment, operation, _, err := d.Store.SubmitCloudDeployment(ctx, domain.Deployment{TenantID: candidate.TenantID, Name: request.Name, Model: request.Model, Runtime: request.Runtime, MinReplicas: request.MinReplicas, MaxReplicas: request.MaxReplicas, AutoscalingEnabled: request.MaxReplicas > request.MinReplicas}, domain.Operation{TenantID: candidate.TenantID, Kind: kind, IdempotencyKey: childKey(candidate.ID, "provision"), RequestJSON: string(encoded), MaxAttempts: 120})
	if err != nil {
		return ProvisionResult{}, err
	}
	if err = childComplete(operation, "new endpoint provisioning"); err != nil {
		return ProvisionResult{}, err
	}
	resolved, err := d.Store.ResolveForTenant(ctx, candidate.TenantID, deployment.Name)
	if err != nil || resolved.Deployment.ActiveRevisionID == "" {
		return ProvisionResult{}, operations.Retryable("optimization_candidate_not_ready", errors.New("provisioned deployment has no active immutable revision"))
	}
	return ProvisionResult{DeploymentName: deployment.Name, RevisionID: resolved.Deployment.ActiveRevisionID}, nil
}

func (d CompositeDriver) provisionEvolution(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, draft optimizer.DeploymentDraft) (ProvisionResult, error) {
	spec, _ := json.Marshal(draftRevisionSpec(draft))
	createRequest, _ := json.Marshal(workflows.RolloutRequest{Name: campaign.TargetDeployment, Spec: spec, TenantID: candidate.TenantID, Actor: "optimization-campaign"})
	create, _, err := d.Store.EnqueueOperation(ctx, domain.Operation{TenantID: candidate.TenantID, Kind: workflows.RolloutCreateKind, ResourceType: "deployment", ResourceName: campaign.TargetDeployment, IdempotencyKey: childKey(candidate.ID, "create-revision"), RequestJSON: string(createRequest), MaxAttempts: 5})
	if err != nil {
		return ProvisionResult{}, err
	}
	if err = childComplete(create, "candidate revision creation"); err != nil {
		return ProvisionResult{}, err
	}
	var created struct {
		CandidateID string `json:"candidate_id"`
	}
	if json.Unmarshal([]byte(create.ResultJSON), &created) != nil || created.CandidateID == "" {
		return ProvisionResult{}, operations.Permanent("optimization_child_result_invalid", errors.New("candidate creation result omitted revision identity"))
	}
	provisionRequest, _ := json.Marshal(workflows.RolloutRequest{Name: campaign.TargetDeployment, CandidateID: created.CandidateID, TenantID: candidate.TenantID, Actor: "optimization-campaign"})
	provision, _, err := d.Store.EnqueueOperation(ctx, domain.Operation{TenantID: candidate.TenantID, Kind: workflows.RolloutProvisionKind, ResourceType: "deployment", ResourceName: campaign.TargetDeployment, IdempotencyKey: childKey(candidate.ID, "provision"), RequestJSON: string(provisionRequest), MaxAttempts: 120})
	if err != nil {
		return ProvisionResult{}, err
	}
	if err = childComplete(provision, "candidate revision provisioning"); err != nil {
		return ProvisionResult{}, err
	}
	return ProvisionResult{DeploymentName: campaign.TargetDeployment, RevisionID: created.CandidateID}, nil
}

func (d CompositeDriver) inputs(ctx context.Context, candidateID string, candidate domain.OptimizationCandidateRun) (domain.OptimizationCampaign, optimizer.DeploymentDraft, error) {
	if d.Store == nil || candidate.ID == "" || candidate.ID != candidateID || candidate.TenantID == "" || candidate.CampaignID == "" {
		return domain.OptimizationCampaign{}, optimizer.DeploymentDraft{}, operations.Permanent("optimization_driver_invalid", errors.New("composite driver requires store and exact candidate identity"))
	}
	campaign, err := d.Store.OptimizationCampaign(ctx, candidate.TenantID, candidate.CampaignID)
	if err != nil {
		return campaign, optimizer.DeploymentDraft{}, err
	}
	if campaign.Intent != IntentNewEndpoint && campaign.Intent != IntentEvolveEndpoint {
		return campaign, optimizer.DeploymentDraft{}, operations.Permanent("optimization_intent_invalid", errors.New("campaign intent is unsupported"))
	}
	draft, draftErr := candidateDraft(candidate)
	if draftErr != nil {
		return campaign, draft, operations.Permanent("optimization_candidate_spec_invalid", errors.New("candidate deployment spec is invalid"))
	}
	return campaign, draft, nil
}

func (d CompositeDriver) authorizeCost(ctx context.Context, draft optimizer.DeploymentDraft, budget Budget) error {
	return AuthorizeCost(ctx, d.Costs, draft, budget, d.now())
}

// AuthorizeCost proves that one candidate's exact maximum execution tuple is
// covered by both a fresh sourced price and the operator's bounded approval.
// The API calls this before approval; the durable driver repeats it immediately
// before every provider mutation so a stale preflight cannot become authority.
func AuthorizeCost(ctx context.Context, costs CostAuthority, draft optimizer.DeploymentDraft, budget Budget, now time.Time) error {
	if costs == nil {
		return operations.Permanent("optimization_cost_authority_unavailable", errors.New("fresh sourced execution pricing is required before provider mutation"))
	}
	now = now.UTC()
	quote, err := costs.Quote(ctx, draft, budget.ExpiresAt)
	if err != nil {
		return operations.Permanent("optimization_cost_quote_unavailable", err)
	}
	if quote.Source == "" || quote.ObservedAt.IsZero() || quote.ObservedAt.After(now) || quote.ValidUntil.Before(budget.ExpiresAt) || quote.HourlyUSD <= 0 || math.IsNaN(quote.HourlyUSD) || math.IsInf(quote.HourlyUSD, 0) {
		return operations.Permanent("optimization_cost_quote_invalid", errors.New("execution cost quote must be fresh, sourced, finite, and positive"))
	}
	projected := quote.HourlyUSD * budget.ExpiresAt.Sub(now).Hours()
	if quote.HardMaximumCostUSD > 0 && quote.HardMaximumCostUSD < projected {
		projected = quote.HardMaximumCostUSD
	}
	if projected > budget.MaxCostUSD {
		return operations.Permanent("optimization_cost_cap_exceeded", fmt.Errorf("maximum authorized candidate spend $%.2f is below sourced worst-case $%.2f", budget.MaxCostUSD, projected))
	}
	return nil
}

func (d CompositeDriver) adoptBenchmark(ctx context.Context, candidate domain.OptimizationCandidateRun, profile performanceprofile.Profile) (domain.BenchmarkResult, bool, error) {
	rows, err := d.Store.BenchmarksForDeployment(ctx, candidate.TenantID, candidate.DeploymentName, 100)
	if err != nil {
		return domain.BenchmarkResult{}, false, err
	}
	for _, row := range rows {
		if row.RevisionID != candidate.RevisionID || row.CreatedAt.Before(candidate.UpdatedAt) {
			continue
		}
		var workload struct {
			Profile        string `json:"profile"`
			ProfileVersion string `json:"profile_version"`
		}
		if json.Unmarshal([]byte(row.WorkloadJSON), &workload) == nil && workload.Profile == profile.Name && workload.ProfileVersion == performanceprofile.Version {
			return row, true, nil
		}
	}
	return domain.BenchmarkResult{}, false, nil
}

func (d CompositeDriver) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

func campaignProfile(campaign domain.OptimizationCampaign, proposalCandidateID string) (string, error) {
	var proposal optimizer.Proposal
	if json.Unmarshal([]byte(campaign.ProposalJSON), &proposal) != nil {
		return "", errors.New("campaign proposal is invalid")
	}
	for _, candidate := range proposal.Candidates {
		if candidate.ID == proposalCandidateID && candidate.BenchmarkProfile != "" {
			return candidate.BenchmarkProfile, nil
		}
	}
	return "", errors.New("campaign proposal omitted candidate benchmark profile")
}

func childComplete(operation domain.Operation, label string) error {
	switch operation.Status {
	case "succeeded":
		return nil
	case "failed", "cancelled":
		message := operation.Message
		if message == "" {
			message = label + " failed"
		}
		if operation.Retryable {
			return operations.Retryable("optimization_child_retryable", errors.New(message))
		}
		return operations.Permanent("optimization_child_failed", errors.New(message))
	default:
		return operations.Retryable("optimization_child_pending", fmt.Errorf("%s is %s", label, operation.Status))
	}
}

func draftCloudRequest(draft optimizer.DeploymentDraft) workflows.CloudRequest {
	return workflows.CloudRequest{Name: draft.Name, Model: draft.Model.ID, ModelRevision: draft.Model.Revision, Runtime: draft.Runtime.Engine, RuntimeVersion: draft.Runtime.Version, RuntimeArgs: draft.Runtime.Args, Workload: draft.Runtime.Workload, Cloud: draft.Provider.Cloud, ProviderAdapter: draft.Provider.Adapter, ComputeMode: draft.Compute.Mode, GPU: draft.Resources.GPU, GPUCount: draft.Resources.GPUCount, Region: draft.Provider.Region, MinReplicas: draft.Scaling.MinReplicas, MaxReplicas: draft.Scaling.MaxReplicas, Serving: draft.Serving}
}

func draftRevisionSpec(draft optimizer.DeploymentDraft) domain.DeploymentRevisionSpec {
	return domain.DeploymentRevisionSpec{Model: draft.Model.ID, ModelRevision: draft.Model.Revision, Runtime: draft.Runtime.Engine, RuntimeVersion: draft.Runtime.Version, RuntimeArgs: draft.Runtime.Args, Workload: draft.Runtime.Workload, RoutingStrategy: draft.Routing.Strategy, MinReplicas: draft.Scaling.MinReplicas, MaxReplicas: draft.Scaling.MaxReplicas, AutoscalingEnabled: draft.Scaling.MaxReplicas > draft.Scaling.MinReplicas, ComputeMode: draft.Compute.Mode, Cloud: draft.Provider.Cloud, ProviderAdapter: draft.Provider.Adapter, GPU: draft.Resources.GPU, GPUCount: draft.Resources.GPUCount, Region: draft.Provider.Region, Serving: draft.Serving}
}

func childKey(candidateID, step string) string { return "optimization:" + candidateID + ":" + step }

func candidateDeploymentName(base, candidateID string) string {
	suffix := candidateID
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	maximumBase := 63 - len("-opt-") - len(suffix)
	base = strings.Trim(strings.ToLower(base), "-")
	if base == "" {
		base = "candidate"
	}
	if len(base) > maximumBase {
		base = strings.TrimRight(base[:maximumBase], "-")
	}
	return base + "-opt-" + suffix
}

func candidateDraft(candidate domain.OptimizationCandidateRun) (optimizer.DeploymentDraft, error) {
	var draft optimizer.DeploymentDraft
	if json.Unmarshal([]byte(candidate.DeploymentSpecJSON), &draft) != nil || strings.TrimSpace(draft.Name) == "" || strings.TrimSpace(draft.Model.ID) == "" {
		return draft, errors.New("candidate deployment spec is invalid")
	}
	return draft, nil
}

func measuredResult(row domain.BenchmarkResult) MeasurementResult {
	evidence, _ := json.Marshal(map[string]any{"benchmark_id": row.ID, "revision_id": row.RevisionID, "workload": json.RawMessage(row.WorkloadJSON), "request_count": row.RequestCount, "failed": row.Failed, "ttft_p95_ms": row.TTFTP95MS, "tpot_p95_ms": row.TPOTP95MS, "latency_p95_ms": row.LatencyP95MS, "output_tokens_second": row.OutputTokenThroughput, "goodput": row.Goodput, "cost": json.RawMessage(row.CostMetadataJSON)})
	return MeasurementResult{BenchmarkID: row.ID, ActualEvidenceJSON: string(evidence)}
}
