package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/apicontract"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizer"
)

func TestCampaignReturnsPersistedProposalWarnings(t *testing.T) {
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeOptimizationCampaignStore{fakeStore: &fakeStore{}}
	handler := (API{Store: store, APIKey: "secret", OptimizationCosts: fakeOptimizationCosts{}, Integrations: registry.Snapshot()}).Handler()
	call := func(method, path, body, key string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer secret")
		if key != "" {
			request.Header.Set("Idempotency-Key", key)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	// No model features: speculative arms are skipped and the proposal says why.
	body := `{"model_identity":"acme/novel-model","model_revision":"` + strings.Repeat("a", 40) + `","provider":"runpod-pods","gpu":"H200","objective":"throughput","max_candidates":4,"search":"recipes"}`
	response := call(http.MethodPost, "/api/v1/optimization/proposals", body, "")
	if response.Code != http.StatusOK {
		t.Fatalf("proposal status=%d body=%s", response.Code, response.Body.String())
	}
	var proposed struct {
		Proposal optimizer.Proposal `json:"proposal"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &proposed); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(proposed.Proposal.Warnings, " ")
	if !strings.Contains(joined, "Speculative-decoding arms were skipped") || !strings.Contains(joined, "Every arm remains unmeasured") {
		t.Fatalf("recipe search proposal lost its warnings: %q", proposed.Proposal.Warnings)
	}
	encoded, err := json.Marshal(map[string]any{"proposal": proposed.Proposal})
	if err != nil {
		t.Fatal(err)
	}
	response = call(http.MethodPost, "/api/v1/optimization/campaigns", string(encoded), "campaign-warnings-1")
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	assertCampaignWarnings(t, "create", response.Body.Bytes(), "campaign", proposed.Proposal.Warnings)

	response = call(http.MethodGet, "/api/v1/optimization/campaigns/"+store.campaign.ID, "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", response.Code, response.Body.String())
	}
	campaign := assertCampaignWarnings(t, "get", response.Body.Bytes(), "campaign", proposed.Proposal.Warnings)
	assertMatchesDocumentedSchema(t, "OptimizationCampaign", campaign)
	candidates, _ := campaign["candidates"].([]any)
	if len(candidates) == 0 {
		t.Fatalf("campaign response has no candidates: %v", campaign)
	}
	for _, candidate := range candidates {
		assertMatchesDocumentedSchema(t, "OptimizationCampaignCandidate", candidate.(map[string]any))
	}

	response = call(http.MethodGet, "/api/v1/optimization/campaigns", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	var listed struct {
		Data []json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &listed); err != nil || len(listed.Data) != 1 {
		t.Fatalf("list body=%s err=%v", response.Body.String(), err)
	}
	assertCampaignWarnings(t, "list", listed.Data[0], "", proposed.Proposal.Warnings)
}

func TestCampaignWarningsKeepNoneDistinctFromUnreadable(t *testing.T) {
	none := optimizationCampaignResponse(domain.OptimizationCampaign{ProposalJSON: `{"schema_version":"infercrane.optimizer.proposal/v1"}`})
	encoded, err := json.Marshal(none["warnings"])
	if err != nil || string(encoded) != "[]" {
		t.Fatalf("a proposal without warnings must report an empty list, got %s err=%v", encoded, err)
	}
	for _, stored := range []string{"", "{", `{"warnings":"not-a-list"}`} {
		if warnings, present := optimizationCampaignResponse(domain.OptimizationCampaign{ProposalJSON: stored})["warnings"]; present {
			t.Fatalf("unreadable proposal %q must omit warnings, got %#v", stored, warnings)
		}
	}
}

// assertCampaignWarnings decodes a campaign (optionally nested under field)
// and requires its warnings to equal the persisted proposal's warnings.
func assertCampaignWarnings(t *testing.T, label string, body []byte, field string, want []string) map[string]any {
	t.Helper()
	var campaign map[string]any
	if field == "" {
		if err := json.Unmarshal(body, &campaign); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	} else {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(body, &envelope); err != nil || json.Unmarshal(envelope[field], &campaign) != nil {
			t.Fatalf("%s: campaign envelope body=%s", label, body)
		}
	}
	raw, _ := campaign["warnings"].([]any)
	got := make([]string, 0, len(raw))
	for _, warning := range raw {
		text, _ := warning.(string)
		got = append(got, text)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s: campaign warnings=%q want %q", label, got, want)
	}
	return campaign
}

// assertMatchesDocumentedSchema keeps the response and the OpenAPI contract in
// lockstep: every returned key is documented and every required key is present.
func assertMatchesDocumentedSchema(t *testing.T, name string, object map[string]any) {
	t.Helper()
	data, err := apicontract.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Components struct {
			Schemas map[string]struct {
				Required   []string       `json:"required"`
				Properties map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	schema, ok := document.Components.Schemas[name]
	if !ok {
		t.Fatalf("OpenAPI document has no %s schema", name)
	}
	for key := range object {
		if _, documented := schema.Properties[key]; !documented {
			t.Errorf("%s response key %q is not documented in OpenAPI", name, key)
		}
	}
	for _, key := range schema.Required {
		if _, present := object[key]; !present {
			t.Errorf("%s response is missing required key %q", name, key)
		}
	}
}
