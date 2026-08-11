package autoscale

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/metrics"
)

type TargetSource interface {
	AutoscalingTargetURLs(context.Context, string) ([]string, error)
}

// VLLMSignals reads provider-runtime gauges without recording prompt or output
// content. A missing queue gauge is an error, not an invented zero.
type VLLMSignals struct {
	Targets TargetSource
	Client  *http.Client
	APIKey  string
}

func (s VLLMSignals) Signals(ctx context.Context, deploymentID string) (Signals, error) {
	urls, err := s.Targets.AutoscalingTargetURLs(ctx, deploymentID)
	if err != nil {
		return Signals{}, err
	}
	if len(urls) == 0 {
		return Signals{}, fmt.Errorf("deployment has no routed targets")
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	var out Signals
	for _, baseURL := range urls {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/metrics", nil)
		if err != nil {
			return Signals{}, err
		}
		if s.APIKey != "" {
			request.Header.Set("Authorization", "Bearer "+s.APIKey)
		}
		response, err := client.Do(request)
		if err != nil {
			return Signals{}, fmt.Errorf("scrape %s: %w", baseURL, err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			return Signals{}, fmt.Errorf("scrape %s returned %s", baseURL, response.Status)
		}
		parsed := metrics.Parse(string(body))
		if parsed.RequestsWaiting == nil || parsed.RequestsRunning == nil {
			return Signals{}, fmt.Errorf("scrape %s omitted vLLM running/waiting gauges", baseURL)
		}
		out.Waiting += *parsed.RequestsWaiting
		out.Running += *parsed.RequestsRunning
	}
	return out, nil
}

func (s VLLMSignals) Waiting(ctx context.Context, deploymentID string) (float64, error) {
	signals, err := s.Signals(ctx, deploymentID)
	return signals.Waiting, err
}
