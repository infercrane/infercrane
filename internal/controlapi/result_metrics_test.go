package controlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/resultmetrics"
)

const prometheusAccept = "application/openmetrics-text;version=1.0.0;q=0.5,text/plain;version=0.0.4;q=0.3,*/*;q=0.1"

// fakeResultMetricsStore serves one snapshot and the report store of
// reportCampaign, and records which workspace each read was for.
type fakeResultMetricsStore struct {
	*fakeReportStore
	snapshot resultmetrics.Snapshot
	err      error
	reads    []string
	loads    int
	loadMu   sync.Mutex
}

func (f *fakeResultMetricsStore) ResultMetrics(_ context.Context, tenant string) (resultmetrics.Snapshot, error) {
	f.saw(tenant)
	f.mu.Lock()
	f.reads = append(f.reads, tenant)
	f.mu.Unlock()
	snapshot := f.snapshot
	snapshot.Campaigns = append([]resultmetrics.Campaign(nil), f.snapshot.Campaigns...)
	return snapshot, f.err
}

func (f *fakeResultMetricsStore) OptimizationCampaign(ctx context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	f.loadMu.Lock()
	f.loads++
	f.loadMu.Unlock()
	return f.fakeReportStore.OptimizationCampaign(ctx, tenant, id)
}

func (f *fakeResultMetricsStore) loadCount() int {
	f.loadMu.Lock()
	defer f.loadMu.Unlock()
	return f.loads
}

func resultMetricsFixture(t *testing.T) (*fakeResultMetricsStore, http.Handler) {
	t.Helper()
	campaign, benchmarks := reportCampaign()
	verdict := "BLOCK"
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	store := &fakeResultMetricsStore{
		fakeReportStore: &fakeReportStore{fakeStore: &fakeStore{principal: domain.Principal{ID: "viewer-a", TenantID: "tenant-a", Role: string(authz.Viewer), Kind: "api_key", Scopes: authz.DefaultScopeNames(authz.Viewer)}}, campaign: campaign, bench: benchmarks},
		snapshot: resultmetrics.Snapshot{
			SchemaVersion: resultmetrics.SchemaVersion, GeneratedAt: at,
			Deployments: []resultmetrics.Deployment{{
				Name: "qwen-prod", LatestGuard: &resultmetrics.GuardResult{Decision: "REJECT", QualityVerdict: &verdict, EvaluatedAt: at},
				GuardDecisions: map[string]int{"REJECT": 1}, QualityVerdicts: map[string]int{"BLOCK": 1}, Rollbacks: map[string]int{"automatic": 2},
			}},
			Campaigns:      []resultmetrics.Campaign{{Endpoint: "qwen-prod", ID: campaign.ID, State: "qualified", UpdatedAt: at}},
			CampaignStates: map[string]int{"qualified": 1},
		},
	}
	return store, (API{Store: store, Authenticator: store, ResultReports: &ResultReports{}}).Handler()
}

