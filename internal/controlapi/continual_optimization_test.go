package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/continualoptimizer"
	"github.com/infercrane/infercrane/internal/domain"
)

// legacyContinualPolicy was stored while the loop still carried a canary
// branch that no handler could reach. Release Guard alone judges canaries.
const legacyContinualPolicy = `{"enabled":true,"minimum_observed_requests":500,"minimum_canary_requests":1000,"drift_threshold":0.25,"minimum_expected_gain":0.03,"minimum_measured_gain":0.03,"minimum_contribution_margin":0.2,"minimum_availability":0.999,"maximum_error_rate":0.01,"maximum_experiment_cost_usd":100,"maximum_candidates_per_campaign":4,"maximum_concurrent_campaigns":1,"cooldown_seconds":21600,"rejection_memory_seconds":2592000,"require_customer_replay":true,"require_manual_promotion":true,"allow_automatic_experimentation":true}`

var retiredCanaryPolicyFields = []string{"minimum_canary_requests", "minimum_measured_gain", "require_manual_promotion"}

type continualPolicyStore struct {
	*fakeStore
	policyJSON string
	saved      *continualoptimizer.Policy
	decision   *continualoptimizer.Decision
}

func (s *continualPolicyStore) SetContinualOptimizationPolicy(_ context.Context, _, name string, policy continualoptimizer.Policy) (domain.ContinualOptimizationPolicyRecord, error) {
	encoded, err := json.Marshal(policy)
	if err != nil {
		return domain.ContinualOptimizationPolicyRecord{}, err
	}
	s.policyJSON, s.saved = string(encoded), &policy
	return domain.ContinualOptimizationPolicyRecord{DeploymentName: name, PolicyJSON: s.policyJSON, Generation: 2}, nil
}

func (s *continualPolicyStore) ContinualOptimizationPolicy(_ context.Context, _, name string) (domain.ContinualOptimizationPolicyRecord, error) {
	return domain.ContinualOptimizationPolicyRecord{DeploymentName: name, PolicyJSON: s.policyJSON, Generation: 1}, nil
}

func (s *continualPolicyStore) ContinualOptimizationState(context.Context, string, string) (domain.ContinualOptimizationState, error) {
	return domain.ContinualOptimizationState{}, domain.ErrNotFound
}

func (s *continualPolicyStore) RecordContinualOptimizationDecision(_ context.Context, _, _ string, _ continualoptimizer.Workload, decision continualoptimizer.Decision) (domain.ContinualOptimizationDecisionRecord, bool, error) {
	s.decision = &decision
	return domain.ContinualOptimizationDecisionRecord{ID: "decision-1"}, true, nil
}

func (s *continualPolicyStore) ContinualOptimizationDecisions(context.Context, string, string, int) ([]domain.ContinualOptimizationDecisionRecord, error) {
	return nil, nil
}

func continualRequest(t *testing.T, handler http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer secret")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestContinualOptimizationPolicyRoundTripsWithoutRetiredCanaryFields(t *testing.T) {
	store := &continualPolicyStore{fakeStore: &fakeStore{}, policyJSON: legacyContinualPolicy}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	const path = "/api/v1/deployments/chat/continual-optimization/policy"

	response := continualRequest(t, handler, http.MethodGet, path, nil)
	var body struct {
		Policy json.RawMessage `json:"policy"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	for _, retired := range retiredCanaryPolicyFields {
		if strings.Contains(string(body.Policy), retired) {
			t.Fatalf("policy echoes retired %s: %s", retired, body.Policy)
		}
	}

	// The returned policy goes back through the strict update unchanged.
	response = continualRequest(t, handler, http.MethodPut, path, body.Policy)
	if response.Code != http.StatusOK || store.saved == nil || *store.saved != continualoptimizer.DefaultPolicy() {
		t.Fatalf("status=%d saved=%+v body=%s", response.Code, store.saved, response.Body.String())
	}
	// Sending a retired canary field explicitly is still refused.
	response = continualRequest(t, handler, http.MethodPut, path, []byte(legacyContinualPolicy))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "minimum_canary_requests") {
		t.Fatalf("retired field accepted: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestContinualOptimizationEvaluatesALegacyPolicyWithoutCanaryDecisions(t *testing.T) {
	store := &continualPolicyStore{fakeStore: &fakeStore{}, policyJSON: legacyContinualPolicy}
	handler := (API{Store: store, APIKey: "secret"}).Handler()
	request := []byte(`{"public_profile":"public-interactive","economics":{"availability":0.999}}`)
	response := continualRequest(t, handler, http.MethodPost, "/api/v1/deployments/chat/continual-optimization/evaluate", request)
	if response.Code != http.StatusCreated || store.decision == nil {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if store.decision.Action != continualoptimizer.ActionScreenPublicPrior || store.decision.PromotionEligible || strings.Contains(response.Body.String(), "automatic_promotion") || strings.Contains(response.Body.String(), "canary") {
		t.Fatalf("decision=%+v body=%s", store.decision, response.Body.String())
	}
}
