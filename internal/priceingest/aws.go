package priceingest

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

const (
	defaultAWSPriceBaseURL = "https://pricing.us-east-1.amazonaws.com"
	awsRegionIndexPath     = "/offers/v1.0/aws/AmazonEC2/current/region_index.json"
	defaultAWSBodyLimit    = int64(384 << 20)
	hardAWSBodyLimit       = int64(512 << 20)
	maxAWSRegions          = 4
	maxAWSConcurrency      = 2
)

// AWSFeed reads current Linux, shared-tenancy, On-Demand EC2 GPU prices from
// AWS's public Price List Bulk API. Price evidence does not imply that an
// instance is in stock, within account quota, or deployable; the launch probe
// owns those checks.
//
// The regional EC2 files are large. A feed therefore refreshes only explicitly
// selected regions (one conservative default), streams CSV instead of holding
// the file in memory, enforces a hard response cap, and reuses a last-good
// region when AWS's versioned URL has not changed.
type AWSFeed struct {
	BaseURL        string
	Regions        []string
	Client         *http.Client
	ValidFor       time.Duration
	MaxBodyBytes   int64
	MaxConcurrency int
	Now            func() time.Time
}

func (f AWSFeed) Refresh(ctx context.Context, catalog *pricing.DynamicCatalog) error {
	if catalog == nil {
		return errors.New("GPU price catalog is nil")
	}
	base, err := awsPriceBaseURL(f.BaseURL)
	if err != nil {
		return err
	}
	regions, err := normalizedAWSRegions(f.Regions)
	if err != nil {
		return err
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	indexURL := base.ResolveReference(&url.URL{Path: awsRegionIndexPath})
	regionURLs, err := fetchAWSRegionURLs(ctx, client, base, indexURL, regions)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	if f.Now != nil {
		now = f.Now().UTC()
	}
	validFor := f.ValidFor
	if validFor <= 0 {
		validFor = 6 * time.Hour
	}
	bodyLimit := f.MaxBodyBytes
	if bodyLimit <= 0 {
		bodyLimit = defaultAWSBodyLimit
	}
	if bodyLimit > hardAWSBodyLimit {
		return fmt.Errorf("AWS price response limit exceeds %d bytes", hardAWSBodyLimit)
	}
	concurrency := f.MaxConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	if concurrency > maxAWSConcurrency {
		concurrency = maxAWSConcurrency
	}

	existing := catalog.Snapshot()
	type result struct {
		prices map[pricing.Request]pricing.Estimate
		err    error
	}
	results := make(chan result, len(regions))
	semaphore := make(chan struct{}, concurrency)
	for _, region := range regions {
		region, source := region, regionURLs[region]
		go func() {
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- result{err: ctx.Err()}
				return
			}
			if cached := currentAWSRegionSnapshot(existing, region, source, now, validFor); len(cached) > 0 {
				results <- result{prices: cached}
				return
			}
			prices, fetchErr := fetchAWSRegionPrices(ctx, client, source, region, now, validFor, bodyLimit)
			results <- result{prices: prices, err: fetchErr}
		}()
	}

	prices := make(map[pricing.Request]pricing.Estimate)
	for range regions {
		regionResult := <-results
		if regionResult.err != nil {
			// ReplaceProvider is deliberately below this loop: a failed region
			// cannot erase or partially replace the last complete snapshot.
			return regionResult.err
		}
		for request, estimate := range regionResult.prices {
			prices[request] = estimate
		}
	}
	if len(prices) == 0 {
		return errors.New("AWS returned no priced GPU instances")
	}
	catalog.ReplaceProvider("aws", prices)
	return nil
}

func awsPriceBaseURL(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		raw = defaultAWSPriceBaseURL
	}
	parsed, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return nil, errors.New("AWS price endpoint is invalid")
	}
	parsed.Path, parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", "", ""
	return parsed, nil
}

func normalizedAWSRegions(input []string) ([]string, error) {
	if len(input) == 0 {
		return []string{"us-east-1"}, nil
	}
	if len(input) > maxAWSRegions {
		return nil, fmt.Errorf("AWS price refresh is limited to %d regions", maxAWSRegions)
	}
	seen := make(map[string]struct{}, len(input))
	regions := make([]string, 0, len(input))
	for _, raw := range input {
		region := strings.ToLower(strings.TrimSpace(raw))
		if region == "" || strings.ContainsAny(region, "/?#") {
			return nil, fmt.Errorf("invalid AWS region %q", raw)
		}
		if _, duplicate := seen[region]; duplicate {
			continue
		}
		seen[region] = struct{}{}
		regions = append(regions, region)
	}
	if len(regions) == 0 {
		return nil, errors.New("AWS price refresh has no regions")
	}
	return regions, nil
}