func scrape(handler http.Handler, accept, token, query string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/metrics"+query, nil)
	if accept != "" {
		request.Header.Set("Accept", accept)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestResultMetricsScrapeIsAuthenticatedAndWorkspaceScoped(t *testing.T) {
	store, handler := resultMetricsFixture(t)

	if response := scrape(handler, prometheusAccept, "", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", response.Code)
	}
	response := scrape(handler, prometheusAccept, "viewer-token", "")
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("status=%d type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{
		`infercrane_release_guard_quality_verdict{deployment="qwen-prod",verdict="BLOCK"} 1`,
		`infercrane_rollbacks_total{deployment="qwen-prod",trigger="automatic"} 2`,
		`infercrane_campaign_state{endpoint="qwen-prod",state="qualified"} 1`,
		// Read from the run's report through the real report loader.
		`infercrane_campaign_outcome{endpoint="qwen-prod",outcome="winner_selected"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s in\n%s", want, body)
		}
	}
	// The report has no sourced price, so cost is unknown: no series.
	if strings.Contains(body, "infercrane_campaign_cost_per_million_output_tokens_usd{") {
		t.Fatalf("unknown cost exported:\n%s", body)
	}
	for _, tenant := range store.tenants {
		if tenant != "tenant-a" {
			t.Fatalf("store asked for workspace %q", tenant)
		}
	}

	// Another workspace's key reads its own workspace only.
	store.principal.TenantID = "tenant-b"
	if response = scrape(handler, prometheusAccept, "viewer-token", ""); response.Code != http.StatusOK || store.reads[len(store.reads)-1] != "tenant-b" {
		t.Fatalf("tenant-b status=%d reads=%v", response.Code, store.reads)
	}
	if strings.Contains(response.Body.String(), "winner_selected") {
		t.Fatal("another workspace's run report was read")
	}

	if response = scrape(handler, prometheusAccept, "viewer-token", "?tenant=tenant-a"); response.Code != http.StatusBadRequest {
		t.Fatalf("query status=%d", response.Code)
	}
}

func TestResultMetricsAnswersJSONToTheSDKs(t *testing.T) {
	_, handler := resultMetricsFixture(t)
	response := scrape(handler, "application/json", "viewer-token", "")
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status=%d type=%q", response.Code, response.Header().Get("Content-Type"))
	}
	var snapshot map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	campaign := snapshot["campaigns"].([]any)[0].(map[string]any)
	if snapshot["schema_version"] != resultmetrics.SchemaVersion || campaign["outcome"] != "winner_selected" {
		t.Fatalf("snapshot = %s", response.Body.String())
	}
	if value, present := campaign["chosen_cost_per_million_output_tokens_usd"]; !present || value != nil {
		t.Fatalf("unknown cost is not an explicit null: %s", response.Body.String())
	}
}

func TestResultMetricsReadsEachReportOnce(t *testing.T) {
	store, handler := resultMetricsFixture(t)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if response := scrape(handler, prometheusAccept, "viewer-token", ""); response.Code != http.StatusOK {
				t.Errorf("status=%d", response.Code)
			}
		}()
	}
	wait.Wait()
	before := store.loadCount()
	if before < 1 || before > 8 {
		t.Fatalf("report loads = %d", before)
	}
	scrape(handler, prometheusAccept, "viewer-token", "")
	if store.loadCount() != before {
		t.Fatal("an unchanged report was assembled again")
	}
	// A run that changed has a new report.
	store.snapshot.Campaigns[0].UpdatedAt = store.snapshot.Campaigns[0].UpdatedAt.Add(time.Minute)
	scrape(handler, prometheusAccept, "viewer-token", "")
	if store.loadCount() != before+1 {
		t.Fatalf("changed run was not read again: %d", store.loadCount())
	}
}

func TestResultMetricsBoundsReportReadsPerScrape(t *testing.T) {
	store, handler := resultMetricsFixture(t)
	store.snapshot.Campaigns = nil
	for index := range 10 {
		store.snapshot.Campaigns = append(store.snapshot.Campaigns, resultmetrics.Campaign{Endpoint: fmt.Sprintf("endpoint-%d", index), ID: fmt.Sprintf("missing-%d", index), State: "running"})
	}
	response := scrape(handler, prometheusAccept, "viewer-token", "")
	if response.Code != http.StatusOK || store.loadCount() != resultReportReadsPerScrape {
		t.Fatalf("status=%d loads=%d", response.Code, store.loadCount())
	}
	if strings.Contains(response.Body.String(), "infercrane_campaign_outcome{") {
		t.Fatal("outcome exported for a report that could not be read")
	}
}

func TestResultMetricsErrorsAndMissingCapability(t *testing.T) {
	store, handler := resultMetricsFixture(t)
	store.err = errors.New("database down")
	if response := scrape(handler, prometheusAccept, "viewer-token", ""); response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "database down") {
		t.Fatalf("store error status=%d body=%s", response.Code, response.Body.String())
	}
	plain := &fakeStore{principal: store.principal}
	if response := scrape((API{Store: plain, Authenticator: plain}).Handler(), prometheusAccept, "viewer-token", ""); response.Code != http.StatusNotImplemented {
		t.Fatalf("missing capability status=%d", response.Code)
	}
}

func TestResultMetricsWithoutReportCacheLeavesOutcomeUnknown(t *testing.T) {
	store, _ := resultMetricsFixture(t)
	handler := (API{Store: store, Authenticator: store}).Handler()
	response := scrape(handler, prometheusAccept, "viewer-token", "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "infercrane_campaign_outcome{") || store.loadCount() != 0 {
		t.Fatalf("status=%d loads=%d body=%s", response.Code, store.loadCount(), response.Body.String())
	}
}

func TestSummarizeReportKeepsOnlyKnownValues(t *testing.T) {
	baseline, winner := 2.0, 1.25
	summary := summarizeReport(optimizationreport.Report{
		Outcome: optimizationreport.Outcome{Kind: optimizationreport.OutcomeWinnerSelected},
		Metrics: []optimizationreport.Metric{{Key: optimizationreport.MetricTTFTP95, Baseline: &winner}, {Key: optimizationreport.MetricCostPerMillion, Baseline: &baseline, Winner: &winner}},
	})
	if summary.outcome == nil || *summary.outcome != "winner_selected" || *summary.baseline != 2 || *summary.chosen != 1.25 {
		t.Fatalf("summary = %+v", summary)
	}
	winner = 9
	if *summary.chosen != 1.25 {
		t.Fatal("summary aliases the report")
	}
	if empty := summarizeReport(optimizationreport.Report{Outcome: optimizationreport.Outcome{Kind: "surprise"}}); empty.outcome != nil || empty.baseline != nil || empty.chosen != nil {
		t.Fatalf("unknown report values were invented: %+v", empty)
	}
}

func TestAcceptsJSON(t *testing.T) {
	for accept, want := range map[string]bool{
		"application/json":                       true,
		"text/plain, application/json;q=0.9":     true,
		prometheusAccept:                         false,
		"text/plain;version=0.0.4":               false,
		"*/*":                                    false,
		"":                                       false,
		"application/jsonp":                      false,
		"application/vnd.infercrane+json, text/": false,
	} {
		if got := acceptsJSON(accept); got != want {
			t.Errorf("acceptsJSON(%q) = %v", accept, got)
		}
	}
}
