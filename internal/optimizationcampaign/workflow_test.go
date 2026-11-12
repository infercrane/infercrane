package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/workflows"
)

type activationStoreFixture struct {
	*coordinatorRepository
	operations          map[string]domain.Operation
	publishedEndpoint   string
	publishedDeployment string
}

func (f *activationStoreFixture) EnqueueOperation(_ context.Context, operation domain.Operation) (domain.Operation, bool, error) {
	if f.operations == nil {
		f.operations = map[string]domain.Operation{}
	}
	if existing, found := f.operations[operation.IdempotencyKey]; found {
		return existing, false, nil
	}
	operation.ID = "operation-" + operation.IdempotencyKey
	operation.Status = "pending"
	f.operations[operation.IdempotencyKey] = operation
	return operation, true, nil
}

func (f *activationStoreFixture) PublishDeploymentEndpoint(_ context.Context, tenant, endpoint, deployment string) (domain.ResolvedEndpoint, error) {
	if tenant != f.campaign.TenantID {
		return domain.ResolvedEndpoint{}, domain.ErrNotFound
	}
	f.publishedEndpoint, f.publishedDeployment = endpoint, deployment
	return domain.ResolvedEndpoint{Endpoint: domain.Endpoint{Name: endpoint, TenantID: tenant}}, nil
}

func TestExecutionHandlerRunsProofLoopAndNeverPromotes(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	request, _ := json.Marshal(ExecuteRequest{TenantID: "tenant", CampaignID: "campaign", Candidates: []string{"candidate-a"}})
	handler := Handlers(coordinator)[ExecuteKind]
	result, err := handler(context.Background(), domain.Operation{RequestJSON: string(request)})
	assertWaitingForHuman(t, result, err)
	if repository.campaign.Candidates[0].State != CandidateGuardPassed || driver.provisionCalls != 1 || driver.measureCalls != 1 || driver.validateCalls != 1 || driver.rankCalls != 1 || driver.guardCalls != 1 {
		t.Fatalf("proof loop incomplete: campaign=%+v driver=%+v", repository.campaign, driver)
	}
}

func TestExecutionCheckpointNamesLongRunningWorkAndCustomKernelBuilds(t *testing.T) {
	regular := checkpointForCandidate(domain.OptimizationCandidateRun{State: CandidateMeasuring}, 0, 2)
	if regular.Name != "benchmark-measure" || regular.Progress != 58 || !strings.Contains(regular.Message, "candidate 1 of 2") {
		t.Fatalf("unexpected measurement checkpoint: %+v", regular)
	}
	kernel := checkpointForCandidate(domain.OptimizationCandidateRun{
		State:                 CandidateProvisioning,
		PredictedEvidenceJSON: `{"technique":"custom_kernel"}`,
	}, 1, 2)
	if kernel.Name != "candidate-build" || kernel.Progress != 30 || !strings.Contains(kernel.Message, "custom kernel") || !strings.Contains(kernel.Message, "candidate 2 of 2") {
		t.Fatalf("custom kernel work was not identified: %+v", kernel)
	}
	waiting := checkpointForCandidate(domain.OptimizationCandidateRun{State: CandidateQualified}, 0, 1)
	if waiting.Name != "awaiting-activation" || waiting.Status != "waiting" || waiting.Progress != 95 {
		t.Fatalf("human boundary checkpoint=%+v", waiting)
	}
}

func TestNewEndpointExecutionStopsQualifiedWithoutFakeReleaseGuard(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 2)
	repository.campaign.Intent = IntentNewEndpoint
	repository.campaign.TargetDeployment = ""
	driver.rankDecisions = map[string]string{"candidate-a": RankSelect, "candidate-b": RankSupersede}
	request, _ := json.Marshal(ExecuteRequest{TenantID: "tenant", CampaignID: "campaign", Candidates: []string{"candidate-a", "candidate-b"}})
	result, err := Handlers(coordinator)[ExecuteKind](context.Background(), domain.Operation{RequestJSON: string(request)})
	assertWaitingForHuman(t, result, err)
	if repository.campaign.Candidates[0].State != CandidateQualified || repository.campaign.Candidates[1].State != CandidateCleaned || driver.cleanupCalls != 1 {
		t.Fatalf("new endpoint did not stop at human activation: result=%s campaign=%+v", result, repository.campaign)
	}
	if driver.guardCalls != 0 {
		t.Fatal("new endpoint campaign invented a Release Guard comparison without a production baseline")
	}
}

