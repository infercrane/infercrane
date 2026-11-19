package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/agentprotocol"
)

var (
	// ErrRevoked means the control plane revoked this agent's key.
	ErrRevoked = errors.New("this agent's key was revoked")
	// ErrUnauthenticated means the control plane does not know this key.
	ErrUnauthenticated = errors.New("the control plane does not accept this agent key")
	// ErrUpgradeRequired means the control plane needs a newer agent.
	ErrUpgradeRequired = errors.New("the control plane needs a newer InferCrane agent")
)

// APIError is a control-plane refusal the agent may retry or report.
type APIError struct {
	Status        int
	Code, Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("control plane returned HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("control plane returned HTTP %d", e.Status)
}

// Client speaks the agent protocol. Every call is an outbound HTTPS request;
// the HTTP client honours HTTPS_PROXY so the agent works behind an egress
// proxy.
type Client struct {
	ControlURL string
	Key        string
	HTTP       *http.Client
	UserAgent  string
}

// CheckControlURL refuses to send an agent key over plain HTTP to anything
// but this machine.
func CheckControlURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("the control-plane URL must be an absolute URL without credentials, query, or fragment")
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		host := parsed.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return errors.New("the agent sends its key only over HTTPS; use an https:// control-plane URL (plain HTTP is allowed only to this machine)")
	}
	return errors.New("the control-plane URL must use https")
}

func (c Client) post(ctx context.Context, path string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err = writer.Write(encoded); err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.ControlURL, "/")+path, &compressed)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	return c.do(request, 4<<20, out)
}

// get makes one bounded GET. limit bounds the response body.
func (c Client) get(ctx context.Context, path string, limit int64, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.ControlURL, "/")+path, nil)
	if err != nil {
		return err
	}
	return c.do(request, limit, out)
}

func (c Client) do(request *http.Request, limit int64, out any) error {
	request.Header.Set("Authorization", "Bearer "+c.Key)
	request.Header.Set("Accept-Encoding", "gzip")
	if c.UserAgent != "" {
		request.Header.Set("User-Agent", c.UserAgent)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("control plane is unreachable: %w", err)
	}
	defer response.Body.Close()
	var body io.Reader = response.Body
	if strings.EqualFold(response.Header.Get("Content-Encoding"), "gzip") {
		decompressed, gzipErr := gzip.NewReader(response.Body)
		if gzipErr != nil {
			return fmt.Errorf("control plane sent an invalid gzip body: %w", gzipErr)
		}
		defer decompressed.Close()
		body = decompressed
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("control plane response exceeds %d bytes", limit)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if out == nil {
			return nil
		}
		return json.Unmarshal(data, out)
	}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &envelope)
	switch {
	case response.StatusCode == http.StatusUnauthorized && envelope.Error.Code == "agent_revoked":
		return ErrRevoked
	case response.StatusCode == http.StatusUnauthorized:
		return ErrUnauthenticated
	case response.StatusCode == http.StatusUpgradeRequired:
		return fmt.Errorf("%w: %s", ErrUpgradeRequired, envelope.Error.Message)
	}
	return &APIError{Status: response.StatusCode, Code: envelope.Error.Code, Message: envelope.Error.Message}
}

// Sync sends one sync.
func (c Client) Sync(ctx context.Context, request agentprotocol.SyncRequest) (agentprotocol.SyncResponse, error) {
	var response agentprotocol.SyncResponse
	err := c.post(ctx, "/api/v1/agent/sync", request, &response)
	return response, err
}

// Register asks the control plane to observe one server.
func (c Client) Register(ctx context.Context, request agentprotocol.RegisterServerRequest) (agentprotocol.RegisterServerResponse, error) {
	var response agentprotocol.RegisterServerResponse
	err := c.post(ctx, "/api/v1/agent/servers", request, &response)
	return response, err
}

// DataPlane pulls the routes, key digests, admission policies, and quota for
// this agent's agent-managed endpoints. generation is the snapshot the agent
// already holds; an unchanged snapshot comes back without its contents.
func (c Client) DataPlane(ctx context.Context, generation string) (agentprotocol.DataPlaneSnapshot, error) {
	var snapshot agentprotocol.DataPlaneSnapshot
	path := "/api/v1/agent/data-plane"
	if generation != "" {
		path += "?generation=" + url.QueryEscape(generation)
	}
	err := c.get(ctx, path, agentprotocol.MaxDataPlaneBodyBytes, &snapshot)
	return snapshot, err
}

// LeaseQuota asks for part of the current minute's request quota.
func (c Client) LeaseQuota(ctx context.Context, request agentprotocol.QuotaLeaseRequest) (agentprotocol.QuotaLeaseResponse, error) {
	var response agentprotocol.QuotaLeaseResponse
	err := c.post(ctx, "/api/v1/agent/quota-leases", request, &response)
	return response, err
}

// Commands long-polls for queued work.
func (c Client) Commands(ctx context.Context, wait time.Duration) (agentprotocol.CommandsResponse, error) {
	var response agentprotocol.CommandsResponse
	err := c.get(ctx, fmt.Sprintf("/api/v1/agent/commands?wait=%ds", int(wait/time.Second)), 4<<20, &response)
	return response, err
}

// UpdateCommand reports progress or the result of one command.
func (c Client) UpdateCommand(ctx context.Context, id string, update agentprotocol.CommandUpdate) (agentprotocol.CommandUpdateResponse, error) {
	var response agentprotocol.CommandUpdateResponse
	err := c.post(ctx, "/api/v1/agent/commands/"+url.PathEscape(id), update, &response)
	return response, err
}

// fatal reports whether an error must stop the agent.
func fatal(err error) bool {
	return errors.Is(err, ErrRevoked) || errors.Is(err, ErrUnauthenticated) || errors.Is(err, ErrUpgradeRequired)
}
