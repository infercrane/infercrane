// Package brezelsandbox adapts the Brezel private-tenant API to InferCrane's
// stable customer sandbox contract.
package brezelsandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/sandboxprovider"
)

const maxJSONBytes = 4 << 20

var (
	safeID              = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)
	environmentRevision = regexp.MustCompile(`^envr_[0-9a-f]{24}$`)
	connectorRevision   = regexp.MustCompile(`^connr_[0-9a-f]{24}$`)
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

type brezelWorkspace struct {
	ID        string         `json:"id"`
	State     string         `json:"state"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	Failure   *brezelFailure `json:"failure,omitempty"`
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

type workspaceMutationEnvelope struct {
	Resource  brezelWorkspace `json:"resource"`
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
	// Provider credentials must never follow a redirect to another origin.
	// Preview redirects are surfaced to InferCrane and deliberately sanitized.
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	base.Path = strings.TrimRight(base.Path, "/")
	return &Client{baseURL: base, token: strings.TrimSpace(config.Token), projectID: strings.TrimSpace(config.ProjectID), allowedTenant: strings.TrimSpace(config.AllowedTenant), templates: templates, defaultTemplate: defaultTemplate, httpClient: &safeClient}, nil
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

func (c *Client) CreateWorkspace(ctx context.Context, tenantID, key, name string) (sandboxprovider.WorkspaceMutation, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return sandboxprovider.WorkspaceMutation{}, err
	}
	if err := validKey(key); err != nil {
		return sandboxprovider.WorkspaceMutation{}, err
	}
	if !safeID.MatchString(strings.TrimSpace(name)) {
		return sandboxprovider.WorkspaceMutation{}, fmt.Errorf("%w: workspace name is invalid", sandboxprovider.ErrInvalid)
	}
	var payload workspaceMutationEnvelope
	if err := c.doJSON(ctx, http.MethodPost, "/v1/workspaces", map[string]string{"name": name}, key, &payload); err != nil {
		return sandboxprovider.WorkspaceMutation{}, err
	}
	return sandboxprovider.WorkspaceMutation{Resource: workspace(payload.Resource), Operation: operation(payload.Operation)}, nil
}

func (c *Client) DeleteWorkspace(ctx context.Context, tenantID, id, key string) (sandboxprovider.WorkspaceMutation, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return sandboxprovider.WorkspaceMutation{}, err
	}
	if !safeID.MatchString(id) {
		return sandboxprovider.WorkspaceMutation{}, sandboxprovider.ErrNotFound
	}
	if err := validKey(key); err != nil {
		return sandboxprovider.WorkspaceMutation{}, err
	}
	var payload workspaceMutationEnvelope
	if err := c.doJSON(ctx, http.MethodDelete, "/v1/workspaces/"+url.PathEscape(id), nil, key, &payload); err != nil {
		return sandboxprovider.WorkspaceMutation{}, err
	}
	return sandboxprovider.WorkspaceMutation{Resource: workspace(payload.Resource), Operation: operation(payload.Operation)}, nil
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
	if in.WorkspaceID != "" {
		if !safeID.MatchString(in.WorkspaceID) {
			return sandboxprovider.Mutation{}, fmt.Errorf("%w: workspace identity is invalid", sandboxprovider.ErrInvalid)
		}
		body["workspace_mounts"] = []map[string]string{{"workspace_id": in.WorkspaceID, "path": "/workspace"}}
	}
	if in.ConnectorRevision != "" {
		if !connectorRevision.MatchString(in.ConnectorRevision) {
			return sandboxprovider.Mutation{}, fmt.Errorf("%w: model connector revision is invalid", sandboxprovider.ErrInvalid)
		}
		body["connector_revisions"] = []string{in.ConnectorRevision}
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

func workspace(value brezelWorkspace) sandboxprovider.Workspace {
	var failure *sandboxprovider.Failure
	if value.Failure != nil {
		failure = &sandboxprovider.Failure{Code: value.Failure.Code, Message: value.Failure.Message, Retryable: value.Failure.Retryable}
	}
	return sandboxprovider.Workspace{ID: value.ID, State: value.State, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, Failure: failure}
}

func (c *Client) RunCommand(ctx context.Context, tenantID, id string, input sandboxprovider.CommandRequest, emit func(sandboxprovider.CommandEvent) error) (string, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return "", err
	}
	if !safeID.MatchString(id) {
		return "", sandboxprovider.ErrNotFound
	}
	if len(input.Argv) == 0 || emit == nil {
		return "", fmt.Errorf("%w: command arguments and event callback are required", sandboxprovider.ErrInvalid)
	}
	response, err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/commands", input, "", "application/x-ndjson")
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", providerError(response)
	}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	var executionID string
	var total int64
	terminal := false
	for scanner.Scan() {
		total += int64(len(scanner.Bytes()))
		if total > 70<<20 {
			return executionID, fmt.Errorf("%w: command stream exceeded its bound", sandboxprovider.ErrUpstream)
		}
		var wire struct {
			ExecutionID string          `json:"execution_id,omitempty"`
			Type        string          `json:"type"`
			PID         uint32          `json:"pid,omitempty"`
			Data        []byte          `json:"data,omitempty"`
			ExitCode    *int32          `json:"exit_code,omitempty"`
			Exited      bool            `json:"exited,omitempty"`
			Status      string          `json:"status,omitempty"`
			Error       json.RawMessage `json:"error,omitempty"`
		}
		if err = json.Unmarshal(scanner.Bytes(), &wire); err != nil {
			return executionID, fmt.Errorf("%w: decode command event", sandboxprovider.ErrUpstream)
		}
		event := sandboxprovider.CommandEvent{ExecutionID: wire.ExecutionID, Type: wire.Type, PID: wire.PID, Data: wire.Data, ExitCode: wire.ExitCode, Exited: wire.Exited, Status: wire.Status}
		if wire.Type == "error" {
			event.Error = "command stream failed"
		}
		if executionID == "" {
			executionID = event.ExecutionID
		} else if event.ExecutionID != "" && event.ExecutionID != executionID {
			return executionID, fmt.Errorf("%w: command execution identity changed", sandboxprovider.ErrUpstream)
		}
		if err = emit(event); err != nil {
			return executionID, err
		}
		if event.Type == "exited" {
			terminal = true
		}
		if event.Type == "error" {
			return executionID, fmt.Errorf("%w: command stream ended before a confirmed exit", sandboxprovider.ErrUpstream)
		}
	}
	if err = scanner.Err(); err != nil {
		return executionID, fmt.Errorf("%w: read command stream: %v", sandboxprovider.ErrUpstream, err)
	}
	if !terminal {
		return executionID, fmt.Errorf("%w: command stream ended before a confirmed exit", sandboxprovider.ErrUpstream)
	}
	return executionID, nil
}

func (c *Client) WriteFile(ctx context.Context, tenantID, id, path string, source io.Reader, size int64) (sandboxprovider.FileInfo, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return sandboxprovider.FileInfo{}, err
	}
	if !safeID.MatchString(id) || strings.TrimSpace(path) == "" || source == nil || size < 0 || size > 32<<20 {
		return sandboxprovider.FileInfo{}, fmt.Errorf("%w: invalid file upload", sandboxprovider.ErrInvalid)
	}
	response, err := c.doReader(ctx, http.MethodPut, filePath(id, path), source, size, "application/octet-stream")
	if err != nil {
		return sandboxprovider.FileInfo{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return sandboxprovider.FileInfo{}, providerError(response)
	}
	var info sandboxprovider.FileInfo
	if err = decodeJSON(response.Body, &info); err != nil {
		return sandboxprovider.FileInfo{}, err
	}
	return info, nil
}

func (c *Client) ReadFile(ctx context.Context, tenantID, id, path string) (sandboxprovider.FileDownload, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return sandboxprovider.FileDownload{}, err
	}
	if !safeID.MatchString(id) || strings.TrimSpace(path) == "" {
		return sandboxprovider.FileDownload{}, fmt.Errorf("%w: invalid file download", sandboxprovider.ErrInvalid)
	}
	response, err := c.doReader(ctx, http.MethodGet, filePath(id, path), nil, 0, "")
	if err != nil {
		return sandboxprovider.FileDownload{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return sandboxprovider.FileDownload{}, providerError(response)
	}
	size, _ := strconv.ParseInt(response.Header.Get("Content-Length"), 10, 64)
	return sandboxprovider.FileDownload{Body: response.Body, FileInfo: sandboxprovider.FileInfo{Size: size, ContentType: response.Header.Get("Content-Type")}}, nil
}

func (c *Client) CreatePortLease(ctx context.Context, tenantID, id string, port uint16, ttlSeconds int64) (sandboxprovider.PortLease, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return sandboxprovider.PortLease{}, err
	}
	if !safeID.MatchString(id) || port == 0 || ttlSeconds < 30 || ttlSeconds > 900 {
		return sandboxprovider.PortLease{}, fmt.Errorf("%w: invalid preview lease request", sandboxprovider.ErrInvalid)
	}
	var payload struct {
		Path      string    `json:"path"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("/v1/sandboxes/%s/ports/%d/leases", url.PathEscape(id), port)
	if err := c.doJSON(ctx, http.MethodPost, path, map[string]int64{"ttl_seconds": ttlSeconds}, "", &payload); err != nil {
		return sandboxprovider.PortLease{}, err
	}
	if !strings.HasPrefix(payload.Path, "/p/") || payload.ExpiresAt.IsZero() {
		return sandboxprovider.PortLease{}, fmt.Errorf("%w: Brezel returned an invalid preview lease", sandboxprovider.ErrUpstream)
	}
	return sandboxprovider.PortLease{Path: payload.Path, ExpiresAt: payload.ExpiresAt}, nil
}

