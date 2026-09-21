package acceleratorlab

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/brezelexecutor"
)

const maxWorkerResponseBytes = 16 << 20

var ErrInvalidWorkerConfig = errors.New("invalid accelerator worker configuration")

// WorkerClient talks to a trusted target-accelerator worker. The worker owns
// the vendor tools (Nsight, rocprof, XProf, or Neuron Explorer) and serving
// executors; InferCrane owns the immutable request, spend boundary, and
// evidence validation in Engine.
type WorkerClient struct {
	baseURL *url.URL
	token   string
	client  *http.Client
}

type WorkerConfig struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

func NewWorkerClient(config WorkerConfig) (*WorkerClient, error) {
	base, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("%w: base URL must be an absolute origin", ErrInvalidWorkerConfig)
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && loopback(base.Hostname())) {
		return nil, fmt.Errorf("%w: base URL must use HTTPS except on loopback", ErrInvalidWorkerConfig)
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, fmt.Errorf("%w: a scoped service token is required", ErrInvalidWorkerConfig)
	}
	client := config.Client
	if client == nil {
		client = &http.Client{Timeout: 4 * time.Hour}
	}
	clone := *client
	if clone.CheckRedirect == nil {
		clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return errors.New("accelerator worker redirects are disabled")
		}
	}
	base.Path = strings.TrimRight(base.Path, "/")
	return &WorkerClient{baseURL: base, token: strings.TrimSpace(config.Token), client: &clone}, nil
}

// NewWorkerClientFromTokenFile enforces the same owner-only secret-file
// boundary used by the Brezel integration.
func NewWorkerClientFromTokenFile(config WorkerConfig, tokenFile string) (*WorkerClient, error) {
	info, err := os.Lstat(strings.TrimSpace(tokenFile))
	if err != nil {
		return nil, fmt.Errorf("read accelerator worker token metadata: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 || info.Size() < 1 || info.Size() > 16*1024 {
		return nil, fmt.Errorf("%w: token file must be a non-empty owner-only regular file", ErrInvalidWorkerConfig)
	}
	data, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read accelerator worker token: %w", err)
	}
	config.Token = strings.TrimSpace(string(data))
	return NewWorkerClient(config)
}

func (c *WorkerClient) Capture(ctx context.Context, input ProfileIntent) (ProfileEvidence, error) {
	var output ProfileEvidence
	if err := c.call(ctx, "/v1/profiles", input.InputDigest, input, &output); err != nil {
		return output, err
	}
	return output, nil
}

func (c *WorkerClient) Generate(ctx context.Context, input GenerationRequest) (GenerationEvidence, error) {
	var output GenerationEvidence
	if err := c.call(ctx, "/v1/kernel-generations", input.InputDigest+":"+input.Candidate.ID, input, &output); err != nil {
		return output, err
	}
	return output, nil
}

func (c *WorkerClient) Qualify(ctx context.Context, input QualificationRequest) (QualificationEvidence, error) {
	var output QualificationEvidence
	if err := c.call(ctx, "/v1/qualifications", input.InputDigest+":"+input.Experiment.ID, input, &output); err != nil {
		return output, err
	}
	return output, nil
}

// Capabilities is fetched during control-plane startup. A configured worker
// must declare what it can actually execute; InferCrane never upgrades the
// built-in contract catalog into a claim about a deployed worker.
func (c *WorkerClient) Capabilities(ctx context.Context) (CapabilityCatalog, error) {
	target := *c.baseURL
	target.Path += "/v1/capabilities"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return CapabilityCatalog{}, fmt.Errorf("create accelerator capability request: %w", err)
	}
	c.authorize(request)
	response, err := c.client.Do(request)
	if err != nil {
		return CapabilityCatalog{}, fmt.Errorf("read accelerator worker capabilities: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return CapabilityCatalog{}, fmt.Errorf("accelerator worker returned HTTP %d", response.StatusCode)
	}
	var catalog CapabilityCatalog
	if err = decodeStrict(response.Body, &catalog); err != nil {
		return CapabilityCatalog{}, err
	}
	if err = catalog.Validate(); err != nil {
		return CapabilityCatalog{}, fmt.Errorf("reject accelerator worker capabilities: %w", err)
	}
	return catalog, nil
}

// Open resolves only artifacts hosted by this worker service. This prevents a
// customer-supplied URI from turning the control plane into an SSRF client.
func (c *WorkerClient) Open(ctx context.Context, reference brezelexecutor.ArtifactRef) (io.ReadCloser, error) {
	target, err := url.Parse(strings.TrimSpace(reference.URI))
	if err != nil || target.Scheme != c.baseURL.Scheme || target.Host != c.baseURL.Host || !strings.HasPrefix(target.Path, strings.TrimRight(c.baseURL.Path, "/")+"/v1/artifacts/") || target.RawQuery != "" || target.Fragment != "" || target.User != nil {
		return nil, errors.New("artifact URI is outside the configured accelerator worker store")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	c.authorize(request)
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		response.Body.Close()
		return nil, fmt.Errorf("accelerator artifact store returned HTTP %d", response.StatusCode)
	}
	return response.Body, nil
}

// Publish stores a verified Brezel output in the worker's artifact service.
// The worker response must echo the digest and size observed by this client.
func (c *WorkerClient) Publish(ctx context.Context, jobID, kind string, reader io.Reader, size int64) (string, error) {
	if size < 1 || strings.TrimSpace(jobID) == "" || strings.TrimSpace(kind) == "" {
		return "", errors.New("artifact publication requires job, kind, and positive size")
	}
	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/v1/artifacts/" + url.PathEscape(jobID) + "/" + url.PathEscape(kind)
	hash := sha256.New()
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), io.TeeReader(reader, hash))
	if err != nil {
		return "", err
	}
	request.ContentLength = size
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("Idempotency-Key", jobID+":"+kind)
	c.authorize(request)
	response, err := c.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", fmt.Errorf("accelerator artifact store returned HTTP %d", response.StatusCode)
	}
	var output Artifact
	if err = decodeStrict(response.Body, &output); err != nil {
		return "", err
	}
	observed := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if output.Size != size || !strings.EqualFold(output.SHA256, observed) || !validArtifact(output) {
		return "", errors.New("accelerator artifact store returned mismatched evidence")
	}
	return output.URI, nil
}

func (c *WorkerClient) call(ctx context.Context, endpoint, idempotencyKey string, input, output any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode accelerator worker request: %w", err)
	}
	target := *c.baseURL
	target.Path += endpoint
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create accelerator worker request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("call accelerator worker: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxWorkerResponseBytes+1)
	payload, readErr := io.ReadAll(limited)
	if readErr != nil {
		return fmt.Errorf("read accelerator worker response: %w", readErr)
	}
	if len(payload) > maxWorkerResponseBytes {
		return errors.New("accelerator worker response exceeds the size limit")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("accelerator worker returned HTTP %d", response.StatusCode)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(output); err != nil {
		return fmt.Errorf("decode accelerator worker response: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("accelerator worker returned trailing JSON")
	}
	return nil
}

func (c *WorkerClient) authorize(request *http.Request) {
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
}

func decodeStrict(reader io.Reader, output any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxWorkerResponseBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode accelerator worker response: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("accelerator worker returned trailing JSON")
	}
	return nil
}

func loopback(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
