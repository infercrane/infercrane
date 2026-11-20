package modelcheck

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/infercrane/infercrane/internal/pricing"
	"github.com/infercrane/infercrane/internal/provideridentity"
)

// SchemaVersion identifies the check report contract.
const SchemaVersion = "infercrane.model-check/v1"

// Verdicts and check states.
const (
	Supported   = "supported"
	Unsupported = "unsupported"
	Unknown     = "unknown"

	Pass = "pass"
	Fail = "fail"
	Info = "info"

	Fits       = "fits"
	DoesNotFit = "does_not_fit"

	TaskChat       = "chat"
	TaskEmbeddings = "embeddings"
)

// Defaults for the load a memory fit is sized for, and their bounds.
const (
	DefaultContextTokens = 8192
	DefaultConcurrency   = 8
	MinContextTokens     = 256
	MaxContextTokens     = 1 << 20
	MaxConcurrency       = 1024

	// GPUMemoryUtilization is vLLM's default share of device memory.
	GPUMemoryUtilization = 0.90
	// RuntimeOverheadGiB covers activations, CUDA graphs, and communication
	// buffers on each GPU. It is an allowance, not a measurement.
	RuntimeOverheadGiB = 2
)

var tensorParallelSizes = []int{1, 2, 4, 8}

// GPU is one accelerator alias the memory fit may choose.
type GPU struct {
	Name      string
	MemoryGiB int
}

// EvaluateOptions sizes the memory fit. GPUs come from current price observations;
// an empty list leaves the fit unknown.
type EvaluateOptions struct {
	ContextTokens int
	Concurrency   int
	GPUs          []GPU
	Registries    RegistrySnapshot
	Now           time.Time
}

// Report is the automatic check of one model at one commit. It is never a
// performance, quality, capacity, or price claim.
type Report struct {
	SchemaVersion     string            `json:"schema_version"`
	Repository        string            `json:"repository"`
	RequestedRevision string            `json:"requested_revision"`
	Revision          string            `json:"revision"`
	Source            string            `json:"source"`
	SourceURL         string            `json:"source_url"`
	CheckedAt         time.Time         `json:"checked_at"`
	Reviewed          bool              `json:"reviewed"`
	PerformanceClaims bool              `json:"performance_claims"`
	Verdict           string            `json:"verdict"`
	Reason            string            `json:"reason"`
	Task              string            `json:"task"`
	PipelineTag       *string           `json:"pipeline_tag"`
	License           *string           `json:"license"`
	Access            Access            `json:"access"`
	Architecture      ArchitectureCheck `json:"architecture"`
	Weights           WeightsFacts      `json:"weights"`
	Config            ConfigFacts       `json:"config"`
	Memory            MemoryFit         `json:"memory"`
	Checks            []Check           `json:"checks"`
}

// Access reports gating as Hugging Face declares it.
type Access struct {
	State     string  `json:"state"`
	Gated     bool    `json:"gated"`
	GatedMode *string `json:"gated_mode"`
	Private   bool    `json:"private"`
	TokenUsed bool    `json:"token_used"`
}

// Access states.
const (
	AccessPublic        = "public"
	AccessTokenUsed     = "token_used"
	AccessTokenRequired = "token_required"
)

// ArchitectureCheck says whether a pinned runtime registers the model's
// architecture for its task.
type ArchitectureCheck struct {
	State          string           `json:"state"`
	Name           *string          `json:"name"`
	ModelType      *string          `json:"model_type"`
	Runtime        *string          `json:"runtime"`
	RuntimeVersion *string          `json:"runtime_version"`
	Reason         string           `json:"reason"`
	CustomCode     bool             `json:"custom_code"`
	Sources        []RegistrySource `json:"sources"`
}

