// Package priceingest imports sourced GPU price observations without treating
// catalog presence as capacity, quota, or deployability evidence.
package priceingest

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

const defaultMaxBytes int64 = 8 << 20

func SkyPilotSources() map[string]string {
	const root = "https://raw.githubusercontent.com/skypilot-org/skypilot-catalog/master/catalogs/v8/"
	return map[string]string{
		"cudo":           root + "cudo/vms.csv",
		"fluidstack":     root + "fluidstack/vms.csv",
		"hyperbolic":     root + "hyperbolic/vms.csv",
		"lambda":         root + "lambda/vms.csv",
		"primeintellect": root + "primeintellect/vms.csv",
		"runpod":         root + "runpod/vms.csv",
		"vast":           root + "vast/vms.csv",
	}
}

type Feed struct {
	Client   *http.Client
	Sources  map[string]string
	ValidFor time.Duration
	MaxBytes int64
	Now      func() time.Time
}

func (f Feed) Refresh(ctx context.Context, catalog *pricing.DynamicCatalog) error {
	if catalog == nil {
		return errors.New("GPU price catalog is nil")
	}
	sources := f.Sources
	if len(sources) == 0 {
		sources = SkyPilotSources()
	}
	providers := make([]string, 0, len(sources))
	for provider := range sources {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	var failures []error
	succeeded := 0
	for _, provider := range providers {
		prices, err := f.fetch(ctx, provider, sources[provider])
		if err != nil {
			failures = append(failures, err)
			continue
		}
		catalog.ReplaceProvider(provider, prices)
		succeeded++
	}
	if succeeded == 0 && len(failures) > 0 {
		return fmt.Errorf("all GPU price sources failed: %w", errors.Join(failures...))
	}
	if len(failures) > 0 {
		return fmt.Errorf("GPU price refresh was partial: %w", errors.Join(failures...))
	}
	return nil
}

func (f Feed) fetch(ctx context.Context, provider, source string) (map[pricing.Request]pricing.Estimate, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("prepare %s catalog request: %w", provider, err)
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch %s catalog: %w", provider, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s catalog: HTTP %d", provider, response.StatusCode)
	}
	maxBytes := f.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	observedAt := time.Now().UTC()
	if f.Now != nil {
		observedAt = f.Now().UTC()
	}
	validFor := f.ValidFor
	if validFor <= 0 {
		validFor = 2 * time.Hour
	}
	return parse(provider, source, observedAt, validFor, io.LimitReader(response.Body, maxBytes+1))
}

func parse(provider, source string, observedAt time.Time, validFor time.Duration, input io.Reader) (map[pricing.Request]pricing.Estimate, error) {
	reader := csv.NewReader(input)
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read %s catalog header: %w", provider, err)
	}
	columns := map[string]int{}
	for index, name := range header {
		columns[strings.TrimSpace(name)] = index
	}
	for _, required := range []string{"AcceleratorName", "AcceleratorCount", "Region", "Price"} {
		if _, ok := columns[required]; !ok {
			return nil, fmt.Errorf("%s catalog is missing %s", provider, required)
		}
	}
	prices := map[pricing.Request]pricing.Estimate{}
	for {
		record, readErr := reader.Read()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read %s catalog row: %w", provider, readErr)
		}
		gpu := strings.TrimSpace(record[columns["AcceleratorName"]])
		region := strings.TrimSpace(record[columns["Region"]])
		count, countErr := strconv.ParseFloat(strings.TrimSpace(record[columns["AcceleratorCount"]]), 64)
		hourly, priceErr := strconv.ParseFloat(strings.TrimSpace(record[columns["Price"]]), 64)
		gpuCount := int(count)
		if gpu == "" || countErr != nil || count != float64(gpuCount) || gpuCount < 1 || priceErr != nil || hourly <= 0 {
			continue
		}
		if region == "" {
			region = "global"
		}
		key := pricing.Request{Cloud: provider, Region: region, GPU: gpu, GPUCount: gpuCount, Replicas: 1}
		estimate := pricing.Estimate{Currency: "USD", Source: source, Hourly: hourly, ObservedAt: observedAt, StaleAfter: validFor}
		if current, ok := prices[key]; !ok || estimate.Hourly < current.Hourly {
			prices[key] = estimate
		}
	}
	if len(prices) == 0 {
		return nil, fmt.Errorf("%s catalog contained no positive on-demand GPU prices", provider)
	}
	return prices, nil
}

func Run(ctx context.Context, feed Feed, catalog *pricing.DynamicCatalog, interval time.Duration, report func(error)) {
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := feed.Refresh(ctx, catalog); err != nil && report != nil {
				report(err)
			}
		}
	}
}
