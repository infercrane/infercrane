package priceingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

const (
	defaultAzureRetailPricesURL = "https://prices.azure.com/api/retail/prices"
	defaultAzureRequestLimit    = 12
	maximumAzureRequestLimit    = 16
	maximumAzureResponseBytes   = 8 << 20
	azureRefreshTimeout         = 30 * time.Second
)

type azureGPUShape struct {
	GPU   string
	Count int
}

// reviewedAzureGPUSKUs is deliberately exact. Azure VM names encode both the
// accelerator and the number of physical GPUs, and guessing from a family
// prefix can turn a fractional GPU or a new SKU into a false comparable. Add a
// SKU only after reviewing its accelerator table in the Azure VM size docs.
var reviewedAzureGPUSKUs = map[string]azureGPUShape{
	"Standard_NC4as_T4_v3":      {GPU: "T4", Count: 1},
	"Standard_NC8as_T4_v3":      {GPU: "T4", Count: 1},
	"Standard_NC16as_T4_v3":     {GPU: "T4", Count: 1},
	"Standard_NC64as_T4_v3":     {GPU: "T4", Count: 4},
	"Standard_NC6s_v3":          {GPU: "V100-16GB", Count: 1},
	"Standard_NC12s_v3":         {GPU: "V100-16GB", Count: 2},
	"Standard_NC24s_v3":         {GPU: "V100-16GB", Count: 4},
	"Standard_NC24rs_v3":        {GPU: "V100-16GB", Count: 4},
	"Standard_NC24ads_A100_v4":  {GPU: "A100-80GB", Count: 1},
	"Standard_NC48ads_A100_v4":  {GPU: "A100-80GB", Count: 2},
	"Standard_NC96ads_A100_v4":  {GPU: "A100-80GB", Count: 4},
	"Standard_NC40ads_H100_v5":  {GPU: "H100-NVL-94GB", Count: 1},
	"Standard_NC80adis_H100_v5": {GPU: "H100-NVL-94GB", Count: 2},
	"Standard_NCC40ads_H100_v5": {GPU: "H100-NVL-94GB", Count: 1},
	"Standard_ND96as_v4":        {GPU: "A100-40GB", Count: 8},
	"Standard_ND96asr_v4":       {GPU: "A100-40GB", Count: 8},
	"Standard_ND96ams_A100_v4":  {GPU: "A100-80GB", Count: 8},
	"Standard_ND96amsr_A100_v4": {GPU: "A100-80GB", Count: 8},
	"Standard_ND96isr_H100_v5":  {GPU: "H100-80GB", Count: 8},
	"Standard_ND96isr_H200_v5":  {GPU: "H200-141GB", Count: 8},
	"Standard_NV36ads_A10_v5":   {GPU: "A10-24GB", Count: 1},
	"Standard_NV36adms_A10_v5":  {GPU: "A10-24GB", Count: 1},
	"Standard_NV72ads_A10_v5":   {GPU: "A10-24GB", Count: 2},
	"Standard_NV12s_v3":         {GPU: "M60", Count: 1},
	"Standard_NV24s_v3":         {GPU: "M60", Count: 2},
	"Standard_NV48s_v3":         {GPU: "M60", Count: 4},
}

// AzureFeed reads public USD Linux pay-as-you-go VM prices from Microsoft's
// unauthenticated Retail Prices API. These are catalog observations only:
// stock, quota, subscription discounts, and launch deployability stay unknown.
type AzureFeed struct {
	BaseURL  string
	Client   *http.Client
	ValidFor time.Duration
	Now      func() time.Time
	// RequestLimit may lower the request budget in tests or constrained callers.
	// It cannot raise the hard production limit.
	RequestLimit int
}

type azureRetailPage struct {
	Items        []azureRetailItem `json:"Items"`
	NextPageLink string            `json:"NextPageLink"`
}

type azureRetailItem struct {
	CurrencyCode         string  `json:"currencyCode"`
	RetailPrice          float64 `json:"retailPrice"`
	UnitPrice            float64 `json:"unitPrice"`
	ArmRegionName        string  `json:"armRegionName"`
	EffectiveStartDate   string  `json:"effectiveStartDate"`
	EffectiveEndDate     string  `json:"effectiveEndDate"`
	MeterID              string  `json:"meterId"`
	MeterName            string  `json:"meterName"`
	ProductName          string  `json:"productName"`
	SKUName              string  `json:"skuName"`
	ServiceName          string  `json:"serviceName"`
	UnitOfMeasure        string  `json:"unitOfMeasure"`
	Type                 string  `json:"type"`
	ReservationTerm      string  `json:"reservationTerm"`
	IsPrimaryMeterRegion bool    `json:"isPrimaryMeterRegion"`
	ArmSKUName           string  `json:"armSkuName"`
}

