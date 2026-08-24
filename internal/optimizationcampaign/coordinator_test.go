package optimizationcampaign

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type coordinatorRepository struct {
	campaign    domain.OptimizationCampaign
	transitions int
}

func (r *coordinatorRepository) OptimizationCampaign(_ context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	if r.campaign.TenantID != tenant || r.campaign.ID != id {
		return domain.OptimizationCampaign{}, domain.ErrNotFound
	}
	return r.campaign, nil
}

func (r *coordinatorRepository) TransitionOptimizationCandidate(_ context.Context, tenant, campaignID, candidateID, from, to string, updates domain.OptimizationCandidateRun) (domain.OptimizationCandidateRun, error) {
	if r.campaign.TenantID != tenant || r.campaign.ID != campaignID {
		return domain.OptimizationCandidateRun{}, domain.ErrNotFound
	}
	if err := ValidateCandidateTransition(from, to); err != nil {
		return domain.OptimizationCandidateRun{}, err
	}
	for index := range r.campaign.Candidates {
		candidate := &r.campaign.Candidates[index]
		if candidate.ID != candidateID {
			continue
		}
		if candidate.State != from {
			return *candidate, domain.ErrConflict
		}
		candidate.State = to
		candidate.EvidenceState, _ = EvidenceForState(to)
		mergeCandidate(candidate, updates)
		r.transitions++
		states := make([]string, 0, len(r.campaign.Candidates))
		for _, row := range r.campaign.Candidates {
			states = append(states, row.State)
		}
		if state, err := AggregateState(states); err == nil {
			r.campaign.State = state
		}
		return *candidate, nil
	}
	return domain.OptimizationCandidateRun{}, domain.ErrNotFound
}

func mergeCandidate(candidate *domain.OptimizationCandidateRun, updates domain.OptimizationCandidateRun) {
	if updates.DeploymentName != "" {
		candidate.DeploymentName = updates.DeploymentName
	}
	if updates.RevisionID != "" {
		candidate.RevisionID = updates.RevisionID
	}
	if updates.BenchmarkID != "" {
		candidate.BenchmarkID = updates.BenchmarkID
	}
	if updates.QualityEvidenceID != "" {
		candidate.QualityEvidenceID = updates.QualityEvidenceID
	}
	if updates.LabEvaluationID != "" {
		candidate.LabEvaluationID = updates.LabEvaluationID
	}
	if updates.ReleaseGuardEvaluationID != "" {
		candidate.ReleaseGuardEvaluationID = updates.ReleaseGuardEvaluationID
	}
	if updates.OptimizedArtifactID != "" {
		candidate.OptimizedArtifactID = updates.OptimizedArtifactID
	}
	if updates.ActualEvidenceJSON != "" {
		candidate.ActualEvidenceJSON = updates.ActualEvidenceJSON
	}
	if updates.FailureCode != "" {
		candidate.FailureCode = updates.FailureCode
	}
}

type coordinatorDriver struct {
	provisionCalls, measureCalls, validateCalls, rankCalls, guardCalls, cleanupCalls int
	lastBudget                                                                       Budget
	qualityPass                                                                      bool
	guardDecision                                                                    string
	rankDecision                                                                     string
	rankDecisions                                                                    map[string]string
	provisionErr                                                                     error
	guardCandidate                                                                   domain.OptimizationCandidateRun
}

func (d *coordinatorDriver) Provision(_ context.Context, _ string, _ domain.OptimizationCandidateRun, budget Budget) (ProvisionResult, error) {
	d.provisionCalls++
	d.lastBudget = budget
	if d.provisionErr != nil {
		return ProvisionResult{}, d.provisionErr
	}
	return ProvisionResult{DeploymentName: "candidate-deployment", RevisionID: "rev-candidate"}, nil
}

func (d *coordinatorDriver) Measure(_ context.Context, _ string, _ domain.OptimizationCandidateRun, budget Budget) (MeasurementResult, error) {
	d.measureCalls++
	d.lastBudget = budget
	return MeasurementResult{BenchmarkID: "benchmark-1", ActualEvidenceJSON: `{"ttft_p95_ms":180}`}, nil
}

func (d *coordinatorDriver) Validate(context.Context, string, domain.OptimizationCandidateRun) (ValidationResult, error) {
	d.validateCalls++
	return ValidationResult{QualityEvidenceID: "quality-1", Passed: d.qualityPass}, nil
}

func (d *coordinatorDriver) Rank(_ context.Context, candidateID string, _ domain.OptimizationCandidateRun) (RankingResult, error) {
	d.rankCalls++
	decision := d.rankDecision
	if d.rankDecisions != nil && d.rankDecisions[candidateID] != "" {
		decision = d.rankDecisions[candidateID]
	}
	return RankingResult{LabEvaluationID: "lab-1", Decision: decision}, nil
}

