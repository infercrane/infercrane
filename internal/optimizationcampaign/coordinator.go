package optimizationcampaign

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

var (
	ErrExecutionAuthority = errors.New("optimization campaign does not have active execution authority")
	ErrApprovalExpired    = errors.New("optimization campaign approval expired")
)

// Repository is the fenced persistence boundary used by the coordinator.
// TransitionCandidate must compare the observed state so that two controllers
// cannot both become mutation owner for one candidate.
type Repository interface {
	OptimizationCampaign(context.Context, string, string) (domain.OptimizationCampaign, error)
	TransitionOptimizationCandidate(context.Context, string, string, string, string, string, domain.OptimizationCandidateRun) (domain.OptimizationCandidateRun, error)
}

// Budget is the maximum authority a driver receives for one candidate. A
// driver must enforce this bound at its provider or builder boundary.
type Budget struct {
	MaxCostUSD float64
	ExpiresAt  time.Time
}

type ProvisionResult struct {
	DeploymentName, RevisionID, OptimizedArtifactID string
}

type MeasurementResult struct {
	BenchmarkID, ActualEvidenceJSON string
}

type ValidationResult struct {
	QualityEvidenceID string
	Passed            bool
	FailureCode       string
}

type RankingResult struct {
	LabEvaluationID string
	Decision        string
	FailureCode     string
}

type GuardResult struct {
	EvaluationID string
	Decision     string
	FailureCode  string
}

// Driver composes existing deployment, benchmark, quality, Release Guard, and
// cleanup primitives. Calls must be idempotent for the supplied candidate ID.
// Implementations own no campaign state and cannot promote production.
type Driver interface {
	Provision(context.Context, string, domain.OptimizationCandidateRun, Budget) (ProvisionResult, error)
	Measure(context.Context, string, domain.OptimizationCandidateRun, Budget) (MeasurementResult, error)
	Validate(context.Context, string, domain.OptimizationCandidateRun) (ValidationResult, error)
	Rank(context.Context, string, domain.OptimizationCandidateRun) (RankingResult, error)
	Guard(context.Context, string, domain.OptimizationCandidateRun) (GuardResult, error)
	Cleanup(context.Context, string, domain.OptimizationCandidateRun) error
}

type StepResult struct {
	CampaignID, CandidateID, From, To string
	Progressed                        bool
	WaitingForHuman                   bool
}

type Coordinator struct {
	Repository Repository
	Driver     Driver
	Now        func() time.Time
}