type azureCandidate struct {
	estimate       pricing.Estimate
	effectiveStart time.Time
}

func (f AzureFeed) Refresh(ctx context.Context, catalog *pricing.DynamicCatalog) error {
	if catalog == nil {
		return errors.New("GPU price catalog is nil")
	}
	endpoint, err := f.endpoint()
	if err != nil {
		return err
	}
	queryURLs := azureRetailQueries(endpoint)
	requestLimit := f.RequestLimit
	if requestLimit <= 0 {
		requestLimit = defaultAzureRequestLimit
	}
	if requestLimit > maximumAzureRequestLimit {
		requestLimit = maximumAzureRequestLimit
	}

	refreshCtx, cancel := context.WithTimeout(ctx, azureRefreshTimeout)
	defer cancel()
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	now := time.Now().UTC()
	if f.Now != nil {
		now = f.Now().UTC()
	}
	validFor := f.ValidFor
	if validFor <= 0 {
		validFor = 5 * time.Minute
	}

	candidates := make(map[pricing.Request]azureCandidate)
	requests := 0
	for _, queryURL := range queryURLs {
		requestURL, parseErr := url.Parse(queryURL)
		if parseErr != nil {
			return errors.New("Azure Retail Prices query is invalid")
		}
		for {
			if requests >= requestLimit {
				return fmt.Errorf("Azure Retail Prices pagination exceeded the %d-request refresh budget", requestLimit)
			}
			requests++
			payload, fetchErr := fetchAzureRetailPage(refreshCtx, client, requestURL)
			if fetchErr != nil {
				return fetchErr
			}
			for _, item := range payload.Items {
				request, estimate, effectiveStart, ok := azurePrice(item, now, validFor, queryURL)
				if !ok {
					continue
				}
				prior, found := candidates[request]
				if !found || effectiveStart.After(prior.effectiveStart) || effectiveStart.Equal(prior.effectiveStart) && estimate.Hourly < prior.estimate.Hourly {
					candidates[request] = azureCandidate{estimate: estimate, effectiveStart: effectiveStart}
				}
			}
			if strings.TrimSpace(payload.NextPageLink) == "" {
				break
			}
			next, nextErr := url.Parse(payload.NextPageLink)
			if nextErr != nil || !sameAzureOrigin(endpoint, next) {
				return errors.New("Azure Retail Prices returned an untrusted pagination link")
			}
			requestURL = next
		}
	}

	prices := make(map[pricing.Request]pricing.Estimate, len(candidates))
	for request, candidate := range candidates {
		prices[request] = candidate.estimate
	}
	if len(prices) == 0 {
		return errors.New("Azure Retail Prices returned no current reviewed Linux GPU VM prices")
	}
	// Publish only after every bounded page succeeded. Any transport, schema, or
	// pagination failure therefore leaves the provider's last-good snapshot intact.
	catalog.ReplaceProvider("azure", prices)
	return nil
}

func (f AzureFeed) endpoint() (*url.URL, error) {
	value := strings.TrimSpace(f.BaseURL)
	if value == "" {
		value = defaultAzureRetailPricesURL
	}
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Scheme != "https" && endpoint.Scheme != "http" || endpoint.Host == "" {
		return nil, errors.New("Azure Retail Prices endpoint is invalid")
	}
	return endpoint, nil
}

func azureRetailQueries(endpoint *url.URL) []string {
	// Azure rejects a single large OR expression before its documented row
	// pagination applies. Four deterministic family shards keep URLs bounded;
	// RequestLimit remains the shared hard budget across all shard pages.
	groups := [][]string{{}, {}, {}, {}}
	skus := make([]string, 0, len(reviewedAzureGPUSKUs))
	for sku := range reviewedAzureGPUSKUs {
		skus = append(skus, sku)
	}
	sort.Strings(skus)
	for _, sku := range skus {
		group := 3
		switch {
		case strings.HasPrefix(sku, "Standard_ND"):
			group = 2
		case strings.HasPrefix(sku, "Standard_NV"):
			group = 3
		case strings.Contains(sku, "A100") || strings.Contains(sku, "H100"):
			group = 1
		default:
			group = 0
		}
		groups[group] = append(groups[group], sku)
	}
	queries := make([]string, 0, len(groups))
	for _, group := range groups {
		selectors := make([]string, 0, len(group))
		for _, sku := range group {
			selectors = append(selectors, "armSkuName eq '"+sku+"'")
		}
		filter := "serviceName eq 'Virtual Machines' and priceType eq 'Consumption' and (" + strings.Join(selectors, " or ") + ")"
		query := endpoint.Query()
		query.Set("currencyCode", "'USD'")
		query.Set("$filter", filter)
		cloned := *endpoint
		cloned.RawQuery = query.Encode()
		cloned.Fragment = ""
		queries = append(queries, cloned.String())
	}
	return queries
}

