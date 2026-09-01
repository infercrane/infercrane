package priceingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

const (
	defaultVastOffersURL      = "https://console.vast.ai/api/v0/bundles/"
	defaultVastShardsPerSweep = 4
)

// These are exact names from Vast's canonical gpu_names/unique endpoint. The
// order front-loads the accelerators most relevant to production inference.
// Each refresh covers a bounded slice and continues at the prior cursor.
var defaultVastGPUs = []string{
	"B200", "H200", "H100 SXM", "L40S",
	"A100 SXM4", "L4", "RTX 4090", "A100 PCIE",
	"H200 NVL", "H100 PCIE", "H100 NVL", "B300",
	"L40", "A40", "A10", "A16", "A800 PCIE", "GB10",
	"RTX 5090", "RTX 5080", "RTX 4090D", "RTX 4080", "RTX 6000Ada",
	"RTX 5880Ada", "RTX 5000Ada", "RTX 4500Ada", "RTX 4000Ada",
	"RTX PRO 6000 S", "RTX PRO 6000 WS", "RTX PRO 6000 Max-Q",
	"RTX A6000", "RTX A5000", "RTX A4500", "RTX A4000", "Tesla T4", "Tesla V100",
}

// VastFeed reads verified, rentable marketplace offers directly from Vast.ai.
// Vast's public endpoint allows five calls per window and caps a response at 64
// rows. A mixed-family query can therefore silently omit expensive GPUs and
// multi-GPU tuples. InferCrane instead refreshes exact one-GPU shards, four per
// sweep, leaving one request of rate-limit headroom. Each successful shard is
// independently complete and is published atomically.
//
// These rows are price observations only. A launch probe still owns account,
// quota, stock, and deployability decisions.
type VastFeed struct {
	APIKey           string
	BaseURL          string
	Client           *http.Client
	ValidFor         time.Duration
	Now              func() time.Time
	GPUQueries       []string
	ShardsPerRefresh int

	mu     sync.Mutex
	cursor int
}

func (f *VastFeed) Refresh(ctx context.Context, catalog *pricing.DynamicCatalog) error {
	if catalog == nil {
		return errors.New("GPU price catalog is nil")
	}
	if f == nil {
		return errors.New("Vast price feed is nil")
	}
	gpus := append([]string(nil), f.GPUQueries...)
	if len(gpus) == 0 {
		gpus = append(gpus, defaultVastGPUs...)
	}
	shards := f.ShardsPerRefresh
	if shards <= 0 {
		shards = defaultVastShardsPerSweep
	}
	if shards > defaultVastShardsPerSweep {
		shards = defaultVastShardsPerSweep
	}
	if shards > len(gpus) {
		shards = len(gpus)
	}

	// A feed instance owns one cursor and one request budget. Serializing refresh
	// calls prevents a startup refresh and timer refresh from sharing the same
	// five-call window.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cursor >= len(gpus) {
		f.cursor = 0
	}
	attemptBudget := defaultVastShardsPerSweep
	visited := 0
	var failures []error
	for visited < shards && attemptBudget > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		gpu := strings.TrimSpace(gpus[f.cursor])
		if gpu == "" {
			failures = append(failures, errors.New("Vast GPU query is empty"))
			f.cursor = (f.cursor + 1) % len(gpus)
			visited++
			continue
		}
		prices, err := f.fetchGPU(ctx, gpu, &attemptBudget)
		if err != nil {
			failures = append(failures, err)
		} else {
			catalog.ReplaceProviderGPU("vast", gpu, prices)
		}
		// A poisoned or unavailable shard must not block every GPU after it. Its
		// last-good row remains untouched and it is retried on the next rotation.
		f.cursor = (f.cursor + 1) % len(gpus)
		visited++
	}
	if len(failures) > 0 {
		return fmt.Errorf("Vast staged refresh had failed shards: %w", errors.Join(failures...))
	}
	return nil
}