// Step advances at most one durable boundary. Persisting intermediate states
// before invoking a driver makes every side effect re-adoptable after a crash.
// Promotion is intentionally absent: GuardPassed always waits for a separate,
// human-authorized Release Guard promotion.
func (c Coordinator) Step(ctx context.Context, tenant, campaignID, candidateID string) (StepResult, error) {
	if c.Repository == nil || c.Driver == nil || strings.TrimSpace(tenant) == "" || strings.TrimSpace(campaignID) == "" || strings.TrimSpace(candidateID) == "" {
		return StepResult{}, errors.New("optimization coordinator requires repository, driver, tenant, campaign, and candidate")
	}
	campaign, err := c.Repository.OptimizationCampaign(ctx, tenant, campaignID)
	if err != nil {
		return StepResult{}, err
	}
	candidate, found := candidateByID(campaign.Candidates, candidateID)
	if !found {
		return StepResult{}, domain.ErrNotFound
	}
	result := StepResult{CampaignID: campaign.ID, CandidateID: candidate.ID, From: candidate.State, To: candidate.State}
	if candidate.State == CandidateGuardPassed {
		result.WaitingForHuman = true
		return result, nil
	}
	if terminalWithoutCleanup(candidate.State) {
		if err = c.Driver.Cleanup(ctx, candidate.ID, candidate); err != nil {
			return result, fmt.Errorf("cleanup candidate: %w", err)
		}
		return c.transition(ctx, campaign, candidate, CandidateCleaned, domain.OptimizationCandidateRun{})
	}
	budget, err := c.budget(campaign)
	if err != nil {
		// An approved campaign whose authority expires must converge to cleanup,
		// not retry forever while paid resources remain allocated.
		if errors.Is(err, ErrApprovalExpired) {
			return c.transition(ctx, campaign, candidate, CandidateFailed, domain.OptimizationCandidateRun{FailureCode: "approval_expired"})
		}
		return result, err
	}

	switch candidate.State {
	case CandidateProposed:
		return c.transition(ctx, campaign, candidate, CandidateProvisioning, domain.OptimizationCandidateRun{})
	case CandidateProvisioning:
		provisioned, provisionErr := c.Driver.Provision(ctx, candidate.ID, candidate, budget)
		if provisionErr != nil {
			return result, fmt.Errorf("provision candidate: %w", provisionErr)
		}
		updates := domain.OptimizationCandidateRun{DeploymentName: provisioned.DeploymentName, RevisionID: provisioned.RevisionID, OptimizedArtifactID: provisioned.OptimizedArtifactID}
		return c.transition(ctx, campaign, candidate, CandidateReady, updates)
	case CandidateReady:
		return c.transition(ctx, campaign, candidate, CandidateMeasuring, domain.OptimizationCandidateRun{})
	case CandidateMeasuring:
		measured, measureErr := c.Driver.Measure(ctx, candidate.ID, candidate, budget)
		if measureErr != nil {
			return result, fmt.Errorf("measure candidate: %w", measureErr)
		}
		updates := domain.OptimizationCandidateRun{BenchmarkID: measured.BenchmarkID, ActualEvidenceJSON: measured.ActualEvidenceJSON}
		return c.transition(ctx, campaign, candidate, CandidateValidating, updates)
	case CandidateValidating:
		validated, validateErr := c.Driver.Validate(ctx, candidate.ID, candidate)
		if validateErr != nil {
			return result, fmt.Errorf("validate candidate: %w", validateErr)
		}
		updates := domain.OptimizationCandidateRun{BenchmarkID: candidate.BenchmarkID, QualityEvidenceID: validated.QualityEvidenceID, FailureCode: validated.FailureCode}
		if !validated.Passed {
			if updates.FailureCode == "" {
				updates.FailureCode = "quality_gate_rejected"
			}
			return c.transition(ctx, campaign, candidate, CandidateRejected, updates)
		}
		return c.transition(ctx, campaign, candidate, CandidateRanked, updates)
	case CandidateRanked:
		ranked, rankErr := c.Driver.Rank(ctx, candidate.ID, candidate)
		if rankErr != nil {
			return result, fmt.Errorf("rank measured candidate: %w", rankErr)
		}
		updates := domain.OptimizationCandidateRun{LabEvaluationID: ranked.LabEvaluationID, FailureCode: ranked.FailureCode}
		switch strings.ToUpper(strings.TrimSpace(ranked.Decision)) {
		case RankSupersede:
			if updates.FailureCode == "" {
				updates.FailureCode = "measured_candidate_superseded"
			}
			return c.transition(ctx, campaign, candidate, CandidateRejected, updates)
		case RankReject:
			if updates.FailureCode == "" {
				updates.FailureCode = "measured_candidate_outside_constraints"
			}
			return c.transition(ctx, campaign, candidate, CandidateRejected, updates)
		case RankInconclusive:
			if updates.FailureCode == "" {
				updates.FailureCode = "measured_ranking_inconclusive"
			}
			return c.transition(ctx, campaign, candidate, CandidateInconclusive, updates)
		case RankSelect:
			// Persist the Lab decision before Release Guard. The next durable
			// step receives a candidate that already carries this identity.
			return c.transition(ctx, campaign, candidate, CandidateGuarding, updates)
		default:
			updates.FailureCode = "measured_ranking_unknown_decision"
			return c.transition(ctx, campaign, candidate, CandidateInconclusive, updates)
		}
	case CandidateGuarding:
		guarded, guardErr := c.Driver.Guard(ctx, candidate.ID, candidate)
		if guardErr != nil {
			return result, fmt.Errorf("evaluate Release Guard: %w", guardErr)
		}
		updates := domain.OptimizationCandidateRun{ReleaseGuardEvaluationID: guarded.EvaluationID}
		if guarded.FailureCode != "" {
			updates.FailureCode = guarded.FailureCode
		}
		switch strings.ToUpper(strings.TrimSpace(guarded.Decision)) {
		case "PASS":
			return c.transition(ctx, campaign, candidate, CandidateGuardPassed, updates)
		case "REJECT":
			if updates.FailureCode == "" {
				updates.FailureCode = "release_guard_rejected"
			}
			return c.transition(ctx, campaign, candidate, CandidateRejected, updates)
		case "INCONCLUSIVE":
			if updates.FailureCode == "" {
				updates.FailureCode = "release_guard_inconclusive"
			}
			return c.transition(ctx, campaign, candidate, CandidateInconclusive, updates)
		default:
			updates.FailureCode = "release_guard_unknown_decision"
			return c.transition(ctx, campaign, candidate, CandidateInconclusive, updates)
		}
	default:
		return result, fmt.Errorf("candidate state %q requires an explicit operator action or is not executable", candidate.State)
	}
}

