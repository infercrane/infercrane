package hfcatalog

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestOfficialMetadataIsBoundedNormalizedAndProvenanced(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/models/Qwen/Qwen3-8B" || r.Header.Get("Accept") != "application/json" || !strings.HasPrefix(r.Header.Get("User-Agent"), "infercrane-hf-catalog/") {
			t.Fatalf("unexpected request: method=%s path=%s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		if got := len(r.URL.Query()["expand"]); got != len(expandedFields) {
			t.Fatalf("expand fields=%v", r.URL.Query()["expand"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id":"Qwen/Qwen3-8B",
  "author":"Qwen",
  "sha":"b968826d9c46dd6066d109eabc6255188de91218",
  "pipeline_tag":"text-generation",
  "private":false,
  "gated":false,
  "downloads":42,
  "likes":7,
  "lastModified":"2026-08-31T10:00:00Z",
  "tags":["transformers","text-generation","transformers"],
  "cardData":{"license":"apache-2.0","language":["en","zh"]},
  "siblings":[{"rfilename":"weights-secret-to-the-catalog.bin"}],
  "future_upstream_field":{"large":"not copied into the normalized record"}
}`))
	}))
	defer server.Close()

	cache, err := New([]string{"Qwen/Qwen3-8B"})
	if err != nil {
		t.Fatal(err)
	}
	client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }, ValidFor: time.Hour}
	if err = cache.Refresh(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	snapshot := cache.Snapshot(now.Add(time.Minute), "qwen")
	if snapshot.State != "current" || len(snapshot.Models) != 1 || snapshot.ConfiguredCount != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	model := snapshot.Models[0]
	if model.Repository != "Qwen/Qwen3-8B" || model.Revision == nil || *model.Revision != "b968826d9c46dd6066d109eabc6255188de91218" || model.Access != "public" || model.License == nil || *model.License != "apache-2.0" || !model.Current {
		t.Fatalf("model=%+v", model)
	}
	if len(model.Tags) != 2 || len(model.Languages) != 2 || !contains(model.UnknownFields, "library_name") || strings.Contains(model.Provenance.Endpoint, "weights-secret") {
		t.Fatalf("unbounded or ambiguous normalized metadata=%+v", model)
	}
}

func TestRefreshRetainsLastGoodRecordOnFailure(t *testing.T) {
	now := time.Date(2026, time.September, 1, 12, 0, 0, 0, time.UTC)
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			http.Error(w, "temporary upstream detail", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"id":"BAAI/bge-m3","sha":"5617a9f61b028005a4858fdac845db406aefb181","private":false,"gated":false,"cardData":{}}`))
	}))
	defer server.Close()

	cache, err := New([]string{"BAAI/bge-m3"})
	if err != nil {
		t.Fatal(err)
	}
	client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now }}
	if err = cache.Refresh(context.Background(), client); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	now = now.Add(time.Hour)
	if err = cache.Refresh(context.Background(), client); err == nil || strings.Contains(err.Error(), "temporary upstream detail") {
		t.Fatalf("unsafe or missing refresh error: %v", err)
	}
	snapshot := cache.Snapshot(now, "")
	if len(snapshot.Models) != 1 || snapshot.Models[0].Revision == nil || *snapshot.Models[0].Revision != "5617a9f61b028005a4858fdac845db406aefb181" || snapshot.State != "partial" {
		t.Fatalf("last-good metadata was not retained: %+v", snapshot)
	}
}

func TestClientBoundsResponsesAndProtectsToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"Qwen/Qwen3-8B","padding":"` + strings.Repeat("x", maxResponseBytes) + `"}`))
	}))
	defer server.Close()
	_, err := (Client{BaseURL: server.URL, HTTPClient: server.Client()}).Fetch(context.Background(), "Qwen/Qwen3-8B")
	if err == nil || !strings.Contains(err.Error(), "exceeded 1 MiB") {
		t.Fatalf("oversized response err=%v", err)
	}
	_, err = (Client{BaseURL: server.URL, Token: "never-send-this", HTTPClient: server.Client()}).Fetch(context.Background(), "Qwen/Qwen3-8B")
	if err == nil || !strings.Contains(err.Error(), "untrusted endpoint") || strings.Contains(err.Error(), "never-send-this") {
		t.Fatalf("token boundary err=%v", err)
	}
}

func TestClientDoesNotFollowAuthenticatedRedirects(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.Hostname() != "huggingface.co" || request.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("unexpected authenticated request: url=%s authorization=%q", request.URL, request.Header.Get("Authorization"))
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"https://attacker.example/steal"}},
			Body:       io.NopCloser(strings.NewReader("redirect")),
			Request:    request,
		}, nil
	})
	_, err := (Client{Token: "secret", HTTPClient: &http.Client{Transport: transport}}).Fetch(context.Background(), "Qwen/Qwen3-8B")
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") || calls.Load() != 1 {
		t.Fatalf("authenticated redirect was followed: calls=%d err=%v", calls.Load(), err)
	}
}

func TestCatalogRejectsUnboundedOrUnsafeRepositorySets(t *testing.T) {
	if _, err := New([]string{"../../etc/passwd"}); err == nil {
		t.Fatal("unsafe repository was accepted")
	}
	repositories := make([]string, maxRepositories+1)
	for index := range repositories {
		repositories[index] = "org/model-" + strings.Repeat("x", index%3)
	}
	if _, err := New(repositories); err == nil {
		t.Fatal("unbounded repository set was accepted")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
