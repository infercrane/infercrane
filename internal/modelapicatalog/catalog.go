// Package modelapicatalog provides the supplier-neutral discovery catalog used
// by Model APIs. It is deliberately separate from curatedrecipe: appearing in
// this catalog is not a deployment, performance, price, or availability claim.
package modelapicatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

const SchemaVersion = "model-api-catalog/v1"

type Pricing struct {
	Currency                      string `json:"currency,omitempty"`
	InputMicrousdPerMillion       *int64 `json:"input_microusd_per_million,omitempty"`
	CachedInputMicrousdPerMillion *int64 `json:"cached_input_microusd_per_million,omitempty"`
	OutputMicrousdPerMillion      *int64 `json:"output_microusd_per_million,omitempty"`
	Provenance                    string `json:"provenance,omitempty"`
	ObservedAt                    string `json:"observed_at,omitempty"`
	ValidUntil                    string `json:"valid_until,omitempty"`
}

type Offer struct {
	ID              string   `json:"id"`
	Supplier        string   `json:"supplier"`
	SupplierSlug    string   `json:"supplier_slug"`
	SupplierModelID string   `json:"supplier_model_id"`
	Adapter         string   `json:"adapter"`
	Protocol        string   `json:"protocol"`
	Access          string   `json:"access"`
	Availability    string   `json:"availability"`
	Regions         []string `json:"regions,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	Pricing         *Pricing `json:"pricing,omitempty"`
	SourceURL       string   `json:"source_url,omitempty"`
}

type Model struct {
	ID                  string   `json:"id"`
	DisplayName         string   `json:"display_name"`
	Publisher           string   `json:"publisher"`
	PublisherSlug       string   `json:"publisher_slug"`
	Repository          string   `json:"repository,omitempty"`
	Family              string   `json:"family"`
	Parameters          string   `json:"parameters,omitempty"`
	Description         string   `json:"description"`
	Tasks               []string `json:"tasks"`
	Capabilities        []string `json:"capabilities"`
	InputModalities     []string `json:"input_modalities"`
	OutputModalities    []string `json:"output_modalities"`
	License             string   `json:"license,omitempty"`
	ContextWindowTokens *int64   `json:"context_window_tokens,omitempty"`
	Access              string   `json:"access"`
	Qualification       string   `json:"qualification"`
	QualificationNote   string   `json:"qualification_note"`
	Offers              []Offer  `json:"offers"`
}

type Catalog struct {
	SchemaVersion string  `json:"schema_version"`
	Models        []Model `json:"models"`
}

type Filter struct {
	Query, Task, Capability, Publisher, Access string
	Offset, Limit                              int
}

type Page struct {
	Models     []Model
	Total      int
	NextOffset *int
}

func Default() Catalog {
	return Catalog{SchemaVersion: SchemaVersion, Models: builtins()}
}

func Load(path string) (Catalog, error) {
	if strings.TrimSpace(path) == "" {
		catalog := Default()
		return catalog, catalog.Validate()
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, fmt.Errorf("read model API catalog: %w", err)
	}
	var catalog Catalog
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&catalog); err != nil {
		return Catalog{}, fmt.Errorf("decode model API catalog: %w", err)
	}
	if err = catalog.Validate(); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func (c Catalog) Validate() error {
	if c.SchemaVersion != SchemaVersion {
		return fmt.Errorf("model API catalog schema_version must be %q", SchemaVersion)
	}
	if len(c.Models) == 0 {
		return errors.New("model API catalog must contain at least one model")
	}
	modelIDs, offerIDs := map[string]struct{}{}, map[string]struct{}{}
	for index, model := range c.Models {
		if !validSlug(model.ID) || model.DisplayName == "" || model.Publisher == "" || !validSlug(model.PublisherSlug) || model.Family == "" || model.Description == "" || len(model.Tasks) == 0 || len(model.Capabilities) == 0 || len(model.InputModalities) == 0 || len(model.OutputModalities) == 0 {
			return fmt.Errorf("model API catalog model %d is incomplete or has an invalid id", index)
		}
		if model.Access != "managed-preview" && model.Access != "request-access" && model.Access != "ready" {
			return fmt.Errorf("model API catalog model %q has invalid access %q", model.ID, model.Access)
		}
		if model.Qualification != "cataloged" && model.Qualification != "supplier-reported" && model.Qualification != "configuration-reviewed" && model.Qualification != "measured" {
			return fmt.Errorf("model API catalog model %q has invalid qualification %q", model.ID, model.Qualification)
		}
		if _, exists := modelIDs[model.ID]; exists {
			return fmt.Errorf("model API catalog model id %q is duplicated", model.ID)
		}
		modelIDs[model.ID] = struct{}{}
		for _, offer := range model.Offers {
			if !validSlug(offer.ID) || offer.Supplier == "" || !validSlug(offer.SupplierSlug) || offer.SupplierModelID == "" || offer.Adapter == "" || offer.Protocol == "" {
				return fmt.Errorf("model API catalog offer for %q is incomplete", model.ID)
			}
			if offer.Access != "connect-provider" && offer.Access != "request-access" && offer.Access != "ready" {
				return fmt.Errorf("model API catalog offer %q has invalid access", offer.ID)
			}
			if offer.Availability != "unknown" && offer.Availability != "available" && offer.Availability != "unavailable" {
				return fmt.Errorf("model API catalog offer %q has invalid availability", offer.ID)
			}
			if _, exists := offerIDs[offer.ID]; exists {
				return fmt.Errorf("model API catalog offer id %q is duplicated", offer.ID)
			}
			offerIDs[offer.ID] = struct{}{}
			if offer.Pricing != nil {
				if offer.Pricing.Currency != "USD" || offer.Pricing.Provenance == "" || offer.Pricing.ObservedAt == "" || offer.Pricing.ValidUntil == "" {
					return fmt.Errorf("model API catalog offer %q pricing needs USD, provenance, observed_at, and valid_until", offer.ID)
				}
				observedAt, observedErr := time.Parse(time.RFC3339, offer.Pricing.ObservedAt)
				validUntil, expiryErr := time.Parse(time.RFC3339, offer.Pricing.ValidUntil)
				if observedErr != nil || expiryErr != nil || !validUntil.After(observedAt) {
					return fmt.Errorf("model API catalog offer %q pricing timestamps must be ordered RFC3339 values", offer.ID)
				}
				for _, value := range []*int64{offer.Pricing.InputMicrousdPerMillion, offer.Pricing.CachedInputMicrousdPerMillion, offer.Pricing.OutputMicrousdPerMillion} {
					if value != nil && *value < 0 {
						return fmt.Errorf("model API catalog offer %q pricing cannot be negative", offer.ID)
					}
				}
			}
		}
	}
	return nil
}

func PricingCurrentAt(pricing *Pricing, at time.Time) bool {
	if pricing == nil {
		return false
	}
	validUntil, err := time.Parse(time.RFC3339, pricing.ValidUntil)
	return err == nil && validUntil.After(at.UTC())
}

func (c Catalog) Find(id string) (Model, bool) {
	for _, model := range c.Models {
		if model.ID == id {
			return clone(model), true
		}
	}
	return Model{}, false
}

func (c Catalog) List(filter Filter) Page {
	limit := filter.Limit
	if limit <= 0 {
		limit = 24
	}
	needle := strings.ToLower(strings.TrimSpace(filter.Query))
	items := make([]Model, 0, len(c.Models))
	for _, model := range c.Models {
		if filter.Task != "" && !contains(model.Tasks, filter.Task) || filter.Capability != "" && !contains(model.Capabilities, filter.Capability) || filter.Publisher != "" && model.PublisherSlug != filter.Publisher || filter.Access != "" && model.Access != filter.Access {
			continue
		}
		if needle != "" {
			haystack := strings.ToLower(strings.Join(append([]string{model.ID, model.DisplayName, model.Publisher, model.Repository, model.Family, model.Description}, model.Tasks...), " "))
			if !strings.Contains(haystack, needle) {
				continue
			}
		}
		items = append(items, clone(model))
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].DisplayName < items[j].DisplayName })
	total := len(items)
	start := filter.Offset
	if start < 0 || start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	var next *int
	if end < total {
		value := end
		next = &value
	}
	return Page{Models: items[start:end], Total: total, NextOffset: next}
}

func clone(model Model) Model {
	model.Tasks = append([]string(nil), model.Tasks...)
	model.Capabilities = append([]string(nil), model.Capabilities...)
	model.InputModalities = append([]string(nil), model.InputModalities...)
	model.OutputModalities = append([]string(nil), model.OutputModalities...)
	model.Offers = append([]Offer(nil), model.Offers...)
	return model
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validSlug(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '.' {
			continue
		}
		return false
	}
	return true
}

type seed struct {
	id, name, publisher, publisherSlug, family, parameters, repository string
	tasks, capabilities                                                []string
}

func builtins() []Model {
	chat := []string{"chat-completions", "streaming"}
	tools := []string{"chat-completions", "streaming", "tool-calling", "structured-output"}
	vision := []string{"chat-completions", "streaming", "vision"}
	seeds := []seed{
		{"llama-3.1-8b-instruct", "Llama 3.1 8B Instruct", "Meta", "meta", "Llama 3.1", "8B", "meta-llama/Llama-3.1-8B-Instruct", []string{"chat", "tools"}, tools},
		{"llama-3.1-70b-instruct", "Llama 3.1 70B Instruct", "Meta", "meta", "Llama 3.1", "70B", "meta-llama/Llama-3.1-70B-Instruct", []string{"chat", "reasoning", "tools"}, tools},
		{"llama-3.1-405b-instruct", "Llama 3.1 405B Instruct", "Meta", "meta", "Llama 3.1", "405B", "meta-llama/Llama-3.1-405B-Instruct", []string{"chat", "reasoning", "tools"}, tools},
		{"llama-3.2-1b-instruct", "Llama 3.2 1B Instruct", "Meta", "meta", "Llama 3.2", "1B", "meta-llama/Llama-3.2-1B-Instruct", []string{"chat", "edge"}, chat},
		{"llama-3.2-3b-instruct", "Llama 3.2 3B Instruct", "Meta", "meta", "Llama 3.2", "3B", "meta-llama/Llama-3.2-3B-Instruct", []string{"chat", "edge"}, chat},
		{"llama-3.2-11b-vision-instruct", "Llama 3.2 11B Vision Instruct", "Meta", "meta", "Llama 3.2", "11B", "meta-llama/Llama-3.2-11B-Vision-Instruct", []string{"chat", "vision"}, vision},
		{"llama-3.2-90b-vision-instruct", "Llama 3.2 90B Vision Instruct", "Meta", "meta", "Llama 3.2", "90B", "meta-llama/Llama-3.2-90B-Vision-Instruct", []string{"chat", "vision"}, vision},
		{"llama-3.3-70b-instruct", "Llama 3.3 70B Instruct", "Meta", "meta", "Llama 3.3", "70B", "meta-llama/Llama-3.3-70B-Instruct", []string{"chat", "reasoning", "tools"}, tools},
		{"qwen2.5-3b-instruct", "Qwen2.5 3B Instruct", "Qwen", "qwen", "Qwen2.5", "3B", "Qwen/Qwen2.5-3B-Instruct", []string{"chat", "edge"}, chat},
		{"qwen2.5-7b-instruct", "Qwen2.5 7B Instruct", "Qwen", "qwen", "Qwen2.5", "7B", "Qwen/Qwen2.5-7B-Instruct", []string{"chat", "tools"}, tools},
		{"qwen2.5-14b-instruct", "Qwen2.5 14B Instruct", "Qwen", "qwen", "Qwen2.5", "14B", "Qwen/Qwen2.5-14B-Instruct", []string{"chat", "tools"}, tools},
		{"qwen2.5-32b-instruct", "Qwen2.5 32B Instruct", "Qwen", "qwen", "Qwen2.5", "32B", "Qwen/Qwen2.5-32B-Instruct", []string{"chat", "reasoning", "tools"}, tools},
		{"qwen2.5-72b-instruct", "Qwen2.5 72B Instruct", "Qwen", "qwen", "Qwen2.5", "72B", "Qwen/Qwen2.5-72B-Instruct", []string{"chat", "reasoning", "tools"}, tools},
		{"qwen2.5-coder-7b-instruct", "Qwen2.5 Coder 7B Instruct", "Qwen", "qwen", "Qwen2.5 Coder", "7B", "Qwen/Qwen2.5-Coder-7B-Instruct", []string{"coding", "chat"}, chat},
		{"qwen2.5-coder-14b-instruct", "Qwen2.5 Coder 14B Instruct", "Qwen", "qwen", "Qwen2.5 Coder", "14B", "Qwen/Qwen2.5-Coder-14B-Instruct", []string{"coding", "chat"}, chat},
		{"qwen2.5-coder-32b-instruct", "Qwen2.5 Coder 32B Instruct", "Qwen", "qwen", "Qwen2.5 Coder", "32B", "Qwen/Qwen2.5-Coder-32B-Instruct", []string{"coding", "chat"}, chat},
		{"qwen2.5-vl-7b-instruct", "Qwen2.5 VL 7B Instruct", "Qwen", "qwen", "Qwen2.5 VL", "7B", "Qwen/Qwen2.5-VL-7B-Instruct", []string{"vision", "documents", "chat"}, vision},
		{"qwen2.5-vl-72b-instruct", "Qwen2.5 VL 72B Instruct", "Qwen", "qwen", "Qwen2.5 VL", "72B", "Qwen/Qwen2.5-VL-72B-Instruct", []string{"vision", "documents", "chat"}, vision},
		{"qwen3-0.6b", "Qwen3 0.6B", "Qwen", "qwen", "Qwen3", "0.6B", "Qwen/Qwen3-0.6B", []string{"chat", "edge"}, chat},
		{"qwen3-1.7b", "Qwen3 1.7B", "Qwen", "qwen", "Qwen3", "1.7B", "Qwen/Qwen3-1.7B", []string{"chat", "edge"}, chat},
		{"qwen3-4b", "Qwen3 4B", "Qwen", "qwen", "Qwen3", "4B", "Qwen/Qwen3-4B", []string{"chat", "reasoning"}, chat},
		{"qwen3-8b", "Qwen3 8B", "Qwen", "qwen", "Qwen3", "8B", "Qwen/Qwen3-8B", []string{"chat", "reasoning", "tools"}, tools},
		{"qwen3-14b", "Qwen3 14B", "Qwen", "qwen", "Qwen3", "14B", "Qwen/Qwen3-14B", []string{"chat", "reasoning", "tools"}, tools},
		{"qwen3-30b-a3b", "Qwen3 30B A3B", "Qwen", "qwen", "Qwen3", "30B A3B", "Qwen/Qwen3-30B-A3B", []string{"chat", "reasoning", "tools"}, tools},
		{"qwen3-32b", "Qwen3 32B", "Qwen", "qwen", "Qwen3", "32B", "Qwen/Qwen3-32B", []string{"chat", "reasoning", "tools"}, tools},
		{"qwen3-235b-a22b", "Qwen3 235B A22B", "Qwen", "qwen", "Qwen3", "235B A22B", "Qwen/Qwen3-235B-A22B", []string{"chat", "reasoning", "tools"}, tools},
		{"qwen3.8-27b", "Qwen3.8 27B", "Qwen", "qwen", "Qwen3.8", "27B", "Qwen/Qwen3.8-27B", []string{"coding", "reasoning", "tools"}, tools},
		{"qwen3.8-flash-next", "Qwen3.8 Flash Next", "Qwen", "qwen", "Qwen3.8", "125B A6B", "Qwen/Qwen3.8-Flash-Next-FP8", []string{"chat", "coding", "vision", "tools"}, vision},
		{"mistral-7b-instruct", "Mistral 7B Instruct", "Mistral AI", "mistral", "Mistral", "7B", "mistralai/Mistral-7B-Instruct-v0.3", []string{"chat", "tools"}, tools},
		{"mistral-nemo-instruct", "Mistral Nemo Instruct", "Mistral AI", "mistral", "Mistral Nemo", "12B", "mistralai/Mistral-Nemo-Instruct-2407", []string{"chat", "tools"}, tools},
		{"mixtral-8x7b-instruct", "Mixtral 8x7B Instruct", "Mistral AI", "mistral", "Mixtral", "8x7B", "mistralai/Mixtral-8x7B-Instruct-v0.1", []string{"chat", "tools"}, tools},
		{"mixtral-8x22b-instruct", "Mixtral 8x22B Instruct", "Mistral AI", "mistral", "Mixtral", "8x22B", "mistralai/Mixtral-8x22B-Instruct-v0.1", []string{"chat", "reasoning", "tools"}, tools},
		{"codestral-22b", "Codestral 22B", "Mistral AI", "mistral", "Codestral", "22B", "mistralai/Codestral-22B-v0.1", []string{"coding", "completion"}, chat},
		{"devstral-small", "Devstral Small", "Mistral AI", "mistral", "Devstral", "24B", "mistralai/Devstral-Small-2505", []string{"coding", "agents", "tools"}, tools},
		{"deepseek-r1", "DeepSeek R1", "DeepSeek", "deepseek", "DeepSeek R1", "671B A37B", "deepseek-ai/DeepSeek-R1", []string{"reasoning", "coding"}, chat},
		{"deepseek-v3", "DeepSeek V3", "DeepSeek", "deepseek", "DeepSeek V3", "671B A37B", "deepseek-ai/DeepSeek-V3", []string{"chat", "coding", "tools"}, tools},
		{"deepseek-r1-distill-qwen-7b", "DeepSeek R1 Distill Qwen 7B", "DeepSeek", "deepseek", "DeepSeek R1 Distill", "7B", "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B", []string{"reasoning", "chat"}, chat},
		{"deepseek-r1-distill-qwen-14b", "DeepSeek R1 Distill Qwen 14B", "DeepSeek", "deepseek", "DeepSeek R1 Distill", "14B", "deepseek-ai/DeepSeek-R1-Distill-Qwen-14B", []string{"reasoning", "chat"}, chat},
		{"deepseek-r1-distill-qwen-32b", "DeepSeek R1 Distill Qwen 32B", "DeepSeek", "deepseek", "DeepSeek R1 Distill", "32B", "deepseek-ai/DeepSeek-R1-Distill-Qwen-32B", []string{"reasoning", "chat"}, chat},
		{"deepseek-r1-distill-llama-8b", "DeepSeek R1 Distill Llama 8B", "DeepSeek", "deepseek", "DeepSeek R1 Distill", "8B", "deepseek-ai/DeepSeek-R1-Distill-Llama-8B", []string{"reasoning", "chat"}, chat},
		{"deepseek-r1-distill-llama-70b", "DeepSeek R1 Distill Llama 70B", "DeepSeek", "deepseek", "DeepSeek R1 Distill", "70B", "deepseek-ai/DeepSeek-R1-Distill-Llama-70B", []string{"reasoning", "chat"}, chat},
		{"gemma-2-2b-it", "Gemma 2 2B IT", "Google", "google", "Gemma 2", "2B", "google/gemma-2-2b-it", []string{"chat", "edge"}, chat},
		{"gemma-2-9b-it", "Gemma 2 9B IT", "Google", "google", "Gemma 2", "9B", "google/gemma-2-9b-it", []string{"chat"}, chat},
		{"gemma-2-27b-it", "Gemma 2 27B IT", "Google", "google", "Gemma 2", "27B", "google/gemma-2-27b-it", []string{"chat", "reasoning"}, chat},
		{"gemma-3-1b-it", "Gemma 3 1B IT", "Google", "google", "Gemma 3", "1B", "google/gemma-3-1b-it", []string{"chat", "edge"}, chat},
		{"gemma-3-4b-it", "Gemma 3 4B IT", "Google", "google", "Gemma 3", "4B", "google/gemma-3-4b-it", []string{"chat", "vision"}, vision},
		{"gemma-3-12b-it", "Gemma 3 12B IT", "Google", "google", "Gemma 3", "12B", "google/gemma-3-12b-it", []string{"chat", "vision"}, vision},
		{"gemma-3-27b-it", "Gemma 3 27B IT", "Google", "google", "Gemma 3", "27B", "google/gemma-3-27b-it", []string{"chat", "vision", "reasoning"}, vision},
		{"phi-3.5-mini-instruct", "Phi 3.5 Mini Instruct", "Microsoft", "microsoft", "Phi 3.5", "3.8B", "microsoft/Phi-3.5-mini-instruct", []string{"chat", "edge"}, chat},
		{"phi-3.5-moe-instruct", "Phi 3.5 MoE Instruct", "Microsoft", "microsoft", "Phi 3.5", "42B A6.6B", "microsoft/Phi-3.5-MoE-instruct", []string{"chat", "reasoning"}, chat},
		{"phi-3.5-vision-instruct", "Phi 3.5 Vision Instruct", "Microsoft", "microsoft", "Phi 3.5", "4.2B", "microsoft/Phi-3.5-vision-instruct", []string{"chat", "vision"}, vision},
		{"phi-4", "Phi 4", "Microsoft", "microsoft", "Phi 4", "14B", "microsoft/phi-4", []string{"chat", "reasoning"}, chat},
		{"granite-3.2-2b-instruct", "Granite 3.2 2B Instruct", "IBM", "ibm", "Granite 3.2", "2B", "ibm-granite/granite-3.2-2b-instruct", []string{"chat", "edge"}, chat},
		{"granite-3.3-8b-instruct", "Granite 3.3 8B Instruct", "IBM", "ibm", "Granite 3.3", "8B", "ibm-granite/granite-3.3-8b-instruct", []string{"chat", "extraction", "summarization"}, chat},
		{"glm-4.5-air", "GLM 4.5 Air", "Z.ai", "zai", "GLM 4.5", "106B A12B", "zai-org/GLM-4.5-Air", []string{"chat", "coding", "tools"}, tools},
		{"glm-5.3-flash", "GLM 5.3 Flash", "Z.ai", "zai", "GLM 5.3", "321B A18B", "zai-org/GLM-5.3-Flash", []string{"chat", "coding", "vision", "tools"}, vision},
		{"bge-m3", "BGE-M3", "BAAI", "baai", "BGE", "568M", "BAAI/bge-m3", []string{"embeddings", "retrieval", "rag"}, []string{"embeddings"}},
		{"bge-large-en-v1.5", "BGE Large EN v1.5", "BAAI", "baai", "BGE", "335M", "BAAI/bge-large-en-v1.5", []string{"embeddings", "retrieval"}, []string{"embeddings"}},
		{"e5-mistral-7b-instruct", "E5 Mistral 7B Instruct", "Microsoft", "microsoft", "E5", "7B", "intfloat/e5-mistral-7b-instruct", []string{"embeddings", "retrieval"}, []string{"embeddings"}},
		{"gte-qwen2-7b-instruct", "GTE Qwen2 7B Instruct", "Alibaba NLP", "alibaba", "GTE", "7B", "Alibaba-NLP/gte-Qwen2-7B-instruct", []string{"embeddings", "retrieval"}, []string{"embeddings"}},
		{"nomic-embed-text-v1.5", "Nomic Embed Text v1.5", "Nomic AI", "nomic", "Nomic Embed", "137M", "nomic-ai/nomic-embed-text-v1.5", []string{"embeddings", "retrieval"}, []string{"embeddings"}},
	}
	// Keep the launch shelf deliberately small. Operators can load a larger
	// catalog from configuration without changing the customer contract, but
	// the built-in experience contains only the models we intend to qualify
	// next instead of presenting an uncurated marketplace.
	featured := map[string]bool{
		"bge-m3":                       true,
		"deepseek-r1-distill-qwen-32b": true,
		"glm-5.3-flash":                true,
		"llama-3.1-8b-instruct":        true,
		"mistral-7b-instruct":          true,
		"qwen3-8b":                     true,
		"qwen3.8-27b":                  true,
		"qwen3.8-flash-next":           true,
	}
	models := make([]Model, 0, len(featured))
	for _, item := range seeds {
		if !featured[item.id] {
			continue
		}
		inputs, outputs := []string{"text"}, []string{"text"}
		if contains(item.capabilities, "vision") {
			inputs = []string{"text", "image"}
		}
		if contains(item.capabilities, "embeddings") {
			outputs = []string{"embeddings"}
		}
		models = append(models, Model{ID: item.id, DisplayName: item.name, Publisher: item.publisher, PublisherSlug: item.publisherSlug, Repository: item.repository, Family: item.family, Parameters: item.parameters, Description: item.name + " is cataloged for Model API discovery. Request managed access before production traffic can be configured.", Tasks: item.tasks, Capabilities: item.capabilities, InputModalities: inputs, OutputModalities: outputs, Access: "request-access", Qualification: "cataloged", QualificationNote: "Identity and capability metadata only. Managed availability, price, performance, and production qualification are unknown until sourced evidence is attached.", Offers: []Offer{}})
	}
	return models
}
