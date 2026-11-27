package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

// fakeStudyStore owns several finished campaigns of tenant-a.
type fakeStudyStore struct {
	*fakeReportStore
	campaigns map[string]domain.OptimizationCampaign
}

func (f *fakeStudyStore) OptimizationCampaign(_ context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	f.saw(tenant)
	campaign, ok := f.campaigns[id]
	if !ok || campaign.TenantID != tenant {
		return domain.OptimizationCampaign{}, domain.ErrNotFound
	}
	return campaign, nil
}

// studyCampaign is reportCampaign at its own load, under its own IDs.
func studyCampaign(id string, load int) (domain.OptimizationCampaign, domain.BenchmarkResult) {
	campaign, benchmarks := reportCampaign()
	campaign.ID = id
	bench := benchmarks[0]
	bench.ID = "bench-" + id
	var workload map[string]any
	_ = json.Unmarshal([]byte(bench.WorkloadJSON), &workload)
	workload["concurrency"] = load
	encoded, _ := json.Marshal(workload)
	bench.WorkloadJSON = string(encoded)
	for index := range campaign.Candidates {
		campaign.Candidates[index].ID += "-" + id
		if campaign.Candidates[index].BenchmarkID != "" {
			campaign.Candidates[index].BenchmarkID = bench.ID
		}
	}
	return campaign, bench
}

func TestTheBandStudyReadsTheCallersCampaignsAsLoadBands(t *testing.T) {
	light, lightBench := studyCampaign("campaign-16", 16)
	heavy, heavyBench := studyCampaign("campaign-64", 64)
	viewer := domain.Principal{ID: "viewer-a", TenantID: "tenant-a", Role: string(authz.Viewer), Kind: "api_key", Scopes: authz.DefaultScopeNames(authz.Viewer)}
	store := &fakeStudyStore{fakeReportStore: &fakeReportStore{fakeStore: &fakeStore{principal: viewer}, bench: []domain.BenchmarkResult{lightBench, heavyBench}},
		campaigns: map[string]domain.OptimizationCampaign{light.ID: light, heavy.ID: heavy}}
	handler := (API{Store: store, Authenticator: store}).Handler()
	get := func(query string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/optimization/band-study"+query, nil)
		request.Header.Set("Authorization", "Bearer viewer-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	response := get("?campaigns=campaign-64,campaign-16")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	assertMatchesDocumentedSchema(t, "OptimizationBandStudy", body)
	text := response.Body.String()
	for _, want := range []string{`"summary":"one_setup"`, `"load":16`, `"load":64`, `"switch_bands":[{"engine":"setup-`, `"schema_version":"infercrane.dev/band-study/v1"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("study lacks %s: %s", want, text)
		}
	}
	for _, tenant := range store.tenants {
		if tenant != "tenant-a" {
			t.Fatalf("read outside the caller's workspace: %v", store.tenants)
		}
	}
	if response = get("?campaigns=campaign-16,campaign-elsewhere"); response.Code != http.StatusNotFound {
		t.Fatalf("a campaign the caller does not own: %d %s", response.Code, response.Body.String())
	}
	if response = get("?campaigns=campaign-16"); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "band_study_mismatch") {
		t.Fatalf("one campaign: %d %s", response.Code, response.Body.String())
	}
	twice, twiceBench := studyCampaign("campaign-16b", 16)
	store.campaigns[twice.ID], store.bench = twice, append(store.bench, twiceBench)
	if response = get("?campaigns=campaign-16,campaign-16b"); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("one load twice: %d %s", response.Code, response.Body.String())
	}
}
