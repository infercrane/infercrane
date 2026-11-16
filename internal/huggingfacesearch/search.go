// Package huggingfacesearch reads the public Hugging Face models API for
// model discovery. It never authenticates, never reaches the inference request
// path, and never reads or writes PostgreSQL. Results are reported exactly as
// Hugging Face returns them and are never reviewed InferCrane evidence.
package huggingfacesearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// DefaultEndpoint is the public Hugging Face Hub.
	DefaultEndpoint = "https://huggingface.co"
	// SchemaVersion identifies the search response contract.
	SchemaVersion = "infercrane.hugging-face-search/v1"

	DefaultLimit   = 20
	MaxLimit       = 50
	MinQueryLength = 2
	MaxQueryLength = 100

	defaultTimeout    = 4 * time.Second
	defaultTTL        = 10 * time.Minute
	defaultMaxEntries = 256
	defaultConcurrent = 8
	maxResponseBytes  = 2 << 20
	userAgent         = "infercrane-model-search"
)

// Pipelines are the Hugging Face pipeline tags the search covers, in the
// order they are queried.
var Pipelines = []string{"text-generation", "feature-extraction"}

var expandFields = []string{"downloads", "gated", "lastModified", "library_name", "pipeline_tag", "tags"}

// InvalidQueryError reports a query or limit outside the accepted bounds.
// Reason is safe to show to the caller.
type InvalidQueryError struct{ Reason string }

func (e *InvalidQueryError) Error() string { return "invalid hugging face search: " + e.Reason }

// UnavailableError reports that Hugging Face could not answer in time or
// returned something other than a usable model list. Reason is safe to show
// to the caller and never contains upstream response bodies.
type UnavailableError struct {
	Reason  string
	Timeout bool
}

func (e *UnavailableError) Error() string { return "hugging face search unavailable: " + e.Reason }

// Model is one search result. Pointer fields are nil when Hugging Face did
// not report the value; they are never filled with a guess.
type Model struct {
	Repository   string  `json:"repository"`
	PipelineTag  string  `json:"pipeline_tag"`
	License      *string `json:"license"`
	Gated        *bool   `json:"gated"`
	GatedMode    *string `json:"gated_mode"`
	Downloads    *int64  `json:"downloads"`
	LastModified *string `json:"last_modified"`
	LibraryName  *string `json:"library_name"`
	Reviewed     bool    `json:"reviewed"`
	SourceKind   string  `json:"source_kind"`
	SourceURL    string  `json:"source_url"`
}

// Result is one complete answer for a query.
type Result struct {
	Query     string    `json:"query"`
	Limit     int       `json:"limit"`
	Source    string    `json:"source"`
	FetchedAt time.Time `json:"fetched_at"`
	Cached    bool      `json:"cached"`
	Models    []Model   `json:"models"`
}

// Searcher queries Hugging Face with a short timeout, a small in-memory
// cache, and a bound on concurrent upstream requests. The zero value is not
// usable; construct one with New.
type Searcher struct {
	endpoint   string
	client     *http.Client
	timeout    time.Duration
	ttl        time.Duration
	maxEntries int
	now        func() time.Time
	slots      chan struct{}

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	result  Result
	expires time.Time
}

// Options configures a Searcher. Zero values select the defaults.
type Options struct {
	Endpoint   string
	Client     *http.Client
	Timeout    time.Duration
	TTL        time.Duration
	MaxEntries int
	Concurrent int
	Now        func() time.Time
}

// New returns a Searcher. The endpoint must be an absolute http(s) URL.
func New(options Options) (*Searcher, error) {
	endpoint := strings.TrimRight(options.Endpoint, "/")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("hugging face search endpoint must be an absolute http(s) URL without a query, got %q", options.Endpoint)
	}
	client := &http.Client{}
	if options.Client != nil {
		copied := *options.Client
		client = &copied
	}
	// The listing API answers directly. Following a redirect would let a
	// misconfigured mirror send the control plane somewhere else.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	searcher := &Searcher{
		endpoint:   endpoint,
		client:     client,
		timeout:    options.Timeout,
		ttl:        options.TTL,
		maxEntries: options.MaxEntries,
		now:        options.Now,
		cache:      map[string]cacheEntry{},
	}
	if searcher.timeout <= 0 {
		searcher.timeout = defaultTimeout
	}
	if searcher.ttl <= 0 {
		searcher.ttl = defaultTTL
	}
	if searcher.maxEntries <= 0 {
		searcher.maxEntries = defaultMaxEntries
	}
	if searcher.now == nil {
		searcher.now = time.Now
	}
	concurrent := options.Concurrent
	if concurrent <= 0 {
		concurrent = defaultConcurrent
	}
	searcher.slots = make(chan struct{}, concurrent)
	return searcher, nil
}