func fetchAWSRegionURLs(ctx context.Context, client *http.Client, base, endpoint *url.URL, regions []string) (map[string]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("prepare AWS region index request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, errors.New("AWS region index request failed")
	}
	defer response.Body.Close()
	if !sameOrigin(endpoint, response.Request.URL) {
		return nil, errors.New("AWS region index redirected outside its configured origin")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("AWS region index returned HTTP %d", response.StatusCode)
	}
	const maxIndexBytes = int64(128 << 10)
	data, err := io.ReadAll(io.LimitReader(response.Body, maxIndexBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read AWS region index: %w", err)
	}
	if int64(len(data)) > maxIndexBytes {
		return nil, errors.New("AWS region index exceeded the response limit")
	}
	var payload struct {
		Regions map[string]struct {
			CurrentVersionURL string `json:"currentVersionUrl"`
		} `json:"regions"`
	}
	if err = json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("decode AWS region index: %w", err)
	}
	resolved := make(map[string]string, len(regions))
	for _, region := range regions {
		entry, ok := payload.Regions[region]
		if !ok || strings.TrimSpace(entry.CurrentVersionURL) == "" {
			return nil, fmt.Errorf("AWS price list has no region %s", region)
		}
		regionURL, err := url.Parse(entry.CurrentVersionURL)
		if err != nil {
			return nil, fmt.Errorf("AWS price list URL for %s is invalid", region)
		}
		regionURL = base.ResolveReference(regionURL)
		if !sameOrigin(base, regionURL) || !strings.HasPrefix(regionURL.Path, "/offers/v1.0/aws/AmazonEC2/") || !strings.HasSuffix(regionURL.Path, "/"+region+"/index.json") {
			return nil, fmt.Errorf("AWS price list URL for %s is outside the expected catalog", region)
		}
		regionURL.Path = strings.TrimSuffix(regionURL.Path, ".json") + ".csv"
		resolved[region] = regionURL.String()
	}
	return resolved, nil
}

func sameOrigin(left, right *url.URL) bool {
	return left != nil && right != nil && strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

func currentAWSRegionSnapshot(existing map[pricing.Request]pricing.Estimate, region, source string, now time.Time, validFor time.Duration) map[pricing.Request]pricing.Estimate {
	prices := make(map[pricing.Request]pricing.Estimate)
	for request, estimate := range existing {
		if request.Cloud != "aws" || request.Region != region || request.Replicas != 1 || estimate.Source != source || estimate.Hourly <= 0 || estimate.Currency != "USD" {
			continue
		}
		// The version URL proves AWS still publishes the same price list; it does
		// not create a new price observation. Preserve the original observation
		// time and extend only its verified-validity window so an unchanged
		// upstream artifact never looks newly generated.
		estimate.StaleAfter = now.Sub(estimate.ObservedAt) + validFor
		prices[request] = estimate
	}
	return prices
}

func fetchAWSRegionPrices(ctx context.Context, client *http.Client, source, region string, now time.Time, validFor time.Duration, bodyLimit int64) (map[pricing.Request]pricing.Estimate, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("prepare AWS price request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("AWS price request for %s failed", region)
	}
	defer response.Body.Close()
	sourceURL, _ := url.Parse(source)
	if !sameOrigin(sourceURL, response.Request.URL) {
		return nil, fmt.Errorf("AWS price list for %s redirected outside its configured origin", region)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("AWS prices for %s returned HTTP %d", region, response.StatusCode)
	}
	if response.ContentLength > bodyLimit {
		return nil, fmt.Errorf("AWS price list for %s exceeds the %d-byte response limit", region, bodyLimit)
	}

	limited := &io.LimitedReader{R: response.Body, N: bodyLimit + 1}
	reader := csv.NewReader(limited)
	reader.FieldsPerRecord = -1
	var columns map[string]int
	prices := make(map[pricing.Request]pricing.Estimate)
	for {
		record, readErr := reader.Read()
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if limited.N == 0 {
				return nil, fmt.Errorf("AWS price list for %s exceeds the %d-byte response limit", region, bodyLimit)
			}
			return nil, fmt.Errorf("decode AWS prices for %s: %w", region, readErr)
		}
		if columns == nil {
			if len(record) > 0 && record[0] == "SKU" {
				columns = make(map[string]int, len(record))
				for index, name := range record {
					columns[name] = index
				}
				for _, required := range []string{"TermType", "PricePerUnit", "Currency", "Product Family", "Instance Type", "Tenancy", "Operating System", "Pre Installed S/W", "operation", "CapacityStatus", "Region Code", "GPU"} {
					if _, ok := columns[required]; !ok {
						return nil, fmt.Errorf("AWS price list for %s is missing %q", region, required)
					}
				}
			}
			continue
		}
		addAWSPriceRow(prices, columns, record, source, region, now, validFor)
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("AWS price list for %s exceeds the %d-byte response limit", region, bodyLimit)
	}
	if columns == nil {
		return nil, fmt.Errorf("AWS price list for %s has no CSV header", region)
	}
	if len(prices) == 0 {
		return nil, fmt.Errorf("AWS returned no priced GPU instances for %s", region)
	}
	return prices, nil
}

