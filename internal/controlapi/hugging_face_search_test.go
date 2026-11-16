package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/huggingfacesearch"
)

func huggingFaceSearchAPI(t *testing.T, handler http.HandlerFunc, timeout time.Duration) (http.Handler, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	searcher, err := huggingfacesearch.New(huggingfacesearch.Options{Endpoint: server.URL, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	// Store is nil on purpose: the search must not read tenant state or
	// PostgreSQL, so any store call would panic and fail the test.
	return (API{APIKey: "secret", HuggingFaceSearch: searcher}).Handler(), &calls
}

func searchRequest(handler http.Handler, target string, authenticated bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if authenticated {
		request.Header.Set("Authorization", "Bearer secret")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestHuggingFaceSearchReturnsUnreviewedLiveResults(t *testing.T) {
	handler, calls := huggingFaceSearchAPI(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("pipeline_tag") {
		case "text-generation":
			_, _ = w.Write([]byte(`[{"id":"microsoft/Phi-3.5-mini-instruct","gated":false,"lastModified":"2025-12-10T20:22:56.000Z","downloads":340333,"tags":["license:mit"],"pipeline_tag":"text-generation","library_name":"transformers"},{"id":"someone/phi-without-metadata","pipeline_tag":"text-generation"}]`))
		case "feature-extraction":
			_, _ = w.Write([]byte(`[{"id":"someone/phi-embed","gated":"auto","downloads":12,"tags":["license:apache-2.0"],"pipeline_tag":"feature-extraction"}]`))
		default:
			t.Errorf("unexpected pipeline %q", r.URL.Query().Get("pipeline_tag"))
		}
	}, time.Second)

	if response := searchRequest(handler, "/api/v1/catalog/hugging-face/search?query=phi", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("an unauthenticated request reached Hugging Face")
	}

	response := searchRequest(handler, "/api/v1/catalog/hugging-face/search?query=phi&limit=5", true)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	var body struct {
		SchemaVersion     string   `json:"schema_version"`
		State             string   `json:"state"`
		Query             string   `json:"query"`
		Limit             int      `json:"limit"`
		FetchedAt         string   `json:"fetched_at"`
		Cached            bool     `json:"cached"`
		Pipelines         []string `json:"pipelines"`
		Reviewed          bool     `json:"reviewed"`
		PerformanceClaims bool     `json:"performance_claims"`
		Limitations       []string `json:"limitations"`
		Models            []struct {
			Repository   string  `json:"repository"`
			PipelineTag  string  `json:"pipeline_tag"`
			License      *string `json:"license"`
			Gated        *bool   `json:"gated"`
			GatedMode    *string `json:"gated_mode"`
			Downloads    *int64  `json:"downloads"`
			LastModified *string `json:"last_modified"`
			Reviewed     bool    `json:"reviewed"`
			SourceKind   string  `json:"source_kind"`
		} `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.SchemaVersion != "infercrane.hugging-face-search/v1" || body.State != "live" || body.Query != "phi" || body.Limit != 5 || body.Cached || body.Reviewed || body.PerformanceClaims || len(body.Limitations) == 0 {
		t.Fatalf("unexpected response envelope %s", response.Body.String())
	}
	if _, err := time.Parse(time.RFC3339, body.FetchedAt); err != nil {
		t.Fatalf("fetched_at %q: %v", body.FetchedAt, err)
	}
	if strings.Join(body.Pipelines, ",") != "text-generation,feature-extraction" {
		t.Fatalf("pipelines = %v", body.Pipelines)
	}
	if len(body.Models) != 3 {
		t.Fatalf("models = %s", response.Body.String())
	}
	phi, embed, bare := body.Models[0], body.Models[1], body.Models[2]
	if phi.Repository != "microsoft/Phi-3.5-mini-instruct" || *phi.License != "mit" || *phi.Gated || *phi.Downloads != 340333 || *phi.LastModified != "2025-12-10T20:22:56.000Z" || phi.Reviewed || phi.SourceKind != "hugging-face-search" {
		t.Fatalf("phi = %+v", phi)
	}
	if embed.Repository != "someone/phi-embed" || embed.PipelineTag != "feature-extraction" || !*embed.Gated || *embed.GatedMode != "auto" {
		t.Fatalf("embedding = %+v", embed)
	}
	if bare.License != nil || bare.Gated != nil || bare.Downloads != nil || bare.LastModified != nil {
		t.Fatalf("unknown values were filled in: %+v", bare)
	}
	for _, unknown := range []string{`"license":null`, `"gated":null`, `"downloads":null`, `"last_modified":null`} {
		if !strings.Contains(response.Body.String(), unknown) {
			t.Fatalf("response does not report %s for the bare model: %s", unknown, response.Body.String())
		}
	}

	cached := searchRequest(handler, "/api/v1/catalog/hugging-face/search?query=PHI&limit=5", true)
	if cached.Code != http.StatusOK || !strings.Contains(cached.Body.String(), `"cached":true`) || !strings.Contains(cached.Body.String(), `"query":"PHI"`) || calls.Load() != 2 {
		t.Fatalf("repeat search was not cached: calls=%d body=%s", calls.Load(), cached.Body.String())
	}
}

func TestHuggingFaceSearchRejectsInvalidRequestsBeforeCallingUpstream(t *testing.T) {
	handler, calls := huggingFaceSearchAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}, time.Second)
	for _, target := range []string{
		"/api/v1/catalog/hugging-face/search",
		"/api/v1/catalog/hugging-face/search?query=p",
		"/api/v1/catalog/hugging-face/search?query=" + strings.Repeat("x", 101),
		"/api/v1/catalog/hugging-face/search?query=phi&limit=0",
		"/api/v1/catalog/hugging-face/search?query=phi&limit=51",
		"/api/v1/catalog/hugging-face/search?query=phi&limit=ten",
		"/api/v1/catalog/hugging-face/search?query=phi&pipeline_tag=text-to-image",
		"/api/v1/catalog/hugging-face/search?query=phi&query=llama",
	} {
		response := searchRequest(handler, target, true)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"code":"invalid_query"`) {
			t.Fatalf("%s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid requests reached Hugging Face %d times", calls.Load())
	}
}

func TestHuggingFaceSearchReportsUnavailableUpstream(t *testing.T) {
	failing, _ := huggingFaceSearchAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}, time.Second)
	response := searchRequest(failing, "/api/v1/catalog/hugging-face/search?query=phi", true)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), `"code":"hugging_face_search_unavailable"`) || !strings.Contains(response.Body.String(), "Hugging Face returned HTTP 503") || !strings.Contains(response.Body.String(), `"retryable":true`) {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	slow, _ := huggingFaceSearchAPI(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}, 50*time.Millisecond)
	response = searchRequest(slow, "/api/v1/catalog/hugging-face/search?query=phi", true)
	if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), "did not answer within 50ms") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	disabled := (API{APIKey: "secret"}).Handler()
	response = searchRequest(disabled, "/api/v1/catalog/hugging-face/search?query=phi", true)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "not enabled on this control plane") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