func (f *VastFeed) fetchGPU(ctx context.Context, gpu string, attemptBudget *int) (map[pricing.Request]pricing.Estimate, error) {
	endpoint := strings.TrimSpace(f.BaseURL)
	if endpoint == "" {
		endpoint = defaultVastOffersURL
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Scheme != "https" && parsedEndpoint.Scheme != "http" {
		return nil, errors.New("Vast offer endpoint is invalid")
	}
	if strings.TrimSpace(f.APIKey) != "" && (parsedEndpoint.Scheme != "https" || !strings.EqualFold(parsedEndpoint.Hostname(), "console.vast.ai")) {
		return nil, errors.New("refusing to send Vast credentials to an untrusted endpoint")
	}
	body, _ := json.Marshal(map[string]any{
		"limit":             1,
		"type":              "on-demand",
		"verified":          map[string]bool{"eq": true},
		"external":          map[string]bool{"eq": false},
		"rentable":          map[string]bool{"eq": true},
		"rented":            map[string]bool{"eq": false},
		"gpu_name":          map[string]string{"eq": gpu},
		"num_gpus":          map[string]int{"eq": 1},
		"allocated_storage": 5,
		"order":             [][]string{{"dph_total", "asc"}},
	})
	data, err := f.request(ctx, endpoint, body, gpu, attemptBudget)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Offers []struct {
			ID           int64   `json:"id"`
			GPUName      string  `json:"gpu_name"`
			GPUs         int     `json:"num_gpus"`
			Hourly       float64 `json:"dph_total"`
			Rentable     bool    `json:"rentable"`
			Rented       bool    `json:"rented"`
			Verification string  `json:"verification"`
		}
	}
	if err = json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode Vast %s offers: %w", gpu, err)
	}
	now := time.Now().UTC()
	if f.Now != nil {
		now = f.Now().UTC()
	}
	validFor := f.ValidFor
	if validFor <= 0 {
		validFor = 30 * time.Minute
	}
	prices := make(map[pricing.Request]pricing.Estimate)
	for _, offer := range payload.Offers {
		if !strings.EqualFold(strings.TrimSpace(offer.GPUName), gpu) || offer.GPUs != 1 || offer.Hourly <= 0 || !offer.Rentable || offer.Rented || !strings.EqualFold(offer.Verification, "verified") {
			continue
		}
		key := pricing.Request{Cloud: "vast", Region: "global", GPU: gpu, GPUCount: 1, Replicas: 1}
		prices[key] = pricing.Estimate{
			Currency: "USD", Hourly: offer.Hourly,
			Source:     defaultVastOffersURL + "#offer-" + strconv.FormatInt(offer.ID, 10),
			ObservedAt: now, StaleAfter: validFor,
		}
		break
	}
	return prices, nil
}

func (f *VastFeed) request(ctx context.Context, endpoint string, body []byte, gpu string, attemptBudget *int) ([]byte, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	for attempt := 0; attempt < 3; attempt++ {
		if attemptBudget == nil || *attemptBudget <= 0 {
			return nil, fmt.Errorf("Vast %s offer request budget exhausted", gpu)
		}
		*attemptBudget--
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("prepare Vast %s offer request: %w", gpu, err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", "InferCrane-GPU-Market/1.0")
		if strings.TrimSpace(f.APIKey) != "" {
			request.Header.Set("Authorization", "Bearer "+f.APIKey)
		}
		response, err := client.Do(request)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt < 2 && waitContext(ctx, time.Duration(attempt+1)*250*time.Millisecond) {
				continue
			}
			return nil, fmt.Errorf("fetch Vast %s offers: %w", gpu, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		response.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read Vast %s offers: %w", gpu, readErr)
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return data, nil
		}
		if attempt < 2 && retryableVastStatus(response.StatusCode) {
			delay, ok := vastRetryDelay(response, attempt)
			if ok && waitContext(ctx, delay) {
				continue
			}
		}
		return nil, fmt.Errorf("Vast %s offers returned HTTP %d", gpu, response.StatusCode)
	}
	return nil, fmt.Errorf("Vast %s offers exhausted retries", gpu)
}

func retryableVastStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func vastRetryDelay(response *http.Response, attempt int) (time.Duration, bool) {
	if response.StatusCode != http.StatusTooManyRequests {
		return time.Duration(attempt+1) * 250 * time.Millisecond, true
	}
	if value := strings.TrimSpace(response.Header.Get("Retry-After")); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
			return time.Duration(seconds)*time.Second + 100*time.Millisecond, true
		}
		if when, err := http.ParseTime(value); err == nil {
			return time.Until(when) + 100*time.Millisecond, true
		}
	}
	if value := strings.TrimSpace(response.Header.Get("X-RateLimit-Reset")); value != "" {
		if reset, err := strconv.ParseFloat(value, 64); err == nil {
			delay := time.Until(time.Unix(int64(reset), 0)) + 100*time.Millisecond
			if delay < 100*time.Millisecond {
				delay = 100 * time.Millisecond
			}
			return delay, true
		}
	}
	// A blind retry spends more of the same window. Let the scheduled runner
	// retry instead when Vast does not state when the quota resets.
	return 0, false
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
