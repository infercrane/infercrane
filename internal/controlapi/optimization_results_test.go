package controlapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/resultprior"
	"github.com/infercrane/infercrane/internal/resultstore"
)

// fakeResultStore keeps result records per tenant in memory, and reduces
// other tenants' shared records as the PostgreSQL store does.
type fakeResultStore struct {
	*fakeStore
	records map[string][]resultstore.Record
}

func (f *fakeResultStore) PutResultRecords(_ context.Context, tenant string, records []resultstore.Record) (int, error) {
	added := 0
	for _, record := range records {
		known := false
		for index, existing := range f.records[tenant] {
			if existing.ID == record.ID {
				f.records[tenant][index].Sharing, known = record.Sharing, true
			}
		}
		if !known {
			f.records[tenant] = append(f.records[tenant], record)
			added++
		}
	}
	return added, nil
}

func (f *fakeResultStore) ListResultRecords(_ context.Context, tenant string, filter resultstore.Filter) ([]resultstore.Record, error) {
	return resultstore.Newest(f.records[tenant], filter), nil
}

func (f *fakeResultStore) GetResultRecord(_ context.Context, tenant, id string) (resultstore.Record, error) {
	for _, record := range f.records[tenant] {
		if record.ID == id {
			return record, nil
		}
	}
	return resultstore.Record{}, resultstore.ErrNotFound
}

func (f *fakeResultStore) SharedResultRecords(_ context.Context, tenant string, filter resultstore.Filter) ([]resultstore.Record, error) {
	var out []resultstore.Record
	for owner, records := range f.records {
		for _, record := range records {
			if owner != tenant && record.Sharing == resultstore.SharingNumbers && filter.Match(record) {
				out = append(out, record.NumbersOnly())
			}
		}
	}
	return out, nil
}

