package huggingfacesearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHub serves /api/models like the public Hugging Face API, with one
// canned listing per pipeline tag.
type fakeHub struct {
	t        *testing.T
	listings map[string]string
	status   int
	block    chan struct{}
	calls    atomic.Int64
	mu       sync.Mutex
	queries  []string
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls.Add(1)
	if r.URL.Path != "/api/models" {
		h.t.Errorf("unexpected path %s", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	query := r.URL.Query()
	h.mu.Lock()
	h.queries = append(h.queries, r.URL.RawQuery)
	h.mu.Unlock()
	if query.Get("sort") != "downloads" || query.Get("direction") != "-1" || query.Get("search") == "" || query.Get("limit") == "" {
		h.t.Errorf("unexpected query %s", r.URL.RawQuery)
	}
	if got := strings.Join(query["expand[]"], ","); got != "downloads,gated,lastModified,library_name,pipeline_tag,tags" {
		h.t.Errorf("unexpected expand fields %q", got)
	}
	if r.Header.Get("Authorization") != "" {
		h.t.Errorf("search must not authenticate to Hugging Face")
	}
	if h.block != nil {
		select {
		case <-h.block:
		case <-r.Context().Done():
			return
		}
	}
	if h.status != 0 {
		w.WriteHeader(h.status)
		_, _ = w.Write([]byte(`{"error":"upstream detail that must not leak"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	listing, found := h.listings[query.Get("pipeline_tag")]
	if !found {
		listing = "[]"
	}
	_, _ = w.Write([]byte(listing))
}

const textGeneration = `[
  {"_id":"1","id":"microsoft/Phi-3.5-mini-instruct","gated":false,"lastModified":"2025-12-10T20:22:56.000Z","downloads":340333,"tags":["transformers","text-generation","license:mit"],"pipeline_tag":"text-generation","library_name":"transformers"},
  {"_id":"2","id":"meta-llama/Llama-3.2-1B-Instruct","gated":"manual","lastModified":"2024-10-24T15:07:51.000Z","downloads":1300834,"tags":["license:llama3.2"],"pipeline_tag":"text-generation","library_name":"transformers"},
  {"_id":"3","id":"someone/phi-unknown","tags":["text-generation"],"pipeline_tag":"text-generation"},
  {"_id":"4","id":"someone/private-phi","private":true,"downloads":9999999,"pipeline_tag":"text-generation"},
  {"_id":"5","id":"not a repository","downloads":9999999,"pipeline_tag":"text-generation"},
  {"_id":"6","id":"someone/mislabeled","downloads":9999999,"pipeline_tag":"image-classification"}
]`

const featureExtraction = `[
  {"_id":"7","id":"BAAI/bge-m3","gated":false,"lastModified":"2024-07-03T14:50:10.000Z","downloads":5000000,"tags":["sentence-transformers","license:mit"],"pipeline_tag":"feature-extraction","library_name":"sentence-transformers"},
  {"_id":"8","id":"MICROSOFT/phi-3.5-mini-instruct","downloads":1,"pipeline_tag":"feature-extraction"}
]`

func newHub(t *testing.T) (*fakeHub, *httptest.Server) {
	t.Helper()
	hub := &fakeHub{t: t, listings: map[string]string{"text-generation": textGeneration, "feature-extraction": featureExtraction}}
	server := httptest.NewServer(hub)
	t.Cleanup(server.Close)
	return hub, server
}

func newSearcher(t *testing.T, options Options) *Searcher {
	t.Helper()
	searcher, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return searcher
}

func TestSearchMergesPipelinesMostDownloadedFirstAndKeepsUnknownsNull(t *testing.T) {
	hub, server := newHub(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	searcher := newSearcher(t, Options{Endpoint: server.URL + "/", Now: func() time.Time { return now }})

	result, err := searcher.Search(context.Background(), "  phi  ", 10)
	if err != nil {
		t.Fatal(err)
	}
	if result.Query != "phi" || result.Limit != 10 || result.Source != server.URL || !result.FetchedAt.Equal(now) || result.Cached {
		t.Fatalf("unexpected result header %+v", result)
	}
	got := make([]string, 0, len(result.Models))
	for _, model := range result.Models {
		got = append(got, model.Repository)
		if model.Reviewed || model.SourceKind != "hugging-face-search" || model.SourceURL != server.URL+"/"+model.Repository {
			t.Fatalf("result is not marked as unreviewed Hugging Face search: %+v", model)
		}
	}
	// Private, malformed, and mislabeled rows are skipped; the second spelling
	// of the Phi repository is a case-insensitive duplicate.
	want := []string{"BAAI/bge-m3", "meta-llama/Llama-3.2-1B-Instruct", "microsoft/Phi-3.5-mini-instruct", "someone/phi-unknown"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("models = %v, want %v", got, want)
	}

	phi := result.Models[2]
	if phi.PipelineTag != "text-generation" || phi.License == nil || *phi.License != "mit" || phi.Gated == nil || *phi.Gated || phi.GatedMode != nil ||
		phi.Downloads == nil || *phi.Downloads != 340333 || phi.LastModified == nil || *phi.LastModified != "2025-12-10T20:22:56.000Z" ||
		phi.LibraryName == nil || *phi.LibraryName != "transformers" {
		t.Fatalf("phi fields were not reported as Hugging Face returned them: %+v", phi)
	}
	llama := result.Models[1]
	if llama.Gated == nil || !*llama.Gated || llama.GatedMode == nil || *llama.GatedMode != "manual" || *llama.License != "llama3.2" {
		t.Fatalf("gated model fields are wrong: %+v", llama)
	}
	unknown := result.Models[3]
	if unknown.License != nil || unknown.Gated != nil || unknown.GatedMode != nil || unknown.Downloads != nil || unknown.LastModified != nil || unknown.LibraryName != nil {
		t.Fatalf("missing upstream values must stay unknown: %+v", unknown)
	}
	if result.Models[0].PipelineTag != "feature-extraction" {
		t.Fatalf("embedding model pipeline = %q", result.Models[0].PipelineTag)
	}
	if hub.calls.Load() != 2 {
		t.Fatalf("expected one upstream call per pipeline, got %d", hub.calls.Load())
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for _, query := range hub.queries {
		if !strings.Contains(query, "search=phi") || !strings.Contains(query, "limit=10") {
			t.Fatalf("upstream query %q does not carry the normalized search and limit", query)
		}
	}
}

func TestSearchTruncatesToLimit(t *testing.T) {
	_, server := newHub(t)
	searcher := newSearcher(t, Options{Endpoint: server.URL})
	result, err := searcher.Search(context.Background(), "phi", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Models) != 2 || result.Models[0].Repository != "BAAI/bge-m3" || result.Models[1].Repository != "meta-llama/Llama-3.2-1B-Instruct" {
		t.Fatalf("unexpected truncated models %+v", result.Models)
	}
}

func TestSearchCachesSuccessesCaseInsensitivelyUntilExpiry(t *testing.T) {
	hub, server := newHub(t)
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC).UnixNano())
	searcher := newSearcher(t, Options{Endpoint: server.URL, TTL: time.Minute, Now: func() time.Time { return time.Unix(0, clock.Load()) }})

	first, err := searcher.Search(context.Background(), "phi", 5)
	if err != nil {
		t.Fatal(err)
	}
	second, err := searcher.Search(context.Background(), "PHI", 5)
	if err != nil {
		t.Fatal(err)
	}
	if hub.calls.Load() != 2 || !second.Cached || second.Query != "PHI" || len(second.Models) != len(first.Models) {
		t.Fatalf("second search was not served from cache: calls=%d cached=%v", hub.calls.Load(), second.Cached)
	}
	second.Models[0].Repository = "mutated/by-caller"
	third, err := searcher.Search(context.Background(), "phi", 5)
	if err != nil {
		t.Fatal(err)
	}
	if third.Models[0].Repository == "mutated/by-caller" {
		t.Fatal("a caller mutated the cached result")
	}
	if _, err = searcher.Search(context.Background(), "phi", 6); err != nil {
		t.Fatal(err)
	}
	if hub.calls.Load() != 4 {
		t.Fatalf("a different limit must not reuse the cached answer, calls=%d", hub.calls.Load())
	}
	clock.Add(int64(time.Minute))
	expired, err := searcher.Search(context.Background(), "phi", 5)
	if err != nil {
		t.Fatal(err)
	}
	if expired.Cached || hub.calls.Load() != 6 {
		t.Fatalf("expired entry was reused: cached=%v calls=%d", expired.Cached, hub.calls.Load())
	}
}

func TestSearchCacheIsBounded(t *testing.T) {
	_, server := newHub(t)
	searcher := newSearcher(t, Options{Endpoint: server.URL, MaxEntries: 3})
	for index := range 10 {
		if _, err := searcher.Search(context.Background(), fmt.Sprintf("query-%d", index), 5); err != nil {
			t.Fatal(err)
		}
	}
	searcher.mu.Lock()
	defer searcher.mu.Unlock()
	if len(searcher.cache) != 3 {
		t.Fatalf("cache holds %d entries, want 3", len(searcher.cache))
	}
	if _, found := searcher.cache["5\x00query-9"]; !found {
		t.Fatal("the newest entry was evicted")
	}
}

func TestSearchReportsUpstreamFailuresWithoutCachingThem(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
		reason string
	}{
		{name: "rate limited", status: http.StatusTooManyRequests, reason: "Hugging Face is rate limiting searches"},
		{name: "server error", status: http.StatusInternalServerError, reason: "Hugging Face returned HTTP 500"},
		{name: "redirect", status: http.StatusFound, reason: "Hugging Face returned HTTP 302"},
		{name: "malformed", body: `{"not":"a list"}`, reason: "Hugging Face returned an unreadable model list"},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub, server := newHub(t)
			hub.status = test.status
			if test.body != "" {
				hub.listings = map[string]string{"text-generation": test.body, "feature-extraction": test.body}
			}
			searcher := newSearcher(t, Options{Endpoint: server.URL})
			_, err := searcher.Search(context.Background(), "phi", 5)
			var unavailable *UnavailableError
			if !errors.As(err, &unavailable) || unavailable.Reason != test.reason || unavailable.Timeout {
				t.Fatalf("err = %#v, want reason %q", err, test.reason)
			}
			if strings.Contains(err.Error(), "must not leak") {
				t.Fatalf("upstream body leaked into the error: %v", err)
			}
			hub.status, hub.listings = 0, map[string]string{"text-generation": textGeneration}
			recovered, err := searcher.Search(context.Background(), "phi", 5)
			if err != nil || recovered.Cached || len(recovered.Models) == 0 {
				t.Fatalf("failure was cached: result=%+v err=%v", recovered, err)
			}
		})
	}
}

func TestSearchTimesOut(t *testing.T) {
	hub, server := newHub(t)
	hub.block = make(chan struct{})
	t.Cleanup(func() { close(hub.block) })
	searcher := newSearcher(t, Options{Endpoint: server.URL, Timeout: 50 * time.Millisecond})
	started := time.Now()
	_, err := searcher.Search(context.Background(), "phi", 5)
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || !unavailable.Timeout || !strings.Contains(unavailable.Reason, "did not answer within 50ms") {
		t.Fatalf("err = %#v, want a timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestSearchBoundsConcurrentUpstreamRequests(t *testing.T) {
	hub, server := newHub(t)
	hub.block = make(chan struct{})
	searcher := newSearcher(t, Options{Endpoint: server.URL, Concurrent: 1, Timeout: 2 * time.Second})

	firstDone := make(chan error, 1)
	go func() {
		_, err := searcher.Search(context.Background(), "first", 5)
		firstDone <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for hub.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := searcher.Search(ctx, "second", 5)
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || unavailable.Reason != "too many searches are in flight" {
		t.Fatalf("err = %#v, want the in-flight bound", err)
	}
	if hub.calls.Load() != 2 {
		t.Fatalf("second search reached Hugging Face while the slot was held: calls=%d", hub.calls.Load())
	}
	close(hub.block)
	if err = <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestSearchIsSafeForConcurrentCallers(t *testing.T) {
	_, server := newHub(t)
	searcher := newSearcher(t, Options{Endpoint: server.URL, MaxEntries: 4})
	var wait sync.WaitGroup
	for index := range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := searcher.Search(context.Background(), fmt.Sprintf("model-%d", index%6), 5); err != nil {
				t.Error(err)
			}
		}()
	}
	wait.Wait()
}

func TestSearchRejectsInvalidInput(t *testing.T) {
	_, server := newHub(t)
	searcher := newSearcher(t, Options{Endpoint: server.URL})
	for _, test := range []struct {
		query string
		limit int
	}{
		{query: "p", limit: 5},
		{query: "   ", limit: 5},
		{query: strings.Repeat("x", MaxQueryLength+1), limit: 5},
		{query: "phi\x00", limit: 5},
		{query: "ph\xffi", limit: 5},
		{query: "phi", limit: 0},
		{query: "phi", limit: MaxLimit + 1},
	} {
		_, err := searcher.Search(context.Background(), test.query, test.limit)
		var invalid *InvalidQueryError
		if !errors.As(err, &invalid) {
			t.Fatalf("Search(%q, %d) err = %v, want InvalidQueryError", test.query, test.limit, err)
		}
	}
}

func TestNewRejectsUnusableEndpoints(t *testing.T) {
	for _, endpoint := range []string{"huggingface.co", "ftp://huggingface.co", "https://", "https://huggingface.co/?mirror=1"} {
		if _, err := New(Options{Endpoint: endpoint}); err == nil {
			t.Fatalf("New accepted endpoint %q", endpoint)
		}
	}
	searcher := newSearcher(t, Options{})
	if searcher.Endpoint() != DefaultEndpoint {
		t.Fatalf("default endpoint = %q", searcher.Endpoint())
	}
}