// Endpoint returns the Hugging Face base URL this searcher reads.
func (s *Searcher) Endpoint() string { return s.endpoint }

// NormalizeQuery trims a query and checks its bounds.
func NormalizeQuery(query string) (string, error) {
	query = strings.Join(strings.Fields(query), " ")
	if !utf8.ValidString(query) {
		return "", &InvalidQueryError{Reason: "query must be valid UTF-8"}
	}
	length := utf8.RuneCountInString(query)
	if length < MinQueryLength || length > MaxQueryLength {
		return "", &InvalidQueryError{Reason: fmt.Sprintf("query must be %d to %d characters", MinQueryLength, MaxQueryLength)}
	}
	for _, r := range query {
		if unicode.IsControl(r) {
			return "", &InvalidQueryError{Reason: "query must not contain control characters"}
		}
	}
	return query, nil
}

// Search returns public models matching query across Pipelines, most
// downloaded first. Results for the same query and limit are reused until the
// cache entry expires. Failures are never cached.
func (s *Searcher) Search(ctx context.Context, query string, limit int) (Result, error) {
	query, err := NormalizeQuery(query)
	if err != nil {
		return Result{}, err
	}
	if limit < 1 || limit > MaxLimit {
		return Result{}, &InvalidQueryError{Reason: fmt.Sprintf("limit must be 1 to %d", MaxLimit)}
	}
	key := fmt.Sprintf("%d\x00%s", limit, strings.ToLower(query))
	if cached, found := s.cached(key); found {
		cached.Query, cached.Cached = query, true
		return cached, nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return Result{}, &UnavailableError{Reason: "too many searches are in flight", Timeout: true}
	}

	type answer struct {
		models []Model
		err    error
	}
	answers := make([]answer, len(Pipelines))
	var wait sync.WaitGroup
	for index, pipeline := range Pipelines {
		wait.Add(1)
		go func() {
			defer wait.Done()
			models, fetchErr := s.fetch(ctx, query, pipeline, limit)
			answers[index] = answer{models: models, err: fetchErr}
		}()
	}
	wait.Wait()

	seen := map[string]struct{}{}
	models := make([]Model, 0, limit*len(Pipelines))
	for _, item := range answers {
		if item.err != nil {
			return Result{}, item.err
		}
		for _, model := range item.models {
			folded := strings.ToLower(model.Repository)
			if _, duplicate := seen[folded]; duplicate {
				continue
			}
			seen[folded] = struct{}{}
			models = append(models, model)
		}
	}
	sort.SliceStable(models, func(i, j int) bool {
		left, right := models[i].Downloads, models[j].Downloads
		if (left == nil) != (right == nil) {
			return left != nil
		}
		if left != nil && *left != *right {
			return *left > *right
		}
		return strings.ToLower(models[i].Repository) < strings.ToLower(models[j].Repository)
	})
	if len(models) > limit {
		models = models[:limit]
	}
	result := Result{Query: query, Limit: limit, Source: s.endpoint, FetchedAt: s.now().UTC(), Models: models}
	s.store(key, result)
	return result, nil
}

func (s *Searcher) cached(key string) (Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.cache[key]
	if !found {
		return Result{}, false
	}
	if !s.now().Before(entry.expires) {
		delete(s.cache, key)
		return Result{}, false
	}
	result := entry.result
	result.Models = append([]Model(nil), entry.result.Models...)
	return result, true
}

func (s *Searcher) store(key string, result Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if _, replacing := s.cache[key]; !replacing && len(s.cache) >= s.maxEntries {
		oldestKey, oldest := "", time.Time{}
		for candidate, entry := range s.cache {
			if !now.Before(entry.expires) {
				delete(s.cache, candidate)
				continue
			}
			if oldestKey == "" || entry.expires.Before(oldest) {
				oldestKey, oldest = candidate, entry.expires
			}
		}
		if len(s.cache) >= s.maxEntries && oldestKey != "" {
			delete(s.cache, oldestKey)
		}
	}
	stored := result
	stored.Models = append([]Model(nil), result.Models...)
	s.cache[key] = cacheEntry{result: stored, expires: now.Add(s.ttl)}
}

