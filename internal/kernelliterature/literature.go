// Package kernelliterature finds recent public research relevant to a measured
// GPU operator hotspot. References are untrusted reading material: they can
// inform a kernel draft, but they never select, qualify, or promote a candidate.
package kernelliterature

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/infercrane/infercrane/internal/kernelplanner"
)

const (
	defaultArxivURL  = "https://export.arxiv.org/api/query"
	maxResponseBytes = 8 << 20
	maxFileBytes     = 2 << 20
	maxTitleRunes    = 200
	maxSummaryRunes  = 1000
	defaultLimit     = 8
)

// Reference is one public document. Summary is untrusted text.
type Reference struct {
	Source    string `json:"source"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	URL       string `json:"url"`
	Published string `json:"published"`
}

type Query struct {
	Family kernelplanner.OperatorFamily
	Since  time.Time
	Limit  int
}

type Searcher interface {
	Search(context.Context, Query) ([]Reference, error)
}

// familyTerms is reviewed search vocabulary keyed by operator, never by model.
var familyTerms = map[kernelplanner.OperatorFamily][]string{
	kernelplanner.ResidualRMSNorm:  {"RMSNorm", "normalization"},
	kernelplanner.QuantizedLinear:  {"GEMM", "quantized"},
	kernelplanner.SwiGLU:           {"SwiGLU", "activation"},
	kernelplanner.AttentionPrefill: {"attention", "prefill"},
	kernelplanner.AttentionDecode:  {"attention", "decode"},
	kernelplanner.KVCache:          {"KV cache", "key-value cache"},
	kernelplanner.LinearRecurrence: {"state space", "linear attention"},
	kernelplanner.StateCommit:      {"state space", "state update"},
	kernelplanner.MoERouting:       {"mixture of experts", "MoE"},
	kernelplanner.Sampling:         {"sampling", "speculative"},
}

var arxivID = regexp.MustCompile(`^[0-9]{4}\.[0-9]{4,6}(v[0-9]+)?$`)

// ArxivSearcher queries the public arXiv API.
type ArxivSearcher struct {
	BaseURL string
	Client  *http.Client
	// Delay separates requests, as the arXiv API terms ask of automated clients.
	Delay time.Duration
}

func (s ArxivSearcher) Search(ctx context.Context, query Query) ([]Reference, error) {
	terms, ok := familyTerms[query.Family]
	if !ok {
		return nil, fmt.Errorf("no reviewed search terms for operator family %q", query.Family)
	}
	quoted := make([]string, 0, len(terms))
	for _, term := range terms {
		quoted = append(quoted, `abs:"`+term+`"`)
	}
	specific, err := s.fetch(ctx, `abs:"GPU kernel" AND (`+strings.Join(quoted, " OR ")+`)`)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.Delay):
	}
	general, err := s.fetch(ctx, `abs:"GPU kernel" AND (abs:LLM OR abs:agent)`)
	if err != nil {
		return nil, err
	}
	return mergeSpecificFirst(specific, general, query), nil
}

// mergeSpecificFirst gives research about the measured operator priority, then
// fills remaining slots with general kernel-generation methods.
func mergeSpecificFirst(specific, general []Reference, query Query) []Reference {
	limit := query.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	selected := selectRecent(specific, query)
	present := make(map[string]struct{}, len(selected))
	for _, reference := range selected {
		present[reference.ID] = struct{}{}
	}
	for _, reference := range selectRecent(general, query) {
		if _, duplicate := present[reference.ID]; !duplicate && len(selected) < limit {
			selected = append(selected, reference)
		}
	}
	return selected
}

func (s ArxivSearcher) fetch(ctx context.Context, search string) ([]Reference, error) {
	base := s.BaseURL
	if base == "" {
		base = defaultArxivURL
	}
	parameters := url.Values{"search_query": {search}, "sortBy": {"submittedDate"}, "sortOrder": {"descending"}, "max_results": {"25"}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+parameters.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "infercrane-kernel-adapter")
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("search arXiv: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search arXiv: unexpected status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read arXiv response: %w", err)
	}
	return parseAtom(body)
}

func parseAtom(body []byte) ([]Reference, error) {
	var feed struct {
		Entries []struct {
			ID        string `xml:"id"`
			Title     string `xml:"title"`
			Summary   string `xml:"summary"`
			Published string `xml:"published"`
		} `xml:"entry"`
	}
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("decode arXiv response: %w", err)
	}
	references := make([]Reference, 0, len(feed.Entries))
	for _, entry := range feed.Entries {
		id := entry.ID[strings.LastIndex(entry.ID, "/")+1:]
		if !arxivID.MatchString(id) {
			continue
		}
		references = append(references, Reference{
			Source: "arxiv", ID: id, Title: clean(entry.Title, maxTitleRunes), Summary: clean(entry.Summary, maxSummaryRunes),
			URL: "https://arxiv.org/abs/" + id, Published: entry.Published,
		})
	}
	return references, nil
}

// FileSearcher serves an operator-curated list, such as vendor whitepapers or
// the only sources a customer's data boundary allows.
type FileSearcher struct{ Path string }

func (s FileSearcher) Search(_ context.Context, query Query) ([]Reference, error) {
	info, err := os.Stat(s.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect literature file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileBytes {
		return nil, errors.New("literature file must be a regular file within the size limit")
	}
	body, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read literature file: %w", err)
	}
	var entries []Reference
	if err = json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("decode literature file: %w", err)
	}
	references := make([]Reference, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasPrefix(entry.URL, "https://") || strings.TrimSpace(entry.ID) == "" {
			return nil, errors.New("literature entries require an id and an https url")
		}
		entry.Source, entry.ID = clean(entry.Source, maxTitleRunes), clean(entry.ID, maxTitleRunes)
		entry.Title, entry.Summary = clean(entry.Title, maxTitleRunes), clean(entry.Summary, maxSummaryRunes)
		references = append(references, entry)
	}
	return selectRecent(references, query), nil
}

func selectRecent(references []Reference, query Query) []Reference {
	limit := query.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	seen := map[string]struct{}{}
	selected := make([]Reference, 0, limit)
	sort.SliceStable(references, func(i, j int) bool {
		if references[i].Published != references[j].Published {
			return references[i].Published > references[j].Published
		}
		return references[i].ID < references[j].ID
	})
	for _, reference := range references {
		if _, duplicate := seen[reference.ID]; duplicate || len(selected) == limit {
			continue
		}
		if published, err := time.Parse(time.RFC3339, reference.Published); err == nil && !query.Since.IsZero() && published.Before(query.Since) {
			continue
		}
		seen[reference.ID] = struct{}{}
		selected = append(selected, reference)
	}
	return selected
}

func clean(value string, limit int) string {
	value = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)), " ")
	runes := []rune(value)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return value
}
