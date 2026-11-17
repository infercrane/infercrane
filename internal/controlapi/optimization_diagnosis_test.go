package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/gpuprofile"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/metrics"
	"github.com/infercrane/infercrane/internal/optimizationreadiness"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// diagnosisCampaignStore serves monitoring, a GPU profile, and campaigns, so
// one test can follow a diagnosis from the proposal into the campaign read.
type diagnosisCampaignStore struct {
	*fakeOptimizationCampaignStore
	snapshot      domain.EndpointMonitoringSnapshot
	monitoringErr error
	profile       gpuprofile.Profile
	profileErr    error
	tenants       []string
}

func (s *diagnosisCampaignStore) EndpointMonitoring(_ context.Context, tenant, endpoint string, _, _ time.Duration) (domain.EndpointMonitoringSnapshot, error) {
	s.tenants = append(s.tenants, tenant+"/"+endpoint)
	return s.snapshot, s.monitoringErr
}

func (s *diagnosisCampaignStore) RecordGPUProfile(context.Context, string, string, gpuprofile.Recording) (gpuprofile.Receipt, error) {
	return gpuprofile.Receipt{}, errors.New("not used")
}

func (s *diagnosisCampaignStore) EndpointGPUProfile(context.Context, string, string) (gpuprofile.Profile, error) {
	return s.profile, s.profileErr
}

func kvPressureSnapshot() domain.EndpointMonitoringSnapshot {
	now := time.Now().UTC()
	zero, latency := 0.0, 4000.0
	snapshot := domain.EndpointMonitoringSnapshot{
		Endpoint: "chat-production", WindowStart: now.Add(-time.Hour), WindowEnd: now, BucketSeconds: 60,
		Summary:  domain.MonitoringSummary{Requests: 200, ErrorRate: &zero, P95LatencyMS: &latency, InputTokens: 200 * 9000, InputTokenSamples: 200},
		Evidence: domain.MonitoringEvidence{Source: "infercrane_gateway_request_records", SampleCount: 200, Fresh: true},
	}
	for name, observed := range map[string]float64{metrics.MeasurementPreemptions: 20, metrics.MeasurementRequestsCompleted: 200, metrics.MeasurementRequestsRunning: 48, metrics.MeasurementKVCacheUsage: 98} {
		value := observed
		snapshot.Evidence.Measurements = append(snapshot.Evidence.Measurements, domain.MeasurementEvidence{Name: name, Value: &value, Availability: "available", EvidenceClass: "measured", Source: "vllm_metrics", SampleCount: 1})
	}
	return snapshot
}

func attentionHeavyProfile() gpuprofile.Profile {
	return gpuprofile.Profile{
		Revision: 2,
		Capture:  gpuprofile.Capture{ID: "capture-1", CaptureMetadata: gpuprofile.CaptureMetadata{Tool: "cupti", EndedAt: time.Now().UTC().Add(-time.Hour)}},
		Frames:   []gpuprofile.Frame{{Kind: gpuprofile.KindKernel, Name: "flash_fwd_splitkv_kernel"}, {Kind: gpuprofile.KindKernel, Name: "sm90_xmma_gemm_bf16"}},
		Stacks:   []gpuprofile.Stack{{Frames: []int{0}, GPUNS: 6e9}, {Frames: []int{1}, GPUNS: 4e9}},
	}
}

