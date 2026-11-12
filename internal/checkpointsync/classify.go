package checkpointsync

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/checkpointcatalog"
)

// Skip reasons are reported in the reviewable diff.
const (
	skipInvalidID       = "invalid repository identifier"
	skipPrivate         = "private, disabled, or unavailable"
	skipGGUF            = "GGUF export, which the product's engines do not serve"
	skipNotTextModel    = "not a text-generation model"
	skipNotLLMLibrary   = "library is not served by an LLM engine"
	skipNoSignal        = "no quantization or speculator signal"
	skipUnquantizedOV   = "unquantized OpenVINO export"
	skipNoBase          = "no declared base model"
	skipSelfBase        = "declares itself as its base model"
	skipUnknownArtifact = "no safetensors or OpenVINO weights at the revision"
)

var (
	textPipelines = []string{"", "text-generation", "image-text-to-text", "any-to-any", "video-text-to-text"}
	nonLLMLibrary = []string{"diffusers", "nemo", "sentence-transformers", "cosmos", "timm", "pytorch", "onnx", "onnxruntime", "gguf"}

	speculatorName         = regexp.MustCompile(`(?i)(^|[-_.])(eagle3?|dflash2?|dspark|speculator)([-_.]|$)`)
	speculatorArchitecture = []string{"eagle", "dflash", "dspark", "draftmodel", "speculator"}
	qatToken               = regexp.MustCompile(`(?i)(^|[-_.])qat([-_.]|$)`)
	mixedToken             = regexp.MustCompile(`(?i)(^|[-_.])mixed([-_.]|$)`)
	awqToken               = regexp.MustCompile(`(?i)(^|[-_.])awq([-_.]|$)`)
	versionLike            = regexp.MustCompile(`^v?\d+\.\d+`)
)

// schemeOrder keeps mixed scheme names stable: lower-precision parts first.
// A part never contains "-": mixed schemes join parts with "-", and
// whole-scheme modifiers such as qat- and awq- are prefixes.
var schemeOrder = []string{"nvfp4", "nvfp4_w4a16", "mxfp4", "fp4", "w2a16", "w3a16", "w4a16", "w4a8", "int4", "fp8", "fp8_block", "ptpc_fp8", "mxfp8", "w8a8", "w8a16", "bf16"}

type classification struct {
	Kind        string
	Tool        string
	ToolVersion string
	Method      string
	Scheme      string
	Speculator  string
	Bases       []string
	Format      string
}

// screen decides from listing data alone whether a repository deserves a
// detail fetch. It returns a skip reason, or "" to keep the repository.
func screen(info ModelInfo) string {
	return screenWith(info, true)
}

// screenMember screens a model that a reviewed collection lists. The
// collection stands in for the quantization signal that listing data often
// lacks for builds that keep it only in hf_quant_config.json; classify still
// requires the signal in the files recorded at the revision.
func screenMember(info ModelInfo) string {
	return screenWith(info, false)
}

func screenWith(info ModelInfo, requireSignal bool) string {
	if !ValidRepository(info.ID) || !checkpointcatalog.ImmutableRevision(info.SHA) {
		return skipInvalidID
	}
	if info.Private || info.Disabled || info.Status != 0 {
		return skipPrivate
	}
	library := strings.ToLower(info.LibraryName)
	if library == "gguf" || info.hasTag("gguf") || strings.Contains(strings.ToLower(info.name()), "gguf") {
		return skipGGUF
	}
	if !slices.Contains(textPipelines, info.PipelineTag) {
		return skipNotTextModel
	}
	if slices.Contains(nonLLMLibrary, library) {
		return skipNotLLMLibrary
	}
	if requireSignal && !quantizationSignal(info, info.Config.QuantizationConfig, nil) {
		return skipNoSignal
	}
	if isOpenVINO(info) && openVINOScheme(info.name()) == "" {
		return skipUnquantizedOV
	}
	bases := baseModels(info)
	if len(bases) == 0 {
		return skipNoBase
	}
	if strings.EqualFold(bases[0], info.ID) {
		return skipSelfBase
	}
	return ""
}

