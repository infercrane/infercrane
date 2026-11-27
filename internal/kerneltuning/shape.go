package kerneltuning

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ShapeFromModelConfig reads a Hugging Face config.json's MoE shape as one
// GPU of a tensor-parallel group of size tensorParallel sees it. ok is false
// for a model without routed experts. A multimodal config's text_config
// holds the language model's fields.
func ShapeFromModelConfig(config []byte, tensorParallel int) (FusedMoEShape, bool, error) {
	if tensorParallel < 1 {
		return FusedMoEShape{}, false, errors.New("tensor parallel size must be at least 1")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(config, &root); err != nil {
		return FusedMoEShape{}, false, errors.New("model config must be a JSON object")
	}
	fields := root
	if text, ok := root["text_config"]; ok {
		var nested map[string]json.RawMessage
		if json.Unmarshal(text, &nested) == nil && len(nested) > 0 {
			fields = nested
		}
	}
	integer := func(names ...string) int {
		for _, name := range names {
			for _, source := range []map[string]json.RawMessage{fields, root} {
				var value int
				if raw, ok := source[name]; ok && json.Unmarshal(raw, &value) == nil && value > 0 {
					return value
				}
			}
		}
		return 0
	}
	experts := integer("num_experts", "n_routed_experts", "num_local_experts")
	if experts == 0 {
		return FusedMoEShape{}, false, nil
	}
	intermediate := integer("moe_intermediate_size")
	if intermediate == 0 {
		intermediate = integer("intermediate_size")
	}
	if intermediate == 0 || intermediate%tensorParallel != 0 {
		return FusedMoEShape{}, false, fmt.Errorf("the experts' intermediate size %d does not split over %d GPUs", intermediate, tensorParallel)
	}
	shape := FusedMoEShape{Experts: experts, Intermediate: intermediate / tensorParallel}
	var quantization struct {
		Method      string `json:"quant_method"`
		Format      string `json:"fmt"`
		WeightBlock []int  `json:"weight_block_size"`
	}
	for _, source := range []map[string]json.RawMessage{fields, root} {
		if raw, ok := source["quantization_config"]; ok {
			_ = json.Unmarshal(raw, &quantization)
			break
		}
	}
	switch quantization.Method {
	case "":
	case "fp8":
		shape.DType = "fp8_w8a8"
		if len(quantization.WeightBlock) == 2 {
			shape.BlockN, shape.BlockK = quantization.WeightBlock[0], quantization.WeightBlock[1]
		}
	default:
		// Other methods name their files differently or use other kernels;
		// none is reviewed.
		return FusedMoEShape{}, false, fmt.Errorf("quantization %q is not a reviewed fused-MoE file type", boundedText(quantization.Method))
	}
	if err := shape.Valid(); err != nil {
		return FusedMoEShape{}, false, err
	}
	return shape, true, nil
}
