package openrouterprovider

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
)

const (
	receiptIngestPath = "/api/v1/admin/marketplace/receipts"
	receiptLookupPath = "/api/v1/admin/marketplace/billing/requests"
)

// HTTPReceiptLedger stores marketplace receipts in the durable InferCrane
// control plane. Inference bytes are flushed before Record is called, keeping
// geographically remote accounting out of TTFT and token delivery latency.
type HTTPReceiptLedger struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewHTTPReceiptLedger(baseURL, token string, client *http.Client) (*HTTPReceiptLedger, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("marketplace ledger URL must be absolute and contain no credentials, query, or fragment")
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname())) {
		return nil, errors.New("plaintext marketplace ledger is allowed only on loopback")
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("marketplace ledger bearer token is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return &HTTPReceiptLedger{baseURL: parsed.String(), token: strings.TrimSpace(token), client: client}, nil
}

func (l *HTTPReceiptLedger) Record(receipt RequestReceipt) error {
	var err error
	receipt, err = prepareReceipt(receipt)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"receipt": receipt})
	if err != nil {
		return err
	}
	return l.doWithRetry(context.Background(), receiptIngestPath, body, nil)
}

func (l *HTTPReceiptLedger) Lookup(channel string, requestIDs []string) ([]RequestCost, error) {
	body, err := json.Marshal(map[string]any{"channel": channel, "request_ids": requestIDs})
	if err != nil {
		return nil, err
	}
	var response struct {
		Requests []RequestCost `json:"requests"`
	}
	if err = l.doWithRetry(context.Background(), receiptLookupPath, body, &response); err != nil {
		return nil, err
	}
	return response.Requests, nil
}

func (l *HTTPReceiptLedger) doWithRetry(parent context.Context, path string, body []byte, destination any) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+path, bytes.NewReader(body))
		if err != nil {
			cancel()
			return err
		}
		request.Header.Set("Authorization", "Bearer "+l.token)
		request.Header.Set("Content-Type", "application/json")
		response, err := l.client.Do(request)
		if err == nil {
			responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			response.Body.Close()
			if readErr != nil {
				err = readErr
			} else if response.StatusCode >= 200 && response.StatusCode < 300 {
				cancel()
				if destination == nil || len(bytes.TrimSpace(responseBody)) == 0 {
					return nil
				}
				if err = json.Unmarshal(responseBody, destination); err != nil {
					return fmt.Errorf("decode marketplace ledger response: %w", err)
				}
				return nil
			} else {
				err = fmt.Errorf("marketplace ledger returned HTTP %d", response.StatusCode)
				if response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests {
					cancel()
					return err
				}
			}
		}
		cancel()
		lastErr = err
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(50*(1<<attempt)) * time.Millisecond)
			select {
			case <-parent.Done():
				timer.Stop()
				return parent.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("marketplace ledger request failed after retries: %w", lastErr)
}
