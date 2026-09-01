package priceingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

const defaultRunPodGraphQLURL = "https://api.runpod.io/graphql"

// RunPodFeed reads current secure-cloud, non-interruptible list prices directly
// from RunPod. Stock and account/region deployability remain the responsibility
// of the separate launch-capacity probe.
type RunPodFeed struct {
	APIKey, BaseURL string
	Client          *http.Client
	ValidFor        time.Duration
	Now             func() time.Time
}

func (f RunPodFeed) Refresh(ctx context.Context, catalog *pricing.DynamicCatalog) error {
	if catalog == nil {
		return errors.New("GPU price catalog is nil")
	}
	query := `query InferCraneGPUOffers { gpuTypes { id displayName lowestPrice(input: {gpuCount: 1, secureCloud: true}) { uninterruptablePrice } } }`
	body, _ := json.Marshal(map[string]string{"query": query})
	endpoint := strings.TrimRight(f.BaseURL, "/")
	if endpoint == "" {
		endpoint = defaultRunPodGraphQLURL
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Scheme != "https" && parsedEndpoint.Scheme != "http" {
		return errors.New("RunPod price endpoint is invalid")
	}
	if strings.TrimSpace(f.APIKey) != "" && (parsedEndpoint.Scheme != "https" || !strings.EqualFold(parsedEndpoint.Hostname(), "api.runpod.io")) {
		return errors.New("refusing to send RunPod credentials to an untrusted endpoint")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("prepare RunPod price request: %w", err)
	}
	if strings.TrimSpace(f.APIKey) != "" {
		request.Header.Set("Authorization", "Bearer "+f.APIKey)
	}
	request.Header.Set("Content-Type", "application/json")
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("RunPod price request failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return fmt.Errorf("read RunPod prices: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("RunPod prices returned HTTP %d", response.StatusCode)
	}
	var payload struct {
		Data struct {
			GPUTypes []struct {
				ID, DisplayName string
				LowestPrice     *struct {
					UninterruptablePrice float64
				}
			}
		}
		Errors []struct{ Message string }
	}
	if err = json.Unmarshal(data, &payload); err != nil {
		return fmt.Errorf("decode RunPod prices: %w", err)
	}
	if len(payload.Errors) > 0 {
		return errors.New("RunPod price query was rejected")
	}
	now := time.Now().UTC()
	if f.Now != nil {
		now = f.Now().UTC()
	}
	validFor := f.ValidFor
	if validFor <= 0 {
		validFor = 2 * time.Minute
	}
	prices := make(map[pricing.Request]pricing.Estimate)
	for _, gpu := range payload.Data.GPUTypes {
		if gpu.LowestPrice == nil || gpu.LowestPrice.UninterruptablePrice <= 0 {
			continue
		}
		providerID := strings.TrimSpace(gpu.ID)
		if providerID == "" {
			continue
		}
		prices[pricing.Request{Cloud: "runpod", Region: "global", GPU: providerID, GPUCount: 1, Replicas: 1}] = pricing.Estimate{
			Currency: "USD", Hourly: gpu.LowestPrice.UninterruptablePrice,
			CostScope:  pricing.CostScopeInstanceTotal,
			Authority:  pricing.PriceAuthorityProviderAPI,
			Source:     "https://api.runpod.io/graphql#gpuTypes.lowestPrice.secureCloud",
			ObservedAt: now, StaleAfter: validFor,
		}
	}
	if len(prices) == 0 {
		return errors.New("RunPod returned no priced GPU offers")
	}
	catalog.ReplaceProvider("runpod", prices)
	return nil
}

func RunProviderFeed(ctx context.Context, refresh func(context.Context) error, interval time.Duration, report func(error)) {
	if interval <= 0 || refresh == nil {
		return
	}
	delay := interval
	timer := time.NewTimer(jittered(delay))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := refresh(ctx); err != nil {
				if report != nil {
					report(err)
				}
				delay *= 2
				if delay > 15*time.Minute {
					delay = 15 * time.Minute
				}
			} else {
				delay = interval
			}
			timer.Reset(jittered(delay))
		}
	}
}

func jittered(delay time.Duration) time.Duration {
	if delay <= 10*time.Millisecond {
		return delay
	}
	spread := delay / 10
	return delay - spread + time.Duration(rand.Int64N(int64(2*spread)))
}
