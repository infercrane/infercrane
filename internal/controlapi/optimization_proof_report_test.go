package controlapi

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/authz"
	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/proofreport"
)

// The Proof Report reads the caller's runs only, matches its documented
// schema, is signed byte for byte when every run finished, and renders the
// same report as a Markdown or HTML attachment.
func TestProofReportIsTenantScopedSignedAndRendered(t *testing.T) {
	_, key, err := passport.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	campaign, benchmarks := reportCampaign()
	store := &fakeReportStore{fakeStore: &fakeStore{principal: domain.Principal{ID: "viewer-a", TenantID: "tenant-a", Role: string(authz.Viewer), Kind: "api_key", Scopes: authz.DefaultScopeNames(authz.Viewer)}}, campaign: campaign, bench: benchmarks}
	handler := (API{Store: store, Authenticator: store, PassportPrivateKey: key}).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer viewer-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}

	response := get("/api/v1/optimization/proof-report?campaigns=campaign-1&title=Proof%20for%20Example%20Labs")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope map[string]any
	if err = json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	assertMatchesDocumentedSchema(t, "ProofReportEnvelope", envelope)
	assertMatchesDocumentedSchema(t, "ProofReport", envelope["report"].(map[string]any))
	verified, err := proofreport.Verify(response.Body.Bytes(), key.Public().(ed25519.PublicKey))
	if err != nil || !verified.Pinned || len(verified.Runs) != 1 || verified.Runs[0] != "campaign-1" {
		t.Fatalf("served report does not verify: %+v err=%v", verified, err)
	}
	var document proofreport.Document
	_ = json.Unmarshal(response.Body.Bytes(), &document)
	// The fixture ran on a simulated CPU, so nothing it measured is a saving.
	run := document.Report.Workloads[0].Runs[0]
	if document.Report.Title != "Proof for Example Labs" || run.Saving.State != proofreport.SavingNotShown || run.Saving.MonthlySavingUSD != nil || !run.Simulated {
		t.Fatalf("report = %+v run = %+v", document.Report, run.Saving)
	}

	markdown := get("/api/v1/optimization/proof-report?campaigns=campaign-1&format=markdown")
	if markdown.Code != http.StatusOK || !strings.HasPrefix(markdown.Header().Get("Content-Type"), "text/markdown") || !strings.Contains(markdown.Header().Get("Content-Disposition"), "attachment") || !strings.HasPrefix(markdown.Body.String(), "# Proof Report") {
		t.Fatalf("markdown: %d %v %.80s", markdown.Code, markdown.Header(), markdown.Body.String())
	}
	page := get("/api/v1/optimization/proof-report?campaigns=campaign-1&format=html")
	if page.Code != http.StatusOK || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") || !strings.Contains(page.Header().Get("Content-Security-Policy"), "default-src 'none'") || page.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(page.Body.String(), "<!doctype html>") {
		t.Fatalf("html: %d %v", page.Code, page.Header())
	}

	for path, status := range map[string]int{
		"/api/v1/optimization/proof-report":                                        http.StatusBadRequest,
		"/api/v1/optimization/proof-report?campaigns=campaign-1,campaign-1":        http.StatusBadRequest,
		"/api/v1/optimization/proof-report?campaigns=campaign-1&format=pdf":        http.StatusBadRequest,
		"/api/v1/optimization/proof-report?campaigns=campaign-1&title=a%0Ab":       http.StatusBadRequest,
		"/api/v1/optimization/proof-report?campaigns=campaign-1,missing":           http.StatusNotFound,
		"/api/v1/optimization/proof-report?campaigns=campaign-1&prepared_for=Acme": http.StatusOK,
	} {
		if response := get(path); response.Code != status {
			t.Errorf("%s: status=%d want %d body=%s", path, response.Code, status, response.Body.String())
		}
	}

	// Another tenant's run is not found, and the store is never asked for
	// any tenant but the caller's.
	store.principal.TenantID = "tenant-b"
	if response = get("/api/v1/optimization/proof-report?campaigns=campaign-1"); response.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status=%d", response.Code)
	}
	for _, tenant := range store.tenants {
		if tenant != "tenant-a" && tenant != "tenant-b" {
			t.Fatalf("store was asked for tenant %q", tenant)
		}
	}

	// Without a key the report says why it is not signed.
	unsigned := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/optimization/proof-report?campaigns=campaign-1", nil)
	request.Header.Set("Authorization", "Bearer viewer-token")
	store.principal.TenantID = "tenant-a"
	(API{Store: store, Authenticator: store}).Handler().ServeHTTP(unsigned, request)
	if !strings.Contains(unsigned.Body.String(), `"signature":null,"unsigned_reason":"signing_key_not_configured"`) {
		t.Fatalf("unsigned: %s", unsigned.Body.String()[max(0, unsigned.Body.Len()-160):])
	}
}
