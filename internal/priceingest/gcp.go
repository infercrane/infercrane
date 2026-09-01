package priceingest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
)

const (
	defaultGCPCatalogURL      = "https://cloudbilling.googleapis.com/v1/services/6F81-5844-456A/skus"
	defaultGCPPageSize        = 5000
	defaultGCPRequestLimit    = 12
	maximumGCPRequestLimit    = 16
	maximumGCPResponseBytes   = 32 << 20
	gcpCatalogRefreshTimeout  = 2 * time.Minute
	gcpComputeEngineServiceID = "6F81-5844-456A"
)

var (
	gcpSKUIDPattern  = regexp.MustCompile(`^[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}$`)
	gcpRegionPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)+[0-9]$`)
)

// reviewedGCPGPUDescriptions maps the exact, first-party Compute Engine billing
// description stem to the accelerator resource identifier accepted by the GCP
// Compute API. A prefix or model substring is intentionally insufficient: new
// virtual-workstation, Spot, DWS, or differently provisioned SKUs must be
// reviewed before they can become comparable price evidence.
var reviewedGCPGPUDescriptions = map[string]string{
	"Nvidia Tesla T4 GPU":          "nvidia-tesla-t4",
	"Nvidia Tesla P4 GPU":          "nvidia-tesla-p4",
	"Nvidia Tesla P100 GPU":        "nvidia-tesla-p100",
	"Nvidia Tesla V100 GPU":        "nvidia-tesla-v100",
	"Nvidia Tesla A100 GPU":        "nvidia-tesla-a100",
	"Nvidia Tesla A100 80GB GPU":   "nvidia-a100-80gb",
	"Nvidia L4 GPU":                "nvidia-l4",
	"Nvidia H100 80GB GPU":         "nvidia-h100-80gb",
	"Nvidia H100 80GB Mega GPU":    "nvidia-h100-mega-80gb",
	"H200 141GB GPU":               "nvidia-h200-141gb",
	"A4 Nvidia B200 (1 gpu slice)": "nvidia-b200",
	"RTX 6000 96GB":                "nvidia-rtx-pro-6000",
}

// GCPFeed reads current public USD on-demand GPU-device rates from the Google
// Cloud Billing Catalog API. These rates are billing components, not complete
// VM prices: accelerator-optimized VMs can also bill CPU, RAM, local SSD, and
// networking. Catalog publication is likewise not evidence of stock, quota, or
// exact-zone deployability; the GCP launch probe owns those checks.
//
// Google's public catalog requires an API consumer identity. APIKey should be a
// restricted key for cloudbilling.googleapis.com. BaseURL exists for tests; a
// real API key is never sent anywhere except the official HTTPS origin.
type GCPFeed struct {
	APIKey       string
	BaseURL      string
	Client       *http.Client
	ValidFor     time.Duration
	Now          func() time.Time
	RequestLimit int
}

type gcpCatalogPage struct {
	SKUs          []gcpCatalogSKU `json:"skus"`
	NextPageToken string          `json:"nextPageToken"`
}

type gcpCatalogSKU struct {
	Name                string           `json:"name"`
	SKUID               string           `json:"skuId"`
	Description         string           `json:"description"`
	ServiceRegions      []string         `json:"serviceRegions"`
	ServiceProviderName string           `json:"serviceProviderName"`
	Category            gcpSKUCategory   `json:"category"`
	PricingInfo         []gcpPricingInfo `json:"pricingInfo"`
	GeoTaxonomy         struct {
		Type    string   `json:"type"`
		Regions []string `json:"regions"`
	} `json:"geoTaxonomy"`
}

type gcpSKUCategory struct {
	ServiceDisplayName string `json:"serviceDisplayName"`
	ResourceFamily     string `json:"resourceFamily"`
	ResourceGroup      string `json:"resourceGroup"`
	UsageType          string `json:"usageType"`
}

type gcpPricingInfo struct {
	EffectiveTime     string               `json:"effectiveTime"`
	PricingExpression gcpPricingExpression `json:"pricingExpression"`
}

type gcpPricingExpression struct {
	UsageUnit       string        `json:"usageUnit"`
	DisplayQuantity float64       `json:"displayQuantity"`
	TieredRates     []gcpTierRate `json:"tieredRates"`
}

type gcpTierRate struct {
	StartUsageAmount float64  `json:"startUsageAmount"`
	UnitPrice        gcpMoney `json:"unitPrice"`
}

type gcpMoney struct {
	CurrencyCode string `json:"currencyCode"`
	Units        string `json:"units"`
	Nanos        int64  `json:"nanos"`
}

type gcpCandidate struct {
	estimate  pricing.Estimate
	effective time.Time
}

func (f GCPFeed) Refresh(ctx context.Context, catalog *pricing.DynamicCatalog) error {
	if catalog == nil {
		return errors.New("GPU price catalog is nil")
	}
	endpoint, official, err := gcpCatalogEndpoint(f.BaseURL)
	if err != nil {
		return err
	}
	apiKey := strings.TrimSpace(f.APIKey)
	if official && apiKey == "" {
		return errors.New("Google Cloud Billing Catalog API key is required")
	}
	if apiKey != "" && !official {
		return errors.New("refusing to send Google Cloud Billing credentials to an untrusted endpoint")
	}
	requestLimit := f.RequestLimit
	if requestLimit <= 0 {
		requestLimit = defaultGCPRequestLimit
	}
	if requestLimit > maximumGCPRequestLimit {
		requestLimit = maximumGCPRequestLimit
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	now := time.Now().UTC()
	if f.Now != nil {
		now = f.Now().UTC()
	}
	validFor := f.ValidFor
	if validFor <= 0 {
		validFor = 6 * time.Hour
	}

	refreshCtx, cancel := context.WithTimeout(ctx, gcpCatalogRefreshTimeout)
	defer cancel()
	candidates := make(map[pricing.Request]gcpCandidate)
	seenTokens := map[string]struct{}{"": {}}
	pageToken := ""
	for page := 0; ; page++ {
		if page >= requestLimit {
			return fmt.Errorf("Google Cloud Billing Catalog pagination exceeded the %d-request refresh budget", requestLimit)
		}
		pageURL := *endpoint
		query := pageURL.Query()
		query.Set("currencyCode", "USD")
		query.Set("pageSize", strconv.Itoa(defaultGCPPageSize))
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		if apiKey != "" {
			query.Set("key", apiKey)
		}
		pageURL.RawQuery = query.Encode()
		payload, fetchErr := fetchGCPCatalogPage(refreshCtx, client, endpoint, &pageURL)
		if fetchErr != nil {
			return fetchErr
		}
		for _, sku := range payload.SKUs {
			gpu, ok := reviewedGCPGPU(sku.Description)
			if !ok || !validGCPCatalogSKU(sku) {
				continue
			}
			hourly, effective, ok := currentGCPHourlyPrice(sku.PricingInfo, now)
			if !ok {
				continue
			}
			fragment := url.Values{"billing_component": {"gpu"}, "sku_id": {sku.SKUID}}.Encode()
			source := defaultGCPCatalogURL + "?currencyCode=USD&pageSize=" + strconv.Itoa(defaultGCPPageSize) + "#" + fragment
			for _, region := range gcpSKURegions(sku) {
				request := pricing.Request{Cloud: "gcp", Region: region, GPU: gpu, GPUCount: 1, Replicas: 1}
				estimate := pricing.Estimate{Currency: "USD", Hourly: hourly, Source: source, ObservedAt: now, StaleAfter: validFor}
				prior, found := candidates[request]
				if !found || effective.After(prior.effective) || effective.Equal(prior.effective) && estimate.Hourly < prior.estimate.Hourly {
					candidates[request] = gcpCandidate{estimate: estimate, effective: effective}
				}
			}
		}
		next := strings.TrimSpace(payload.NextPageToken)
		if next == "" {
			break
		}
		if len(next) > 4096 {
			return errors.New("Google Cloud Billing Catalog returned an oversized page token")
		}
		if _, duplicate := seenTokens[next]; duplicate {
			return errors.New("Google Cloud Billing Catalog returned a repeated page token")
		}
		seenTokens[next] = struct{}{}
		pageToken = next
	}

	prices := make(map[pricing.Request]pricing.Estimate, len(candidates))
	for request, candidate := range candidates {
		prices[request] = candidate.estimate
	}
	if len(prices) == 0 {
		return errors.New("Google Cloud Billing Catalog returned no current reviewed on-demand GPU prices")
	}
	// Publication is deliberately below the complete pagination loop. A partial,
	// oversized, timed-out, or unauthenticated refresh keeps the last-good GCP
	// snapshot byte-for-byte intact.
	catalog.ReplaceProvider("gcp", prices)
	return nil
}

func gcpCatalogEndpoint(raw string) (*url.URL, bool, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		value = defaultGCPCatalogURL
	}
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Scheme != "https" && endpoint.Scheme != "http" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, false, errors.New("Google Cloud Billing Catalog endpoint is invalid")
	}
	official := strings.EqualFold(endpoint.Scheme, "https") && strings.EqualFold(endpoint.Hostname(), "cloudbilling.googleapis.com") && effectivePort(endpoint) == "443" && endpoint.Path == "/v1/services/"+gcpComputeEngineServiceID+"/skus"
	return endpoint, official, nil
}

func fetchGCPCatalogPage(ctx context.Context, client *http.Client, origin, pageURL *url.URL) (gcpCatalogPage, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL.String(), nil)
	if err != nil {
		return gcpCatalogPage{}, fmt.Errorf("prepare Google Cloud Billing Catalog request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "InferCrane-GPU-Market/1.0")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return gcpCatalogPage{}, ctx.Err()
		}
		return gcpCatalogPage{}, fmt.Errorf("fetch Google Cloud Billing Catalog: %w", err)
	}
	defer response.Body.Close()
	if !sameAzureOrigin(origin, response.Request.URL) {
		return gcpCatalogPage{}, errors.New("Google Cloud Billing Catalog redirected outside its configured origin")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumGCPResponseBytes+1))
	if err != nil {
		return gcpCatalogPage{}, fmt.Errorf("read Google Cloud Billing Catalog: %w", err)
	}
	if len(data) > maximumGCPResponseBytes {
		return gcpCatalogPage{}, errors.New("Google Cloud Billing Catalog response exceeded the body limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return gcpCatalogPage{}, fmt.Errorf("Google Cloud Billing Catalog returned HTTP %d", response.StatusCode)
	}
	var payload gcpCatalogPage
	if err = json.Unmarshal(data, &payload); err != nil {
		return gcpCatalogPage{}, fmt.Errorf("decode Google Cloud Billing Catalog: %w", err)
	}
	return payload, nil
}

func reviewedGCPGPU(description string) (string, bool) {
	description = strings.TrimSpace(description)
	for stem, gpu := range reviewedGCPGPUDescriptions {
		if strings.HasPrefix(description, stem+" running in ") && len(strings.TrimSpace(strings.TrimPrefix(description, stem+" running in "))) > 0 {
			return gpu, true
		}
	}
	return "", false
}

func validGCPCatalogSKU(sku gcpCatalogSKU) bool {
	return gcpSKUIDPattern.MatchString(strings.TrimSpace(sku.SKUID)) &&
		strings.TrimSpace(sku.Name) == "services/"+gcpComputeEngineServiceID+"/skus/"+strings.TrimSpace(sku.SKUID) &&
		strings.EqualFold(strings.TrimSpace(sku.ServiceProviderName), "Google") &&
		strings.EqualFold(strings.TrimSpace(sku.Category.ServiceDisplayName), "Compute Engine") &&
		strings.EqualFold(strings.TrimSpace(sku.Category.ResourceFamily), "Compute") &&
		strings.EqualFold(strings.TrimSpace(sku.Category.UsageType), "OnDemand")
}

func currentGCPHourlyPrice(infos []gcpPricingInfo, now time.Time) (float64, time.Time, bool) {
	selected := -1
	var selectedAt time.Time
	for index, info := range infos {
		effective, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(info.EffectiveTime))
		if err != nil || effective.After(now) {
			continue
		}
		if effective.Equal(selectedAt) {
			// The latest catalog endpoint documents a chronological timeline.
			// Conflicting entries at the same instant are not safe to guess between.
			return 0, time.Time{}, false
		}
		if selected < 0 || effective.After(selectedAt) {
			selected, selectedAt = index, effective.UTC()
		}
	}
	if selected < 0 {
		return 0, time.Time{}, false
	}
	expression := infos[selected].PricingExpression
	if expression.UsageUnit != "h" || expression.DisplayQuantity != 1 || len(expression.TieredRates) != 1 || expression.TieredRates[0].StartUsageAmount != 0 {
		return 0, time.Time{}, false
	}
	money := expression.TieredRates[0].UnitPrice
	if money.CurrencyCode != "USD" || money.Nanos < 0 || money.Nanos > 999999999 {
		return 0, time.Time{}, false
	}
	units, err := strconv.ParseInt(money.Units, 10, 64)
	if err != nil || units < 0 {
		return 0, time.Time{}, false
	}
	hourly := float64(units) + float64(money.Nanos)/1e9
	if hourly <= 0 || math.IsNaN(hourly) || math.IsInf(hourly, 0) {
		return 0, time.Time{}, false
	}
	return hourly, selectedAt, true
}

func gcpSKURegions(sku gcpCatalogSKU) []string {
	regions := sku.GeoTaxonomy.Regions
	if len(regions) == 0 {
		regions = sku.ServiceRegions
	}
	seen := make(map[string]struct{}, len(regions))
	result := make([]string, 0, len(regions))
	for _, raw := range regions {
		region := strings.ToLower(strings.TrimSpace(raw))
		if !gcpRegionPattern.MatchString(region) {
			continue
		}
		if _, duplicate := seen[region]; duplicate {
			continue
		}
		seen[region] = struct{}{}
		result = append(result, region)
	}
	return result
}
