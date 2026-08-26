// Package controlclient implements the durable control-plane HTTP semantics
// shared by first-party delivery integrations. It never talks to PostgreSQL or
// provider APIs.
package controlclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
)

type APIError struct {
	Status                               int
	Code, Category, Message, Remediation string
	Retryable                            bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("control API %d [%s]: %s", e.Status, e.Code, e.Message)
}

type Client struct {
	BaseURL, APIKey, UserAgent string
	HTTP                       *http.Client
	PollInterval               time.Duration
}

type Deployment struct {
	ID, Name, Model, Runtime, ObservedState, ActiveRevisionID, CandidateRevisionID string
	Cloud, ComputeMode, GPU, Region, ModelRevision                                 string
	MinReplicas, MaxReplicas                                                       int
	EndpointNames                                                                  []string
}

func New(baseURL, apiKey, userAgent string) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:18000"
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return nil, errors.New("control URL must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	if !strings.HasSuffix(baseURL, "/api/v1") {
		baseURL += "/api/v1"
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, errors.New("control API key is required")
	}
	return &Client{BaseURL: baseURL, APIKey: apiKey, UserAgent: userAgent, HTTP: &http.Client{Timeout: 30 * time.Second}, PollInterval: time.Second}, nil
}

func (c *Client) Do(ctx context.Context, method, path, idempotencyKey string, request, response any) error {
	var body io.Reader
	if request != nil {
		encoded, err := json.Marshal(request)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if request != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("control API request: %w", err)
	}
	defer res.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("read control API response: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code, Category, Message, Remediation string
				Retryable                            bool
			} `json:"error"`
		}
		_ = json.Unmarshal(contents, &envelope)
		if envelope.Error.Code == "" {
			envelope.Error.Code = "http_error"
		}
		if envelope.Error.Message == "" {
			envelope.Error.Message = http.StatusText(res.StatusCode)
		}
		return &APIError{Status: res.StatusCode, Code: envelope.Error.Code, Category: envelope.Error.Category, Message: envelope.Error.Message, Remediation: envelope.Error.Remediation, Retryable: envelope.Error.Retryable}
	}
	if response == nil || len(contents) == 0 {
		return nil
	}
	if err = json.Unmarshal(contents, response); err != nil {
		return fmt.Errorf("decode control API response: %w", err)
	}
	return nil
}

func (c *Client) Operation(ctx context.Context, id string) (domain.Operation, error) {
	var operation domain.Operation
	err := c.Do(ctx, http.MethodGet, "/operations/"+url.PathEscape(id), "", nil, &operation)
	return operation, err
}

func (c *Client) Wait(ctx context.Context, id string) (domain.Operation, error) {
	interval := c.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		operation, err := c.Operation(ctx, id)
		if err != nil {
			if ctx.Err() != nil {
				return operation, fmt.Errorf("stop waiting for operation %s; durable operation continues: %w", id, ctx.Err())
			}
			return operation, err
		}
		switch operation.Status {
		case "succeeded":
			return operation, nil
		case "failed":
			return operation, fmt.Errorf("operation %s failed [%s]: %s", id, defaultString(operation.ErrorCode, "operation_failed"), operation.Message)
		case "cancelled":
			return operation, fmt.Errorf("operation %s was cancelled: %s", id, operation.Message)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return operation, fmt.Errorf("stop waiting for operation %s; durable operation continues: %w", id, ctx.Err())
		case <-timer.C:
		}
	}
}

func (c *Client) Deployment(ctx context.Context, name string) (Deployment, map[string]any, error) {
	var result struct {
		Deployment struct {
			ID                  string   `json:"id"`
			Name                string   `json:"name"`
			Model               string   `json:"model"`
			Runtime             string   `json:"runtime"`
			ObservedState       string   `json:"observed_state"`
			ActiveRevisionID    string   `json:"active_revision_id"`
			CandidateRevisionID string   `json:"candidate_revision_id"`
			MinReplicas         int      `json:"min_replicas"`
			MaxReplicas         int      `json:"max_replicas"`
			EndpointNames       []string `json:"endpoint_names"`
		} `json:"deployment"`
		Lifecycle map[string]any `json:"lifecycle_status"`
		Revisions []struct {
			ID   string `json:"id"`
			Spec struct {
				Cloud         string `json:"cloud"`
				ComputeMode   string `json:"compute_mode"`
				GPU           string `json:"gpu"`
				Region        string `json:"region"`
				ModelRevision string `json:"model_revision"`
			} `json:"spec"`
		} `json:"revisions"`
	}
	err := c.Do(ctx, http.MethodGet, "/deployments/"+url.PathEscape(name), "", nil, &result)
	row := result.Deployment
	out := Deployment{ID: row.ID, Name: row.Name, Model: row.Model, Runtime: row.Runtime, ObservedState: row.ObservedState, ActiveRevisionID: row.ActiveRevisionID, CandidateRevisionID: row.CandidateRevisionID, MinReplicas: row.MinReplicas, MaxReplicas: row.MaxReplicas, EndpointNames: row.EndpointNames}
	for _, revision := range result.Revisions {
		if revision.ID == row.ActiveRevisionID {
			out.Cloud, out.ComputeMode, out.GPU, out.Region, out.ModelRevision = revision.Spec.Cloud, revision.Spec.ComputeMode, revision.Spec.GPU, revision.Spec.Region, revision.Spec.ModelRevision
			break
		}
	}
	return out, result.Lifecycle, err
}

func (c *Client) CreateDeployment(ctx context.Context, request any, key string) (domain.Operation, error) {
	var result struct {
		Operation domain.Operation `json:"operation"`
	}
	err := c.Do(ctx, http.MethodPost, "/deployments", key, request, &result)
	return result.Operation, err
}

func (c *Client) DeleteDeployment(ctx context.Context, name, key string) (domain.Operation, error) {
	var result struct {
		Operation domain.Operation `json:"operation"`
	}
	err := c.Do(ctx, http.MethodDelete, "/deployments/"+url.PathEscape(name), key, nil, &result)
	return result.Operation, err
}

func (c *Client) Rollout(ctx context.Context, name, action, revision, key string, request any) (domain.Operation, error) {
	path := "/deployments/" + url.PathEscape(name)
	switch action {
	case "create":
		path += "/rollouts"
	case "evaluate":
		path += "/rollouts/guard/evaluate"
	case "rollback":
		path += "/rollback"
	default:
		path += "/rollouts/" + url.PathEscape(revision) + "/" + action
	}
	var result struct {
		Operation domain.Operation `json:"operation"`
	}
	err := c.Do(ctx, http.MethodPost, path, key, request, &result)
	return result.Operation, err
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
