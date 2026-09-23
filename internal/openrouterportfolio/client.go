package openrouterportfolio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultOpenRouterBaseURL = "https://openrouter.ai"

type Client struct {
	BaseURL            string
	HuggingFaceBaseURL string
	APIKey             string
	HTTP               *http.Client
	Now                func() time.Time
}

type catalogResponse struct {
	Data []catalogModel `json:"data"`
}

type catalogModel struct {
	ID            string `json:"id"`
	CanonicalSlug string `json:"canonical_slug"`
	HuggingFaceID string `json:"hugging_face_id"`
	ContextLength int64  `json:"context_length"`
	Architecture  struct {
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
	Pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
	SupportedParameters []string `json:"supported_parameters"`
}

type rankingsResponse struct {
	Data []struct {
		Date           string `json:"date"`
		ModelPermaslug string `json:"model_permaslug"`
		TotalTokens    string `json:"total_tokens"`
	} `json:"data"`
}

type endpointsResponse struct {
	Data struct {
		Endpoints []struct {
			ProviderName string `json:"provider_name"`
			Pricing      struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
			UptimeLast30M float64 `json:"uptime_last_30m"`
		} `json:"endpoints"`
	} `json:"data"`
}

type huggingFaceModelResponse struct {
	Safetensors struct {
		Total int64 `json:"total"`
	} `json:"safetensors"`
}

// Snapshot returns the highest-demand open-weight text-output models. The
// endpoint limit is applied before per-model endpoint calls so one daily run
// has a predictable request budget.
func (c *Client) Snapshot(ctx context.Context, start, end time.Time, limit int) ([]MarketModel, error) {
	if limit < 1 || limit > 500 {
		return nil, errors.New("snapshot limit must be between 1 and 500")
	}
	if end.Before(start) {
		return nil, errors.New("snapshot end must not precede start")
	}
	var catalog catalogResponse
	if err := c.getJSON(ctx, "/api/v1/models", false, &catalog); err != nil {
		return nil, fmt.Errorf("fetch model catalog: %w", err)
	}
	query := url.Values{}
	query.Set("start_date", start.UTC().Format("2006-01-02"))
	query.Set("end_date", end.UTC().Format("2006-01-02"))
	var rankings rankingsResponse
	if err := c.getJSON(ctx, "/api/v1/datasets/rankings-daily?"+query.Encode(), true, &rankings); err != nil {
		return nil, fmt.Errorf("fetch demand rankings: %w", err)
	}
	days := int(end.UTC().Truncate(24*time.Hour).Sub(start.UTC().Truncate(24*time.Hour))/(24*time.Hour)) + 1
	if days < 1 {
		days = 1
	}
	demand := make(map[string]int64)
	for _, row := range rankings.Data {
		value, err := strconv.ParseInt(row.TotalTokens, 10, 64)
		if err != nil || value < 0 {
			return nil, fmt.Errorf("invalid token count for %q", row.ModelPermaslug)
		}
		demand[row.ModelPermaslug] += value
	}
	type candidate struct {
		model  catalogModel
		tokens int64
	}
	candidates := make([]candidate, 0, len(catalog.Data))
	for _, model := range catalog.Data {
		// Variant products such as :batch have different latency and billing
		// semantics. The initial InferCrane portfolio is the interactive provider
		// lane; variants receive a separate capacity model instead of contaminating
		// this ranking.
		if strings.TrimSpace(model.HuggingFaceID) == "" || strings.Contains(model.ID, ":") || !contains(model.Architecture.OutputModalities, "text") {
			continue
		}
		tokens := demand[model.CanonicalSlug] / int64(days)
		if tokens <= 0 {
			continue
		}
		candidates = append(candidates, candidate{model: model, tokens: tokens})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].tokens == candidates[j].tokens {
			return candidates[i].model.ID < candidates[j].model.ID
		}
		return candidates[i].tokens > candidates[j].tokens
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	markets := make([]MarketModel, 0, len(candidates))
	for _, candidate := range candidates {
		model := candidate.model
		parameterCount, weightGiB, minimumGPUs := c.modelFootprint(ctx, model.HuggingFaceID)
		var endpoints endpointsResponse
		path := "/api/v1/models/" + escapeModelID(model.ID) + "/endpoints"
		if err := c.getJSON(ctx, path, false, &endpoints); err != nil {
			return nil, fmt.Errorf("fetch endpoints for %s: %w", model.ID, err)
		}
		offers := make([]CompetitorOffer, 0, len(endpoints.Data.Endpoints))
		bestUptime := 0.0
		for _, endpoint := range endpoints.Data.Endpoints {
			input, inputErr := perMillion(endpoint.Pricing.Prompt)
			output, outputErr := perMillion(endpoint.Pricing.Completion)
			if inputErr != nil || outputErr != nil || input <= 0 || output <= 0 || strings.TrimSpace(endpoint.ProviderName) == "" {
				continue
			}
			offers = append(offers, CompetitorOffer{Provider: endpoint.ProviderName, InputUSDPerMillion: input, OutputUSDPerMillion: output, UptimePercent: endpoint.UptimeLast30M})
			bestUptime = mathMax(bestUptime, endpoint.UptimeLast30M)
		}
		if len(offers) == 0 {
			input, inputErr := perMillion(model.Pricing.Prompt)
			output, outputErr := perMillion(model.Pricing.Completion)
			if inputErr != nil || outputErr != nil || input <= 0 || output <= 0 {
				continue
			}
			offers = append(offers, CompetitorOffer{Provider: "OpenRouter catalog", InputUSDPerMillion: input, OutputUSDPerMillion: output})
		}
		// Store a real endpoint pair rather than independently combining two
		// price minima that may never be purchasable from the same provider.
		floor := offers[0]
		for _, offer := range offers[1:] {
			if offer.InputUSDPerMillion+offer.OutputUSDPerMillion < floor.InputUSDPerMillion+floor.OutputUSDPerMillion {
				floor = offer
			}
		}
		markets = append(markets, MarketModel{
			ModelID: model.ID, CanonicalSlug: model.CanonicalSlug, HuggingFaceID: model.HuggingFaceID,
			DailyTotalTokens: candidate.tokens, ProviderCount: len(offers),
			FloorInputUSDPerMillion: floor.InputUSDPerMillion, FloorOutputUSDPerMillion: floor.OutputUSDPerMillion,
			BestUptimePercent: bestUptime, ContextLength: model.ContextLength,
			ParameterCount: parameterCount, EstimatedFP8WeightGiB: weightGiB, MinimumH10080GBCount: minimumGPUs,
			SupportedParameters: append([]string(nil), model.SupportedParameters...), CompetitorOffers: offers, CapturedAt: now,
		})
	}
	return markets, nil
}

func (c *Client) modelFootprint(ctx context.Context, huggingFaceID string) (int64, float64, int) {
	base := strings.TrimRight(strings.TrimSpace(c.HuggingFaceBaseURL), "/")
	if base == "" {
		base = "https://huggingface.co"
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopback(parsed.Hostname())) {
		return 0, 0, 0
	}
	var payload huggingFaceModelResponse
	path := "/api/models/" + escapeModelID(huggingFaceID) + "?expand=safetensors"
	if err := c.getJSONFrom(ctx, base+path, false, &payload); err != nil || payload.Safetensors.Total <= 0 {
		return 0, 0, 0
	}
	weightGiB := float64(payload.Safetensors.Total) / float64(1<<30)
	// Reserve roughly 10 GiB per H100 for runtime state before accounting for
	// workload-specific KV cache. This is a lower-bound screening topology.
	minimumGPUs := int(math.Ceil(weightGiB / 70))
	if minimumGPUs < 1 {
		minimumGPUs = 1
	}
	return payload.Safetensors.Total, weightGiB, minimumGPUs
}

func (c *Client) getJSON(ctx context.Context, path string, authenticated bool, destination any) error {
	base := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if base == "" {
		base = defaultOpenRouterBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && !isLoopback(parsed.Hostname())) {
		return errors.New("OpenRouter base URL must be HTTPS or loopback")
	}
	if authenticated && strings.TrimSpace(c.APIKey) == "" {
		return errors.New("OpenRouter API key is required for demand rankings")
	}
	return c.getJSONFrom(ctx, base+path, authenticated, destination)
}

func (c *Client) getJSONFrom(ctx context.Context, endpoint string, authenticated bool, destination any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "InferCrane-OpenRouter-Portfolio/1.0")
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.APIKey))
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

func perMillion(value string) (float64, error) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, err
	}
	return parsed * 1_000_000, nil
}

func escapeModelID(value string) string {
	parts := strings.Split(value, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func mathMax(left, right float64) float64 {
	if right > left {
		return right
	}
	return left
}
