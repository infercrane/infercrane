package controlapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
)

type fakeHostedModelAPIUsageStore struct {
	*fakeStore
	snapshot domain.HostedModelAPIUsageSnapshot
	tenant   string
	model    string
	window   time.Duration
	bucket   time.Duration
	err      error
}

func (f *fakeHostedModelAPIUsageStore) HostedModelAPIUsage(_ context.Context, tenant, model string, window, bucket time.Duration) (domain.HostedModelAPIUsageSnapshot, error) {
	f.tenant, f.model, f.window, f.bucket = tenant, model, window, bucket
	return f.snapshot, f.err
}

func TestHostedModelAPIUsageIsTenantScopedAndTruthful(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	input, output := int64(120), int64(40)
	base := &fakeStore{principal: domain.Principal{ID: "reader", TenantID: "tenant-a", Name: "reader", Role: string(authz.Viewer), Scopes: []string{"read"}}}
	usage := &fakeHostedModelAPIUsageStore{fakeStore: base, snapshot: domain.HostedModelAPIUsageSnapshot{
		WindowStart: now.Add(-time.Hour), WindowEnd: now, BucketSeconds: 300, Currency: "USD",
		Summary:  domain.HostedModelAPIUsageSummary{TransmittedRequests: 2, SettledRequests: 1, PendingReconciliationRequests: 1, TokenUsageSamples: 1, SettlementEntries: 1, InputTokens: &input, OutputTokens: &output, SettledSpendMicrousd: 2400},
		Models:   []domain.HostedModelAPIUsageModel{{ProductID: "glm-5.2", LatestRequestAt: now, Usage: domain.HostedModelAPIUsageSummary{TransmittedRequests: 2}}},
		Series:   []domain.HostedModelAPIUsageBucket{},
		Evidence: domain.HostedModelAPIUsageEvidence{Source: "model_api_usage_reservations+model_api_usage_ledger", RequestScope: "funded_supplier_attempts", LatestRequestAt: &now, ContentRecorded: false, Available: []string{"requests", "tokens", "settled_spend"}, Unavailable: []string{"errors", "latency", "ttft"}},
	}}
	handler := (API{Store: usage, Authenticator: usage}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-usage?window_seconds=3600&bucket_seconds=300&model=glm-5.2", nil)
	request.Header.Set("Authorization", "Bearer tenant-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, body)
	}
	if usage.tenant != "tenant-a" || usage.model != "glm-5.2" || usage.window != time.Hour || usage.bucket != 5*time.Minute {
		t.Fatalf("unexpected store request tenant=%q model=%q window=%s bucket=%s", usage.tenant, usage.model, usage.window, usage.bucket)
	}
	for _, expected := range []string{`"transmitted_requests":2`, `"pending_reconciliation_requests":1`, `"input_tokens":120`, `"settled_spend_microusd":2400`, `"request_scope":"funded_supplier_attempts"`, `"content_recorded":false`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing %s in %s", expected, body)
		}
	}
	for _, forbidden := range []string{`"supplier"`, `"supplier_model_id"`, `"error_count"`, `"p95_latency_ms"`, `"p95_ttft_ms"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("hosted usage leaked or invented %s in %s", forbidden, body)
		}
	}
}

func TestHostedModelAPIUsageRejectsUnboundedOrAmbiguousQueries(t *testing.T) {
	base := &fakeStore{principal: domain.Principal{ID: "reader", TenantID: "tenant-a", Name: "reader", Role: string(authz.Viewer), Scopes: []string{"read"}}}
	usage := &fakeHostedModelAPIUsageStore{fakeStore: base}
	handler := (API{Store: usage, Authenticator: usage}).Handler()
	for _, test := range []struct {
		query string
		want  int
	}{
		{"?unexpected=1", http.StatusBadRequest},
		{"?model=a&model=b", http.StatusBadRequest},
		{"?window_seconds=abc", http.StatusBadRequest},
		{"?window_seconds=2592000&bucket_seconds=60", http.StatusUnprocessableEntity},
		{"?model=%20glm-5.2", http.StatusUnprocessableEntity},
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-usage"+test.query, nil)
		request.Header.Set("Authorization", "Bearer tenant-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("query=%q status=%d want=%d body=%s", test.query, response.Code, test.want, response.Body.String())
		}
	}
}

func TestHostedModelAPIUsageRequiresReadScope(t *testing.T) {
	base := &fakeStore{principal: domain.Principal{ID: "operator", TenantID: "tenant-a", Name: "operator", Role: string(authz.Operator), Scopes: []string{"deploy"}}}
	usage := &fakeHostedModelAPIUsageStore{fakeStore: base}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/model-api-usage", nil)
	request.Header.Set("Authorization", "Bearer tenant-token")
	response := httptest.NewRecorder()
	(API{Store: usage, Authenticator: usage}).Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
