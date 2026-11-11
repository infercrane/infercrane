package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizer"
)

func TestRecipeSearchProposalPersistsAsReviewBoundedCampaign(t *testing.T) {
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeOptimizationCampaignStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret", OptimizationCosts: fakeOptimizationCosts{}, Integrations: registry.Snapshot()}).Handler()

	body := `{"model_identity":"acme/novel-model","model_revision":"` + strings.Repeat("a", 40) + `","provider":"runpod-pods","gpu":"H200","objective":"throughput","max_candidates":4,"search":"recipes","model_features":["native_mtp"],"model_config_digest":"sha256:` + strings.Repeat("b", 64) + `"}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/optimization/proposals", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"provider_mutation":false`) || !strings.Contains(response.Body.String(), `"performance_claims":false`) {
		t.Fatalf("proposal status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Proposal optimizer.Proposal `json:"proposal"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Proposal.Candidates) != 4 || envelope.Proposal.Candidates[0].ConfigurationProfile != "recipe-sglang-0520-control" {
		t.Fatalf("search proposal lost its control: %+v", envelope.Proposal.Candidates)
	}

	encoded, err := json.Marshal(map[string]any{"proposal": envelope.Proposal})
	if err != nil {
		t.Fatal(err)
	}
	create := httptest.NewRequest(http.MethodPost, "/api/v1/optimization/campaigns", strings.NewReader(string(encoded)))
	create.Header.Set("Authorization", "Bearer secret")
	create.Header.Set("Idempotency-Key", "recipe-search-campaign-1")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, create)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"provider_mutation":false`) || len(store.campaign.Candidates) != 4 {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
}