func (c *Client) ProxyPreview(ctx context.Context, tenantID, leasePath string, input sandboxprovider.PreviewRequest) (sandboxprovider.PreviewResponse, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return sandboxprovider.PreviewResponse{}, err
	}
	if input.Method != http.MethodGet && input.Method != http.MethodHead {
		return sandboxprovider.PreviewResponse{}, fmt.Errorf("%w: preview method is not supported", sandboxprovider.ErrInvalid)
	}
	lease, err := url.Parse(leasePath)
	if err != nil || lease.IsAbs() || lease.Host != "" || lease.RawQuery != "" || !strings.HasPrefix(lease.Path, "/p/") || strings.Contains(lease.Path, "..") {
		return sandboxprovider.PreviewResponse{}, fmt.Errorf("%w: preview lease is invalid", sandboxprovider.ErrInvalid)
	}
	suffix, err := safePreviewSuffix(input.Path)
	if err != nil {
		return sandboxprovider.PreviewResponse{}, err
	}
	requestPath := strings.TrimRight(lease.EscapedPath(), "/") + suffix
	if input.RawQuery != "" {
		if _, parseErr := url.ParseQuery(input.RawQuery); parseErr != nil {
			return sandboxprovider.PreviewResponse{}, fmt.Errorf("%w: preview query is invalid", sandboxprovider.ErrInvalid)
		}
		requestPath += "?" + input.RawQuery
	}
	response, err := c.do(ctx, input.Method, requestPath, nil, "", "*/*")
	if err != nil {
		return sandboxprovider.PreviewResponse{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		return sandboxprovider.PreviewResponse{}, providerError(response)
	}
	allowed := map[string]bool{"Content-Type": true, "Content-Length": true, "Cache-Control": true, "ETag": true, "Last-Modified": true, "Content-Encoding": true}
	headers := make(map[string][]string)
	for name, values := range response.Header {
		canonical := http.CanonicalHeaderKey(name)
		if allowed[canonical] {
			headers[canonical] = append([]string(nil), values...)
		}
	}
	return sandboxprovider.PreviewResponse{StatusCode: response.StatusCode, Header: headers, Body: response.Body}, nil
}

