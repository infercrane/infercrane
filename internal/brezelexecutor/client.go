// Package brezelexecutor runs bounded InferCrane artifact-build jobs inside
// Brezel sandboxes. It deliberately does not use Brezel as a GPU worker: the
// sandbox prepares and verifies content-addressed artifacts, while the normal
// InferCrane campaign driver benchmarks those artifacts on exact GPU hardware.
package brezelexecutor

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
	"path"
	"regexp"
	"strings"
	"time"
)

const (
	JobSchemaVersion    = "infercrane.brezel-optimization-job/v1"
	ResultSchemaVersion = "infercrane.brezel-optimization-result/v1"
	manifestPath        = "/workspace/infercrane/input/job.json"
	resultPath          = "/workspace/infercrane/output/result.json"
	maxJSONBytes        = 4 << 20
	maxEventBytes       = 2 << 20
	maxCommandBytes     = 4 << 20
)

var safeID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)

type Kind string

const (
	KindSourceScreen   Kind = "kernel-source-screen"
	KindCPUCorrectness Kind = "kernel-cpu-correctness"
	KindArtifactBuild  Kind = "optimized-artifact-build"
)

// ArtifactRef is content-addressed input. Credentials stay in Brezel connector
// policy and never enter this manifest.
type ArtifactRef struct {
	Name   string `json:"name"`
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
}

type Candidate struct {
	ImplementationID string `json:"implementation_id"`
	OperatorFamily   string `json:"operator_family"`
	Backend          string `json:"backend"`
	SourceRevision   string `json:"source_revision"`
	License          string `json:"license,omitempty"`
}

// Job is intentionally declarative. The caller cannot supply argv, shell
// fragments, environment variables, or a runner path.
type Job struct {
	SchemaVersion string        `json:"schema_version"`
	ID            string        `json:"id"`
	CampaignID    string        `json:"campaign_id"`
	CandidateID   string        `json:"candidate_id"`
	Kind          Kind          `json:"kind"`
	Model         string        `json:"model"`
	ModelRevision string        `json:"model_revision"`
	TargetSM      string        `json:"target_sm,omitempty"`
	Candidate     Candidate     `json:"candidate"`
	Inputs        []ArtifactRef `json:"inputs,omitempty"`
	TimeoutSecs   int           `json:"timeout_seconds"`
}

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

type OutputArtifact struct {
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Result contains evidence identities, never source or command output.
type Result struct {
	SchemaVersion string           `json:"schema_version"`
	JobID         string           `json:"job_id"`
	InputDigest   string           `json:"input_digest"`
	Status        string           `json:"status"`
	Checks        []Check          `json:"checks,omitempty"`
	Artifacts     []OutputArtifact `json:"artifacts,omitempty"`
	Limitations   []string         `json:"limitations,omitempty"`
	SandboxID     string           `json:"-"`
	ExecutionID   string           `json:"-"`
	Receipt       json.RawMessage  `json:"-"`
}

type Config struct {
	BaseURL             string
	Token               string
	ProjectID           string
	EnvironmentRevision string
	RunnerPath          string
	SandboxTTL          time.Duration
	Client              *http.Client
}

type Client struct {
	baseURL             *url.URL
	token               string
	projectID           string
	environmentRevision string
	runnerPath          string
	sandboxTTL          time.Duration
	httpClient          *http.Client
}

type mutationEnvelope struct {
	Resource struct {
		ID    string `json:"id"`
		State string `json:"state"`
	} `json:"resource"`
}

type commandEvent struct {
	Type        string `json:"type"`
	Data        string `json:"data"`
	ExecutionID string `json:"execution_id"`
	ExitCode    *int   `json:"exit_code"`
}

var (
	ErrInvalidConfig        = errors.New("invalid Brezel executor configuration")
	ErrInvalidJob           = errors.New("invalid Brezel optimization job")
	ErrCommandIndeterminate = errors.New("Brezel command outcome is indeterminate")
)

func New(config Config) (*Client, error) {
	base, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("%w: base URL must be an absolute origin", ErrInvalidConfig)
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && loopbackHost(base.Hostname())) {
		return nil, fmt.Errorf("%w: base URL must use HTTPS except on loopback", ErrInvalidConfig)
	}
	if strings.TrimSpace(config.Token) == "" || !safeID.MatchString(strings.TrimSpace(config.ProjectID)) || !safeID.MatchString(strings.TrimSpace(config.EnvironmentRevision)) {
		return nil, fmt.Errorf("%w: token, project ID, and immutable environment revision are required", ErrInvalidConfig)
	}
	runner := strings.TrimSpace(config.RunnerPath)
	if runner == "" {
		runner = "/opt/infercrane/bin/run-optimization-job"
	}
	if !strings.HasPrefix(runner, "/opt/infercrane/bin/") || path.Clean(runner) != runner {
		return nil, fmt.Errorf("%w: runner must be a fixed path under /opt/infercrane/bin", ErrInvalidConfig)
	}
	ttl := config.SandboxTTL
	if ttl == 0 {
		ttl = 15 * time.Minute
	}
	if ttl < time.Minute || ttl > time.Hour {
		return nil, fmt.Errorf("%w: sandbox TTL must be between one minute and one hour", ErrInvalidConfig)
	}
	httpClient := config.Client
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Minute}
	}
	base.Path = strings.TrimRight(base.Path, "/")
	return &Client{baseURL: base, token: strings.TrimSpace(config.Token), projectID: strings.TrimSpace(config.ProjectID), environmentRevision: strings.TrimSpace(config.EnvironmentRevision), runnerPath: runner, sandboxTTL: ttl, httpClient: httpClient}, nil
}