func TestExecutionHandlerMeasuresEveryCandidateBeforeRanking(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 2)
	driver.rankDecisions = map[string]string{"candidate-a": RankSupersede, "candidate-b": RankSelect}
	request, _ := json.Marshal(ExecuteRequest{TenantID: "tenant", CampaignID: "campaign", Candidates: []string{"candidate-a", "candidate-b"}})
	result, err := Handlers(coordinator)[ExecuteKind](context.Background(), domain.Operation{RequestJSON: string(request)})
	assertWaitingForHuman(t, result, err)
	if repository.campaign.Candidates[0].State != CandidateCleaned || repository.campaign.Candidates[1].State != CandidateGuardPassed || driver.cleanupCalls != 1 {
		t.Fatalf("measured rank was not enforced: %+v", repository.campaign.Candidates)
	}
	if driver.measureCalls != 2 || driver.validateCalls != 2 || driver.rankCalls != 2 || driver.guardCalls != 1 {
		t.Fatalf("ranking crossed the phase barrier incorrectly: %+v", driver)
	}
}

func TestExecutionHandlerMeasuresTheControlFirst(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 3)
	repository.campaign.Intent = IntentNewEndpoint
	for index := range repository.campaign.Candidates {
		repository.campaign.Candidates[index].ProposalCandidateID = "proposal-" + string(rune('a'+index))
	}
	proposal, _ := json.Marshal(optimizer.Proposal{StartingPoint: &optimizer.StartingPoint{CandidateID: "proposal-c"}})
	repository.campaign.ProposalJSON = string(proposal)
	driver.rankDecisions = map[string]string{"candidate-a": RankSupersede, "candidate-b": RankSupersede, "candidate-c": RankSelect}
	request, _ := json.Marshal(ExecuteRequest{TenantID: "tenant", CampaignID: "campaign", Candidates: []string{"candidate-a", "candidate-b", "candidate-c"}})
	result, err := Handlers(coordinator)[ExecuteKind](context.Background(), domain.Operation{RequestJSON: string(request)})
	assertWaitingForHuman(t, result, err)
	if strings.Join(driver.measured, ",") != "candidate-c,candidate-a,candidate-b" {
		t.Fatalf("the control was not measured first: %v", driver.measured)
	}
	if got := controlFirst(repository.campaign, []string{"candidate-b", "candidate-a"}); strings.Join(got, ",") != "candidate-b,candidate-a" {
		t.Fatalf("a candidate set without its control was reordered: %v", got)
	}
}

// barrierStore is the coordinator repository plus the evidence the production
// ranker reads, so the workflow ranks through the real measured barrier.
type barrierStore struct {
	*coordinatorRepository
	benchmarks []domain.BenchmarkResult
}