func safePreviewSuffix(value string) (string, error) {
	value = strings.TrimPrefix(value, "/")
	if value == "" {
		return "/", nil
	}
	parts := strings.Split(value, "/")
	for index, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil || decoded == ".." || decoded == "." || strings.Contains(decoded, "\\") {
			return "", fmt.Errorf("%w: preview path is invalid", sandboxprovider.ErrInvalid)
		}
		parts[index] = url.PathEscape(decoded)
	}
	return "/" + strings.Join(parts, "/"), nil
}

func (c *Client) Events(ctx context.Context, tenantID, id string) ([]sandboxprovider.Event, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return nil, err
	}
	if !safeID.MatchString(id) {
		return nil, sandboxprovider.ErrNotFound
	}
	var payload struct {
		Events []struct {
			ID          string         `json:"id"`
			ResourceID  string         `json:"resource_id"`
			OperationID string         `json:"operation_id"`
			Type        string         `json:"type"`
			State       string         `json:"state"`
			Sequence    int64          `json:"sequence"`
			At          time.Time      `json:"at"`
			Details     map[string]any `json:"details"`
		} `json:"events"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id)+"/events", nil, "", &payload); err != nil {
		return nil, err
	}
	out := make([]sandboxprovider.Event, 0, len(payload.Events))
	for _, item := range payload.Events {
		out = append(out, sandboxprovider.Event{ID: item.ID, Sequence: item.Sequence, OperationID: item.OperationID, Type: item.Type, State: item.State, At: item.At, Details: safeEventDetails(item.Details)})
	}
	return out, nil
}

func (c *Client) Receipt(ctx context.Context, tenantID, id string) (sandboxprovider.ReceiptEvidence, error) {
	if err := c.authorizeTenant(tenantID); err != nil {
		return sandboxprovider.ReceiptEvidence{}, err
	}
	if !safeID.MatchString(id) {
		return sandboxprovider.ReceiptEvidence{}, sandboxprovider.ErrNotFound
	}
	var envelope struct {
		PayloadType string `json:"payloadType"`
		Payload     string `json:"payload"`
		Signatures  []struct {
			KeyID string `json:"keyid"`
			Sig   string `json:"sig"`
		} `json:"signatures"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id)+"/receipt", nil, "", &envelope); err != nil {
		return sandboxprovider.ReceiptEvidence{}, err
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return sandboxprovider.ReceiptEvidence{}, err
	}
	digest := sha256.Sum256(encoded)
	evidence := sandboxprovider.ReceiptEvidence{Digest: "sha256:" + hex.EncodeToString(digest[:]), Signed: len(envelope.Signatures) == 1 && envelope.Signatures[0].Sig != ""}
	if len(envelope.Signatures) == 1 {
		evidence.KeyID = envelope.Signatures[0].KeyID
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err == nil {
		var statement struct {
			Predicate struct {
				State, EnvironmentRevision, Assurance string
			} `json:"predicate"`
		}
		if json.Unmarshal(payload, &statement) == nil {
			evidence.State = statement.Predicate.State
			evidence.EnvironmentRevision = statement.Predicate.EnvironmentRevision
			evidence.Assurance = statement.Predicate.Assurance
		}
	}
	return evidence, nil
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
	response, err := c.do(ctx, method, requestPath, body, idempotencyKey, "application/json")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return providerError(response)
	}
	return decodeJSON(response.Body, out)
}

