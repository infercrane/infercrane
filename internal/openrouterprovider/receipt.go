package openrouterprovider

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const receiptSchemaVersion = "infercrane.dev/marketplace-request-receipt/v1"

// RequestReceipt is deliberately content-free. It is sufficient to reconcile
// provider usage and investigate latency or failures without retaining prompts,
// completions, client addresses, or authorization material.
type RequestReceipt struct {
	SchemaVersion      string    `json:"schema_version"`
	RecordedAt         time.Time `json:"recorded_at"`
	RequestID          string    `json:"request_id"`
	Channel            string    `json:"channel"`
	Model              string    `json:"model"`
	UpstreamModel      string    `json:"upstream_model"`
	Stream             bool      `json:"stream"`
	StatusCode         int       `json:"status_code"`
	Outcome            string    `json:"outcome"`
	DurationMS         int64     `json:"duration_ms"`
	TimeToFirstTokenMS int64     `json:"time_to_first_token_ms,omitempty"`
	PromptTokens       int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens   int64     `json:"completion_tokens,omitempty"`
	TotalTokens        int64     `json:"total_tokens,omitempty"`
	CostNanoUSD        int64     `json:"cost_nano_usd,omitempty"`
	FinishReason       string    `json:"finish_reason,omitempty"`
}

func (r RequestReceipt) Validate() error {
	if r.SchemaVersion != receiptSchemaVersion {
		return fmt.Errorf("unsupported marketplace receipt schema %q", r.SchemaVersion)
	}
	if r.RecordedAt.IsZero() {
		return errors.New("marketplace receipt recorded_at is required")
	}
	if !supportedChannel(r.Channel) {
		return fmt.Errorf("unsupported marketplace receipt channel %q", r.Channel)
	}
	if r.RequestID == "" || len(r.RequestID) > 256 {
		return errors.New("marketplace receipt request_id is required and must not exceed 256 characters")
	}
	if r.Model == "" || len(r.Model) > 256 || r.UpstreamModel == "" || len(r.UpstreamModel) > 256 {
		return errors.New("marketplace receipt model identities are required and must not exceed 256 characters")
	}
	if r.StatusCode < 100 || r.StatusCode > 599 || r.Outcome == "" || len(r.Outcome) > 128 {
		return errors.New("marketplace receipt status_code or outcome is invalid")
	}
	if r.DurationMS < 0 || r.TimeToFirstTokenMS < 0 || r.PromptTokens < 0 || r.CompletionTokens < 0 || r.TotalTokens < 0 || r.CostNanoUSD < 0 {
		return errors.New("marketplace receipt counters cannot be negative")
	}
	if r.TotalTokens != 0 && r.TotalTokens != r.PromptTokens+r.CompletionTokens {
		return errors.New("marketplace receipt total_tokens must equal prompt_tokens plus completion_tokens")
	}
	if len(r.FinishReason) > 128 {
		return errors.New("marketplace receipt finish_reason must not exceed 128 characters")
	}
	return nil
}

func prepareReceipt(receipt RequestReceipt) (RequestReceipt, error) {
	if receipt.SchemaVersion == "" {
		receipt.SchemaVersion = receiptSchemaVersion
	}
	if receipt.RecordedAt.IsZero() {
		receipt.RecordedAt = time.Now().UTC()
	}
	if receipt.Channel == "" {
		receipt.Channel = ChannelOpenRouter
	}
	return receipt, receipt.Validate()
}

type ReceiptRecorder interface {
	Record(RequestReceipt) error
}

// ReceiptLedger adds the bounded lookup required by inference marketplaces
// that settle successful requests after serving them. Lookups are scoped to
// the authenticated channel so one marketplace cannot enumerate another
// marketplace's usage.
type ReceiptLedger interface {
	ReceiptRecorder
	Lookup(channel string, requestIDs []string) ([]RequestCost, error)
}

type RequestCost struct {
	RequestID   string `json:"requestId"`
	CostNanoUSD int64  `json:"costNanoUsd"`
}

// JSONLReceiptRecorder appends owner-only, newline-delimited receipts and syncs
// each completed request before returning. The low request rate of the initial
// provider lane makes durability more valuable than batched write throughput.
type JSONLReceiptRecorder struct {
	mu      sync.Mutex
	file    *os.File
	encoder *json.Encoder
	costs   map[string]RequestCost
}

func NewJSONLReceiptRecorder(path string) (*JSONLReceiptRecorder, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("receipt path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 {
		file.Close()
		return nil, errors.New("receipt file must be an owner-only regular file")
	}
	costs, err := loadRequestCosts(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	if _, err = file.Seek(0, io.SeekEnd); err != nil {
		file.Close()
		return nil, err
	}
	return &JSONLReceiptRecorder{file: file, encoder: json.NewEncoder(file), costs: costs}, nil
}

func (r *JSONLReceiptRecorder) Record(receipt RequestReceipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var err error
	receipt, err = prepareReceipt(receipt)
	if err != nil {
		return err
	}
	if err := r.encoder.Encode(receipt); err != nil {
		return err
	}
	if err := r.file.Sync(); err != nil {
		return err
	}
	if receipt.Channel != "" && receipt.RequestID != "" && receipt.StatusCode >= 200 && receipt.StatusCode < 400 && receipt.Outcome == "completed" {
		r.costs[receiptCostKey(receipt.Channel, receipt.RequestID)] = RequestCost{RequestID: receipt.RequestID, CostNanoUSD: receipt.CostNanoUSD}
	}
	return nil
}

func (r *JSONLReceiptRecorder) Lookup(channel string, requestIDs []string) ([]RequestCost, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]RequestCost, 0, len(requestIDs))
	seen := make(map[string]struct{}, len(requestIDs))
	for _, requestID := range requestIDs {
		if _, duplicate := seen[requestID]; duplicate {
			continue
		}
		seen[requestID] = struct{}{}
		if cost, found := r.costs[receiptCostKey(channel, requestID)]; found {
			result = append(result, cost)
		}
	}
	return result, nil
}

func loadRequestCosts(file *os.File) (map[string]RequestCost, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	costs := make(map[string]RequestCost)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 16<<10), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		var receipt RequestReceipt
		if err := json.Unmarshal(scanner.Bytes(), &receipt); err != nil {
			return nil, fmt.Errorf("decode request receipt line %d: %w", line, err)
		}
		channel := receipt.Channel
		if channel == "" {
			// Receipts produced before marketplace support were OpenRouter-only.
			channel = ChannelOpenRouter
		}
		if receipt.RequestID != "" && receipt.StatusCode >= 200 && receipt.StatusCode < 400 && receipt.Outcome == "completed" {
			costs[receiptCostKey(channel, receipt.RequestID)] = RequestCost{RequestID: receipt.RequestID, CostNanoUSD: receipt.CostNanoUSD}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return costs, nil
}

func receiptCostKey(channel, requestID string) string {
	return channel + "\x00" + requestID
}

func (r *JSONLReceiptRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}