func (s *barrierStore) BenchmarksForModel(_ context.Context, _ string, model string, _ int) ([]domain.BenchmarkResult, error) {
	var rows []domain.BenchmarkResult
	for _, row := range s.benchmarks {
		if row.ModelIdentity == model {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (s *barrierStore) RecordLabEvaluation(_ context.Context, tenant string, value domain.LabEvaluation) (domain.LabEvaluation, error) {
	value.TenantID = tenant
	return value, nil
}

// barrierDriver returns the fixture's exact evidence identities, keeps one
// candidate's deployment converge running for a single attempt, and ranks
// with PersistedRanker.
type barrierDriver struct {
	coordinatorDriver
	ranker     PersistedRanker
	converging map[string]bool
}

func (d *barrierDriver) Provision(_ context.Context, candidateID string, _ domain.OptimizationCandidateRun, _ Budget) (ProvisionResult, error) {
	d.provisionCalls++
	if d.converging[candidateID] {
		d.converging[candidateID] = false
		return ProvisionResult{}, operations.Retryable("optimization_child_pending", errors.New("new endpoint provisioning is running"))
	}
	suffix := strings.TrimPrefix(candidateID, "candidate-")
	return ProvisionResult{DeploymentName: "deployment-" + suffix, RevisionID: "revision-" + suffix}, nil
}

func (d *barrierDriver) Measure(_ context.Context, candidateID string, _ domain.OptimizationCandidateRun, _ Budget) (MeasurementResult, error) {
	d.measureCalls++
	d.measured = append(d.measured, candidateID)
	return MeasurementResult{BenchmarkID: "benchmark-" + strings.TrimPrefix(candidateID, "candidate-")}, nil
}

func (d *barrierDriver) Validate(_ context.Context, candidateID string, _ domain.OptimizationCandidateRun) (ValidationResult, error) {
	d.validateCalls++
	return ValidationResult{QualityEvidenceID: "quality-" + strings.TrimPrefix(candidateID, "candidate-"), Passed: true}, nil
}

func (d *barrierDriver) Rank(ctx context.Context, _ string, candidate domain.OptimizationCandidateRun) (RankingResult, error) {
	d.rankCalls++
	return d.ranker.Rank(ctx, candidate)
}

// The control is ranked in the same attempt that starts the next candidate's
// deployment. When that deployment converges, the next attempt must record it
// and measure the remaining candidates instead of retrying the control's
// ranking against a peer that is still provisioning. Once every candidate is
// measured, a new-endpoint winner that is already qualified must not block
// ranking its peers.
func TestExecutionHandlerAdoptsLaterProvisioningAfterTheControlIsRanked(t *testing.T) {
	now := time.Now().UTC()
	noDifference := MetricComparison{ControlMean: 220, CandidateMean: 216, Delta: -4.0 / 220, NoiseFloor: 0.05, Decision: DecisionNoMeasurableDifference}
	campaign, benchmarks := threeCandidateFixture(t, 216, 218, noDifference, noDifference)
	cost, expiry := 30.0, now.Add(time.Hour)
	campaign.Intent, campaign.State, campaign.ApprovedMaxCostUSD, campaign.ApprovalExpiresAt, campaign.MaxCandidates = IntentNewEndpoint, CampaignApproved, &cost, &expiry, 3
	for index := range campaign.Candidates {
		candidate := &campaign.Candidates[index]
		candidate.State, candidate.RevisionID, candidate.BenchmarkID, candidate.QualityEvidenceID = CandidateProposed, "", "", ""
	}
	store := &barrierStore{coordinatorRepository: &coordinatorRepository{campaign: campaign}, benchmarks: benchmarks}
	driver := &barrierDriver{ranker: PersistedRanker{Store: store}, converging: map[string]bool{"candidate-b": true}}
	handler := Handlers(Coordinator{Repository: store, Driver: driver, Now: func() time.Time { return now }})[ExecuteKind]
	request, _ := json.Marshal(ExecuteRequest{TenantID: "tenant", CampaignID: "campaign", Candidates: []string{"candidate-a", "candidate-b", "candidate-c"}})
	operation := domain.Operation{RequestJSON: string(request)}
	states := func() string {
		values := make([]string, 0, len(store.campaign.Candidates))
		for _, candidate := range store.campaign.Candidates {
			values = append(values, candidate.State)
		}
		return strings.Join(values, ",")
	}

	_, err := handler(t.Context(), operation)
	var failure operations.Failure
	if !errors.As(err, &failure) || !failure.Retryable || failure.Code != "optimization_child_pending" || states() != "ranked,provisioning,proposed" || driver.rankCalls != 0 {
		t.Fatalf("first attempt err=%v states=%s ranks=%d, want control ranked and candidate-b provisioning", err, states(), driver.rankCalls)
	}

	result, err := handler(t.Context(), operation)
	assertWaitingForHuman(t, result, err)
	if states() != "qualified,cleaned,cleaned" {
		t.Fatalf("campaign did not converge after candidate-b's deployment converged: states=%s", states())
	}
	adopted := store.campaign.Candidates[1]
	if adopted.DeploymentName != "deployment-b" || adopted.RevisionID != "revision-b" || adopted.BenchmarkID != "benchmark-b" || adopted.LabEvaluationID == "" || adopted.FailureCode != FailureNoMeasurableDifference {
		t.Fatalf("candidate-b lost its converged deployment or measured ranking: %+v", adopted)
	}
	if strings.Join(driver.measured, ",") != "candidate-a,candidate-b,candidate-c" || driver.provisionCalls != 4 || driver.rankCalls != 3 || driver.cleanupCalls != 2 || driver.guardCalls != 0 {
		t.Fatalf("unexpected driver work: measured=%v provision=%d rank=%d cleanup=%d guard=%d", driver.measured, driver.provisionCalls, driver.rankCalls, driver.cleanupCalls, driver.guardCalls)
	}

	result, err = handler(t.Context(), operation)
	assertWaitingForHuman(t, result, err)
	if states() != "qualified,cleaned,cleaned" || driver.rankCalls != 3 {
		t.Fatalf("a retry while waiting for activation re-ranked or moved candidates: states=%s ranks=%d", states(), driver.rankCalls)
	}
}

func assertWaitingForHuman(t *testing.T, result string, err error) {
	t.Helper()
	var failure operations.Failure
	if result != "" || !errors.As(err, &failure) || !failure.Retryable || failure.Code != "optimization_waiting_for_human" {
		t.Fatalf("result=%q err=%v, want durable human-approval wait", result, err)
	}
}

func TestExecutionHandlerRejectsMalformedOrDuplicateCandidateSet(t *testing.T) {
	handler := Handlers(Coordinator{})[ExecuteKind]
	for name, body := range map[string]string{
		"malformed": `{`,
		"empty":     `{"tenant_id":"tenant","campaign_id":"campaign","candidates":[]}`,
		"duplicate": `{"tenant_id":"tenant","campaign_id":"campaign","candidates":["a","a"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := handler(context.Background(), domain.Operation{RequestJSON: body})
			var failure operations.Failure
			if err == nil || !strings.Contains(err.Error(), "optimization execution") || !errors.As(err, &failure) || failure.Retryable {
				t.Fatalf("malformed request was not a permanent failure: %#v", err)
			}
		})
	}
}

func TestExecutionCancellationFencesThenCleansWithoutPromotion(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	repository.campaign.Candidates[0].State = CandidateProvisioning
	repository.campaign.State = CampaignRunning
	request, _ := json.Marshal(ExecuteRequest{TenantID: "tenant", CampaignID: "campaign", Candidates: []string{"candidate-a"}})
	result, err := Handlers(coordinator)[ExecuteKind+".cancel"](context.Background(), domain.Operation{RequestJSON: string(request)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"cleanup":"completed"`) || !strings.Contains(result, `"promotion":"not_performed"`) {
		t.Fatalf("unexpected cancellation result %s", result)
	}
	if repository.campaign.Candidates[0].State != CandidateCleaned || driver.cleanupCalls != 1 || driver.guardCalls != 0 {
		t.Fatalf("unsafe cancellation: campaign=%+v driver=%+v", repository.campaign, driver)
	}
}

func TestExplicitCleanupOperationCleansCampaignAfterExecutionFinished(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	repository.campaign.Candidates[0].State = CandidateCancelled
	repository.campaign.State = CampaignCancelled
	request, _ := json.Marshal(ExecuteRequest{TenantID: "tenant", CampaignID: "campaign", Candidates: []string{"candidate-a"}})
	result, err := Handlers(coordinator)[CleanupKind](context.Background(), domain.Operation{RequestJSON: string(request)})
	if err != nil || repository.campaign.Candidates[0].State != CandidateCleaned || driver.cleanupCalls != 1 || !strings.Contains(result, `"cleanup":"completed"`) {
		t.Fatalf("explicit cleanup did not converge: result=%s err=%v campaign=%+v driver=%+v", result, err, repository.campaign, driver)
	}
}

func TestExecutionHandlerPersistsAndCleansPermanentDriverFailure(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	repository.campaign.Candidates[0].State = CandidateProvisioning
	repository.campaign.State = CampaignRunning
	driver.provisionErr = operations.Permanent("unsupported_tuple", errors.New("runtime tuple is not qualified"))
	request, _ := json.Marshal(ExecuteRequest{TenantID: "tenant", CampaignID: "campaign", Candidates: []string{"candidate-a"}})
	result, err := Handlers(coordinator)[ExecuteKind](context.Background(), domain.Operation{RequestJSON: string(request)})
	if err != nil || repository.campaign.Candidates[0].State != CandidateCleaned || repository.campaign.Candidates[0].FailureCode != "unsupported_tuple" || driver.cleanupCalls != 1 || !strings.Contains(result, `"promotion":"not_performed"`) {
		t.Fatalf("permanent driver failure did not converge: result=%s err=%v candidate=%+v cleanup=%d", result, err, repository.campaign.Candidates[0], driver.cleanupCalls)
	}
}

func TestActivationHandlerActivatesQualifiedNewEndpointIdempotently(t *testing.T) {
	now := time.Now().UTC()
	repository, _, _ := approvedCoordinatorFixture(now, 1)
	repository.campaign.Intent = IntentNewEndpoint
	repository.campaign.TargetDeployment = ""
	repository.campaign.State = CampaignQualified
	repository.campaign.Candidates[0].State = CandidateQualified
	repository.campaign.Candidates[0].DeploymentName = "llama-production-candidate"
	repository.campaign.Candidates[0].RevisionID = "revision-qualified"
	repository.campaign.Candidates[0].DeploymentSpecJSON = `{"name":"llama-production","model":{"id":"meta-llama/Llama-3.1-8B-Instruct"}}`
	store := &activationStoreFixture{coordinatorRepository: repository}
	payload, _ := json.Marshal(ActivateRequest{TenantID: "tenant", CampaignID: "campaign", CandidateID: "candidate-a", Actor: "operator"})
	refreshes := 0
	handler := ActivationHandlers(store, func(context.Context) error { refreshes++; return nil }, func() time.Time { return now })[ActivateKind]

	result, err := handler(context.Background(), domain.Operation{RequestJSON: string(payload)})
	if err != nil || !strings.Contains(result, `"state":"promoted"`) || !strings.Contains(result, `"endpoint":"llama-production"`) || repository.campaign.Candidates[0].State != CandidatePromoted || len(store.operations) != 0 || store.publishedEndpoint != "llama-production" || store.publishedDeployment != "llama-production-candidate" || refreshes != 1 {
		t.Fatalf("new endpoint activation result=%s err=%v campaign=%+v operations=%+v", result, err, repository.campaign, store.operations)
	}
	result, err = handler(context.Background(), domain.Operation{RequestJSON: string(payload)})
	if err != nil || !strings.Contains(result, `"state":"promoted"`) || repository.transitions != 1 {
		t.Fatalf("idempotent activation result=%s err=%v transitions=%d", result, err, repository.transitions)
	}
}

func TestActivationHandlerDelegatesEvolutionToGuardedRolloutAndAdoptsResult(t *testing.T) {
	now := time.Now().UTC()
	repository, _, _ := approvedCoordinatorFixture(now, 1)
	repository.campaign.State = CampaignQualified
	repository.campaign.Candidates[0].State = CandidateGuardPassed
	repository.campaign.Candidates[0].DeploymentName = "llama-production"
	repository.campaign.Candidates[0].RevisionID = "revision-candidate"
	store := &activationStoreFixture{coordinatorRepository: repository}
	payload, _ := json.Marshal(ActivateRequest{TenantID: "tenant", CampaignID: "campaign", CandidateID: "candidate-a", Actor: "operator"})
	refreshes := 0
	handler := ActivationHandlers(store, func(context.Context) error { refreshes++; return nil }, func() time.Time { return now })[ActivateKind]

	_, err := handler(context.Background(), domain.Operation{RequestJSON: string(payload)})
	var failure operations.Failure
	if !errors.As(err, &failure) || !failure.Retryable || failure.Code != "optimization_child_pending" || repository.transitions != 0 {
		t.Fatalf("pending rollout was not awaited safely: err=%v transitions=%d", err, repository.transitions)
	}
	key := childKey("candidate-a", "activate")
	child := store.operations[key]
	if child.Kind != workflows.RolloutPromoteKind || child.ResourceName != "production" {
		t.Fatalf("unexpected guarded rollout child: %+v", child)
	}
	var rollout workflows.RolloutRequest
	if json.Unmarshal([]byte(child.RequestJSON), &rollout) != nil || rollout.CandidateID != "revision-candidate" || rollout.Name != "production" || rollout.Actor != "operator" {
		t.Fatalf("guarded rollout lost exact activation identity: %+v", child)
	}
	child.Status = "succeeded"
	store.operations[key] = child
	result, err := handler(context.Background(), domain.Operation{RequestJSON: string(payload)})
	if err != nil || !strings.Contains(result, `"state":"promoted"`) || repository.campaign.Candidates[0].State != CandidatePromoted || len(store.operations) != 1 || refreshes != 1 {
		t.Fatalf("completed rollout was not adopted: result=%s err=%v campaign=%+v operations=%+v", result, err, repository.campaign, store.operations)
	}
}

func TestNewEndpointActivationWaitsForRoutePublicationBeforePromotion(t *testing.T) {
	now := time.Now().UTC()
	repository, _, _ := approvedCoordinatorFixture(now, 1)
	repository.campaign.Intent = IntentNewEndpoint
	repository.campaign.TargetDeployment = ""
	repository.campaign.State = CampaignQualified
	repository.campaign.Candidates[0].State = CandidateQualified
	repository.campaign.Candidates[0].DeploymentName = "coder-candidate"
	repository.campaign.Candidates[0].RevisionID = "revision-qualified"
	repository.campaign.Candidates[0].DeploymentSpecJSON = `{"name":"coder-production","model":{"id":"Qwen/Qwen3-8B"}}`
	store := &activationStoreFixture{coordinatorRepository: repository}
	payload, _ := json.Marshal(ActivateRequest{TenantID: "tenant", CampaignID: "campaign", CandidateID: "candidate-a", Actor: "operator"})
	refreshes := 0
	handler := ActivationHandlers(store, func(context.Context) error {
		refreshes++
		if refreshes == 1 {
			return errors.New("route generation not published")
		}
		return nil
	}, func() time.Time { return now })[ActivateKind]

	_, err := handler(context.Background(), domain.Operation{RequestJSON: string(payload)})
	var failure operations.Failure
	if !errors.As(err, &failure) || !failure.Retryable || failure.Code != "optimization_route_publish_pending" || repository.campaign.Candidates[0].State != CandidateQualified || repository.transitions != 0 {
		t.Fatalf("candidate promoted before route publication: err=%v campaign=%+v", err, repository.campaign)
	}
	result, err := handler(context.Background(), domain.Operation{RequestJSON: string(payload)})
	if err != nil || !strings.Contains(result, `"endpoint":"coder-production"`) || repository.campaign.Candidates[0].State != CandidatePromoted || refreshes != 2 {
		t.Fatalf("route publication retry did not converge: result=%s err=%v campaign=%+v", result, err, repository.campaign)
	}
}

func TestActivationHandlerRejectsExpiredOrUnqualifiedCandidateBeforeMutation(t *testing.T) {
	now := time.Now().UTC()
	for name, testCase := range map[string]struct {
		state    string
		expiry   time.Time
		wantCode string
	}{
		"unqualified": {state: CandidateRanked, expiry: now.Add(time.Hour), wantCode: "optimization_candidate_not_qualified"},
		"expired":     {state: CandidateGuardPassed, expiry: now.Add(-time.Second), wantCode: "optimization_approval_expired"},
	} {
		t.Run(name, func(t *testing.T) {
			repository, _, _ := approvedCoordinatorFixture(now, 1)
			repository.campaign.Candidates[0].State = testCase.state
			repository.campaign.Candidates[0].RevisionID = "revision-candidate"
			repository.campaign.ApprovalExpiresAt = &testCase.expiry
			store := &activationStoreFixture{coordinatorRepository: repository}
			payload, _ := json.Marshal(ActivateRequest{TenantID: "tenant", CampaignID: "campaign", CandidateID: "candidate-a", Actor: "operator"})
			_, err := ActivationHandlers(store, func(context.Context) error { return nil }, func() time.Time { return now })[ActivateKind](context.Background(), domain.Operation{RequestJSON: string(payload)})
			var failure operations.Failure
			if !errors.As(err, &failure) || failure.Retryable || failure.Code != testCase.wantCode || len(store.operations) != 0 || repository.transitions != 0 {
				t.Fatalf("unsafe activation err=%v operations=%+v transitions=%d", err, store.operations, repository.transitions)
			}
		})
	}
}
