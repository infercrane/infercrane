// Package hfcatalog ingests bounded, normalized metadata from the official
// Hugging Face Hub API. It is intentionally separate from curatedrecipe:
// upstream metadata is discovery evidence, never a deployment recipe,
// compatibility qualification, performance claim, or availability signal.
package hfcatalog

import (
	"bytes"
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
)

const (
	SchemaVersion       = "infercrane.hugging-face-catalog/v1"
	defaultBaseURL      = "https://huggingface.co"
	defaultValidFor     = 24 * time.Hour
	defaultHTTPTimeout  = 10 * time.Second
	maxRepositories     = 64
	maxResponseBytes    = 1 << 20
	maxTags             = 64
	maxLanguages        = 32
	maxMetadataTextSize = 256
)

var expandedFields = []string{"author", "cardData", "downloads", "gated", "lastModified", "library_name", "likes", "pipeline_tag", "private", "sha", "tags"}

type Provenance struct {
	Provider    string    `json:"provider"`
	Endpoint    string    `json:"endpoint"`
	RetrievedAt time.Time `json:"retrieved_at"`
	ValidUntil  time.Time `json:"valid_until"`
}

// Model contains only normalized, bounded metadata needed for catalog
// discovery. Pointer fields preserve the distinction between zero/false and
// metadata the upstream response did not provide.
type Model struct {
	Repository        string     `json:"repository"`
	Author            *string    `json:"author,omitempty"`
	Revision          *string    `json:"revision,omitempty"`
	PipelineTag       *string    `json:"pipeline_tag,omitempty"`
	LibraryName       *string    `json:"library_name,omitempty"`
	Private           *bool      `json:"private,omitempty"`
	Access            string     `json:"access"`
	Downloads         *int64     `json:"downloads,omitempty"`
	Likes             *int64     `json:"likes,omitempty"`
	LastModified      *time.Time `json:"last_modified,omitempty"`
	License           *string    `json:"license,omitempty"`
	Languages         []string   `json:"languages,omitempty"`
	Tags              []string   `json:"tags,omitempty"`
	UnknownFields     []string   `json:"unknown_fields,omitempty"`
	MetadataTruncated bool       `json:"metadata_truncated"`
	Current           bool       `json:"current"`
	Provenance        Provenance `json:"provenance"`
}

type Snapshot struct {
	SchemaVersion       string     `json:"schema_version"`
	State               string     `json:"state"`
	Models              []Model    `json:"models"`
	ConfiguredCount     int        `json:"configured_count"`
	LastRefreshAttempt  *time.Time `json:"last_refresh_attempt,omitempty"`
	LastSuccessfulFetch *time.Time `json:"last_successful_fetch,omitempty"`
	Limitations         []string   `json:"limitations"`
}

// Cache publishes an atomic snapshot. A failed fetch retains the last good
// record for that repository rather than replacing it with missing data.
type Cache struct {
	mu           sync.RWMutex
	repositories []string
	models       map[string]Model
	lastAttempt  time.Time
	lastSuccess  time.Time
	lastPartial  bool
}

func New(repositories []string) (*Cache, error) {
	if len(repositories) == 0 {
		return nil, errors.New("Hugging Face catalog requires at least one repository")
	}
	if len(repositories) > maxRepositories {
		return nil, fmt.Errorf("Hugging Face catalog supports at most %d repositories", maxRepositories)
	}
	seen := make(map[string]struct{}, len(repositories))
	normalized := make([]string, 0, len(repositories))
	for _, repository := range repositories {
		repository = strings.TrimSpace(repository)
		if !validRepository(repository) {
			return nil, fmt.Errorf("invalid Hugging Face repository %q", repository)
		}
		if _, exists := seen[strings.ToLower(repository)]; exists {
			continue
		}
		seen[strings.ToLower(repository)] = struct{}{}
		normalized = append(normalized, repository)
	}
	sort.Slice(normalized, func(i, j int) bool { return strings.ToLower(normalized[i]) < strings.ToLower(normalized[j]) })
	return &Cache{repositories: normalized, models: make(map[string]Model)}, nil
}

func validRepository(repository string) bool {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || len(repository) > 192 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || len(part) > 96 {
			return false
		}
		for _, char := range part {
			if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
				continue
			}
			return false
		}
	}
	return true
}

type Client struct {
	BaseURL, Token string
	HTTPClient     *http.Client
	ValidFor       time.Duration
	Now            func() time.Time
}

