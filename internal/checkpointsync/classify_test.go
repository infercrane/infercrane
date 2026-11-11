package checkpointsync

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
)

func decoded(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestQuantizationSchemesFollowPublisherConfigs(t *testing.T) {
	for name, test := range map[string]struct {
		got, want string
	}{
		"compressed-tensors mixed": {compressedTensorsScheme(decoded(t, `{"format":"mixed-precision","config_groups":{"a":{"format":"float-quantized","weights":{"num_bits":8,"type":"float"}},"b":{"format":"nvfp4-pack-quantized","weights":{"num_bits":4,"type":"float","group_size":16}}}}`)), "mixed-nvfp4-fp8"},
		"compressed-tensors w4a16": {compressedTensorsScheme(decoded(t, `{"format":"pack-quantized","config_groups":{"a":{"weights":{"num_bits":4,"type":"int"}}}}`)), "w4a16"},
		"compressed-tensors w8a8":  {compressedTensorsScheme(decoded(t, `{"format":"int-quantized","config_groups":{"a":{"weights":{"num_bits":8,"type":"int"},"input_activations":{"num_bits":8,"type":"int"}}}}`)), "w8a8"},
		"compressed-tensors mxfp4": {compressedTensorsScheme(decoded(t, `{"format":"mxfp4-pack-quantized"}`)), "mxfp4"},
		"modelopt groups":          {modelOptScheme(decoded(t, `{"config_groups":{"a":{"weights":{"num_bits":8,"type":"float"}},"b":{"weights":{"num_bits":4,"type":"float","group_size":16}}}}`), nil, false), "mixed-nvfp4-fp8"},
		"modelopt hf quant mixed":  {modelOptScheme(nil, []byte(`{"quantization":{"quant_algo":"MIXED_PRECISION","quantized_layers":{"a":{"quant_algo":"NVFP4"},"b":{"quant_algo":"FP8_PB_WO"},"c":{"quant_algo":"W4A16_NVFP4"}}}}`), true), "mixed-nvfp4-nvfp4_w4a16-fp8_block"},
		"modelopt flat hf quant":   {modelOptScheme(nil, []byte(`{"quant_algo":"NVFP4"}`), true), "nvfp4"},
		"modelopt fp4 method":      {modelOptScheme(decoded(t, `{"quant_method":"modelopt_fp4"}`), nil, false), "nvfp4"},
		"quark mxfp4":              {quarkScheme(decoded(t, `{"global_quant_config":{"weight":{"dtype":"fp4","group_size":32,"scale_format":"e8m0"}}}`)), "mxfp4"},
		"quark ptpc":               {quarkScheme(decoded(t, `{"global_quant_config":{"weight":{"dtype":"fp8_e4m3","qscheme":"per_channel"},"input_tensors":{"dtype":"fp8_e4m3","qscheme":"per_channel","is_dynamic":true}}}`)), "ptpc_fp8"},
		"quark mixed awq":          {quarkScheme(decoded(t, `{"algo_config":[{"name":"awq"}],"global_quant_config":{"weight":{"dtype":"fp4","group_size":32}},"layer_quant_config":{"*attn*":{"weight":{"dtype":"fp8_e4m3","qscheme":"per_tensor"}}}}`)), "awq-mixed-mxfp4-fp8"},
		"auto-round int4":          {autoRoundScheme(decoded(t, `{"bits":4,"data_type":"int"}`)), "w4a16"},
		"auto-round mxfp4":         {autoRoundScheme(decoded(t, `{"bits":4,"data_type":"mx_fp"}`)), "mxfp4"},
		"openvino int8":            {openVINOScheme("Qwen3.8-27B-int8-ov"), "int8"},
		"openvino fp16":            {openVINOScheme("Qwen3.8-27B-fp16-ov"), ""},
	} {
		if test.got != test.want {
			t.Errorf("%s: got %q want %q", name, test.got, test.want)
		}
	}
}

func TestScreenKeepsOnlyLinkedTextModelsWithQuantizationOrSpeculatorSignals(t *testing.T) {
	revision := strings.Repeat("a", 40)
	base := func(id string) ModelInfo {
		return ModelInfo{ID: id, SHA: revision, PipelineTag: "text-generation", CardData: CardData{BaseModel: stringList{"acme/base"}}, Config: ModelConfig{QuantizationConfig: map[string]any{"quant_method": "compressed-tensors"}}}
	}
	gguf := base("acme/model-GGUF")
	asr := base("acme/model-asr")
	asr.PipelineTag = "automatic-speech-recognition"
	plain := base("acme/model")
	plain.Config.QuantizationConfig = nil
	unlinked := base("acme/model-fp8")
	unlinked.CardData.BaseModel = nil
	self := base("acme/self")
	self.CardData.BaseModel = stringList{"acme/self"}
	speculator := base("acme/model-speculator.dflash2")
	speculator.Config.QuantizationConfig = nil
	unsafe := base("acme/../escape")
	for info, want := range map[*ModelInfo]string{&gguf: skipGGUF, &asr: skipNotTextModel, &plain: skipNoSignal, &unlinked: skipNoBase, &self: skipSelfBase, &unsafe: skipInvalidID} {
		if got := screen(*info); got != want {
			t.Errorf("%s: got %q want %q", info.ID, got, want)
		}
	}
	if got := screen(speculator); got != "" || speculatorMethod(speculator) != "dflash2" {
		t.Fatalf("speculator was not kept: %q %q", got, speculatorMethod(speculator))
	}
}

func TestCardParsingFindsExactLineageEngineGatesAndPriors(t *testing.T) {
	card := []byte("Source revision: `dba1be0a40aa45a94ad051997016db3960a90277`.\n" +
		"Needs [vllm-project/vllm#61530](https://github.com/vllm-project/vllm/pull/61530) and sglang>=0.5.20.\n" +
		"Earlier revision `3431dde3247c13b5957f682b1e3c6fcae2566079`.\n\n" +
		"| Benchmark | BF16 | NVFP4 |\n|---|---|---|\n| **GSM8K** | 96.1 | [95.8](https://example.invalid) |\n\n" +
		"| Flag | Value |\n|---|---|\n| tp | two |\n")
	if got := declaredBaseRevision(card, "deepseek-ai/DeepSeek-V4.1-Flash"); got != "dba1be0a40aa45a94ad051997016db3960a90277" {
		t.Fatalf("declared base revision = %q", got)
	}
	if got := declaredBaseRevision([]byte("The previous revision is `3431dde3247c13b5957f682b1e3c6fcae2566079`."), "acme/base"); got != "" {
		t.Fatalf("a non-base revision was taken as lineage: %q", got)
	}
	linked := []byte("Built from https://huggingface.co/acme/base/tree/" + strings.Repeat("c", 40) + ".")
	if got := declaredBaseRevision(linked, "acme/base"); got != strings.Repeat("c", 40) {
		t.Fatalf("linked base revision = %q", got)
	}
	requirements := engineRequirements(card, checkpointcatalog.Policy{})
	if len(requirements) != 2 || requirements[0].Engine != "sglang" || requirements[0].MinimumVersion != "0.5.20" || requirements[1].PullRequest != "vllm-project/vllm#61530" {
		t.Fatalf("engine requirements = %+v", requirements)
	}
	resolved := engineRequirements(card, checkpointcatalog.Policy{EngineResolutions: []checkpointcatalog.EngineResolution{{PullRequest: "vllm-project/vllm#61530", MinimumVersion: "0.32.0", Note: "released"}}})
	if len(resolved) != 2 || resolved[1].State != checkpointcatalog.EngineMinimumVersion || resolved[1].MinimumVersion != "0.32.0" {
		t.Fatalf("resolved pull request did not become a minimum version: %+v", resolved)
	}
	html := []byte("Intro\n<table>\n<tr><th>Benchmark</th><th>BF16</th><th>NVFP4</th></tr>\n<tr><td><b>GPQA Diamond</b></td><td>88.92</td><td>88.01</td></tr>\n</table>\n")
	if got := priorTables(html, strings.Repeat("e", 40)); len(got) != 1 || got[0].Columns[0] != "Benchmark" || got[0].Rows[0][0] != "GPQA Diamond" || got[0].Rows[0][2] != "88.01" {
		t.Fatalf("HTML prior table = %+v", got)
	}
	priors := priorTables(card, strings.Repeat("d", 40))
	if len(priors) != 1 || priors[0].Rows[0][0] != "GSM8K" || priors[0].Rows[0][2] != "95.8" || priors[0].EvidenceState != checkpointcatalog.EvidenceUnmeasured {
		t.Fatalf("prior tables = %+v", priors)
	}
}
