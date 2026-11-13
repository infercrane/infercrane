package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// fakeReportStore owns one finished campaign for tenant-a and records every
// tenant the report handler asked for.
type fakeReportStore struct {
	*fakeStore
	mu       sync.Mutex
	campaign domain.OptimizationCampaign
	bench    []domain.BenchmarkResult
	tenants  []string
}

func (f *fakeReportStore) saw(tenant string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tenants = append(f.tenants, tenant)
}

func (f *fakeReportStore) OptimizationCampaign(_ context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	f.saw(tenant)
	if tenant != f.campaign.TenantID || id != f.campaign.ID {
		return domain.OptimizationCampaign{}, domain.ErrNotFound
	}
	return f.campaign, nil
}

func (f *fakeReportStore) BenchmarksForDeployment(_ context.Context, tenant, name string, _ int) ([]domain.BenchmarkResult, error) {
	f.saw(tenant)
	rows := []domain.BenchmarkResult{}
	for _, row := range f.bench {
		if row.TenantID == tenant && row.DeploymentName == name {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (f *fakeReportStore) QualityEvidenceForDeployment(_ context.Context, tenant, _ string, _ int) ([]domain.QualityEvidence, error) {
	f.saw(tenant)
	return nil, nil
}

func (f *fakeReportStore) ReleaseGuardEvaluations(_ context.Context, tenant, _ string, _ int) ([]domain.ReleaseGuardEvaluation, error) {
	f.saw(tenant)
	return nil, nil
}

func (f *fakeReportStore) ReplayTraceByDigest(_ context.Context, tenant, _ string) (domain.ReplayTrace, error) {
	f.saw(tenant)
	return domain.ReplayTrace{}, domain.ErrNotFound
}

func reportCampaign() (domain.OptimizationCampaign, []domain.BenchmarkResult) {
	proposal, _ := json.Marshal(optimizer.Proposal{Input: optimizer.Request{ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive", WorkloadSource: optimizer.WorkloadSourceReviewedDefault}, StartingPoint: &optimizer.StartingPoint{CandidateID: "proposal-control"}})
	spec := func(args ...string) string {
		var draft optimizer.DeploymentDraft
		draft.Name, draft.Model.ID, draft.Runtime.Engine, draft.Runtime.Args = "qwen", "Qwen/Qwen3-0.6B", "custom-oci", args
		draft.Resources.GPU, draft.Resources.GPUCount, draft.Scaling.MaxReplicas = "SIMULATED-CPU", 1, 1
		encoded, _ := json.Marshal(draft)
		return string(encoded)
	}
	measured := optimizationcampaign.MeasuredEvidence{Method: optimizationcampaign.MeasurementMethod, Control: optimizationcampaign.ControlEvidence{Role: optimizationcampaign.ControlRoleBracketed, CandidateID: "control", Runs: 4, MaxDrift: 0.01, DriftLimit: 0.05}, Comparison: map[string]optimizationcampaign.MetricComparison{
		"output_token_throughput": {HigherIsBetter: true, ControlMean: 20, CandidateMean: 26, Delta: 0.3, NoiseFloor: 0.04, Decision: optimizationcampaign.DecisionImproved},
		"ttft_p95_ms":             {ControlMean: 900, CandidateMean: 700, Delta: -0.222, NoiseFloor: 0.05, Decision: optimizationcampaign.DecisionImproved},
	}}
	workload, _ := json.Marshal(map[string]any{"workload_source": "synthetic_profile", "concurrency": 2, "measured": measured})
	tps, ttft := 26.0, 700.0
	campaign := domain.OptimizationCampaign{ID: "campaign-1", TenantID: "tenant-a", ModelIdentity: "Qwen/Qwen3-0.6B", Objective: "interactive", Intent: optimizationcampaign.IntentNewEndpoint, State: optimizationcampaign.CampaignQualified, InputDigest: strings.Repeat("a", 64), ProposalJSON: string(proposal), Candidates: []domain.OptimizationCandidateRun{
		{ID: "control", ProposalCandidateID: "proposal-control", Rank: 1, State: optimizationcampaign.CandidateCleaned, DeploymentSpecJSON: spec()},
		{ID: "winner", ProposalCandidateID: "proposal-winner", Rank: 2, State: optimizationcampaign.CandidateQualified, DeploymentSpecJSON: spec("--parallel", "4"), DeploymentName: "qwen-opt-winner", RevisionID: "rev-winner", BenchmarkID: "bench-winner"},
	}}
	return campaign, []domain.BenchmarkResult{{ID: "bench-winner", TenantID: "tenant-a", DeploymentName: "qwen-opt-winner", RevisionID: "rev-winner", GPU: "SIMULATED-CPU", WorkloadJSON: string(workload), OutputTokenThroughput: &tps, TTFTP95MS: &ttft, CostMetadataJSON: `{"available":false}`}}
}

func TestOptimizationCampaignReportIsTenantScopedAndDocumented(t *testing.T) {
	campaign, benchmarks := reportCampaign()
	store := &fakeReportStore{fakeStore: &fakeStore{principal: domain.Principal{ID: "viewer-a", TenantID: "tenant-a", Role: string(authz.Viewer), Kind: "api_key", Scopes: authz.DefaultScopeNames(authz.Viewer)}}, campaign: campaign, bench: benchmarks}
	handler := (API{Store: store, Authenticator: store}).Handler()
	get := func(path, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	response := get("/api/v1/optimization/campaigns/campaign-1/report", "viewer-token")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Report map[string]any `json:"report"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	assertMatchesDocumentedSchema(t, "OptimizationCampaignReport", envelope.Report)
	var report optimizationreport.Report
	encoded, _ := json.Marshal(envelope.Report)
	if err := json.Unmarshal(encoded, &report); err != nil {
		t.Fatal(err)
	}
	if !report.SimulatedAccelerator || report.Banner != optimizationreport.CPUBanner || report.Outcome.Kind != optimizationreport.OutcomeWinnerSelected {
		t.Fatalf("report = %+v", report)
	}
	// The control's own benchmark is absent here, but the winner was measured
	// in turns with it, so speed is compared; price is not sourced, so cost
	// stays null rather than zero.
	for _, metric := range report.Metrics {
		switch metric.Key {
		case optimizationreport.MetricTTFTP95:
			if metric.Decision != optimizationreport.DecisionImproved || metric.Baseline == nil || *metric.Baseline != 900 {
				t.Fatalf("ttft = %+v", metric)
			}
		case optimizationreport.MetricCostPerMillion:
			if metric.Baseline != nil || metric.Winner != nil || metric.Decision != optimizationreport.DecisionUnknown {
				t.Fatalf("cost = %+v", metric)
			}
		}
	}
	if !strings.Contains(response.Body.String(), `"cost_per_million_output_tokens"`) || !strings.Contains(response.Body.String(), `"winner":null`) {
		t.Fatalf("unknown values are not explicit nulls: %s", response.Body.String())
	}

	// Another tenant cannot read it, and the handler never asks the store for
	// anything but the caller's tenant.
	store.principal.TenantID = "tenant-b"
	if response = get("/api/v1/optimization/campaigns/campaign-1/report", "viewer-token"); response.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status=%d body=%s", response.Code, response.Body.String())
	}
	if response = get("/api/v1/optimization/campaigns/missing/report", "viewer-token"); response.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d", response.Code)
	}
	for _, tenant := range store.tenants {
		if tenant != "tenant-a" && tenant != "tenant-b" {
			t.Fatalf("store was asked for tenant %q", tenant)
		}
	}
	if response = get("/api/v1/optimization/campaigns/campaign-1/report", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", response.Code)
	}

	unconfigured := (API{Store: &fakeStore{}, APIKey: "secret"}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/optimization/campaigns/campaign-1/report", nil)
	request.Header.Set("Authorization", "Bearer secret")
	recorder := httptest.NewRecorder()
	unconfigured.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured status=%d", recorder.Code)
	}
}