func fetchAzureRetailPage(ctx context.Context, client *http.Client, pageURL *url.URL) (azureRetailPage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL.String(), nil)
	if err != nil {
		return azureRetailPage{}, fmt.Errorf("prepare Azure Retail Prices request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "InferCrane-GPU-Market/1.0")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return azureRetailPage{}, ctx.Err()
		}
		return azureRetailPage{}, fmt.Errorf("fetch Azure Retail Prices: %w", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumAzureResponseBytes+1))
	if err != nil {
		return azureRetailPage{}, fmt.Errorf("read Azure Retail Prices: %w", err)
	}
	if len(data) > maximumAzureResponseBytes {
		return azureRetailPage{}, errors.New("Azure Retail Prices response exceeded the body limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return azureRetailPage{}, fmt.Errorf("Azure Retail Prices returned HTTP %d", response.StatusCode)
	}
	var payload azureRetailPage
	if err = json.Unmarshal(data, &payload); err != nil {
		return azureRetailPage{}, fmt.Errorf("decode Azure Retail Prices: %w", err)
	}
	return payload, nil
}

func azurePrice(item azureRetailItem, now time.Time, validFor time.Duration, queryURL string) (pricing.Request, pricing.Estimate, time.Time, bool) {
	shape, reviewed := reviewedAzureGPUSKUs[strings.TrimSpace(item.ArmSKUName)]
	region := strings.TrimSpace(item.ArmRegionName)
	if !reviewed || shape.Count < 1 || region == "" || !strings.EqualFold(strings.TrimSpace(item.CurrencyCode), "USD") ||
		!strings.EqualFold(strings.TrimSpace(item.ServiceName), "Virtual Machines") || !strings.EqualFold(strings.TrimSpace(item.Type), "Consumption") ||
		!strings.EqualFold(strings.TrimSpace(item.UnitOfMeasure), "1 Hour") || !item.IsPrimaryMeterRegion || strings.TrimSpace(item.ReservationTerm) != "" ||
		item.RetailPrice <= 0 || strings.Contains(strings.ToLower(item.ProductName), "windows") || azureDiscountMeter(item) {
		return pricing.Request{}, pricing.Estimate{}, time.Time{}, false
	}
	effectiveStart, err := time.Parse(time.RFC3339, strings.TrimSpace(item.EffectiveStartDate))
	if err != nil || effectiveStart.After(now) {
		return pricing.Request{}, pricing.Estimate{}, time.Time{}, false
	}
	if value := strings.TrimSpace(item.EffectiveEndDate); value != "" {
		effectiveEnd, endErr := time.Parse(time.RFC3339, value)
		if endErr != nil || !effectiveEnd.After(now) {
			return pricing.Request{}, pricing.Estimate{}, time.Time{}, false
		}
	}
	source, err := url.Parse(queryURL)
	if err != nil {
		return pricing.Request{}, pricing.Estimate{}, time.Time{}, false
	}
	// Source always names Microsoft's public API, even when tests inject a local
	// transport. The fragment identifies the exact selected meter and SKU while
	// the query records the reviewed PAYG request that produced it.
	official, _ := url.Parse(defaultAzureRetailPricesURL)
	official.RawQuery = source.RawQuery
	official.Fragment = url.Values{"arm_sku": {item.ArmSKUName}, "meter_id": {item.MeterID}}.Encode()
	request := pricing.Request{Cloud: "azure", Region: region, GPU: shape.GPU, GPUCount: shape.Count, Replicas: 1}
	estimate := pricing.Estimate{Currency: "USD", Hourly: item.RetailPrice, Source: official.String(), ObservedAt: now, StaleAfter: validFor}
	return request, estimate, effectiveStart.UTC(), true
}

func azureDiscountMeter(item azureRetailItem) bool {
	value := strings.ToLower(strings.TrimSpace(item.MeterName) + " " + strings.TrimSpace(item.SKUName))
	return strings.Contains(value, "spot") || strings.Contains(value, "low priority")
}

func sameAzureOrigin(endpoint, candidate *url.URL) bool {
	if endpoint == nil || candidate == nil || candidate.Scheme != "https" && candidate.Scheme != "http" {
		return false
	}
	return strings.EqualFold(endpoint.Scheme, candidate.Scheme) && strings.EqualFold(endpoint.Host, candidate.Host)
}