// NewFromTokenFile reads a Brezel service token from a regular owner-only
// file. Keeping the credential out of process arguments and configuration
// snapshots mirrors Brezel's own client contract.
func NewFromTokenFile(config Config, tokenFile string) (*Client, error) {
	info, err := os.Lstat(strings.TrimSpace(tokenFile))
	if err != nil {
		return nil, fmt.Errorf("read Brezel token metadata: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 || info.Size() < 1 || info.Size() > 16*1024 {
		return nil, fmt.Errorf("%w: Brezel token file must be a non-empty owner-only regular file", ErrInvalidConfig)
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read Brezel token: %w", err)
	}
	config.Token = strings.TrimSpace(string(data))
	return New(config)
}

// Run creates or adopts a sandbox for the exact manifest, executes only the
// baked-in runner, validates its typed result, deletes the sandbox, and returns
// the signed lifecycle receipt. A transport failure during command execution is
// never retried automatically.
func (c *Client) Run(ctx context.Context, job Job) (result Result, runErr error) {
	manifest, digest, err := encodeJob(job)
	if err != nil {
		return Result{}, err
	}
	sandboxID, err := c.createSandbox(ctx, digest)
	if err != nil {
		return Result{}, err
	}
	result.SandboxID = sandboxID
	deleted := false
	defer func() {
		if deleted {
			return
		}
		if cleanupErr := c.deleteSandbox(context.WithoutCancel(ctx), sandboxID); cleanupErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("cleanup Brezel sandbox: %w", cleanupErr))
		}
	}()

	// A deterministic create key can adopt work after a controller crash. If a
	// complete result already exists, do not replay the non-idempotent command.
	if existing, readErr := c.readResult(ctx, sandboxID); readErr == nil {
		result = existing
	} else if !errors.Is(readErr, errNotFound) {
		return Result{}, readErr
	} else {
		if err = c.writeFile(ctx, sandboxID, manifestPath, manifest); err != nil {
			return Result{}, fmt.Errorf("write Brezel job manifest: %w", err)
		}
		executionID, commandErr := c.runCommand(ctx, sandboxID, job.TimeoutSecs)
		if commandErr != nil {
			return Result{}, commandErr
		}
		result, err = c.readResult(ctx, sandboxID)
		if err != nil {
			return Result{}, fmt.Errorf("read Brezel optimization result: %w", err)
		}
		result.ExecutionID = executionID
	}
	if err = validateResult(result, job.ID, digest); err != nil {
		return Result{}, err
	}
	result.SandboxID = sandboxID
	if err = c.deleteSandbox(ctx, sandboxID); err != nil {
		return Result{}, fmt.Errorf("delete Brezel sandbox: %w", err)
	}
	deleted = true
	receipt, err := c.readReceipt(ctx, sandboxID)
	if err != nil {
		return Result{}, fmt.Errorf("read Brezel lifecycle receipt: %w", err)
	}
	result.Receipt = receipt
	return result, nil
}