// selectionScreen screens a listing item, using the collection member screen
// when a reviewed collection also lists it.
func selectionScreen(item ModelInfo, collectionMembers map[string]struct{}) string {
	if _, member := collectionMembers[item.ID]; member {
		return screenMember(item)
	}
	return screen(item)
}

// quantizationSignal reports whether a record or its files identify a
// quantized checkpoint, a speculator, or an OpenVINO export.
func quantizationSignal(info ModelInfo, config map[string]any, hfQuant []byte) bool {
	return isSpeculator(info) || isOpenVINO(info) || quantMethod(config) != "" || isModelOpt(info, hfQuant)
}

// classify refines the screen with the files recorded at the pinned revision.
func classify(info ModelInfo, files fileReader) (classification, string) {
	if reason := screenWith(info, false); reason != "" {
		return classification{}, reason
	}
	config := info.Config.QuantizationConfig
	if raw, found := files("config.json"); found {
		var full struct {
			QuantizationConfig map[string]any `json:"quantization_config"`
		}
		if json.Unmarshal(raw, &full) == nil && full.QuantizationConfig != nil {
			config = full.QuantizationConfig
		}
	}
	hfQuant, hasHFQuant := files("hf_quant_config.json")
	if !quantizationSignal(info, config, hfQuant) {
		return classification{}, skipNoSignal
	}
	format := artifactFormat(info)
	if format == "" {
		return classification{}, skipUnknownArtifact
	}
	if isSpeculator(info) {
		return classification{Kind: checkpointcatalog.KindSpeculator, Speculator: speculatorMethod(info), Bases: baseModels(info), Format: format}, ""
	}
	out := classification{Kind: checkpointcatalog.KindQuantized, Format: format, Method: quantMethod(config), ToolVersion: "unknown", Bases: baseModels(info)}
	readme, _ := files("README.md")
	switch {
	case isOpenVINO(info):
		out.Tool, out.Method, out.Scheme = "openvino", "openvino", openVINOScheme(info.name())
	case isModelOpt(info, hfQuant):
		out.Tool, out.Scheme, out.ToolVersion = "modelopt", modelOptScheme(config, hfQuant, hasHFQuant), modelOptVersion(hfQuant, readme)
	case out.Method == "compressed-tensors":
		out.Tool, out.Scheme, out.ToolVersion = "llm-compressor", compressedTensorsScheme(config), stringOr(config, "version", "unknown")
	case out.Method == "quark":
		out.Tool, out.Scheme, out.ToolVersion = "quark", quarkScheme(config), stringOr(config, "version", "unknown")
		if awqToken.MatchString(info.name()) && !strings.HasPrefix(out.Scheme, "awq-") {
			out.Scheme = "awq-" + out.Scheme
		}
	case strings.Contains(out.Method, "auto-round") || out.Method == "auto_round":
		out.Tool, out.Scheme, out.ToolVersion = "auto-round", autoRoundScheme(config), stringOr(config, "autoround_version", "unknown")
	case out.Method == "fp8":
		out.Tool, out.Scheme = "transformers-fp8", "fp8"
		if config["weight_block_size"] != nil {
			out.Scheme = "fp8_block"
		}
	case out.Method == "gptq" || out.Method == "awq":
		out.Tool, out.Scheme = out.Method, fmt.Sprintf("%s-w%da16", out.Method, intOf(config, "bits"))
	case out.Method != "":
		out.Tool, out.Scheme = out.Method, out.Method
	default:
		out.Tool, out.Method, out.Scheme = "unknown", "unknown", "unknown"
	}
	if out.Scheme == "" {
		out.Scheme = "unknown"
	}
	if qatToken.MatchString(info.name()) && !strings.HasPrefix(out.Scheme, "qat-") {
		out.Scheme = "qat-" + out.Scheme
	}
	if out.Tool == "auto-round" && mixedToken.MatchString(info.name()) {
		out.Scheme += "_mixed"
	}
	return out, ""
}

