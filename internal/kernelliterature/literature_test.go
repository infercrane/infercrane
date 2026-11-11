package kernelliterature

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/infercrane/infercrane/internal/kernelplanner"
)

const atomFixture = `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom">
<entry><id>http://arxiv.org/abs/2610.36623v1</id><title> Batch
 Invariance </title><summary>Enforce bitwise consistency.</summary><published>2026-10-27T00:00:00Z</published></entry>
<entry><id>http://arxiv.org/abs/2501.00001v1</id><title>Old</title><summary>Too old.</summary><published>2025-01-01T00:00:00Z</published></entry>
<entry><id>http://evil.example/../../x</id><title>Bad id</title><summary>Ignored.</summary><published>2026-10-01T00:00:00Z</published></entry>
</feed>`

func TestArxivSearchIsOperatorScopedRecentAndDeduplicated(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query().Get("search_query"))
		_, _ = w.Write([]byte(atomFixture))
	}))
	defer server.Close()

	references, err := ArxivSearcher{BaseURL: server.URL}.Search(context.Background(), Query{Family: kernelplanner.ResidualRMSNorm, Since: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if len(references) != 1 || references[0].ID != "2610.36623v1" || references[0].Title != "Batch Invariance" || references[0].URL != "https://arxiv.org/abs/2610.36623v1" {
		t.Fatalf("references=%+v", references)
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "RMSNorm") || strings.Contains(queries[0], "Qwen") {
		t.Fatalf("queries=%v", queries)
	}
}

func TestOperatorSpecificResearchOutranksNewerGeneralPapers(t *testing.T) {
	specific := []Reference{{ID: "old-specific", Published: "2026-02-01T00:00:00Z"}}
	general := []Reference{{ID: "new-general", Published: "2026-11-01T00:00:00Z"}, {ID: "old-specific", Published: "2026-02-01T00:00:00Z"}}
	merged := mergeSpecificFirst(specific, general, Query{Limit: 5})
	if len(merged) != 2 || merged[0].ID != "old-specific" || merged[1].ID != "new-general" {
		t.Fatalf("merged=%+v", merged)
	}
	if bounded := mergeSpecificFirst(specific, general, Query{Limit: 1}); len(bounded) != 1 || bounded[0].ID != "old-specific" {
		t.Fatalf("bounded=%+v", bounded)
	}
}

func TestArxivSearchFailsOnUnknownFamilyOrServerError(t *testing.T) {
	if _, err := (ArxivSearcher{}).Search(context.Background(), Query{Family: "unreviewed"}); err == nil {
		t.Fatal("unknown family accepted")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer server.Close()
	if _, err := (ArxivSearcher{BaseURL: server.URL}).Search(context.Background(), Query{Family: kernelplanner.Sampling}); err == nil {
		t.Fatal("server error accepted")
	}
}

func TestFileSearcherRequiresHTTPSAndCleansText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refs.json")
	if err := os.WriteFile(path, []byte(`[{"source":"vendor","id":"wp-1","title":"White\u0000paper","summary":"Notes","url":"https://example.com/wp","published":"2026-09-01T00:00:00Z"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	references, err := FileSearcher{Path: path}.Search(context.Background(), Query{Family: kernelplanner.Sampling})
	if err != nil || len(references) != 1 || references[0].Title != "Whitepaper" {
		t.Fatalf("references=%+v err=%v", references, err)
	}
	if err = os.WriteFile(path, []byte(`[{"id":"x","url":"http://example.com/x"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = (FileSearcher{Path: path}).Search(context.Background(), Query{Family: kernelplanner.Sampling}); err == nil {
		t.Fatal("non-https reference accepted")
	}
}

func TestCleanTruncatesLongUntrustedText(t *testing.T) {
	if got := clean(strings.Repeat("a", maxSummaryRunes+50), maxSummaryRunes); len(got) != maxSummaryRunes {
		t.Fatalf("len=%d", len(got))
	}
}