func encodeJob(job Job) ([]byte, string, error) {
	if job.SchemaVersion == "" {
		job.SchemaVersion = JobSchemaVersion
	}
	if job.SchemaVersion != JobSchemaVersion || !safeID.MatchString(job.ID) || !safeID.MatchString(job.CampaignID) || !safeID.MatchString(job.CandidateID) || strings.TrimSpace(job.Model) == "" || strings.TrimSpace(job.ModelRevision) == "" {
		return nil, "", fmt.Errorf("%w: schema, identities, model, and immutable model revision are required", ErrInvalidJob)
	}
	switch job.Kind {
	case KindSourceScreen, KindCPUCorrectness, KindArtifactBuild:
	default:
		return nil, "", fmt.Errorf("%w: unsupported job kind %q", ErrInvalidJob, job.Kind)
	}
	if !safeID.MatchString(job.Candidate.ImplementationID) || strings.TrimSpace(job.Candidate.OperatorFamily) == "" || strings.TrimSpace(job.Candidate.Backend) == "" || strings.TrimSpace(job.Candidate.SourceRevision) == "" {
		return nil, "", fmt.Errorf("%w: a pinned implementation candidate is required", ErrInvalidJob)
	}
	if job.TimeoutSecs == 0 {
		job.TimeoutSecs = 600
	}
	if job.TimeoutSecs < 1 || job.TimeoutSecs > 3600 {
		return nil, "", fmt.Errorf("%w: timeout must be between 1 and 3600 seconds", ErrInvalidJob)
	}
	for _, input := range job.Inputs {
		parsed, err := url.Parse(strings.TrimSpace(input.URI))
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "s3") || parsed.Host == "" || !safeID.MatchString(input.Name) || !validSHA256(input.SHA256) {
			return nil, "", fmt.Errorf("%w: every input must have a safe name, HTTPS or S3 URI, and SHA-256 digest", ErrInvalidJob)
		}
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		return nil, "", fmt.Errorf("encode Brezel job: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return encoded, hex.EncodeToString(sum[:]), nil
}

func (c *Client) createSandbox(ctx context.Context, digest string) (string, error) {
	body := map[string]any{
		"environment_revision": c.environmentRevision,
		"lifecycle":            map[string]any{"expires_after_seconds": int(c.sandboxTTL.Seconds())},
		"network":              map[string]any{"allow_internet": false},
	}
	var envelope mutationEnvelope
	if err := c.doJSON(ctx, http.MethodPost, "/v1/sandboxes", body, "infercrane-build-"+digest[:32], &envelope); err != nil {
		return "", fmt.Errorf("create Brezel sandbox: %w", err)
	}
	if envelope.Resource.ID == "" || envelope.Resource.State != "running" {
		return "", errors.New("Brezel did not confirm a running sandbox")
	}
	return envelope.Resource.ID, nil
}

func (c *Client) writeFile(ctx context.Context, sandboxID, filePath string, data []byte) error {
	request, err := c.request(ctx, http.MethodPut, "/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files?path="+url.QueryEscape(filePath), bytes.NewReader(data), "application/octet-stream", "")
	if err != nil {
		return err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("Brezel file request failed: %w", err)
	}
	defer response.Body.Close()
	return statusError(response)
}

func (c *Client) runCommand(ctx context.Context, sandboxID string, timeoutSecs int) (string, error) {
	if timeoutSecs == 0 {
		timeoutSecs = 600
	}
	body := map[string]any{"argv": []string{c.runnerPath, "--manifest", manifestPath, "--result", resultPath}, "cwd": "/workspace", "env": map[string]string{}, "timeout_seconds": timeoutSecs}
	encoded, _ := json.Marshal(body)
	request, err := c.request(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(sandboxID)+"/commands", bytes.NewReader(encoded), "application/json", "")
	if err != nil {
		return "", err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCommandIndeterminate, err)
	}
	defer response.Body.Close()
	if err = statusError(response); err != nil {
		return "", err
	}
	if media := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])); media != "application/x-ndjson" {
		return "", errors.New("Brezel command response was not NDJSON")
	}
	scanner := bufio.NewScanner(io.LimitReader(response.Body, maxCommandBytes+1))
	scanner.Buffer(make([]byte, 64*1024), maxEventBytes)
	var executionID string
	exited := false
	for scanner.Scan() {
		var event commandEvent
		if err = json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return "", fmt.Errorf("%w: invalid command event", ErrCommandIndeterminate)
		}
		if event.ExecutionID != "" {
			executionID = event.ExecutionID
		}
		switch event.Type {
		case "stdout", "stderr":
			if _, err = base64.StdEncoding.DecodeString(event.Data); err != nil {
				return "", fmt.Errorf("%w: invalid command output encoding", ErrCommandIndeterminate)
			}
		case "exited":
			if exited || event.ExitCode == nil {
				return "", fmt.Errorf("%w: invalid terminal event", ErrCommandIndeterminate)
			}
			exited = true
			if *event.ExitCode != 0 {
				return "", fmt.Errorf("Brezel optimization runner exited with status %d", *event.ExitCode)
			}
		case "error":
			return "", ErrCommandIndeterminate
		default:
			return "", fmt.Errorf("%w: unknown command event", ErrCommandIndeterminate)
		}
	}
	if err = scanner.Err(); err != nil || !exited {
		return "", fmt.Errorf("%w: command stream ended before a confirmed exit", ErrCommandIndeterminate)
	}
	return executionID, nil
}