func gymRecords(t *testing.T) []resultstore.Record {
	t.Helper()
	body, err := os.ReadFile("../resultstore/testdata/synthetic/map-hybrid.json")
	if err != nil {
		t.Fatal(err)
	}
	records, err := resultstore.Import("map-hybrid.json", body, resultstore.ImportOptions{Model: resultstore.Model{Architecture: resultstore.ArchitectureHybrid, ParamsB: 27, Checkpoint: "fp8"},
		RecordedAt: time.Date(2001, 1, 2, 3, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestResultRecordsAreReadInsideTheCallersTenantAndSharedOnlyAsNumbers(t *testing.T) {
	operator := domain.Principal{ID: "operator-a", TenantID: "tenant-a", Role: string(authz.Operator), Kind: "api_key", Scopes: authz.DefaultScopeNames(authz.Operator)}
	store := &fakeResultStore{fakeStore: &fakeStore{principal: operator}, records: map[string][]resultstore.Record{}}
	handler := (API{Store: store, Authenticator: store}).Handler()
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		var reader *bytes.Reader
		if body != nil {
			encoded, _ := json.Marshal(body)
			reader = bytes.NewReader(encoded)
		} else {
			reader = bytes.NewReader(nil)
		}
		request := httptest.NewRequest(method, "/api/v1/optimization/results"+path, reader)
		request.Header.Set("Authorization", "Bearer token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	records := gymRecords(t)
	// Tenant A imports with numbers sharing; the sharing in the records themselves is ignored.
	records[0].Sharing = resultstore.SharingNumbers
	response := call(http.MethodPost, "", map[string]any{"schema_version": resultstore.BundleSchemaV1, "records": records})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"sharing":"private"`) {
		t.Fatalf("import: %d %s", response.Code, response.Body.String())
	}
	for _, record := range store.records["tenant-a"] {
		if record.Sharing != resultstore.SharingPrivate {
			t.Fatal("a record's own sharing field was taken over the request's")
		}
	}
	response = call(http.MethodPost, "", map[string]any{"schema_version": resultstore.BundleSchemaV1, "sharing": "numbers", "records": records})
	var receipt struct{ Added, Received int }
	_ = json.Unmarshal(response.Body.Bytes(), &receipt)
	if response.Code != http.StatusOK || receipt.Added != 0 || receipt.Received != len(records) {
		t.Fatalf("re-import: %d %s", response.Code, response.Body.String())
	}
	tampered := records[1]
	tampered.Observed.Median++
	if response = call(http.MethodPost, "", map[string]any{"schema_version": resultstore.BundleSchemaV1, "records": []resultstore.Record{tampered}}); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a record whose number no longer matches its ID: %d", response.Code)
	}
	if response = call(http.MethodGet, "?kind=short&min_load=128&metric="+resultstore.MetricOutput, nil); response.Code != http.StatusOK {
		t.Fatalf("list: %d %s", response.Code, response.Body.String())
	}
	var listed struct{ Data []resultstore.Record }
	_ = json.Unmarshal(response.Body.Bytes(), &listed)
	if len(listed.Data) != 4 {
		t.Fatalf("short/128 has four setups, listed %d", len(listed.Data))
	}
	if response = call(http.MethodGet, "/"+records[2].ID, nil); response.Code != http.StatusOK {
		t.Fatalf("get: %d", response.Code)
	}
	if response = call(http.MethodGet, "?limit=x", nil); response.Code != http.StatusBadRequest {
		t.Fatalf("bad limit: %d", response.Code)
	}

	// Tenant B sees none of it by list or get, but its ranking reads A's shared numbers.
	store.principal = domain.Principal{ID: "viewer-b", TenantID: "tenant-b", Role: string(authz.Viewer), Kind: "api_key", Scopes: authz.DefaultScopeNames(authz.Viewer)}
	response = call(http.MethodGet, "", nil)
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"data":[]}` {
		t.Fatalf("tenant B listed: %s", response.Body.String())
	}
	if response = call(http.MethodGet, "/"+records[2].ID, nil); response.Code != http.StatusNotFound {
		t.Fatalf("tenant B read tenant A's record: %d", response.Code)
	}
	if response = call(http.MethodPost, "", map[string]any{"schema_version": resultstore.BundleSchemaV1, "records": records[:1]}); response.Code != http.StatusForbidden {
		t.Fatalf("a viewer imported records: %d", response.Code)
	}
	query := resultprior.Query{Model: resultstore.Model{Architecture: resultstore.ArchitectureHybrid, ParamsB: 27, Checkpoint: "fp8", Features: []string{"native_mtp"}},
		Accelerator: "H100", EngineVersion: "0.34.0", Kind: "short", Load: 112, Metric: resultstore.MetricOutput, Current: resultstore.Knobs{Engine: "vllm"},
		Candidates: []resultprior.Candidate{{Name: "three", Knobs: resultstore.Knobs{Engine: "vllm", Speculation: "mtp", Drafts: 3}}, {Name: "one", Knobs: resultstore.Knobs{Engine: "vllm", Speculation: "mtp", Drafts: 1}}}}
	response = call(http.MethodPost, "/rank", query)
	if response.Code != http.StatusOK {
		t.Fatalf("rank: %d %s", response.Code, response.Body.String())
	}
	var ranking resultprior.Ranking
	_ = json.Unmarshal(response.Body.Bytes(), &ranking)
	if len(ranking.Ranked) != 2 || ranking.Ranked[0].Name != "one" || !ranking.Ranked[0].Known || !ranking.Ranked[0].Evidence[0].Shared {
		t.Fatalf("at 112 short requests one draft should lead from the shared map: %+v", ranking)
	}
	for _, private := range []string{"map-hybrid", "example/hybrid-27b-fp8", records[0].ID} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatalf("the ranking named %q from another tenant", private)
		}
	}
	if response = call(http.MethodPost, "/rank", resultprior.Query{Metric: "x"}); response.Code != http.StatusBadRequest {
		t.Fatalf("an empty ranking: %d", response.Code)
	}
}
