package openrouterprovider

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const receiptSchemaVersion = "infercrane.dev/openrouter-request-receipt/v1"

// RequestReceipt is deliberately content-free. It is sufficient to reconcile
// provider usage and investigate latency or failures without retaining prompts,
// completions, client addresses, or authorization material.
type RequestReceipt struct {
	SchemaVersion      string    `json:"schema_version"`
	RecordedAt         time.Time `json:"recorded_at"`
	RequestID          string    `json:"request_id"`
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
	FinishReason       string    `json:"finish_reason,omitempty"`
}

type ReceiptRecorder interface {
	Record(RequestReceipt) error
}

// JSONLReceiptRecorder appends owner-only, newline-delimited receipts and syncs
// each completed request before returning. The low request rate of the initial
// provider lane makes durability more valuable than batched write throughput.
type JSONLReceiptRecorder struct {
	mu      sync.Mutex
	file    *os.File
	encoder *json.Encoder
}

func NewJSONLReceiptRecorder(path string) (*JSONLReceiptRecorder, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("receipt path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
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
	return &JSONLReceiptRecorder{file: file, encoder: json.NewEncoder(file)}, nil
}

func (r *JSONLReceiptRecorder) Record(receipt RequestReceipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if receipt.SchemaVersion == "" {
		receipt.SchemaVersion = receiptSchemaVersion
	}
	if receipt.RecordedAt.IsZero() {
		receipt.RecordedAt = time.Now().UTC()
	}
	if err := r.encoder.Encode(receipt); err != nil {
		return err
	}
	return r.file.Sync()
}

func (r *JSONLReceiptRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.file.Close()
}