func (d *coordinatorDriver) Guard(_ context.Context, _ string, candidate domain.OptimizationCandidateRun) (GuardResult, error) {
	d.guardCalls++
	d.guardCandidate = candidate
	return GuardResult{EvaluationID: "guard-1", Decision: d.guardDecision}, nil
}

func (d *coordinatorDriver) Cleanup(context.Context, string, domain.OptimizationCandidateRun) error {
	d.cleanupCalls++
	return nil
}

func approvedCoordinatorFixture(now time.Time, candidates int) (*coordinatorRepository, *coordinatorDriver, Coordinator) {
	cost := 20.0
	expiry := now.Add(time.Hour)
	runs := make([]domain.OptimizationCandidateRun, candidates)
	for index := range runs {
		runs[index] = domain.OptimizationCandidateRun{ID: "candidate-" + string(rune('a'+index)), TenantID: "tenant", CampaignID: "campaign", State: CandidateProposed, EvidenceState: "unmeasured", BenchmarkID: ""}
	}
	repository := &coordinatorRepository{campaign: domain.OptimizationCampaign{ID: "campaign", TenantID: "tenant", State: CampaignApproved, ApprovedMaxCostUSD: &cost, ApprovalExpiresAt: &expiry, MaxCandidates: candidates, Candidates: runs}}
	driver := &coordinatorDriver{qualityPass: true, rankDecision: RankSelect, guardDecision: "PASS"}
	coordinator := Coordinator{Repository: repository, Driver: driver, Now: func() time.Time { return now }}
	return repository, driver, coordinator
}

func TestCoordinatorRequiresBoundedUnexpiredApprovalBeforeMutation(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	repository.campaign.State = CampaignAwaitingApproval
	if _, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a"); err == nil {
		t.Fatal("candidate progressed without approved execution authority")
	}
	if driver.provisionCalls != 0 || repository.transitions != 0 {
		t.Fatalf("unapproved campaign mutated state: driver=%d transitions=%d", driver.provisionCalls, repository.transitions)
	}

	repository.campaign.State = CampaignApproved
	expired := now.Add(-time.Second)
	repository.campaign.ApprovalExpiresAt = &expired
	result, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a")
	if err != nil || result.To != CandidateFailed {
		t.Fatalf("expired candidate did not fail closed: result=%+v err=%v", result, err)
	}
	if driver.provisionCalls != 0 || repository.transitions != 1 || repository.campaign.Candidates[0].FailureCode != "approval_expired" {
		t.Fatalf("expired campaign mutated state: driver=%d transitions=%d", driver.provisionCalls, repository.transitions)
	}
}

func TestCoordinatorResumesEveryBoundaryAndStopsBeforePromotion(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 2)
	for step := 0; step < 7; step++ {
		result, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a")
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		if !result.Progressed {
			t.Fatalf("step %d did not progress: %+v", step, result)
		}
	}
	candidate := repository.campaign.Candidates[0]
	if candidate.State != CandidateGuardPassed || candidate.EvidenceState != "qualified" || candidate.DeploymentName != "candidate-deployment" || candidate.RevisionID != "rev-candidate" || candidate.BenchmarkID != "benchmark-1" || candidate.QualityEvidenceID != "quality-1" || candidate.LabEvaluationID != "lab-1" || candidate.ReleaseGuardEvaluationID != "guard-1" {
		t.Fatalf("unexpected qualified candidate: %+v", candidate)
	}
	if driver.provisionCalls != 1 || driver.measureCalls != 1 || driver.validateCalls != 1 || driver.rankCalls != 1 || driver.guardCalls != 1 || driver.cleanupCalls != 0 {
		t.Fatalf("unexpected driver calls: %+v", driver)
	}
	if driver.guardCandidate.State != CandidateGuarding || driver.guardCandidate.LabEvaluationID != "lab-1" {
		t.Fatalf("Release Guard did not receive persisted Lab evidence: %+v", driver.guardCandidate)
	}
	if math.Abs(driver.lastBudget.MaxCostUSD-10) > 0.0001 || !driver.lastBudget.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("candidate did not receive its bounded share: %+v", driver.lastBudget)
	}
	result, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a")
	if err != nil || result.Progressed || !result.WaitingForHuman || repository.campaign.Candidates[0].State != CandidateGuardPassed {
		t.Fatalf("coordinator crossed human promotion boundary: result=%+v err=%v", result, err)
	}
}

func TestCoordinatorPersistsRankingBeforeInvokingReleaseGuard(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	for step := 0; step < 6; step++ {
		result, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a")
		if err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		if !result.Progressed {
			t.Fatalf("step %d did not progress: %+v", step, result)
		}
	}
	candidate := repository.campaign.Candidates[0]
	if candidate.State != CandidateGuarding || candidate.LabEvaluationID != "lab-1" || candidate.ReleaseGuardEvaluationID != "" || driver.guardCalls != 0 {
		t.Fatalf("ranking and Guard were not separated by a durable boundary: candidate=%+v driver=%+v", candidate, driver)
	}
	if _, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a"); err != nil {
		t.Fatal(err)
	}
	if driver.guardCalls != 1 || driver.guardCandidate.LabEvaluationID != "lab-1" {
		t.Fatalf("Guard did not observe durable rank evidence: %+v", driver)
	}
}