func (c *Client) do(ctx context.Context, method, requestPath string, body any, idempotencyKey, accept string) (*http.Response, error) {
	var reader io.Reader
	var size int64
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader, size = bytes.NewReader(encoded), int64(len(encoded))
	}
	return c.request(ctx, method, requestPath, reader, size, "application/json", accept, idempotencyKey)
}

func (c *Client) doReader(ctx context.Context, method, requestPath string, body io.Reader, size int64, contentType string) (*http.Response, error) {
	return c.request(ctx, method, requestPath, body, size, contentType, "application/json", "")
}

func (c *Client) request(ctx context.Context, method, requestPath string, body io.Reader, size int64, contentType, accept, idempotencyKey string) (*http.Response, error) {
	endpoint := *c.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + strings.Split(requestPath, "?")[0]
	if index := strings.IndexByte(requestPath, '?'); index >= 0 {
		endpoint.RawQuery = requestPath[index+1:]
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("X-Project-ID", c.projectID)
	if accept == "" {
		accept = "application/json"
	}
	request.Header.Set("Accept", accept)
	if body != nil && contentType != "" {
		request.Header.Set("Content-Type", contentType)
		request.ContentLength = size
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", sandboxprovider.ErrUpstream, err)
	}
	return response, nil
}

func decodeJSON(reader io.Reader, out any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxJSONBytes+1))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("%w: decode Brezel response", sandboxprovider.ErrUpstream)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing Brezel response data", sandboxprovider.ErrUpstream)
	}
	return nil
}

func filePath(id, path string) string {
	query := url.Values{"path": []string{path}}
	return "/v1/sandboxes/" + url.PathEscape(id) + "/files?" + query.Encode()
}

func safeEventDetails(details map[string]any) map[string]any {
	if len(details) == 0 {
		return nil
	}
	allowed := map[string]struct{}{
		"bytes": {}, "code": {}, "duration_ms": {}, "execution_id": {}, "exit_code": {},
		"kind": {}, "output_bytes": {}, "result": {}, "timeout_seconds": {},
	}
	out := make(map[string]any)
	for key, value := range details {
		if _, ok := allowed[key]; ok {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
