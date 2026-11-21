package controlapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// A buffered run measures no time to first token, so a TTFT limit on it would
// reject every setup only after the whole measurement. Both the proposal and
// the run creation refuse it before anything is stored or provisioned.
func TestOptimizationRejectsLimitsABufferedRunCannotMeasure(t *testing.T) {
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
	const request = `{"model_identity":"mistral-7b-instruct","provider":"aws","region":"eu-central-1","gpu":"L40S","objective":"interactive","workload_profile":"buffered"`

	response := call(http.MethodPost, "/api/v1/optimization/proposals", request+`,"max_ttft_p95_ms":200}`, "")
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "invalid_optimization_request") || !strings.Contains(response.Body.String(), "max_ttft_p95_ms cannot be measured on the buffered profile") {
		t.Fatalf("proposal status=%d body=%s", response.Code, response.Body.String())
	}

	response = call(http.MethodPost, "/api/v1/optimization/proposals", request+`}`, "")
	if response.Code != http.StatusOK {
		t.Fatalf("proposal status=%d body=%s", response.Code, response.Body.String())
	}
	var proposed struct {
		Proposal optimizer.Proposal `json:"proposal"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &proposed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(proposed.Proposal.Warnings, " "), "ranks setups on p95 request latency") {
		t.Fatalf("warnings=%q", proposed.Proposal.Warnings)
	}

	// A client can still submit a proposal it assembled itself, with a
	// matching digest. Run creation checks the limit again.
	tampered := proposed.Proposal
	limit := 200.0
	tampered.Input.MaxTTFTP95MS = &limit
	input, err := json.Marshal(tampered.Input)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(input)
	tampered.InputDigest = hex.EncodeToString(sum[:])
	if err = optimizer.ValidateProposal(tampered); err != nil {
		t.Fatalf("fixture must pass proposal validation: %v", err)
	}
	encoded, err := json.Marshal(map[string]any{"proposal": tampered})
	if err != nil {
		t.Fatal(err)
	}
	response = call(http.MethodPost, "/api/v1/optimization/campaigns", string(encoded), "buffered-ttft-1")
	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "invalid_optimization_proposal") || !strings.Contains(response.Body.String(), "max_ttft_p95_ms cannot be measured") {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	if store.campaign.ID != "" {
		t.Fatalf("a run that cannot measure its limit was stored: %+v", store.campaign)
	}
}