func (c Client) Fetch(ctx context.Context, repository string) (Model, error) {
	if !validRepository(repository) {
		return Model{}, errors.New("invalid Hugging Face repository")
	}
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = defaultBaseURL
	}
	parsedBase, err := url.Parse(base)
	if err != nil || (parsedBase.Scheme != "http" && parsedBase.Scheme != "https") || parsedBase.Host == "" || parsedBase.User != nil || parsedBase.RawQuery != "" || parsedBase.Fragment != "" {
		return Model{}, errors.New("invalid Hugging Face API base URL")
	}
	if strings.TrimSpace(c.Token) != "" && (parsedBase.Scheme != "https" || !strings.EqualFold(parsedBase.Hostname(), "huggingface.co") || parsedBase.Port() != "" && parsedBase.Port() != "443") {
		return Model{}, errors.New("refusing to send Hugging Face credentials to an untrusted endpoint")
	}
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPTimeout)
	defer cancel()
	parts := strings.Split(repository, "/")
	endpoint := base + "/api/models/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
	parsedEndpoint, _ := url.Parse(endpoint)
	query := parsedEndpoint.Query()
	for _, field := range expandedFields {
		query.Add("expand", field)
	}
	parsedEndpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedEndpoint.String(), nil)
	if err != nil {
		return Model{}, fmt.Errorf("create Hugging Face metadata request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "infercrane-hf-catalog/1")
	if strings.TrimSpace(c.Token) != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	// Never forward a bearer token across a redirect. The official model-info
	// endpoint is canonical and should answer directly.
	boundedClient := *client
	boundedClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := boundedClient.Do(request)
	if err != nil {
		return Model{}, errors.New("Hugging Face metadata request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Model{}, fmt.Errorf("Hugging Face metadata returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return Model{}, errors.New("read Hugging Face metadata response")
	}
	if len(body) > maxResponseBytes {
		return Model{}, errors.New("Hugging Face metadata response exceeded 1 MiB")
	}
	return c.normalize(repository, parsedEndpoint.String(), body)
}

func (c Client) normalize(repository, endpoint string, body []byte) (Model, error) {
	var upstream struct {
		ID           string          `json:"id"`
		ModelID      string          `json:"modelId"`
		Author       *string         `json:"author"`
		SHA          *string         `json:"sha"`
		PipelineTag  *string         `json:"pipeline_tag"`
		LibraryName  *string         `json:"library_name"`
		Private      *bool           `json:"private"`
		Gated        json.RawMessage `json:"gated"`
		Downloads    *int64          `json:"downloads"`
		Likes        *int64          `json:"likes"`
		LastModified *string         `json:"lastModified"`
		Tags         []string        `json:"tags"`
		CardData     json.RawMessage `json:"cardData"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&upstream); err != nil {
		return Model{}, errors.New("decode Hugging Face metadata response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Model{}, errors.New("Hugging Face metadata response must contain exactly one JSON object")
	}
	identity := upstream.ID
	if identity == "" {
		identity = upstream.ModelID
	}
	if !strings.EqualFold(strings.TrimSpace(identity), repository) {
		return Model{}, errors.New("Hugging Face metadata identity did not match the requested repository")
	}
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	validFor := c.ValidFor
	if validFor <= 0 {
		validFor = defaultValidFor
	}
	model := Model{
		Repository:    repository,
		Author:        boundedString(upstream.Author),
		PipelineTag:   boundedString(upstream.PipelineTag),
		LibraryName:   boundedString(upstream.LibraryName),
		Private:       upstream.Private,
		Access:        normalizeAccess(upstream.Private, upstream.Gated),
		Downloads:     nonnegative(upstream.Downloads),
		Likes:         nonnegative(upstream.Likes),
		Provenance:    Provenance{Provider: "huggingface_hub_api", Endpoint: endpoint, RetrievedAt: now, ValidUntil: now.Add(validFor)},
		UnknownFields: make([]string, 0, 8),
	}
	for _, value := range []*string{upstream.Author, upstream.PipelineTag, upstream.LibraryName} {
		if value != nil && len(strings.TrimSpace(*value)) > maxMetadataTextSize {
			model.MetadataTruncated = true
		}
	}
	if upstream.SHA != nil && validCommit(*upstream.SHA) {
		value := strings.ToLower(strings.TrimSpace(*upstream.SHA))
		model.Revision = &value
	} else {
		model.UnknownFields = append(model.UnknownFields, "revision")
	}
	if upstream.LastModified != nil {
		if parsed, err := time.Parse(time.RFC3339, *upstream.LastModified); err == nil {
			parsed = parsed.UTC()
			model.LastModified = &parsed
		} else {
			model.UnknownFields = append(model.UnknownFields, "last_modified")
		}
	} else {
		model.UnknownFields = append(model.UnknownFields, "last_modified")
	}
	var tagsTruncated bool
	model.Tags, tagsTruncated = boundedStrings(upstream.Tags, maxTags)
	model.MetadataTruncated = model.MetadataTruncated || tagsTruncated
	license, languages, cardUnknown, truncated := normalizeCardData(upstream.CardData)
	model.License, model.Languages = license, languages
	model.MetadataTruncated = model.MetadataTruncated || truncated
	model.UnknownFields = append(model.UnknownFields, cardUnknown...)
	if model.Author == nil {
		model.UnknownFields = append(model.UnknownFields, "author")
	}
	if model.PipelineTag == nil {
		model.UnknownFields = append(model.UnknownFields, "pipeline_tag")
	}
	if model.LibraryName == nil {
		model.UnknownFields = append(model.UnknownFields, "library_name")
	}
	if model.Private == nil {
		model.UnknownFields = append(model.UnknownFields, "private")
	}
	if model.Downloads == nil {
		model.UnknownFields = append(model.UnknownFields, "downloads")
	}
	if model.Likes == nil {
		model.UnknownFields = append(model.UnknownFields, "likes")
	}
	if upstream.Tags == nil {
		model.UnknownFields = append(model.UnknownFields, "tags")
	}
	if len(upstream.Gated) == 0 || string(upstream.Gated) == "null" {
		model.UnknownFields = append(model.UnknownFields, "gated")
	}
	sort.Strings(model.UnknownFields)
	return model, nil
}

func boundedString(value *string) *string {
	if value == nil {
		return nil
	}
	normalized := strings.TrimSpace(*value)
	if normalized == "" || len(normalized) > maxMetadataTextSize {
		return nil
	}
	return &normalized
}

func nonnegative(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}
	copy := *value
	return &copy
}

func validCommit(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F' {
			continue
		}
		return false
	}
	return true
}

func normalizeAccess(private *bool, gated json.RawMessage) string {
	if private != nil && *private {
		return "private"
	}
	var gatedBool bool
	if len(gated) > 0 && json.Unmarshal(gated, &gatedBool) == nil {
		if gatedBool {
			return "gated"
		}
		if private != nil {
			return "public"
		}
	}
	var gatedMode string
	if json.Unmarshal(gated, &gatedMode) == nil && gatedMode != "" {
		if len(gatedMode) > 64 {
			return "gated"
		}
		return "gated-" + strings.ToLower(gatedMode)
	}
	if private != nil && !*private {
		return "unknown"
	}
	return "unknown"
}

func normalizeCardData(raw json.RawMessage) (*string, []string, []string, bool) {
	unknown := make([]string, 0, 2)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil, []string{"languages", "license"}, false
	}
	var card map[string]json.RawMessage
	if json.Unmarshal(raw, &card) != nil {
		return nil, nil, []string{"languages", "license"}, false
	}
	var license *string
	truncated := false
	if value, exists := card["license"]; exists {
		var decoded string
		if json.Unmarshal(value, &decoded) == nil {
			license = boundedString(&decoded)
			if len(strings.TrimSpace(decoded)) > maxMetadataTextSize {
				unknown = append(unknown, "license")
				truncated = true
			}
		}
	}
	if license == nil && !containsString(unknown, "license") {
		unknown = append(unknown, "license")
	}
	languages, languagesTruncated := decodeStringList(card["language"], maxLanguages)
	if len(languages) == 0 {
		unknown = append(unknown, "languages")
	}
	return license, languages, unknown, truncated || languagesTruncated
}

func decodeStringList(raw json.RawMessage, limit int) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil {
		var single string
		if json.Unmarshal(raw, &single) != nil {
			return nil, false
		}
		values = []string{single}
	}
	return boundedStrings(values, limit)
}

func boundedStrings(values []string, limit int) ([]string, bool) {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	truncated := false
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > maxMetadataTextSize {
			truncated = true
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return out, truncated
}

func (c *Cache) Refresh(ctx context.Context, client Client) error {
	if c == nil {
		return errors.New("Hugging Face catalog cache is nil")
	}
	repositories := append([]string(nil), c.repositories...)
	type result struct {
		model Model
		err   error
	}
	results := make([]result, len(repositories))
	jobs := make(chan int)
	workers := min(4, len(repositories))
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				results[index].model, results[index].err = client.Fetch(ctx, repositories[index])
			}
		}()
	}
	for index := range repositories {
		select {
		case jobs <- index:
		case <-ctx.Done():
			close(jobs)
			group.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	group.Wait()

	attemptedAt := time.Now().UTC()
	if client.Now != nil {
		attemptedAt = client.Now().UTC()
	}
	c.mu.Lock()
	next := cloneModels(c.models)
	succeeded := 0
	failures := make([]string, 0)
	for index, result := range results {
		if result.err != nil {
			failures = append(failures, repositories[index]+": "+result.err.Error())
			continue
		}
		next[repositories[index]] = result.model
		succeeded++
	}
	c.lastAttempt = attemptedAt
	if succeeded > 0 {
		c.models = next
		c.lastSuccess = attemptedAt
	}
	c.lastPartial = len(failures) > 0
	c.mu.Unlock()
	if len(failures) > 0 {
		return errors.New("Hugging Face catalog refresh incomplete: " + strings.Join(failures, "; "))
	}
	return nil
}

func (c *Cache) Snapshot(now time.Time, query string) Snapshot {
	if c == nil {
		return Snapshot{SchemaVersion: SchemaVersion, State: "unavailable", Limitations: limitations()}
	}
	query = strings.ToLower(strings.TrimSpace(query))
	c.mu.RLock()
	models := cloneModels(c.models)
	configuredCount := len(c.repositories)
	lastAttempt, lastSuccess, lastPartial := c.lastAttempt, c.lastSuccess, c.lastPartial
	c.mu.RUnlock()
	items := make([]Model, 0, len(models))
	currentCount := 0
	for _, model := range models {
		model.Current = !model.Provenance.RetrievedAt.IsZero() && now.Before(model.Provenance.ValidUntil)
		if model.Current {
			currentCount++
		}
		if query != "" && !modelMatches(model, query) {
			continue
		}
		items = append(items, model)
	}
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Repository) < strings.ToLower(items[j].Repository)
	})
	state := "current"
	if len(models) == 0 {
		state = "unavailable"
	} else if currentCount == 0 {
		state = "stale"
	} else if currentCount != configuredCount || len(models) != configuredCount || lastPartial {
		state = "partial"
	}
	snapshot := Snapshot{SchemaVersion: SchemaVersion, State: state, Models: items, ConfiguredCount: configuredCount, Limitations: limitations()}
	if !lastAttempt.IsZero() {
		value := lastAttempt
		snapshot.LastRefreshAttempt = &value
	}
	if !lastSuccess.IsZero() {
		value := lastSuccess
		snapshot.LastSuccessfulFetch = &value
	}
	return snapshot
}

func modelMatches(model Model, query string) bool {
	values := []string{model.Repository, model.Access}
	for _, value := range []*string{model.Author, model.PipelineTag, model.LibraryName, model.License} {
		if value != nil {
			values = append(values, *value)
		}
	}
	values = append(values, model.Tags...)
	values = append(values, model.Languages...)
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}

func cloneModels(source map[string]Model) map[string]Model {
	out := make(map[string]Model, len(source))
	for key, model := range source {
		model.Author = clonePointer(model.Author)
		model.Revision = clonePointer(model.Revision)
		model.PipelineTag = clonePointer(model.PipelineTag)
		model.LibraryName = clonePointer(model.LibraryName)
		model.Private = clonePointer(model.Private)
		model.Downloads = clonePointer(model.Downloads)
		model.Likes = clonePointer(model.Likes)
		model.LastModified = clonePointer(model.LastModified)
		model.License = clonePointer(model.License)
		model.Tags = append([]string(nil), model.Tags...)
		model.Languages = append([]string(nil), model.Languages...)
		model.UnknownFields = append([]string(nil), model.UnknownFields...)
		out[key] = model
	}
	return out
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func limitations() []string {
	return []string{
		"Hugging Face metadata is upstream discovery evidence, not an InferCrane-reviewed deployment recipe.",
		"Metadata does not establish runtime compatibility, GPU fit, performance, price, capacity, license approval, or deployability.",
		"Only the bounded repositories already configured by InferCrane are fetched; this endpoint is not a mirror of the Hugging Face Hub.",
	}
}