// CancelCandidate fences an active candidate before cleaning any resources it
// owns. It never cleans a promoted or observed candidate because that could
// remove production behind an operator's back.
func (c Coordinator) CancelCandidate(ctx context.Context, tenant, campaignID, candidateID string) (StepResult, error) {
	if c.Repository == nil || c.Driver == nil || strings.TrimSpace(tenant) == "" || strings.TrimSpace(campaignID) == "" || strings.TrimSpace(candidateID) == "" {
		return StepResult{}, errors.New("optimization coordinator requires repository, driver, tenant, campaign, and candidate")
	}
	campaign, err := c.Repository.OptimizationCampaign(ctx, tenant, campaignID)
	if err != nil {
		return StepResult{}, err
	}
	candidate, found := candidateByID(campaign.Candidates, candidateID)
	if !found {
		return StepResult{}, domain.ErrNotFound
	}
	result := StepResult{CampaignID: campaign.ID, CandidateID: candidate.ID, From: candidate.State, To: candidate.State}
	switch candidate.State {
	case CandidateCleaned:
		return result, nil
	case CandidatePromoted, CandidateObserved:
		return result, errors.New("promoted optimization candidate requires an explicit rollback, not execution cancellation")
	case CandidateRejected, CandidateInconclusive, CandidateFailed, CandidateCancelled:
		if err = c.Driver.Cleanup(ctx, candidate.ID, candidate); err != nil {
			return result, fmt.Errorf("cleanup candidate: %w", err)
		}
		return c.transition(ctx, campaign, candidate, CandidateCleaned, domain.OptimizationCandidateRun{})
	default:
		return c.transition(ctx, campaign, candidate, CandidateCancelled, domain.OptimizationCandidateRun{FailureCode: "execution_cancelled"})
	}
}

func (c Coordinator) transition(ctx context.Context, campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, to string, updates domain.OptimizationCandidateRun) (StepResult, error) {
	row, err := c.Repository.TransitionOptimizationCandidate(ctx, campaign.TenantID, campaign.ID, candidate.ID, candidate.State, to, updates)
	if err != nil {
		return StepResult{CampaignID: campaign.ID, CandidateID: candidate.ID, From: candidate.State, To: candidate.State}, err
	}
	return StepResult{CampaignID: campaign.ID, CandidateID: candidate.ID, From: candidate.State, To: row.State, Progressed: row.State != candidate.State, WaitingForHuman: row.State == CandidateGuardPassed}, nil
}

func (c Coordinator) budget(campaign domain.OptimizationCampaign) (Budget, error) {
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	if campaign.CancelRequested || (campaign.State != CampaignApproved && campaign.State != CampaignRunning && campaign.State != CampaignRanked) {
		return Budget{}, ErrExecutionAuthority
	}
	if campaign.ApprovedMaxCostUSD == nil || *campaign.ApprovedMaxCostUSD <= 0 || campaign.ApprovalExpiresAt == nil {
		return Budget{}, fmt.Errorf("%w: bounded cost or expiry is missing", ErrExecutionAuthority)
	}
	if !campaign.ApprovalExpiresAt.After(now) {
		return Budget{}, ErrApprovalExpired
	}
	count := campaign.MaxCandidates
	if count < 1 {
		count = len(campaign.Candidates)
	}
	if count < 1 {
		return Budget{}, errors.New("optimization campaign has no bounded candidates")
	}
	return Budget{MaxCostUSD: *campaign.ApprovedMaxCostUSD / float64(count), ExpiresAt: campaign.ApprovalExpiresAt.UTC()}, nil
}

func candidateByID(candidates []domain.OptimizationCandidateRun, id string) (domain.OptimizationCandidateRun, bool) {
	for _, candidate := range candidates {
		if candidate.ID == id {
			return candidate, true
		}
	}
	return domain.OptimizationCandidateRun{}, false
}

func terminalWithoutCleanup(state string) bool {
	return state == CandidateRejected || state == CandidateInconclusive || state == CandidateFailed || state == CandidateCancelled
}
