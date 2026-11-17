package modelcheck

import (
	"encoding/json"
	"errors"
	"strings"
)

// Config is the part of a model's config.json that decides runtime support
// and memory. Zero values mean config.json did not declare the field.
type Config struct {
	Digest           string   `json:"digest"`
	Architectures    []string `json:"architectures"`
	ModelType        string   `json:"model_type"`
	Layers           int      `json:"layers"`
	HiddenSize       int      `json:"hidden_size"`
	AttentionHeads   int      `json:"attention_heads"`
	KVHeads          int      `json:"kv_heads"`
	HeadDim          int      `json:"head_dim"`
	KVLoRARank       int      `json:"kv_lora_rank"`
	QKRopeHeadDim    int      `json:"qk_rope_head_dim"`
	ContextLength    int      `json:"context_length"`
	DType            string   `json:"dtype"`
	Quantization     string   `json:"quantization"`
	QuantizationBits int      `json:"quantization_bits"`
	CustomCode       bool     `json:"custom_code"`
}

type rawConfig struct {
	Architectures         []string        `json:"architectures"`
	ModelType             string          `json:"model_type"`
	NumHiddenLayers       *int            `json:"num_hidden_layers"`
	NLayer                *int            `json:"n_layer"`
	NumLayers             *int            `json:"num_layers"`
	HiddenSize            *int            `json:"hidden_size"`
	NEmbd                 *int            `json:"n_embd"`
	DModel                *int            `json:"d_model"`
	NumAttentionHeads     *int            `json:"num_attention_heads"`
	NHead                 *int            `json:"n_head"`
	NumKeyValueHeads      *int            `json:"num_key_value_heads"`
	MultiQueryGroupNum    *int            `json:"multi_query_group_num"`
	HeadDim               *int            `json:"head_dim"`
	KVLoRARank            *int            `json:"kv_lora_rank"`
	QKRopeHeadDim         *int            `json:"qk_rope_head_dim"`
	MaxPositionEmbeddings *int            `json:"max_position_embeddings"`
	NPositions            *int            `json:"n_positions"`
	MaxSeqLen             *int            `json:"max_seq_len"`
	SeqLength             *int            `json:"seq_length"`
	MaxSequenceLength     *int            `json:"max_sequence_length"`
	TorchDType            string          `json:"torch_dtype"`
	DType                 string          `json:"dtype"`
	QuantizationConfig    json.RawMessage `json:"quantization_config"`
	AutoMap               json.RawMessage `json:"auto_map"`
	TextConfig            *rawConfig      `json:"text_config"`
	LLMConfig             *rawConfig      `json:"llm_config"`
}

// ParseConfig reads config.json. Multimodal configs that nest the language
// model under text_config or llm_config take its sizes from there.
func ParseConfig(raw []byte) (Config, error) {
	var parsed rawConfig
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Config{}, err
	}
	config := Config{Digest: ConfigDigest(raw), Architectures: append([]string(nil), parsed.Architectures...), ModelType: parsed.ModelType}
	config.CustomCode = len(parsed.AutoMap) > 0 && string(parsed.AutoMap) != "null" && string(parsed.AutoMap) != "{}"
	config.DType = strings.ToLower(first(parsed.DType, parsed.TorchDType))
	config.Quantization, config.QuantizationBits = quantization(parsed.QuantizationConfig)
	language := &parsed
	if parsed.firstInt(parsed.NumHiddenLayers, parsed.NLayer, parsed.NumLayers) == 0 {
		for _, nested := range []*rawConfig{parsed.TextConfig, parsed.LLMConfig} {
			if nested != nil {
				language = nested
				break
			}
		}
	}
	config.Layers = language.firstInt(language.NumHiddenLayers, language.NLayer, language.NumLayers)
	config.HiddenSize = language.firstInt(language.HiddenSize, language.NEmbd, language.DModel)
	config.AttentionHeads = language.firstInt(language.NumAttentionHeads, language.NHead)
	config.KVHeads = language.firstInt(language.NumKeyValueHeads, language.MultiQueryGroupNum)
	config.HeadDim = language.firstInt(language.HeadDim)
	config.KVLoRARank = language.firstInt(language.KVLoRARank)
	config.QKRopeHeadDim = language.firstInt(language.QKRopeHeadDim)
	config.ContextLength = language.firstInt(language.MaxPositionEmbeddings, language.NPositions, language.MaxSeqLen, language.SeqLength, language.MaxSequenceLength)
	if config.ContextLength == 0 {
		config.ContextLength = parsed.firstInt(parsed.MaxPositionEmbeddings, parsed.NPositions, parsed.MaxSeqLen, parsed.SeqLength, parsed.MaxSequenceLength)
	}
	if config.DType == "" && language != &parsed {
		config.DType = strings.ToLower(first(language.DType, language.TorchDType))
	}
	if len(config.Architectures) == 0 && config.ModelType == "" && config.Layers == 0 {
		return Config{}, errors.New("config.json declares no architecture, model type, or layers")
	}
	return config, nil
}

func (rawConfig) firstInt(values ...*int) int {
	for _, value := range values {
		if value != nil && *value > 0 {
			return *value
		}
	}
	return 0
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func quantization(raw json.RawMessage) (string, int) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", 0
	}
	var parsed struct {
		Method string `json:"quant_method"`
		Bits   int    `json:"bits"`
		Format string `json:"format"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return "unknown", 0
	}
	method := strings.ToLower(strings.TrimSpace(parsed.Method))
	if method == "" {
		method = "unknown"
	}
	return method, max(parsed.Bits, 0)
}

// KVCacheBytesPerToken is the served KV cache size of one token across all
// layers at 16-bit precision, or 0 when config.json does not declare enough
// to compute it. Multi-head latent attention caches its compressed latent.
func (c Config) KVCacheBytesPerToken() int64 {
	if c.Layers <= 0 {
		return 0
	}
	if c.KVLoRARank > 0 {
		return int64(c.Layers) * int64(c.KVLoRARank+c.QKRopeHeadDim) * 2
	}
	heads := c.KVHeads
	if heads <= 0 {
		heads = c.AttentionHeads
	}
	headDim := c.HeadDim
	if headDim <= 0 && c.AttentionHeads > 0 && c.HiddenSize > 0 {
		headDim = c.HiddenSize / c.AttentionHeads
	}
	if heads <= 0 || headDim <= 0 {
		return 0
	}
	return 2 * int64(c.Layers) * int64(heads) * int64(headDim) * 2
}
