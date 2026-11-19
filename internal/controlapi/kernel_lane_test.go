package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizer"
)

func kernelLaneApproval(t *testing.T, laneEnabled bool) (*httptest.ResponseRecorder, *fakeOptimizationCampaignStore, *int) {
	t.Helper()
	base := &fakeStore{principal: domain.Principal{ID: "operator-1", TenantID: "tenant-1", Name: "operator", Role: "operator", Scopes: []string{"read", "deploy"}}}
	var draft optimizer.DeploymentDraft
	draft.Provider.Cloud, draft.Provider.Adapter = "runpod", "runpod-pods"
	draft.ComputeConnectionID = "connection-1"
	draftJSON, _ := json.Marshal(draft)
	predicted, _ := json.Marshal(map[string]any{"technique": "custom_kernel", "lane": optimizationcampaign.LaneKernel})
	store := &fakeOptimizationCampaignStore{fakeStore: base, campaign: domain.OptimizationCampaign{
		ID: "campaign-1", TenantID: "tenant-1", State: optimizationcampaign.CampaignAwaitingApproval,
		Candidates: []domain.OptimizationCandidateRun{
			{ID: "control", DeploymentSpecJSON: string(draftJSON), PredictedEvidenceJSON: `{}`},
			{ID: "kernel", DeploymentSpecJSON: string(draftJSON), PredictedEvidenceJSON: string(predicted)},
		},
	}}
	connections := &fakeComputeConnections{item: domain.ComputeConnection{ID: "connection-1", TenantID: "tenant-1", Provider: "runpod", Adapter: "runpod-pods", Status: "verified"}, credential: "tenant-key"}
	quotes := 0
	costs := optimizationcampaign.ConnectionPricingAuthority{Connections: connections, Providers: map[string]func(string) optimizationcampaign.CostAuthority{
		"runpod": func(string) optimizationcampaign.CostAuthority {
			return optimizationCostFunc(func(_ context.Context, _ string, _ optimizer.DeploymentDraft, requiredUntil time.Time) (optimizationcampaign.CostQuote, error) {
				quotes++
				return optimizationcampaign.CostQuote{HourlyUSD: 2, Source: "runpod-live", ObservedAt: requiredUntil.Add(-time.Hour), ValidUntil: requiredUntil.Add(time.Hour)}, nil
			})
		},
	}}
	handler := (API{Store: store, Authenticator: base, OptimizationCosts: costs, ComputeConnections: connections, AcceleratorLabEnabled: true, KernelLaneEnabled: laneEnabled}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/optimization/campaigns/campaign-1/approve", strings.NewReader(`{"max_cost_usd":40,"expires_in_seconds":3600}`))
	request.Header.Set("Authorization", "Bearer tenant-session")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response, store, &quotes
}

func TestApprovalStartsTheKernelLaneWithItsShareOfTheCap(t *testing.T) {
	response, store, quotes := kernelLaneApproval(t, true)
	if response.Code != http.StatusAccepted || store.campaign.State != optimizationcampaign.CampaignApproved {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	// Only the settings setup provisions machines, so only it is priced.
	if *quotes != 1 {
		t.Fatalf("priced %d setups, want only the settings setup", *quotes)
	}
	var lane optimizationcampaign.KernelLaneRequest
	if store.operation.Kind != optimizationcampaign.KernelLaneKind || json.Unmarshal([]byte(store.operation.RequestJSON), &lane) != nil {
		t.Fatalf("kernel lane not queued: %+v", store.operation)
	}
	if lane.CandidateID != "kernel" || lane.CampaignID != "campaign-1" || lane.MaxCostUSD != 20 || store.operation.IdempotencyKey != "optimization-kernel-lane:kernel" {
		t.Fatalf("lane = %+v key=%s", lane, store.operation.IdempotencyKey)
	}
}

func TestApprovalRefusesAKernelSetupWithoutALane(t *testing.T) {
	response, store, _ := kernelLaneApproval(t, false)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "kernel_lane_unavailable") || store.campaign.State == optimizationcampaign.CampaignApproved {
		t.Fatalf("status=%d body=%s state=%s", response.Code, response.Body.String(), store.campaign.State)
	}
}
