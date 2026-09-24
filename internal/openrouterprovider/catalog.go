// Package openrouterprovider implements the provider-facing model catalog
// contract without changing InferCrane's customer-facing OpenAI model list.
package openrouterprovider

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"os"
	"strings"
)

type Catalog struct {
	Data []Model `json:"data"`
}

type Model struct {
	SchemaVersion    string             `json:"schema_version"`
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	HuggingFaceID    string             `json:"hugging_face_id"`
	Created          int64              `json:"created"`
	Quantization     string             `json:"quantization,omitempty"`
	Tokenizer        string             `json:"tokenizer,omitempty"`
	Description      string             `json:"description,omitempty"`
	InputModalities  []InputModality    `json:"input_modalities"`
	OutputModalities []OutputModality   `json:"output_modalities"`
	Pricing          []Price            `json:"pricing,omitempty"`
	Capacity         []Capacity         `json:"capacity,omitempty"`
	Passthrough      map[string]any     `json:"passthrough_parameters,omitempty"`
	IsReady          bool               `json:"is_ready"`
	IsFree           bool               `json:"is_free"`
	DiscountToUser   float64            `json:"discount_to_user,omitempty"`
	OpenRouter       OpenRouterIdentity `json:"openrouter"`
	Datacenters      []Datacenter       `json:"datacenters,omitempty"`
	DeploymentRegion string             `json:"deployment_region,omitempty"`
	Compliance       *Compliance        `json:"compliance,omitempty"`
}

type InputModality struct {
	Type            string         `json:"type"`
	SupportedInputs map[string]any `json:"supported_inputs,omitempty"`
	Pricing         []Price        `json:"pricing,omitempty"`
	Capacity        []Capacity     `json:"capacity,omitempty"`
	Passthrough     map[string]any `json:"passthrough_parameters,omitempty"`
}

type OutputModality struct {
	Type                string         `json:"type"`
	MaxLength           *Limit         `json:"max_length,omitempty"`
	Streaming           bool           `json:"streaming,omitempty"`
	SupportedParameters map[string]any `json:"supported_parameters"`
	Pricing             []Price        `json:"pricing,omitempty"`
	Capacity            []Capacity     `json:"capacity,omitempty"`
	Passthrough         map[string]any `json:"passthrough_parameters,omitempty"`
}

type Limit struct {
	Value int64  `json:"value"`
	Unit  string `json:"unit,omitempty"`
}

type Price struct {
	Type    string `json:"type"`
	Unit    string `json:"unit"`
	CostUSD string `json:"cost_usd"`
}

type Capacity struct {
	Type  string `json:"type"`
	Unit  string `json:"unit"`
	Per   string `json:"per,omitempty"`
	Value int64  `json:"value"`
}

type OpenRouterIdentity struct {
	Slug string `json:"slug"`
}

type Datacenter struct {
	CountryCode string `json:"country_code"`
	Region      string `json:"region"`
}

type Compliance struct {
	ZDR   bool `json:"zdr"`
	HIPAA bool `json:"hipaa"`
}

// EffectiveTokenPrices returns the user-visible per-million token prices after
// the catalog discount is applied. Operational revenue telemetry must use
// these values rather than the undiscounted list price.
func (c Catalog) EffectiveTokenPrices(modelID string) (float64, float64, bool, error) {
	for _, model := range c.Data {
		if model.ID != modelID {
			continue
		}
		var input, output float64
		var hasInput, hasOutput bool
		for _, modality := range model.InputModalities {
			for _, price := range modality.Pricing {
				if price.Type == "prompt" && price.Unit == "token" {
					value, err := pricePerMillion(price.CostUSD, model.DiscountToUser)
					if err != nil {
						return 0, 0, false, fmt.Errorf("OpenRouter model %q prompt price: %w", model.ID, err)
					}
					input, hasInput = value, true
				}
			}
		}
		for _, modality := range model.OutputModalities {
			for _, price := range modality.Pricing {
				if price.Type == "completion" && price.Unit == "token" {
					value, err := pricePerMillion(price.CostUSD, model.DiscountToUser)
					if err != nil {
						return 0, 0, false, fmt.Errorf("OpenRouter model %q completion price: %w", model.ID, err)
					}
					output, hasOutput = value, true
				}
			}
		}
		return input, output, hasInput && hasOutput, nil
	}
	return 0, 0, false, fmt.Errorf("OpenRouter model %q is absent from the catalog", modelID)
}

func pricePerMillion(costUSD string, discount float64) (float64, error) {
	value, ok := new(big.Rat).SetString(costUSD)
	if !ok || value.Sign() < 0 {
		return 0, fmt.Errorf("invalid decimal cost_usd %q", costUSD)
	}
	perToken, _ := value.Float64()
	return perToken * 1_000_000 * (1 - discount), nil
}

func Load(path string) (*Catalog, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read OpenRouter provider catalog: %w", err)
	}
	var catalog Catalog
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&catalog); err != nil {
		return nil, fmt.Errorf("decode OpenRouter provider catalog: %w", err)
	}
	if err = catalog.Validate(); err != nil {
		return nil, err
	}
	return &catalog, nil
}

