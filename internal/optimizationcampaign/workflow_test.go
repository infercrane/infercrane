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
)

func TestExecutionHandlerRunsProofLoopAndNeverPromotes(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	request, _ := json.Marshal(ExecuteRequest{TenantID: "tenant", CampaignID: "campaign", Candidates: []string{"candidate-a"}})
	handler := Handlers(coordinator)[ExecuteKind]
	result, err := handler(context.Background(), domain.Operation{RequestJSON: string(request)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"waiting_for_human":["candidate-a"]`) || !strings.Contains(result, `"promotion":"not_performed"`) {
		t.Fatalf("unexpected execution result %s", result)
	}
	if repository.campaign.Candidates[0].State != CandidateGuardPassed || driver.provisionCalls != 1 || driver.measureCalls != 1 || driver.validateCalls != 1 || driver.rankCalls != 1 || driver.guardCalls != 1 {
		t.Fatalf("proof loop incomplete: campaign=%+v driver=%+v", repository.campaign, driver)
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
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"waiting_for_human":["candidate-a"]`) || repository.campaign.Candidates[0].State != CandidateQualified || repository.campaign.Candidates[1].State != CandidateRejected {
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
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"waiting_for_human":["candidate-b"]`) {
		t.Fatalf("unexpected execution result %s", result)
	}
	if repository.campaign.Candidates[0].State != CandidateRejected || repository.campaign.Candidates[1].State != CandidateGuardPassed {
		t.Fatalf("measured rank was not enforced: %+v", repository.campaign.Candidates)
	}
	if driver.measureCalls != 2 || driver.validateCalls != 2 || driver.rankCalls != 2 || driver.guardCalls != 1 {
		t.Fatalf("ranking crossed the phase barrier incorrectly: %+v", driver)
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

func TestExecutionHandlerPreservesPermanentDriverFailure(t *testing.T) {
	now := time.Now().UTC()
	repository, driver, coordinator := approvedCoordinatorFixture(now, 1)
	repository.campaign.Candidates[0].State = CandidateProvisioning
	repository.campaign.State = CampaignRunning
	driver.provisionErr = operations.Permanent("unsupported_tuple", errors.New("runtime tuple is not qualified"))
	request, _ := json.Marshal(ExecuteRequest{TenantID: "tenant", CampaignID: "campaign", Candidates: []string{"candidate-a"}})
	_, err := Handlers(coordinator)[ExecuteKind](context.Background(), domain.Operation{RequestJSON: string(request)})
	var failure operations.Failure
	if !errors.As(err, &failure) || failure.Retryable || failure.Code != "unsupported_tuple" {
		t.Fatalf("permanent driver failure was converted into retries: %#v", err)
	}
}