var errNotFound = errors.New("Brezel resource not found")

func (c *Client) readResult(ctx context.Context, sandboxID string) (Result, error) {
	request, err := c.request(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files?path="+url.QueryEscape(resultPath), nil, "", "")
	if err != nil {
		return Result{}, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return Result{}, fmt.Errorf("read Brezel result: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return Result{}, errNotFound
	}
	if err = statusError(response); err != nil {
		return Result{}, err
	}
	var result Result
	if err = decodeStrictJSON(response.Body, &result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func validateResult(result Result, jobID, digest string) error {
	if result.SchemaVersion != ResultSchemaVersion || result.JobID != jobID || !strings.EqualFold(result.InputDigest, digest) || (result.Status != "passed" && result.Status != "rejected") {
		return errors.New("Brezel optimization result does not match the admitted job")
	}
	for _, artifact := range result.Artifacts {
		if strings.TrimSpace(artifact.Kind) == "" || !validSHA256(artifact.SHA256) || artifact.Size < 0 {
			return errors.New("Brezel optimization result contains an invalid artifact identity")
		}
	}
	return nil
}

func (c *Client) deleteSandbox(ctx context.Context, sandboxID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(sandboxID), nil, "infercrane-delete-"+stableID(sandboxID), &mutationEnvelope{})
}

func (c *Client) readReceipt(ctx context.Context, sandboxID string) (json.RawMessage, error) {
	request, err := c.request(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(sandboxID)+"/receipt", nil, "", "")
	if err != nil {
		return nil, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err = statusError(response); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxJSONBytes+1))
	if err != nil || len(data) > maxJSONBytes || !json.Valid(data) {
		return nil, errors.New("Brezel returned an invalid lifecycle receipt")
	}
	return json.RawMessage(data), nil
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
	request, err := c.request(ctx, method, requestPath, reader, "application/json", idempotencyKey)
	if err != nil {
		return err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("Brezel request failed: %w", err)
	}
	defer response.Body.Close()
	if err = statusError(response); err != nil {
		return err
	}
	if out != nil {
		return decodeJSON(response.Body, out)
	}
	return nil
}

func (c *Client) request(ctx context.Context, method, requestPath string, body io.Reader, contentType, idempotencyKey string) (*http.Request, error) {
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
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return request, nil
}

func statusError(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &payload)
	message := strings.TrimSpace(payload.Error.Message)
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("Brezel API returned HTTP %d: %s", response.StatusCode, message)
}

func decodeJSON(reader io.Reader, out any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxJSONBytes+1))
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("decode Brezel response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("decode Brezel response: trailing data")
	}
	return nil
}

func decodeStrictJSON(reader io.Reader, out any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxJSONBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("decode Brezel result: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("decode Brezel result: trailing data")
	}
	return nil
}

func validSHA256(value string) bool {
	value = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), "sha256:")
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func stableID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