func isSpeculator(info ModelInfo) bool {
	if strings.EqualFold(info.LibraryName, "speculators") || speculatorName.MatchString(info.name()) {
		return true
	}
	for _, architecture := range info.Config.Architectures {
		lower := strings.ToLower(architecture)
		// A conditional-generation class is a complete multimodal model, such
		// as NVIDIA's Eagle2 vision-language family, never a draft head.
		if strings.HasSuffix(lower, "forconditionalgeneration") {
			continue
		}
		for _, marker := range speculatorArchitecture {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}
	return false
}

func speculatorMethod(info ModelInfo) string {
	text := strings.ToLower(info.name() + " " + strings.Join(info.Config.Architectures, " "))
	for _, method := range []string{"dflash2", "dflash", "dspark", "eagle3", "eagle", "mtp"} {
		if strings.Contains(text, method) {
			return method
		}
	}
	return "draft-model"
}

func isOpenVINO(info ModelInfo) bool {
	return strings.EqualFold(info.LibraryName, "openvino") || info.hasTag("openvino") || info.hasFile("openvino_model.xml")
}

func openVINOScheme(name string) string {
	for _, token := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		switch token {
		case "int4", "int8", "nf4", "int2", "fp8", "mxfp4":
			return token
		}
	}
	return ""
}

func isModelOpt(info ModelInfo, hfQuant []byte) bool {
	if strings.Contains(strings.ToLower(info.LibraryName), "model optimizer") || info.hasTag("modelopt") || strings.HasPrefix(quantMethod(info.Config.QuantizationConfig), "modelopt") {
		return true
	}
	// hf_quant_config.json is the Model Optimizer export format. Some exports
	// omit the producer block; any other named producer is not ModelOpt.
	var document struct {
		Producer *struct {
			Name string `json:"name"`
		} `json:"producer"`
		Quantization struct {
			Algorithm string `json:"quant_algo"`
		} `json:"quantization"`
	}
	if len(hfQuant) == 0 || json.Unmarshal(hfQuant, &document) != nil {
		return false
	}
	if document.Producer != nil {
		return strings.EqualFold(document.Producer.Name, "modelopt")
	}
	return document.Quantization.Algorithm != ""
}

var modelOptCardVersion = regexp.MustCompile(`(?i)modelopt\W{0,6}v?(\d+\.\d+\.\d+(?:rc\d+|\.?dev\d+|\.post\d+)?)`)

func modelOptVersion(hfQuant, readme []byte) string {
	var producer struct {
		Producer struct {
			Version string `json:"version"`
		} `json:"producer"`
	}
	declared := ""
	if len(hfQuant) > 0 && json.Unmarshal(hfQuant, &producer) == nil {
		declared = strings.TrimSpace(producer.Producer.Version)
	}
	if versionLike.MatchString(declared) && declared != "0.0.0" {
		return strings.TrimPrefix(declared, "v")
	}
	if match := modelOptCardVersion.FindSubmatch(readme); match != nil {
		return string(match[1])
	}
	if declared != "" {
		return declared
	}
	return "unknown"
}

func modelOptScheme(config map[string]any, hfQuant []byte, hasHFQuant bool) string {
	if hasHFQuant {
		var document map[string]any
		if json.Unmarshal(hfQuant, &document) == nil {
			quantization := mapOf(document, "quantization")
			if quantization == nil {
				quantization = document
			}
			if scheme := modelOptAlgorithm(quantization); scheme != "" {
				return scheme
			}
		}
	}
	if scheme := modelOptAlgorithm(config); scheme != "" {
		return scheme
	}
	if scheme := compressedTensorsScheme(config); scheme != "" && scheme != "unknown" {
		return scheme
	}
	switch quantMethod(config) {
	case "modelopt_fp4":
		return "nvfp4"
	case "modelopt_mixed":
		return "mixed-precision"
	}
	return "unknown"
}