func addAWSPriceRow(prices map[pricing.Request]pricing.Estimate, columns map[string]int, record []string, source, region string, now time.Time, validFor time.Duration) {
	value := func(name string) string {
		index := columns[name]
		if index < 0 || index >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[index])
	}
	if value("TermType") != "OnDemand" || value("Currency") != "USD" || value("Product Family") != "Compute Instance" ||
		value("Tenancy") != "Shared" || value("Operating System") != "Linux" || value("Pre Installed S/W") != "NA" ||
		value("operation") != "RunInstances" || value("CapacityStatus") != "Used" || value("Region Code") != region {
		return
	}
	instanceType := strings.ToLower(value("Instance Type"))
	gpu := awsGPUModel(instanceType)
	if gpu == "" {
		return
	}
	count, err := strconv.ParseFloat(value("GPU"), 64)
	if err != nil || count < 1 || count > 64 || math.Trunc(count) != count {
		// Fractional GPUs cannot be represented by pricing.Request.GPUCount.
		return
	}
	hourly, err := strconv.ParseFloat(value("PricePerUnit"), 64)
	if err != nil || hourly <= 0 || math.IsNaN(hourly) || math.IsInf(hourly, 0) {
		return
	}
	request := pricing.Request{Cloud: "aws", Region: region, GPU: gpu, GPUCount: int(count), Replicas: 1}
	estimate := pricing.Estimate{Currency: "USD", Hourly: hourly, CostScope: pricing.CostScopeInstanceTotal, Authority: pricing.PriceAuthorityProviderAPI, Source: source, ObservedAt: now, StaleAfter: validFor}
	if current, ok := prices[request]; !ok || estimate.Hourly < current.Hourly {
		prices[request] = estimate
	}
}

func awsGPUModel(instanceType string) string {
	family, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(instanceType)), ".")
	switch family {
	case "g2":
		return "NVIDIA GRID K520"
	case "g3", "g3s":
		return "NVIDIA M60"
	case "g4ad":
		return "AMD Radeon Pro V520"
	case "g4dn":
		return "NVIDIA T4"
	case "g5":
		return "NVIDIA A10G"
	case "g5g":
		return "NVIDIA T4G"
	case "g6", "g6f", "gr6", "gr6f":
		return "NVIDIA L4"
	case "g6e":
		return "NVIDIA L40S"
	case "g7":
		return "NVIDIA RTX PRO 4500 Blackwell Server Edition"
	case "g7e":
		return "NVIDIA RTX PRO 6000 Blackwell Server Edition"
	case "p2":
		return "NVIDIA K80"
	case "p3", "p3dn":
		return "NVIDIA V100"
	case "p4d":
		return "NVIDIA A100-SXM4-40GB"
	case "p4de":
		return "NVIDIA A100-SXM4-80GB"
	case "p5":
		return "NVIDIA H100 80GB HBM3"
	case "p5e", "p5en":
		return "NVIDIA H200"
	case "p6-b200", "p6e-gb200":
		return "NVIDIA B200"
	case "p6-b300":
		return "NVIDIA B300"
	default:
		return ""
	}
}
