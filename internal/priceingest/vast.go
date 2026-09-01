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

const defaultVastOffersURL = "https://console.vast.ai/api/v0/bundles/"

var defaultVastGPUQueryGroups = [][]string{
	{"H100 NVL", "H100 PCIE", "H100 SXM", "H200", "H200 NVL", "B200", "B300"},
	{"A10", "A100 PCIE", "A100 SXM4", "A40"},
	{"L4", "L40", "L40S"},
	{"RTX 3090", "RTX 3090 Ti", "RTX 4090", "RTX 4090D", "RTX 5090"},
	{"RTX 6000Ada", "RTX A6000"},
}

// VastFeed reads current verified marketplace offers directly from Vast.ai.
// The returned rows are price observations only. A launch probe still owns the
// account, quota, and deployability decision.
type VastFeed struct {
	BaseURL    string
	Client     *http.Client
	ValidFor   time.Duration
	Now        func() time.Time
	GPUQueries []string
	// GPUQueryGroups lets one Vast request cover a similarly priced family.
	// Vast currently permits five requests per window and caps each response at
	// 64 offers, so the production defaults deliberately use exactly five
	// price-band groups instead of one request per accelerator.
	GPUQueryGroups [][]string
	Workers        int
}

func (f VastFeed) Refresh(ctx context.Context, catalog *pricing.DynamicCatalog) error {
	if catalog == nil {
		return errors.New("GPU price catalog is nil")
	}
	groups := cloneGPUQueryGroups(f.GPUQueryGroups)
	if len(groups) == 0 && len(f.GPUQueries) > 0 {
		for _, gpu := range f.GPUQueries {
			groups = append(groups, []string{gpu})
		}
	}
	if len(groups) == 0 {
		groups = cloneGPUQueryGroups(defaultVastGPUQueryGroups)
	}
	workers := f.Workers
	if workers <= 0 {
		// Vast's public marketplace API is intentionally queried serially. The
		// production catalog uses its full five-request window, so concurrency
		// would turn a healthy refresh into a burst that is easy to throttle.
		workers = 1
	}
	if workers > len(groups) {
		workers = len(groups)
	}

	type result struct {
		prices map[pricing.Request]pricing.Estimate
		err    error
	}
	jobs := make(chan []string)
	results := make(chan result, len(groups))
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for gpus := range jobs {
				prices, err := f.fetchGPUs(ctx, gpus)
				results <- result{prices: prices, err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, gpus := range groups {
			select {
			case jobs <- gpus:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		group.Wait()
		close(results)
	}()

	prices := make(map[pricing.Request]pricing.Estimate)
	var failures []error
	for item := range results {
		if item.err != nil {
			failures = append(failures, item.err)
			continue
		}
		for request, estimate := range item.prices {
			if current, ok := prices[request]; !ok || estimate.Hourly < current.Hourly {
				prices[request] = estimate
			}
		}
	}
	// Publish atomically. A partial provider sweep must never delete a last-good
	// row and present the surviving subset as a complete current market.
	if len(failures) > 0 {
		return fmt.Errorf("Vast offer refresh was incomplete: %w", errors.Join(failures...))
	}
	if len(prices) == 0 {
		return errors.New("Vast returned no verified rentable GPU offers")
	}
	catalog.ReplaceProvider("vast", prices)
	return nil
}

func (f VastFeed) fetchGPUs(ctx context.Context, gpus []string) (map[pricing.Request]pricing.Estimate, error) {
	if len(gpus) == 0 {
		return nil, errors.New("Vast GPU query group is empty")
	}
	endpoint := strings.TrimSpace(f.BaseURL)
	if endpoint == "" {
		endpoint = defaultVastOffersURL
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Scheme != "https" && parsedEndpoint.Scheme != "http" {
		return nil, errors.New("Vast offer endpoint is invalid")
	}
	gpuFilter := map[string]any{"in": gpus}
	if len(gpus) == 1 {
		gpuFilter = map[string]any{"eq": gpus[0]}
	}
	body, _ := json.Marshal(map[string]any{
		"limit":    64,
		"type":     "ondemand",
		"verified": map[string]bool{"eq": true},
		"rentable": map[string]bool{"eq": true},
		"rented":   map[string]bool{"eq": false},
		"gpu_name": gpuFilter,
		"order":    [][]string{{"dph_total", "asc"}},
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("prepare Vast %s offer request: %w", strings.Join(gpus, ", "), err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "InferCrane-GPU-Market/1.0")
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch Vast %s offers: %w", strings.Join(gpus, ", "), err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read Vast %s offers: %w", strings.Join(gpus, ", "), err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Vast %s offers returned HTTP %d", strings.Join(gpus, ", "), response.StatusCode)
	}
	var payload struct {
		Offers []struct {
			ID           int64   `json:"id"`
			GPUName      string  `json:"gpu_name"`
			GPUs         int     `json:"num_gpus"`
			Hourly       float64 `json:"dph_total"`
			Geolocation  string  `json:"geolocation"`
			Rentable     bool    `json:"rentable"`
			Rented       bool    `json:"rented"`
			Verification string  `json:"verification"`
		}
	}
	if err = json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode Vast %s offers: %w", strings.Join(gpus, ", "), err)
	}
	now := time.Now().UTC()
	if f.Now != nil {
		now = f.Now().UTC()
	}
	validFor := f.ValidFor
	if validFor <= 0 {
		validFor = 10 * time.Minute
	}
	prices := make(map[pricing.Request]pricing.Estimate)
	allowed := make(map[string]struct{}, len(gpus))
	for _, gpu := range gpus {
		allowed[strings.ToLower(strings.TrimSpace(gpu))] = struct{}{}
	}
	for _, offer := range payload.Offers {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(offer.GPUName))]; !ok || offer.GPUs < 1 || offer.Hourly <= 0 || !offer.Rentable || offer.Rented || !strings.EqualFold(offer.Verification, "verified") {
			continue
		}
		region := vastRegion(offer.Geolocation)
		key := pricing.Request{Cloud: "vast", Region: region, GPU: offer.GPUName, GPUCount: offer.GPUs, Replicas: 1}
		estimate := pricing.Estimate{
			Currency: "USD", Hourly: offer.Hourly,
			Source:     defaultVastOffersURL + "#offer-" + strconv.FormatInt(offer.ID, 10),
			ObservedAt: now, StaleAfter: validFor,
		}
		if current, ok := prices[key]; !ok || estimate.Hourly < current.Hourly {
			prices[key] = estimate
		}
	}
	return prices, nil
}

func cloneGPUQueryGroups(groups [][]string) [][]string {
	cloned := make([][]string, 0, len(groups))
	for _, group := range groups {
		if len(group) > 0 {
			cloned = append(cloned, append([]string(nil), group...))
		}
	}
	return cloned
}

func vastRegion(geolocation string) string {
	parts := strings.Split(geolocation, ",")
	for index := len(parts) - 1; index >= 0; index-- {
		if value := strings.TrimSpace(parts[index]); value != "" {
			return strings.ToUpper(value)
		}
	}
	return "global"
}