func modelOptAlgorithm(quantization map[string]any) string {
	algorithm := strings.ToUpper(stringOr(quantization, "quant_algo", ""))
	if algorithm == "" {
		return ""
	}
	if algorithm != "MIXED_PRECISION" {
		return modelOptAlgorithmName(algorithm)
	}
	schemes := map[string]struct{}{}
	for _, layer := range mapOf(quantization, "quantized_layers") {
		if config, ok := layer.(map[string]any); ok {
			if name := modelOptAlgorithmName(strings.ToUpper(stringOr(config, "quant_algo", ""))); name != "" {
				schemes[name] = struct{}{}
			}
		}
	}
	return mixedScheme(schemes, "mixed-precision")
}

func modelOptAlgorithmName(algorithm string) string {
	switch algorithm {
	case "":
		return ""
	case "NVFP4":
		return "nvfp4"
	case "FP8", "FP8_PER_CHANNEL_PER_TOKEN":
		return "fp8"
	case "FP8_PB_WO", "FP8_BLOCK_SCALES":
		return "fp8_block"
	case "W4A16_NVFP4":
		return "nvfp4_w4a16"
	case "MXFP4":
		return "mxfp4"
	case "MXFP8":
		return "mxfp8"
	case "W4A16_AWQ":
		return "w4a16_awq"
	case "W4A8_AWQ":
		return "w4a8_awq"
	case "NVFP4_AWQ":
		return "nvfp4_awq"
	}
	return strings.ToLower(algorithm)
}

func compressedTensorsScheme(config map[string]any) string {
	top := stringOr(config, "format", "")
	schemes := map[string]struct{}{}
	for _, value := range mapOf(config, "config_groups") {
		if group, ok := value.(map[string]any); ok {
			if scheme := compressedTensorsGroupScheme(group, top); scheme != "" {
				schemes[scheme] = struct{}{}
			}
		}
	}
	if len(schemes) == 0 {
		return compressedTensorsGroupScheme(map[string]any{}, top)
	}
	return mixedScheme(schemes, "mixed-precision")
}

func compressedTensorsGroupScheme(group map[string]any, top string) string {
	format := stringOr(group, "format", "")
	if format == "" || format == "mixed-precision" {
		format = top
	}
	weights, activations := mapOf(group, "weights"), mapOf(group, "input_activations")
	bits, kind, size := intOf(weights, "num_bits"), stringOr(weights, "type", ""), intOf(weights, "group_size")
	switch format {
	case "nvfp4-pack-quantized":
		return "nvfp4"
	case "mxfp4-pack-quantized":
		return "mxfp4"
	case "mxfp8-quantized":
		return "mxfp8"
	case "marlin-24":
		return "w4a16_sparse24"
	case "int-quantized":
		return fmt.Sprintf("w%da%d", bits, intOf(activations, "num_bits"))
	case "pack-quantized":
		if len(activations) > 0 {
			return fmt.Sprintf("w%da%d", bits, intOf(activations, "num_bits"))
		}
		return fmt.Sprintf("w%da16", bits)
	case "float-quantized", "naive-quantized":
		if kind == "int" {
			if len(activations) > 0 {
				return fmt.Sprintf("w%da%d", bits, intOf(activations, "num_bits"))
			}
			return fmt.Sprintf("w%da16", bits)
		}
		return "fp8"
	}
	switch {
	case kind == "float" && bits == 4 && size == 16:
		return "nvfp4"
	case kind == "float" && bits == 4:
		return "mxfp4"
	case kind == "float" && bits == 8:
		return "fp8"
	case kind == "int" && bits > 0 && len(activations) == 0:
		return fmt.Sprintf("w%da16", bits)
	case kind == "int" && bits > 0:
		return fmt.Sprintf("w%da%d", bits, intOf(activations, "num_bits"))
	}
	return ""
}

func quarkScheme(config map[string]any) string {
	global := mapOf(config, "global_quant_config")
	base := quarkLayerScheme(mapOf(global, "weight"), mapOf(global, "input_tensors"))
	schemes := map[string]struct{}{}
	if base != "" {
		schemes[base] = struct{}{}
	}
	for _, key := range []string{"layer_quant_config", "layer_type_quant_config"} {
		for _, value := range mapOf(config, key) {
			if layer, ok := value.(map[string]any); ok {
				if scheme := quarkLayerScheme(mapOf(layer, "weight"), mapOf(layer, "input_tensors")); scheme != "" {
					schemes[scheme] = struct{}{}
				}
			}
		}
	}
	scheme := mixedScheme(schemes, "")
	if scheme == "" {
		return "unknown"
	}
	if algorithm, err := json.Marshal(config["algo_config"]); err == nil && strings.Contains(strings.ToLower(string(algorithm)), "awq") {
		scheme = "awq-" + scheme
	}
	return scheme
}