// RegistrySource cites one runtime registry the check read.
type RegistrySource struct {
	Runtime   string `json:"runtime"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	SourceURL string `json:"source_url"`
}

// WeightsFacts is the safetensors summary and weight file format.
type WeightsFacts struct {
	State        string           `json:"state"`
	Format       string           `json:"format"`
	Parameters   *int64           `json:"parameters"`
	StoredBytes  *int64           `json:"stored_bytes"`
	DTypes       map[string]int64 `json:"dtypes"`
	Quantization *string          `json:"quantization"`
	Reason       string           `json:"reason"`
}

// ConfigFacts is what config.json declares, when it could be read.
type ConfigFacts struct {
	State                string  `json:"state"`
	Digest               *string `json:"digest"`
	Layers               *int    `json:"layers"`
	HiddenSize           *int    `json:"hidden_size"`
	AttentionHeads       *int    `json:"attention_heads"`
	KVHeads              *int    `json:"kv_heads"`
	HeadDim              *int    `json:"head_dim"`
	ContextLength        *int    `json:"context_length"`
	DType                *string `json:"dtype"`
	KVCacheBytesPerToken *int64  `json:"kv_cache_bytes_per_token"`
}

// MemoryFit sizes one replica for the target load.
type MemoryFit struct {
	State          string   `json:"state"`
	Reason         string   `json:"reason"`
	GPU            *string  `json:"gpu"`
	GPUCount       *int     `json:"gpu_count"`
	GPUMemoryGiB   *int     `json:"gpu_memory_gib"`
	ContextTokens  int      `json:"context_tokens"`
	Concurrency    int      `json:"concurrency"`
	WeightsBytes   *int64   `json:"weights_bytes"`
	KVCacheBytes   *int64   `json:"kv_cache_bytes"`
	RequiredBytes  *int64   `json:"required_bytes"`
	Layouts        []Layout `json:"layouts"`
	Assumptions    []string `json:"assumptions"`
	ConsideredGPUs []string `json:"considered_gpus"`
}

// Layout is one GPU type and tensor-parallel size that fits.
type Layout struct {
	GPU                 string `json:"gpu"`
	GPUCount            int    `json:"gpu_count"`
	GPUMemoryGiB        int    `json:"gpu_memory_gib"`
	PerGPURequiredBytes int64  `json:"per_gpu_required_bytes"`
}

// Check is one line of what was checked, for people.
type Check struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

// PricedGPUs lists the planning accelerators that have at least one current
// price observation, smallest device memory first. A GPU nobody prices is
// not a GPU InferCrane can propose.
func PricedGPUs(prices map[pricing.Request]pricing.Estimate, now time.Time) []GPU {
	out := make([]GPU, 0, len(provideridentity.PlanningAccelerators()))
	for _, alias := range provideridentity.PlanningAccelerators() {
		memory, known := provideridentity.AcceleratorMemoryGiB(alias)
		if !known {
			continue
		}
		for request, estimate := range prices {
			if request.Replicas == 1 && !estimate.Stale(now) && estimate.Hourly >= 0 && !math.IsNaN(estimate.Hourly) && !math.IsInf(estimate.Hourly, 0) && provideridentity.MatchesGPU(request.Cloud, request.GPU, alias) {
				out = append(out, GPU{Name: alias, MemoryGiB: memory})
				break
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].MemoryGiB < out[j].MemoryGiB })
	return out
}

// NormalizeLoad bounds a requested context and concurrency, filling defaults.
// A model that declares a shorter context caps the target at that length.
func NormalizeLoad(contextTokens, concurrency, declared int) (int, int) {
	if contextTokens <= 0 {
		contextTokens = DefaultContextTokens
	}
	contextTokens = min(max(contextTokens, MinContextTokens), MaxContextTokens)
	if declared > 0 && contextTokens > declared {
		contextTokens = declared
	}
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	return contextTokens, min(concurrency, MaxConcurrency)
}

// Evaluate turns what Hugging Face reported into the automatic check.
func Evaluate(model Model, options EvaluateOptions) Report {
	report := Report{
		SchemaVersion: SchemaVersion, Repository: model.Repository, RequestedRevision: model.RequestedRevision, Revision: model.Commit,
		Source: "hugging-face", SourceURL: "https://huggingface.co/" + model.Repository + "/tree/" + model.Commit,
		CheckedAt: model.ReadAt, Reviewed: false, PerformanceClaims: false,
		PipelineTag: optionalString(model.PipelineTag), License: optionalString(model.License),
	}
	if report.CheckedAt.IsZero() {
		report.CheckedAt = options.Now.UTC()
	}
	report.Access = access(model)
	report.Task = task(model, options.Registries)
	report.Config = configFacts(model)
	report.Weights = weightsFacts(model)
	report.Architecture = architecture(model, report.Task, options.Registries)
	report.Memory = memoryFit(model, report.Task, options)
	report.Checks = checks(model, report, options.Registries)
	report.Verdict, report.Reason = verdict(model, report)
	return report
}

func optionalString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func access(model Model) Access {
	result := Access{Gated: model.Gated != "", Private: model.Private, TokenUsed: model.TokenUsed, GatedMode: optionalString(model.Gated)}
	switch {
	case model.ConfigState == ConfigTokenRequired:
		result.State = AccessTokenRequired
	case model.TokenUsed && (result.Gated || result.Private):
		result.State = AccessTokenUsed
	case result.Gated || result.Private:
		result.State = AccessTokenRequired
	default:
		result.State = AccessPublic
	}
	return result
}

var (
	chatPipelines      = []string{"text-generation", "image-text-to-text", "text2text-generation", "any-to-any"}
	embeddingPipelines = []string{"feature-extraction", "sentence-similarity"}
	generationTables   = []string{"text_generation", "multimodal"}
)

func task(model Model, registries RegistrySnapshot) string {
	switch {
	case slices.Contains(chatPipelines, model.PipelineTag):
		return TaskChat
	case slices.Contains(embeddingPipelines, model.PipelineTag):
		return TaskEmbeddings
	case model.PipelineTag != "":
		return ""
	}
	vllm, _ := registries.Runtime("vllm")
	for _, name := range model.Architectures {
		tables := vllm.Architectures[name]
		if slices.ContainsFunc(tables, func(table string) bool { return slices.Contains(generationTables, table) }) {
			return TaskChat
		}
		if slices.Contains(tables, "embedding") {
			return TaskEmbeddings
		}
	}
	return ""
}

func configFacts(model Model) ConfigFacts {
	facts := ConfigFacts{State: model.ConfigState}
	if facts.State == "" {
		facts.State = ConfigNotFound
	}
	config := model.Config
	if config == nil {
		return facts
	}
	positive := func(value int) *int {
		if value <= 0 {
			return nil
		}
		return &value
	}
	facts.Digest = optionalString(config.Digest)
	facts.Layers, facts.HiddenSize, facts.AttentionHeads = positive(config.Layers), positive(config.HiddenSize), positive(config.AttentionHeads)
	facts.KVHeads, facts.HeadDim, facts.ContextLength = positive(config.KVHeads), positive(config.HeadDim), positive(config.ContextLength)
	facts.DType = optionalString(config.DType)
	if perToken := config.KVCacheBytesPerToken(); perToken > 0 {
		facts.KVCacheBytesPerToken = &perToken
	}
	return facts
}

// storedBytes and servedBytes are bytes per parameter by safetensors dtype.
// vLLM's default dtype serves 32-bit checkpoints at 16 bits.
var storedBytes = map[string]int64{"F64": 8, "F32": 4, "F16": 2, "BF16": 2, "I64": 8, "I32": 4, "U32": 4, "I16": 2, "U16": 2, "I8": 1, "U8": 1, "BOOL": 1, "F8_E4M3": 1, "F8_E5M2": 1, "F8_E8M0": 1}

func servedBytesPerParameter(dtype string) (int64, bool) {
	if dtype == "F32" || dtype == "F64" {
		return 2, true
	}
	size, known := storedBytes[dtype]
	return size, known
}

func weightsFacts(model Model) WeightsFacts {
	facts := WeightsFacts{State: Unknown, Format: "unknown", DTypes: map[string]int64{}}
	switch {
	case model.Files.Safetensors || model.Weights != nil:
		facts.Format = "safetensors"
	case model.Files.GGUF:
		facts.Format = "gguf"
	case model.Files.PyTorch:
		facts.Format = "pytorch"
	}
	if model.Config != nil && model.Config.Quantization != "" {
		facts.Quantization = &model.Config.Quantization
	}
	if model.Weights == nil {
		facts.Reason = "Hugging Face publishes no safetensors summary for this commit, so its size is unknown."
		if facts.Format == "gguf" {
			facts.Reason = "This repository holds GGUF files only. InferCrane deploys safetensors checkpoints."
		}
		return facts
	}
	parameters, stored := model.Weights.Parameters, int64(0)
	for dtype, count := range model.Weights.ByDType {
		facts.DTypes[dtype] = count
		size, known := storedBytes[dtype]
		if !known {
			facts.Parameters = &parameters
			facts.Reason = "Hugging Face reports a tensor type (" + dtype + ") InferCrane does not size, so the stored size is unknown."
			return facts
		}
		stored += count * size
	}
	facts.State, facts.Parameters, facts.StoredBytes = "read", &parameters, &stored
	facts.Reason = "Parameter counts by tensor type from Hugging Face's safetensors summary."
	return facts
}

func sources(registries RegistrySnapshot) []RegistrySource {
	out := make([]RegistrySource, 0, len(registries.Runtimes))
	for _, registry := range registries.Runtimes {
		out = append(out, RegistrySource{Runtime: registry.Runtime, Version: registry.Version, Commit: registry.Commit, SourceURL: registry.SourceURL})
	}
	return out
}

func architecture(model Model, task string, registries RegistrySnapshot) ArchitectureCheck {
	check := ArchitectureCheck{State: Unknown, ModelType: optionalString(model.ModelType), CustomCode: model.Config != nil && model.Config.CustomCode, Sources: sources(registries)}
	vllm, _ := registries.Runtime("vllm")
	sglang, _ := registries.Runtime("sglang")
	names := model.Architectures
	if task == "" && model.PipelineTag != "" {
		check.State = Unsupported
		check.Reason = "InferCrane deploys text-generation and embedding models; Hugging Face lists this one as " + model.PipelineTag + "."
		return check
	}
	if len(names) == 0 {
		check.Reason = "Hugging Face reports no model architecture for this commit."
		return check
	}
	check.Name = &names[0]
	versions := "vLLM " + vllm.Version + " or SGLang " + sglang.Version
	switch task {
	case TaskChat:
		for index := range names {
			name := names[index]
			if slices.ContainsFunc(vllm.Architectures[name], func(table string) bool { return slices.Contains(generationTables, table) }) {
				return supported(check, &names[index], "vllm", vllm.Version, name+" is in vLLM "+vllm.Version+"'s model registry.")
			}
		}
		for index := range names {
			name := names[index]
			if _, registered := sglang.Architectures[name]; registered && generativeName(name) {
				return supported(check, &names[index], "sglang", sglang.Version, name+" is not in vLLM "+vllm.Version+"'s registry but is in SGLang "+sglang.Version+"'s.")
			}
		}
	case TaskEmbeddings:
		for index := range names {
			name := names[index]
			if slices.Contains(vllm.Architectures[name], "embedding") {
				return supported(check, &names[index], "vllm", vllm.Version, name+" is an embedding model in vLLM "+vllm.Version+"'s registry.")
			}
		}
		for _, name := range names {
			if slices.ContainsFunc(vllm.Architectures[name], func(table string) bool { return slices.Contains(generationTables, table) }) {
				check.Reason = "vLLM " + vllm.Version + " registers " + name + " for text generation. Serving it for embeddings needs a pooling conversion InferCrane does not set automatically, so support is unknown."
				return check
			}
		}
	default:
		check.Reason = "Hugging Face reports no task for this model, and its architecture does not say whether it generates text or embeddings."
		return check
	}
	name := names[0]
	check.State = Unsupported
	switch {
	case vllm.PreviouslySupported[name] != "":
		check.Reason = "vLLM removed " + name + " after " + vllm.PreviouslySupported[name] + ", and SGLang " + sglang.Version + " does not register it."
	case vllm.OutOfTree[name] != "":
		check.Reason = name + " needs a vLLM plugin (" + vllm.OutOfTree[name] + ") that InferCrane does not install."
	case len(vllm.Architectures[name]) > 0:
		check.Reason = "vLLM " + vllm.Version + " registers " + name + " only as " + strings.ReplaceAll(strings.Join(vllm.Architectures[name], ", "), "_", " ") + ", not for " + task + "."
	default:
		check.Reason = name + " is not in the model registry of " + versions + "."
	}
	if check.CustomCode {
		check.Reason += " The repository ships its own model code, which InferCrane does not run."
	}
	return check
}

func supported(check ArchitectureCheck, name *string, runtime, version, reason string) ArchitectureCheck {
	check.State, check.Name, check.Runtime, check.RuntimeVersion, check.Reason = Supported, name, &runtime, &version, reason
	if check.CustomCode {
		check.Reason += " The repository also ships its own model code; InferCrane uses the runtime's implementation and does not run it."
	}
	return check
}

func generativeName(name string) bool {
	for _, suffix := range []string{"ForCausalLM", "ForConditionalGeneration", "LMHeadModel"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func memoryFit(model Model, task string, options EvaluateOptions) MemoryFit {
	declared := 0
	if model.Config != nil {
		declared = model.Config.ContextLength
	}
	contextTokens, concurrency := NormalizeLoad(options.ContextTokens, options.Concurrency, declared)
	fit := MemoryFit{State: Unknown, ContextTokens: contextTokens, Concurrency: concurrency, Layouts: []Layout{}, ConsideredGPUs: []string{}}
	fit.Assumptions = []string{
		fmt.Sprintf("%d requests at %d tokens each", concurrency, contextTokens),
		fmt.Sprintf("vLLM uses %.0f%% of each GPU's memory", GPUMemoryUtilization*100),
		fmt.Sprintf("%d GiB per GPU for activations and runtime buffers", RuntimeOverheadGiB),
		"16-bit KV cache; 32-bit weights served at 16 bits",
	}
	for _, gpu := range options.GPUs {
		fit.ConsideredGPUs = append(fit.ConsideredGPUs, gpu.Name)
	}
	if model.Weights == nil {
		fit.Reason = "The model's size is unknown, so InferCrane cannot size a GPU for it."
		return fit
	}
	var weights int64
	for dtype, count := range model.Weights.ByDType {
		size, known := servedBytesPerParameter(dtype)
		if !known {
			fit.Reason = "Hugging Face reports a tensor type (" + dtype + ") InferCrane does not size."
			return fit
		}
		weights += count * size
	}
	fit.WeightsBytes = &weights
	var kvTotal, perToken int64
	switch task {
	case TaskEmbeddings:
		fit.Assumptions = append(fit.Assumptions, "embedding models keep no KV cache between requests")
	default:
		if model.Config == nil {
			fit.Reason = "config.json could not be read, so the KV cache size is unknown."
			if model.ConfigState == ConfigTokenRequired {
				fit.Reason = "config.json needs a Hugging Face token, so the KV cache size is unknown."
			}
			return fit
		}
		perToken = model.Config.KVCacheBytesPerToken()
		if perToken <= 0 {
			fit.Reason = "config.json does not declare the layers and attention heads needed to size the KV cache."
			return fit
		}
		kvTotal = perToken * int64(contextTokens) * int64(concurrency)
		fit.Assumptions = append(fit.Assumptions, "every layer keeps a full-length KV cache, an upper bound for sliding-window models")
	}
	fit.KVCacheBytes = &kvTotal
	if len(options.GPUs) == 0 {
		fit.Reason = "No GPU has a current price observation, and InferCrane only proposes GPUs it can price."
		return fit
	}
	overhead := int64(RuntimeOverheadGiB) << 30
	heads, kvHeads := 0, 0
	if model.Config != nil {
		heads, kvHeads = model.Config.AttentionHeads, model.Config.KVHeads
		if kvHeads <= 0 {
			kvHeads = heads
		}
	}
	mla := model.Config != nil && model.Config.KVLoRARank > 0
	for _, gpu := range options.GPUs {
		usable := int64(float64(int64(gpu.MemoryGiB)<<30) * GPUMemoryUtilization)
		for _, size := range tensorParallelSizes {
			if size > 1 && heads > 0 && heads%size != 0 {
				continue
			}
			kvPerGPU := kvTotal
			if !mla && size > 1 {
				kvPerGPU = kvTotal / int64(min(size, max(kvHeads, 1)))
			}
			perGPU := weights/int64(size) + kvPerGPU + overhead
			if perGPU <= usable {
				fit.Layouts = append(fit.Layouts, Layout{GPU: gpu.Name, GPUCount: size, GPUMemoryGiB: gpu.MemoryGiB, PerGPURequiredBytes: perGPU})
				break
			}
		}
	}
	// Fewest GPUs first, then the smallest GPU: one replica on one device
	// needs no tensor parallelism and is the easiest to find capacity for.
	sort.SliceStable(fit.Layouts, func(i, j int) bool {
		a, b := fit.Layouts[i], fit.Layouts[j]
		if a.GPUCount != b.GPUCount {
			return a.GPUCount < b.GPUCount
		}
		if a.GPUMemoryGiB != b.GPUMemoryGiB {
			return a.GPUMemoryGiB < b.GPUMemoryGiB
		}
		return a.GPU < b.GPU
	})
	if len(fit.Layouts) == 0 {
		largest := options.GPUs[len(options.GPUs)-1]
		fit.State = DoesNotFit
		fit.Reason = fmt.Sprintf("It needs more than 8 × %s (%d GiB each) for %d requests at %d tokens.", largest.Name, largest.MemoryGiB, concurrency, contextTokens)
		return fit
	}
	chosen := fit.Layouts[0]
	required := chosen.PerGPURequiredBytes * int64(chosen.GPUCount)
	fit.State, fit.GPU, fit.GPUCount, fit.GPUMemoryGiB, fit.RequiredBytes = Fits, &chosen.GPU, &chosen.GPUCount, &chosen.GPUMemoryGiB, &required
	fit.Reason = fmt.Sprintf("Weights plus KV cache for %d requests at %d tokens fit %d × %s, the fewest and smallest priced GPUs that hold them.", concurrency, contextTokens, chosen.GPUCount, chosen.GPU)
	if task == TaskEmbeddings {
		fit.Reason = fmt.Sprintf("The weights fit %d × %s, the fewest and smallest priced GPUs that hold them; an embedding model keeps no KV cache.", chosen.GPUCount, chosen.GPU)
	}
	return fit
}

func checks(model Model, report Report, registries RegistrySnapshot) []Check {
	out := make([]Check, 0, 7)
	architectureState := map[string]string{Supported: Pass, Unsupported: Fail, Unknown: Unknown}[report.Architecture.State]
	label := "Architecture support unknown"
	switch report.Architecture.State {
	case Supported:
		label = "Runs on " + runtimeLabel(*report.Architecture.Runtime) + " " + *report.Architecture.RuntimeVersion
	case Unsupported:
		label = "Not supported by the pinned runtimes"
	}
	out = append(out, Check{Name: "architecture", State: architectureState, Label: label, Detail: report.Architecture.Reason})
	weights := Check{Name: "weights", State: Unknown, Label: "Size unknown", Detail: report.Weights.Reason}
	if report.Weights.Format == "gguf" && model.Weights == nil {
		weights.State, weights.Label = Fail, "GGUF files only"
	} else if report.Weights.State == "read" {
		weights.State, weights.Label = Pass, fmt.Sprintf("%s parameters · %s", Parameters(*report.Weights.Parameters), Bytes(*report.Weights.StoredBytes))
	}
	out = append(out, weights)
	if report.Weights.Quantization != nil {
		out = append(out, quantizationCheck(*report.Weights.Quantization, report, registries))
	}
	out = append(out, configCheck(model))
	if model.Files.Listed {
		tokenizer := Check{Name: "tokenizer", State: Pass, Label: "Tokenizer files present", Detail: "The runtime loads the tokenizer from these files."}
		if !model.Files.Tokenizer {
			tokenizer = Check{Name: "tokenizer", State: Unknown, Label: "No tokenizer file found", Detail: "No tokenizer.json, tokenizer.model, tokenizer_config.json, vocab.json, or tiktoken file is listed; the runtime may not start."}
		}
		out = append(out, tokenizer)
	}
	memory := Check{Name: "memory", State: Unknown, Label: "GPU fit unknown", Detail: report.Memory.Reason}
	switch report.Memory.State {
	case Fits:
		memory.State, memory.Label = Pass, fmt.Sprintf("Fits %d × %s", *report.Memory.GPUCount, *report.Memory.GPU)
	case DoesNotFit:
		memory.State, memory.Label = Fail, "Too large for one replica"
	}
	out = append(out, memory)
	licenseCheck := Check{Name: "license", State: Info, Label: "License: " + model.License, Detail: "Declared by the model card; review its terms before production use."}
	if model.License == "" {
		licenseCheck = Check{Name: "license", State: Unknown, Label: "No license declared", Detail: "The model card declares no license; review the repository before production use."}
	}
	out = append(out, licenseCheck)
	accessCheck := Check{Name: "access", State: Pass, Label: "Public", Detail: "Anyone can download this model."}
	switch report.Access.State {
	case AccessTokenUsed:
		accessCheck.Label, accessCheck.Detail = "Gated: read with your token", "Read with your saved Hugging Face token."
		if model.Private {
			accessCheck.Label = "Private: read with your token"
		}
	case AccessTokenRequired:
		accessCheck.State, accessCheck.Label = Fail, "Gated: needs a Hugging Face token"
		accessCheck.Detail = "Accept the model's terms on Hugging Face, then save a token under Settings › Connections."
		if model.Private {
			accessCheck.Label = "Private: needs a Hugging Face token"
		}
		if model.TokenUsed {
			accessCheck.Detail = "The saved token cannot read config.json. Accept the model's terms on Hugging Face with that account."
		}
	}
	out = append(out, accessCheck)
	if report.Architecture.CustomCode {
		out = append(out, Check{Name: "custom_code", State: Info, Label: "Ships its own model code", Detail: "InferCrane never runs repository code; it uses the runtime's own implementation."})
	}
	return out
}

// configCheck says whether config.json was read at the pinned revision.
func configCheck(model Model) Check {
	switch model.ConfigState {
	case ConfigRead:
		return Check{Name: "config", State: Pass, Label: "config.json read", Detail: "Read at the pinned revision."}
	case ConfigTokenRequired:
		return Check{Name: "config", State: Unknown, Label: "config.json needs a token", Detail: "The repository did not return config.json without a token."}
	case ConfigInvalid:
		return Check{Name: "config", State: Fail, Label: "config.json is not valid", Detail: "config.json could not be parsed."}
	}
	return Check{Name: "config", State: Fail, Label: "No config.json", Detail: "The runtimes need config.json at the model's root."}
}

func quantizationCheck(method string, report Report, registries RegistrySnapshot) Check {
	label := "Quantized: " + method
	if report.Architecture.Runtime != nil && *report.Architecture.Runtime != "vllm" {
		return Check{Name: "quantization", State: Unknown, Label: label, Detail: "Whether " + runtimeLabel(*report.Architecture.Runtime) + " loads this quantization was not checked."}
	}
	vllm, _ := registries.Runtime("vllm")
	if !slices.Contains(vllm.QuantizationMethods, method) {
		return Check{Name: "quantization", State: Unknown, Label: label, Detail: method + " is not a vLLM " + vllm.Version + " quantization method."}
	}
	return Check{Name: "quantization", State: Pass, Label: label, Detail: method + " is a vLLM " + vllm.Version + " quantization method."}
}

func verdict(model Model, report Report) (string, string) {
	switch {
	case model.Disabled:
		return Unsupported, "Hugging Face has disabled this repository."
	case report.Task == "" && report.Architecture.State == Unsupported:
		return Unsupported, report.Architecture.Reason
	case report.Weights.Format == "gguf" && model.Weights == nil:
		return Unsupported, report.Weights.Reason
	case report.Architecture.State == Unsupported:
		return Unsupported, report.Architecture.Reason
	case report.Memory.State == DoesNotFit:
		return Unsupported, report.Memory.Reason
	case report.Access.State == AccessTokenRequired:
		return Unknown, "Gated model: add a Hugging Face token to finish the check."
	case report.Architecture.State == Unknown:
		return Unknown, report.Architecture.Reason
	case report.Memory.State == Unknown:
		return Unknown, report.Memory.Reason
	}
	return Supported, "Checked automatically: " + report.Architecture.Reason + " " + report.Memory.Reason
}

func runtimeLabel(runtime string) string {
	switch runtime {
	case "vllm":
		return "vLLM"
	case "sglang":
		return "SGLang"
	}
	return runtime
}

// Parameters formats a parameter count: "14.7B", "335M".
func Parameters(count int64) string {
	switch {
	case count >= 1_000_000_000:
		return trimDecimal(float64(count)/1e9) + "B"
	case count >= 1_000_000:
		return trimDecimal(float64(count)/1e6) + "M"
	case count >= 1_000:
		return trimDecimal(float64(count)/1e3) + "K"
	}
	return fmt.Sprintf("%d", count)
}

// Bytes formats a size in decimal gigabytes or megabytes: "29.3 GB".
func Bytes(count int64) string {
	if count >= 1_000_000_000 {
		return trimDecimal(float64(count)/1e9) + " GB"
	}
	return trimDecimal(float64(count)/1e6) + " MB"
}

func trimDecimal(value float64) string {
	text := fmt.Sprintf("%.1f", value)
	return strings.TrimSuffix(text, ".0")
}