func TestCoordinatorRejectsQualityRegressionAndCleansCandidate(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	driver.qualityPass = false
	for step := 0; step < 5; step++ {
		if _, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a"); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
	}
	if state := repository.campaign.Candidates[0].State; state != CandidateRejected {
		t.Fatalf("quality regression reached %q, want rejected", state)
	}
	if _, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a"); err != nil {
		t.Fatal(err)
	}
	if state := repository.campaign.Candidates[0].State; state != CandidateCleaned {
		t.Fatalf("rejected candidate reached %q, want cleaned", state)
	}
	if driver.guardCalls != 0 || driver.cleanupCalls != 1 || repository.campaign.Candidates[0].FailureCode != "quality_gate_rejected" {
		t.Fatalf("unsafe rejection behavior: driver=%+v candidate=%+v", driver, repository.campaign.Candidates[0])
	}
}

func TestCoordinatorLeavesRetryableDriverFailureAtAdoptableState(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	if _, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a"); err != nil {
		t.Fatal(err)
	}
	driver.provisionErr = errors.New("response lost after remote create")
	if _, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a"); err == nil {
		t.Fatal("lost response was silently accepted")
	}
	if state := repository.campaign.Candidates[0].State; state != CandidateProvisioning {
		t.Fatalf("lost response left non-adoptable state %q", state)
	}
	driver.provisionErr = nil
	if _, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a"); err != nil {
		t.Fatal(err)
	}
	if driver.provisionCalls != 2 || repository.campaign.Candidates[0].State != CandidateReady {
		t.Fatalf("idempotent retry did not adopt progress: calls=%d state=%s", driver.provisionCalls, repository.campaign.Candidates[0].State)
	}
}

func TestCoordinatorExpiresAuthorityAndCleansAnAllocatedCandidate(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	if _, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a"); err != nil {
		t.Fatal(err)
	}
	if state := repository.campaign.Candidates[0].State; state != CandidateReady {
		t.Fatalf("candidate reached %q before expiry, want ready", state)
	}
	now = now.Add(2 * time.Hour)
	coordinator.Now = func() time.Time { return now }
	result, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a")
	if err != nil || result.To != CandidateFailed {
		t.Fatalf("expired authority did not fail closed: result=%+v err=%v", result, err)
	}
	if driver.measureCalls != 0 || repository.campaign.Candidates[0].FailureCode != "approval_expired" {
		t.Fatalf("expired candidate continued paid work: driver=%+v candidate=%+v", driver, repository.campaign.Candidates[0])
	}
	if _, err = coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a"); err != nil {
		t.Fatal(err)
	}
	if state := repository.campaign.Candidates[0].State; state != CandidateCleaned || driver.cleanupCalls != 1 {
		t.Fatalf("expired candidate was not cleaned: state=%s cleanup=%d", state, driver.cleanupCalls)
	}
}

func TestCoordinatorCancellationNeverRemovesPromotedProduction(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	repository.campaign.Candidates[0].State = CandidateProvisioning
	result, err := coordinator.CancelCandidate(context.Background(), "tenant", "campaign", "candidate-a")
	if err != nil || result.To != CandidateCancelled {
		t.Fatalf("active candidate was not fenced before cleanup: result=%+v err=%v", result, err)
	}
	if driver.cleanupCalls != 0 {
		t.Fatal("cleanup ran before cancellation was durably fenced")
	}
	if _, err = coordinator.CancelCandidate(context.Background(), "tenant", "campaign", "candidate-a"); err != nil {
		t.Fatal(err)
	}
	if repository.campaign.Candidates[0].State != CandidateCleaned || driver.cleanupCalls != 1 {
		t.Fatalf("cancelled candidate was not cleaned exactly once: candidate=%+v cleanup=%d", repository.campaign.Candidates[0], driver.cleanupCalls)
	}

	repository.campaign.Candidates[0].State = CandidatePromoted
	repository.campaign.State = CampaignPromoted
	if _, err = coordinator.CancelCandidate(context.Background(), "tenant", "campaign", "candidate-a"); err == nil {
		t.Fatal("promoted production candidate was cancellable through execution cleanup")
	}
	if driver.cleanupCalls != 1 {
		t.Fatal("promoted production resources were cleaned")
	}
}

func TestCoordinatorTreatsUnknownGuardDecisionAsInconclusive(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	driver.guardDecision = "NEW_UPSTREAM_ENUM"
	for step := 0; step < 7; step++ {
		if _, err := coordinator.Step(context.Background(), "tenant", "campaign", "candidate-a"); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
	}
	candidate := repository.campaign.Candidates[0]
	if candidate.State != CandidateInconclusive || candidate.FailureCode != "release_guard_unknown_decision" {
		t.Fatalf("unknown guard decision did not fail closed: %+v", candidate)
	}
}