// upstreamModel is the subset of the Hugging Face listing this package reads.
type upstreamModel struct {
	ID           string          `json:"id"`
	PipelineTag  *string         `json:"pipeline_tag"`
	Tags         []string        `json:"tags"`
	Gated        json.RawMessage `json:"gated"`
	Downloads    *int64          `json:"downloads"`
	LastModified *string         `json:"lastModified"`
	LibraryName  *string         `json:"library_name"`
	Private      bool            `json:"private"`
	Disabled     bool            `json:"disabled"`
}

func (s *Searcher) fetch(ctx context.Context, query, pipeline string, limit int) ([]Model, error) {
	parameters := url.Values{}
	parameters.Set("search", query)
	parameters.Set("pipeline_tag", pipeline)
	parameters.Set("sort", "downloads")
	parameters.Set("direction", "-1")
	parameters.Set("limit", fmt.Sprint(limit))
	for _, field := range expandFields {
		parameters.Add("expand[]", field)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint+"/api/models?"+parameters.Encode(), nil)
	if err != nil {
		return nil, &UnavailableError{Reason: "the search request could not be built"}
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", userAgent)
	response, err := s.client.Do(request)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &UnavailableError{Reason: fmt.Sprintf("Hugging Face did not answer within %s", s.timeout), Timeout: true}
		}
		return nil, &UnavailableError{Reason: "could not reach Hugging Face"}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, &UnavailableError{Reason: "Hugging Face is rate limiting searches"}
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, &UnavailableError{Reason: fmt.Sprintf("Hugging Face returned HTTP %d", response.StatusCode)}
	}
	var listing []upstreamModel
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes))
	if err = decoder.Decode(&listing); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, &UnavailableError{Reason: fmt.Sprintf("Hugging Face did not answer within %s", s.timeout), Timeout: true}
		}
		return nil, &UnavailableError{Reason: "Hugging Face returned an unreadable model list"}
	}
	models := make([]Model, 0, len(listing))
	for _, item := range listing {
		if item.Private || item.Disabled || !validRepository(item.ID) {
			continue
		}
		// The API filtered on pipeline_tag. A row that reports a different
		// pipeline is skipped rather than relabeled.
		if item.PipelineTag != nil && *item.PipelineTag != pipeline {
			continue
		}
		gated, mode := gatedState(item.Gated)
		models = append(models, Model{
			Repository:   item.ID,
			PipelineTag:  pipeline,
			License:      licenseTag(item.Tags),
			Gated:        gated,
			GatedMode:    mode,
			Downloads:    item.Downloads,
			LastModified: item.LastModified,
			LibraryName:  item.LibraryName,
			Reviewed:     false,
			SourceKind:   "hugging-face-search",
			SourceURL:    s.endpoint + "/" + item.ID,
		})
		if len(models) == limit {
			break
		}
	}
	return models, nil
}

// gatedState reads Hugging Face's gated field, which is false or the gating
// mode ("auto" or "manual"). A missing or unrecognized value stays unknown.
func gatedState(raw json.RawMessage) (*bool, *string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var flag bool
	if err := json.Unmarshal(raw, &flag); err == nil {
		return &flag, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil && mode != "" {
		gated := true
		return &gated, &mode
	}
	return nil, nil
}

// licenseTag returns the value of the first license:<id> tag, or nil when
// Hugging Face lists none.
func licenseTag(tags []string) *string {
	for _, tag := range tags {
		if value, found := strings.CutPrefix(tag, "license:"); found && value != "" {
			return &value
		}
	}
	return nil
}

// validRepository accepts the organization/name form Hugging Face uses for
// model repositories, so a result can be passed to /build?model= unchanged.
func validRepository(id string) bool {
	owner, name, found := strings.Cut(id, "/")
	if !found || owner == "" || name == "" || strings.Contains(name, "/") || len(id) > 200 {
		return false
	}
	for _, r := range id {
		if !(r == '/' || r == '-' || r == '_' || r == '.' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
