// Package brezelsandbox adapts the Brezel private-tenant API to InferCrane's
// stable customer sandbox contract.
package brezelsandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/sandboxprovider"
)

const maxJSONBytes = 4 << 20

var (
	safeID              = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)
	environmentRevision = regexp.MustCompile(`^envr_[0-9a-f]{64}$`)
)

type Config struct {
	BaseURL         string
	Token           string
	ProjectID       string
	AllowedTenant   string
	Templates       map[string]string
	DefaultTemplate string
	Client          *http.Client
}

type Client struct {
	baseURL         *url.URL
	token           string
	projectID       string
	allowedTenant   string
	templates       map[string]string
	defaultTemplate string
	httpClient      *http.Client
}

type brezelLifecycle struct {
	StandbyAfterSeconds int64 `json:"standby_after_seconds,omitempty"`
	ExpiresAfterSeconds int64 `json:"expires_after_seconds"`
	AutoResume          bool  `json:"auto_resume,omitempty"`
}

type brezelNetwork struct {
	AllowInternet bool `json:"allow_internet"`
}

type brezelFailure struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type brezelSandbox struct {
	ID                  string          `json:"id"`
	EnvironmentRevision string          `json:"environment_revision"`
	State               string          `json:"state"`
	Lifecycle           brezelLifecycle `json:"lifecycle"`
	Network             brezelNetwork   `json:"network"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	ExpiresAt           time.Time       `json:"expires_at"`
	Failure             *brezelFailure  `json:"failure,omitempty"`
}

type brezelOperation struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	ResourceID string         `json:"resource_id"`
	State      string         `json:"state"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
	Failure    *brezelFailure `json:"failure,omitempty"`
}

type mutationEnvelope struct {
	Resource  brezelSandbox   `json:"resource"`
	Operation brezelOperation `json:"operation"`
}

func New(config Config) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("%w: Brezel URL must be an absolute origin", sandboxprovider.ErrInvalid)
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && loopbackHost(base.Hostname())) {
		return nil, fmt.Errorf("%w: Brezel URL must use HTTPS except on loopback", sandboxprovider.ErrInvalid)
	}
	if strings.TrimSpace(config.Token) == "" || !safeID.MatchString(strings.TrimSpace(config.ProjectID)) || !safeID.MatchString(strings.TrimSpace(config.AllowedTenant)) {
		return nil, fmt.Errorf("%w: token, project ID, and allowed tenant are required", sandboxprovider.ErrInvalid)
	}
	if len(config.Templates) == 0 {
		return nil, fmt.Errorf("%w: at least one approved Brezel template is required", sandboxprovider.ErrInvalid)
	}
	templates := make(map[string]string, len(config.Templates))
	for id, revision := range config.Templates {
		id, revision = strings.TrimSpace(id), strings.TrimSpace(revision)
		if !safeID.MatchString(id) || !environmentRevision.MatchString(revision) {
			return nil, fmt.Errorf("%w: template IDs and immutable environment revisions are invalid", sandboxprovider.ErrInvalid)
		}
		templates[id] = revision
	}
	defaultTemplate := strings.TrimSpace(config.DefaultTemplate)
	if defaultTemplate == "" && len(templates) == 1 {
		for id := range templates {
			defaultTemplate = id
		}
	}
	if _, ok := templates[defaultTemplate]; !ok {
		return nil, fmt.Errorf("%w: default template must name an approved template", sandboxprovider.ErrInvalid)
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	base.Path = strings.TrimRight(base.Path, "/")
	return &Client{baseURL: base, token: strings.TrimSpace(config.Token), projectID: strings.TrimSpace(config.ProjectID), allowedTenant: strings.TrimSpace(config.AllowedTenant), templates: templates, defaultTemplate: defaultTemplate, httpClient: client}, nil
}