func (c Catalog) Validate() error {
	if len(c.Data) == 0 {
		return errors.New("OpenRouter provider catalog must contain at least one model")
	}
	seen := make(map[string]struct{}, len(c.Data))
	for index, model := range c.Data {
		if model.SchemaVersion != "2.5" {
			return fmt.Errorf("OpenRouter model %d must use schema_version 2.5", index)
		}
		if model.ID == "" || model.Name == "" || model.HuggingFaceID == "" || model.Created <= 0 {
			return fmt.Errorf("OpenRouter model %d has incomplete identity", index)
		}
		if model.OpenRouter.Slug == "" || model.OpenRouter.Slug != model.ID {
			return fmt.Errorf("OpenRouter model %q must use its id as openrouter.slug", model.ID)
		}
		if math.IsNaN(model.DiscountToUser) || math.IsInf(model.DiscountToUser, 0) || model.DiscountToUser >= 1 {
			return fmt.Errorf("OpenRouter model %q has invalid discount_to_user %v", model.ID, model.DiscountToUser)
		}
		if _, duplicate := seen[model.ID]; duplicate {
			return fmt.Errorf("duplicate OpenRouter model id %q", model.ID)
		}
		seen[model.ID] = struct{}{}
		if len(model.InputModalities) == 0 || len(model.OutputModalities) == 0 {
			return fmt.Errorf("OpenRouter model %q must declare input and output modalities", model.ID)
		}
		if model.IsReady && (len(model.Datacenters) == 0 || strings.TrimSpace(model.DeploymentRegion) == "" || model.Compliance == nil) {
			return fmt.Errorf("ready OpenRouter model %q must declare datacenter, deployment region, and compliance", model.ID)
		}
		for _, datacenter := range model.Datacenters {
			if len(datacenter.CountryCode) != 2 || datacenter.CountryCode != strings.ToUpper(datacenter.CountryCode) || strings.TrimSpace(datacenter.Region) == "" {
				return fmt.Errorf("OpenRouter model %q has an invalid datacenter", model.ID)
			}
		}
		for _, modality := range model.InputModalities {
			if modality.Type == "" {
				return fmt.Errorf("OpenRouter model %q has an input modality without type", model.ID)
			}
			if err := validatePrices(model.ID, modality.Pricing); err != nil {
				return err
			}
			if err := validateCapacity(model.ID, modality.Capacity); err != nil {
				return err
			}
		}
		for _, modality := range model.OutputModalities {
			if modality.Type == "" || modality.SupportedParameters == nil {
				return fmt.Errorf("OpenRouter model %q has an incomplete output modality", model.ID)
			}
			if err := validatePrices(model.ID, modality.Pricing); err != nil {
				return err
			}
			if err := validateCapacity(model.ID, modality.Capacity); err != nil {
				return err
			}
		}
		if err := validatePrices(model.ID, model.Pricing); err != nil {
			return err
		}
		if err := validateCapacity(model.ID, model.Capacity); err != nil {
			return err
		}
	}
	return nil
}

func validatePrices(model string, prices []Price) error {
	for _, price := range prices {
		if price.Type == "" || price.Unit == "" || price.CostUSD == "" {
			return fmt.Errorf("OpenRouter model %q has incomplete pricing", model)
		}
		value, ok := new(big.Rat).SetString(price.CostUSD)
		if !ok || value.Sign() < 0 {
			return fmt.Errorf("OpenRouter model %q has invalid decimal cost_usd %q", model, price.CostUSD)
		}
	}
	return nil
}

func validateCapacity(model string, capacities []Capacity) error {
	for _, capacity := range capacities {
		if capacity.Type == "" || capacity.Unit == "" || capacity.Value <= 0 {
			return fmt.Errorf("OpenRouter model %q has invalid capacity", model)
		}
	}
	return nil
}

func (c Catalog) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=30")
	_ = json.NewEncoder(w).Encode(c)
}

// ServeOpenAI exposes the model metadata shape used by Hugging Face Inference
// Providers and generic OpenAI-compatible gateways. OpenRouter continues to
// receive its richer provider catalog from ServeHTTP.
func (c Catalog) ServeOpenAI(w http.ResponseWriter, modelID string, policy ChannelPolicy) {
	var selected *Model
	for index := range c.Data {
		if c.Data[index].ID == modelID {
			selected = &c.Data[index]
			break
		}
	}
	if selected == nil {
		writeProviderError(w, http.StatusNotFound, "Unknown model", "invalid_request_error")
		return
	}
	contextLength := int64(0)
	for _, modality := range selected.InputModalities {
		if value, ok := modality.SupportedInputs["max_context_length"].(map[string]any); ok {
			switch typed := value["value"].(type) {
			case float64:
				contextLength = int64(typed)
			case int64:
				contextLength = typed
			case int:
				contextLength = int64(typed)
			}
		}
	}
	response := map[string]any{
		"object": "list",
		"data": []map[string]any{{
			"id":             selected.ID,
			"object":         "model",
			"created":        selected.Created,
			"owned_by":       "infercrane",
			"context_length": contextLength,
			"pricing": map[string]float64{
				"input":  policy.InputPricePerMillionUSD,
				"output": policy.OutputPricePerMillionUSD,
			},
		}},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, max-age=30")
	_ = json.NewEncoder(w).Encode(response)
}