func quarkLayerScheme(weight, input map[string]any) string {
	dtype := strings.ToLower(stringOr(weight, "dtype", ""))
	switch {
	case dtype == "":
		return ""
	case strings.Contains(dtype, "fp4"):
		if stringOr(weight, "scale_format", "") == "e8m0" || intOf(weight, "group_size") == 32 {
			return "mxfp4"
		}
		return "fp4"
	case strings.Contains(dtype, "fp8"):
		if stringOr(weight, "qscheme", "") == "per_channel" && len(input) > 0 && input["is_dynamic"] == true && stringOr(input, "qscheme", "") != "per_tensor" {
			return "ptpc_fp8"
		}
		return "fp8"
	case strings.Contains(dtype, "int4"):
		if len(input) > 0 {
			return "w4a8"
		}
		return "w4a16"
	case strings.Contains(dtype, "int8"):
		if len(input) > 0 {
			return "w8a8"
		}
		return "w8a16"
	}
	return dtype
}

func autoRoundScheme(config map[string]any) string {
	bits, dataType, activationBits := intOf(config, "bits"), strings.ToLower(stringOr(config, "data_type", "int")), intOf(config, "act_bits")
	switch {
	case strings.Contains(dataType, "mx"):
		return fmt.Sprintf("mxfp%d", bits)
	case strings.Contains(dataType, "nv"):
		return fmt.Sprintf("nvfp%d", bits)
	case strings.Contains(dataType, "fp") && bits == 8:
		return "fp8"
	case bits == 0:
		return "unknown"
	case activationBits == 0 || activationBits >= 16:
		return fmt.Sprintf("w%da16", bits)
	}
	return fmt.Sprintf("w%da%d", bits, activationBits)
}

func mixedScheme(schemes map[string]struct{}, fallback string) string {
	if len(schemes) == 0 {
		return fallback
	}
	names := make([]string, 0, len(schemes))
	for name := range schemes {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := slices.Index(schemeOrder, names[i]), slices.Index(schemeOrder, names[j])
		if a == -1 {
			a = len(schemeOrder)
		}
		if b == -1 {
			b = len(schemeOrder)
		}
		if a != b {
			return a < b
		}
		return names[i] < names[j]
	})
	if len(names) == 1 {
		return names[0]
	}
	return "mixed-" + strings.Join(names, "-")
}

func artifactFormat(info ModelInfo) string {
	if isOpenVINO(info) {
		return "openvino-ir"
	}
	for _, sibling := range info.Siblings {
		if strings.HasSuffix(sibling.Name, ".safetensors") {
			return "safetensors"
		}
	}
	return ""
}

func baseModels(info ModelInfo) []string {
	var out []string
	for _, base := range info.CardData.BaseModel {
		if !slices.ContainsFunc(out, func(existing string) bool { return strings.EqualFold(existing, base) }) && !strings.EqualFold(base, info.ID) {
			out = append(out, base)
		}
	}
	if len(out) == 0 && len(info.CardData.BaseModel) > 0 {
		return []string{info.CardData.BaseModel[0]}
	}
	return out
}

func quantMethod(config map[string]any) string {
	return strings.ToLower(strings.TrimSpace(stringOr(config, "quant_method", "")))
}

func mapOf(value map[string]any, key string) map[string]any {
	if value == nil {
		return nil
	}
	nested, _ := value[key].(map[string]any)
	return nested
}

func stringOr(value map[string]any, key, fallback string) string {
	if value == nil {
		return fallback
	}
	if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	return fallback
}

func intOf(value map[string]any, key string) int {
	if value == nil {
		return 0
	}
	number, _ := value[key].(float64)
	return int(number)
}