func NewFromTokenFile(config Config, tokenFile string) (*Client, error) {
	info, err := os.Lstat(strings.TrimSpace(tokenFile))
	if err != nil {
		return nil, fmt.Errorf("read Brezel token metadata: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 || info.Size() < 1 || info.Size() > 16*1024 {
		return nil, fmt.Errorf("%w: Brezel token file must be a non-empty owner-only regular file", sandboxprovider.ErrInvalid)
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read Brezel token: %w", err)
	}
	config.Token = strings.TrimSpace(string(data))
	return New(config)
}

func (c *Client) Capabilities(ctx context.Context, tenantID string) (sandboxprovider.Capabilities, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return sandboxprovider.Capabilities{}, err
	}
	var payload struct {
		Runtime       string `json:"runtime"`
		Qualification string `json:"qualification"`
		Note          string `json:"qualification_note"`
		Implemented   struct {
			HostileCodeIsolation bool `json:"hostile_code_isolation"`
			DenyByDefaultEgress  bool `json:"deny_by_default_egress"`
			FilesystemCheckpoint bool `json:"filesystem_checkpoint"`
			CommandStreaming     bool `json:"command_streaming"`
			FileReadWrite        bool `json:"file_read_write"`
			AuthenticatedPorts   bool `json:"authenticated_ports"`
			DurableWorkspaces    bool `json:"durable_workspaces"`
		} `json:"implemented"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/capabilities", nil, "", &payload); err != nil {
		return sandboxprovider.Capabilities{}, err
	}
	templates := make([]sandboxprovider.Template, 0, len(c.templates))
	for id, revision := range c.templates {
		templates = append(templates, sandboxprovider.Template{ID: id, Label: label(id), EnvironmentRevision: revision})
	}
	sort.Slice(templates, func(i, j int) bool { return templates[i].ID < templates[j].ID })
	return sandboxprovider.Capabilities{
		Provider: "brezel", Product: "InferCrane Sandboxes", State: "ready", Assurance: "private-tenant-preview",
		Runtime: payload.Runtime, Qualification: payload.Qualification, QualificationNote: payload.Note, Templates: templates,
		Features: sandboxprovider.Features{
			HostileCodeIsolation: payload.Implemented.HostileCodeIsolation,
			DenyByDefaultEgress:  payload.Implemented.DenyByDefaultEgress,
			PauseResume:          true, FilesystemCheckpoint: payload.Implemented.FilesystemCheckpoint,
			CommandStreaming: payload.Implemented.CommandStreaming, FileReadWrite: payload.Implemented.FileReadWrite,
			HTTPPreview: payload.Implemented.AuthenticatedPorts, DurableWorkspaces: payload.Implemented.DurableWorkspaces,
			InteractivePTY: false, GPU: false,
		},
	}, nil
}

func (c *Client) List(ctx context.Context, tenantID string, includeTerminal bool) ([]sandboxprovider.Sandbox, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return nil, err
	}
	requestPath := "/v1/sandboxes"
	if includeTerminal {
		requestPath += "?include_terminal=true"
	}
	var payload struct {
		Sandboxes []brezelSandbox `json:"sandboxes"`
	}
	if err := c.doJSON(ctx, http.MethodGet, requestPath, nil, "", &payload); err != nil {
		return nil, err
	}
	items := make([]sandboxprovider.Sandbox, 0, len(payload.Sandboxes))
	for _, item := range payload.Sandboxes {
		items = append(items, c.sandbox(item))
	}
	return items, nil
}

func (c *Client) Get(ctx context.Context, tenantID, id string) (sandboxprovider.Sandbox, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return sandboxprovider.Sandbox{}, err
	}
	if !safeID.MatchString(id) {
		return sandboxprovider.Sandbox{}, sandboxprovider.ErrNotFound
	}
	var payload brezelSandbox
	if err := c.doJSON(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), nil, "", &payload); err != nil {
		return sandboxprovider.Sandbox{}, err
	}
	return c.sandbox(payload), nil
}

func (c *Client) Create(ctx context.Context, tenantID, key string, in sandboxprovider.CreateRequest) (sandboxprovider.Mutation, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return sandboxprovider.Mutation{}, err
	}
	if err := validKey(key); err != nil {
		return sandboxprovider.Mutation{}, err
	}
	templateID := strings.TrimSpace(in.TemplateID)
	if templateID == "" {
		templateID = c.defaultTemplate
	}
	revision, ok := c.templates[templateID]
	if !ok {
		return sandboxprovider.Mutation{}, fmt.Errorf("%w: template is not approved", sandboxprovider.ErrInvalid)
	}
	if in.ExpiresAfterSeconds == 0 {
		in.ExpiresAfterSeconds = 3600
	}
	if in.ExpiresAfterSeconds < 30 || in.ExpiresAfterSeconds > 30*24*60*60 {
		return sandboxprovider.Mutation{}, fmt.Errorf("%w: ttl_seconds must be between 30 and 2592000", sandboxprovider.ErrInvalid)
	}
	if in.StandbyAfterSeconds < 0 || in.StandbyAfterSeconds > 0 && in.StandbyAfterSeconds >= in.ExpiresAfterSeconds {
		return sandboxprovider.Mutation{}, fmt.Errorf("%w: standby_after_seconds must be less than ttl_seconds", sandboxprovider.ErrInvalid)
	}
	if in.NetworkMode != "" && in.NetworkMode != "offline" {
		return sandboxprovider.Mutation{}, fmt.Errorf("%w: only the offline network profile is available in this preview", sandboxprovider.ErrInvalid)
	}
	body := map[string]any{
		"environment_revision": revision,
		"lifecycle":            map[string]any{"expires_after_seconds": in.ExpiresAfterSeconds, "standby_after_seconds": in.StandbyAfterSeconds, "auto_resume": in.AutoResume},
		"network":              map[string]any{"allow_internet": false},
	}
	return c.mutate(ctx, http.MethodPost, "/v1/sandboxes", body, key)
}

func (c *Client) Pause(ctx context.Context, tenantID, id, key string) (sandboxprovider.Mutation, error) {
	return c.lifecycle(ctx, tenantID, id, key, ":pause", http.MethodPost)
}

func (c *Client) Resume(ctx context.Context, tenantID, id, key string) (sandboxprovider.Mutation, error) {
	return c.lifecycle(ctx, tenantID, id, key, ":resume", http.MethodPost)
}

func (c *Client) Delete(ctx context.Context, tenantID, id, key string) (sandboxprovider.Mutation, error) {
	return c.lifecycle(ctx, tenantID, id, key, "", http.MethodDelete)
}

func (c *Client) lifecycle(ctx context.Context, tenantID, id, key, suffix, method string) (sandboxprovider.Mutation, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return sandboxprovider.Mutation{}, err
	}
	if !safeID.MatchString(id) {
		return sandboxprovider.Mutation{}, sandboxprovider.ErrNotFound
	}
	if err := validKey(key); err != nil {
		return sandboxprovider.Mutation{}, err
	}
	return c.mutate(ctx, method, "/v1/sandboxes/"+url.PathEscape(id)+suffix, nil, key)
}

func (c *Client) mutate(ctx context.Context, method, requestPath string, body any, key string) (sandboxprovider.Mutation, error) {
	var payload mutationEnvelope
	if err := c.doJSON(ctx, method, requestPath, body, key, &payload); err != nil {
		return sandboxprovider.Mutation{}, err
	}
	return sandboxprovider.Mutation{Resource: c.sandbox(payload.Resource), Operation: operation(payload.Operation)}, nil
}

func (c *Client) sandbox(value brezelSandbox) sandboxprovider.Sandbox {
	templateID := ""
	for id, revision := range c.templates {
		if revision == value.EnvironmentRevision {
			templateID = id
			break
		}
	}
	var failure *sandboxprovider.Failure
	if value.Failure != nil {
		failure = &sandboxprovider.Failure{Code: value.Failure.Code, Message: value.Failure.Message, Retryable: value.Failure.Retryable}
	}
	return sandboxprovider.Sandbox{
		ID: value.ID, Provider: "brezel", TemplateID: templateID, EnvironmentRevision: value.EnvironmentRevision, State: value.State,
		Lifecycle: sandboxprovider.Lifecycle{ExpiresAfterSeconds: value.Lifecycle.ExpiresAfterSeconds, StandbyAfterSeconds: value.Lifecycle.StandbyAfterSeconds, AutoResume: value.Lifecycle.AutoResume},
		Network:   sandboxprovider.Network{Mode: "offline", AllowInternet: value.Network.AllowInternet},
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, ExpiresAt: value.ExpiresAt, Failure: failure,
	}
}

func operation(value brezelOperation) sandboxprovider.Operation {
	var failure *sandboxprovider.Failure
	if value.Failure != nil {
		failure = &sandboxprovider.Failure{Code: value.Failure.Code, Message: value.Failure.Message, Retryable: value.Failure.Retryable}
	}
	return sandboxprovider.Operation{ID: value.ID, Kind: value.Kind, ResourceID: value.ResourceID, State: value.State, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, Failure: failure}
}

func (c *Client) authorizeTenant(tenantID string) error {
	if tenantID != c.allowedTenant {
		return sandboxprovider.ErrForbidden
	}
	return nil
}

func validKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 128 {
		return fmt.Errorf("%w: Idempotency-Key is required and must not exceed 128 characters", sandboxprovider.ErrInvalid)
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, requestPath string, body any, idempotencyKey string, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + strings.Split(requestPath, "?")[0]
	if index := strings.IndexByte(requestPath, '?'); index >= 0 {
		endpoint.RawQuery = requestPath[index+1:]
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("X-Project-ID", c.projectID)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%w: %v", sandboxprovider.ErrUpstream, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return providerError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxJSONBytes+1))
	if err = decoder.Decode(out); err != nil {
		return fmt.Errorf("%w: decode Brezel response", sandboxprovider.ErrUpstream)
	}
	var extra any
	if err = decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing Brezel response data", sandboxprovider.ErrUpstream)
	}
	return nil
}

func providerError(response *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &payload)
	message := strings.TrimSpace(payload.Error.Message)
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	base := sandboxprovider.ErrUpstream
	switch response.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		base = sandboxprovider.ErrInvalid
	case http.StatusUnauthorized, http.StatusForbidden:
		base = sandboxprovider.ErrUnavailable
	case http.StatusNotFound:
		base = sandboxprovider.ErrNotFound
	case http.StatusConflict:
		base = sandboxprovider.ErrConflict
	}
	return fmt.Errorf("%w: %s", base, message)
}

func label(id string) string {
	words := strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(id))
	for index := range words {
		words[index] = strings.ToUpper(words[index][:1]) + words[index][1:]
	}
	return strings.Join(words, " ")
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