func diagnosisHandler(t *testing.T, store *diagnosisCampaignStore) func(method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	handler := (API{Store: store, APIKey: "secret", OptimizationCosts: fakeOptimizationCosts{}, Integrations: registry.Snapshot()}).Handler()
	return func(method, path, body, key string) *httptest.ResponseRecorder {
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
}

const diagnosisProposalBody = `{"model_identity":"acme/novel-open-model","model_revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","provider":"runpod-pods","gpu":"H100","runtimes":["vllm"],"max_candidates":3,"diagnosis_endpoint":"chat-production"}`

func TestProposalOrdersCandidatesByTheEndpointsRecordedDiagnosis(t *testing.T) {
	store := &diagnosisCampaignStore{fakeOptimizationCampaignStore: &fakeOptimizationCampaignStore{fakeStore: &fakeStore{}}, snapshot: kvPressureSnapshot(), profile: attentionHeavyProfile()}
	call := diagnosisHandler(t, store)

	response := call(http.MethodPost, "/api/v1/optimization/proposals", diagnosisProposalBody, "")
	if response.Code != http.StatusOK {
		t.Fatalf("proposal status=%d body=%s", response.Code, response.Body.String())
	}
	var proposed struct {
		Proposal optimizer.Proposal `json:"proposal"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &proposed); err != nil {
		t.Fatal(err)
	}
	proposal := proposed.Proposal
	diagnosis := proposal.Input.Diagnosis
	if diagnosis == nil || diagnosis.Endpoint != "chat-production" || diagnosis.PrimaryBottleneck != optimizationreadiness.BottleneckKVCachePressure || diagnosis.GPUProfile == nil || diagnosis.GPUProfile.Shares[optimizer.KernelClassAttention] != .6 {
		t.Fatalf("server did not record the endpoint diagnosis: %+v", diagnosis)
	}
	if len(store.tenants) != 1 || store.tenants[0] != "global/chat-production" {
		t.Fatalf("diagnosis read outside the caller's workspace: %v", store.tenants)
	}
	var order []string
	for _, candidate := range proposal.Candidates {
		if candidate.Reason == nil {
			t.Fatalf("candidate without a reason: %+v", candidate)
		}
		order = append(order, candidate.Reason.Role+":"+candidate.Reason.Technique)
	}
	if strings.Join(order, ",") != "control:,diagnosis:fp8-kv-cache,diagnosis:kv-sequence-cap" {
		t.Fatalf("order=%v", order)
	}
	if fp8 := proposal.Candidates[1].Reason; !contains(fp8.Rules, optimizer.RuleProfileAttentionHeavy) || fp8.ExpectedBenefit != "high" {
		t.Fatalf("the GPU profile did not corroborate the KV choice: %+v", fp8)
	}

	encoded, err := json.Marshal(map[string]any{"proposal": proposal})
	if err != nil {
		t.Fatal(err)
	}
	response = call(http.MethodPost, "/api/v1/optimization/campaigns", string(encoded), "diagnosis-campaign-1")
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	response = call(http.MethodGet, "/api/v1/optimization/campaigns/"+store.campaign.ID, "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("get status=%d body=%s", response.Code, response.Body.String())
	}
	var read struct {
		Campaign map[string]any `json:"campaign"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatal(err)
	}
	assertMatchesDocumentedSchema(t, "OptimizationCampaign", read.Campaign)
	plan, _ := read.Campaign["diagnosis"].(map[string]any)
	if plan["state"] != optimizer.DiagnosisApplied || plan["endpoint"] != "chat-production" || plan["window_start"] == nil || !strings.Contains(plan["summary"].(string), "FP8 KV cache") {
		t.Fatalf("campaign read lost the diagnosis plan: %#v", read.Campaign["diagnosis"])
	}
	candidates, _ := read.Campaign["candidates"].([]any)
	if len(candidates) != 3 {
		t.Fatalf("candidates=%#v", read.Campaign["candidates"])
	}
	for _, raw := range candidates {
		candidate := raw.(map[string]any)
		reason, _ := candidate["reason"].(map[string]any)
		if reason == nil || reason["summary"] == "" {
			t.Fatalf("campaign candidate lost its reason: %#v", candidate)
		}
		if candidate["rank"].(float64) == 2 && reason["technique"] != optimizer.TechniqueFP8KVCache {
			t.Fatalf("rank 2 reason=%#v", reason)
		}
	}
}

func TestProposalDiagnosisIsReadByTheServerOnly(t *testing.T) {
	store := &diagnosisCampaignStore{fakeOptimizationCampaignStore: &fakeOptimizationCampaignStore{fakeStore: &fakeStore{}}, snapshot: kvPressureSnapshot(), profileErr: gpuprofile.ErrNotRecorded}
	call := diagnosisHandler(t, store)

	forged := `{"model_identity":"acme/novel-open-model","model_revision":"` + strings.Repeat("a", 40) + `","provider":"runpod-pods","gpu":"H100","diagnosis":{"source":"endpoint_readiness"}}`
	if response := call(http.MethodPost, "/api/v1/optimization/proposals", forged, ""); response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "diagnosis_endpoint") {
		t.Fatalf("client diagnosis status=%d body=%s", response.Code, response.Body.String())
	}
	if response := call(http.MethodPost, "/api/v1/optimization/proposals", strings.Replace(diagnosisProposalBody, "chat-production", "a/b", 1), ""); response.Code != http.StatusBadRequest {
		t.Fatalf("path-like endpoint status=%d", response.Code)
	}

	response := call(http.MethodPost, "/api/v1/optimization/proposals", diagnosisProposalBody, "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"gpu_profile"`) {
		t.Fatalf("a missing GPU profile must stay absent: status=%d body=%s", response.Code, response.Body.String())
	}

	store.monitoringErr = domain.ErrNotFound
	if response = call(http.MethodPost, "/api/v1/optimization/proposals", diagnosisProposalBody, ""); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "diagnosis_endpoint_not_found") {
		t.Fatalf("unknown endpoint status=%d body=%s", response.Code, response.Body.String())
	}
	store.monitoringErr = errors.New("database down")
	if response = call(http.MethodPost, "/api/v1/optimization/proposals", diagnosisProposalBody, ""); response.Code != http.StatusInternalServerError {
		t.Fatalf("store failure status=%d body=%s", response.Code, response.Body.String())
	}
	store.monitoringErr, store.profileErr = nil, errors.New("profile store down")
	if response = call(http.MethodPost, "/api/v1/optimization/proposals", diagnosisProposalBody, ""); response.Code != http.StatusInternalServerError {
		t.Fatalf("a failing profile read must not silently drop evidence: status=%d", response.Code)
	}

	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	plain := (API{Store: &fakeStore{}, APIKey: "secret", Integrations: registry.Snapshot()}).Handler()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/optimization/proposals", strings.NewReader(diagnosisProposalBody))
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	plain.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("store without monitoring status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestCampaignDiagnosisStaysAbsentWithoutARecordedPlan(t *testing.T) {
	for _, stored := range []string{"", `{"input":{"workload_profile":"interactive"}}`, `{"diagnosis":{"state":"applied"},"input":{}}`} {
		if plan, present := optimizationCampaignResponse(domain.OptimizationCampaign{ProposalJSON: stored}, campaignQualityInputs{})["diagnosis"]; present {
			t.Fatalf("proposal %q recorded no diagnosis input, got %#v", stored, plan)
		}
	}
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
