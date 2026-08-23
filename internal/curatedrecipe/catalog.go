// Package curatedrecipe is a small reviewed configuration catalog. Entries
// are not benchmark claims; measured recipes remain control-plane evidence.
package curatedrecipe

import "strings"

type Entry struct {
	Name             string           `json:"name"`
	DisplayName      string           `json:"display_name"`
	Publisher        string           `json:"publisher"`
	Description      string           `json:"description"`
	UseCase          string           `json:"use_case"`
	Tasks            []string         `json:"tasks"`
	Model            string           `json:"model"`
	Revision         string           `json:"revision"`
	Runtime          string           `json:"runtime"`
	RuntimeArgs      []string         `json:"runtime_args"`
	Protocol         string           `json:"protocol"`
	Capabilities     []string         `json:"capabilities"`
	InputModalities  []string         `json:"input_modalities"`
	OutputModalities []string         `json:"output_modalities"`
	License          string           `json:"license"`
	LicenseURL       string           `json:"license_url"`
	Gated            bool             `json:"gated"`
	EvidenceClass    string           `json:"evidence_class"`
	EvidenceSummary  string           `json:"evidence_summary"`
	Source           string           `json:"source"`
	ReviewedAt       string           `json:"reviewed_at"`
	Profiles         []ServingProfile `json:"profiles"`
}

// ServingProfile is a reviewed starting configuration. It intentionally does
// not contain price, throughput, or latency estimates: those only become
// trustworthy after a tenant records measured benchmark evidence.
type ServingProfile struct {
	Name               string   `json:"name"`
	DisplayName        string   `json:"display_name"`
	Description        string   `json:"description"`
	Runtime            string   `json:"runtime"`
	ComputeMode        string   `json:"compute_mode"`
	GPUHint            string   `json:"gpu_hint"`
	MinReplicas        int      `json:"min_replicas"`
	MaxReplicas        int      `json:"max_replicas"`
	RuntimeArgs        []string `json:"runtime_args"`
	EvidenceClass      string   `json:"evidence_class"`
	QualificationScope string   `json:"qualification_scope"`
	Limitations        []string `json:"limitations"`
}

const configurationEvidence = "configuration-verified"
const configurationScope = "Model identity, revision, license metadata, protocol, and runtime configuration were reviewed. Provider capacity, GPU fit, performance, and price are not measured claims."

func elasticProfile(runtime string) ServingProfile {
	return elasticProfileFor(runtime, "L40S")
}

// vLLM generation profiles are candidate configurations grounded in upstream
// tuning guidance. They are deliberately configuration-verified rather than
// performance claims; the exact model/GPU/workload must still be benchmarked.
func vllmGenerationProfiles(gpu string) []ServingProfile {
	balanced := elasticProfileFor("vllm", gpu)
	balanced.Name = "vllm-balanced"
	balanced.DisplayName = "vLLM balanced"
	balanced.RuntimeArgs = []string{"--enable-prefix-caching"}
	balanced.Description = "Balanced generation candidate with prefix reuse enabled for repeated prompt prefixes."
	balanced.Limitations = append(balanced.Limitations, "Prefix caching helps only workloads with reusable prefixes and must be measured on the intended prompt distribution.")

	interactive := balanced
	interactive.Name = "vllm-interactive"
	interactive.DisplayName = "vLLM interactive"
	interactive.Description = "Decode-oriented candidate with a smaller chunked-prefill token budget."
	interactive.RuntimeArgs = []string{"--enable-prefix-caching", "--max-num-batched-tokens", "2048"}
	interactive.Limitations = append([]string{}, balanced.Limitations...)
	interactive.Limitations = append(interactive.Limitations, "A smaller batch-token budget may improve inter-token latency while reducing prefill throughput; compare exact workloads.")

	throughput := balanced
	throughput.Name = "vllm-throughput"
	throughput.DisplayName = "vLLM throughput"
	throughput.Description = "Throughput-oriented candidate with a larger chunked-prefill token budget."
	throughput.RuntimeArgs = []string{"--enable-prefix-caching", "--max-num-batched-tokens", "16384"}
	throughput.Limitations = append([]string{}, balanced.Limitations...)
	throughput.Limitations = append(throughput.Limitations, "A larger batch-token budget can improve throughput and prefill TTFT while worsening decode latency or memory pressure; qualify overload behavior.")

	return []ServingProfile{balanced, interactive, throughput}
}

func elasticProfileFor(runtime, gpu string) ServingProfile {
	displayRuntime := map[string]string{
		"vllm":   "vLLM",
		"sglang": "SGLang",
	}[runtime]
	if displayRuntime == "" {
		displayRuntime = strings.ToUpper(runtime[:1]) + runtime[1:]
	}
	return ServingProfile{
		Name:               runtime + "-elastic",
		DisplayName:        displayRuntime + " elastic",
		Description:        "A durable autoscaling deployment with one warm replica and bounded scale-out.",
		Runtime:            runtime,
		ComputeMode:        "elastic",
		GPUHint:            gpu,
		MinReplicas:        1,
		MaxReplicas:        2,
		RuntimeArgs:        []string{},
		EvidenceClass:      configurationEvidence,
		QualificationScope: configurationScope,
		Limitations:        []string{"Confirm accelerator memory fit and provider availability with plan and real qualification before production."},
	}
}

func elasticProfileWithLimitations(runtime, gpu string, limitations ...string) ServingProfile {
	profile := elasticProfileFor(runtime, gpu)
	profile.Limitations = append(profile.Limitations, limitations...)
	return profile
}

var catalog = []Entry{
	{Name: "mistral-7b-instruct", DisplayName: "Mistral 7B Instruct", Publisher: "Mistral AI", Description: "Compact instruction model with an Apache-2.0 model license.", UseCase: "chat and tool-oriented inference", Tasks: []string{"chat", "tools"}, Model: "mistralai/Mistral-7B-Instruct-v0.3", Revision: "c170c708c41dac9275d15a8fff4eca08d52bab71", Runtime: "vllm", Protocol: "chat", Capabilities: []string{"chat-completions", "streaming", "tool-calling"}, InputModalities: []string{"text"}, OutputModalities: []string{"text"}, License: "apache-2.0", LicenseURL: "https://huggingface.co/mistralai/Mistral-7B-Instruct-v0.3/blob/main/LICENSE", EvidenceClass: configurationEvidence, EvidenceSummary: configurationScope, Source: "https://huggingface.co/mistralai/Mistral-7B-Instruct-v0.3", ReviewedAt: "2026-08-13", Profiles: vllmGenerationProfiles("L40S")},
	{Name: "qwen3-8b", DisplayName: "Qwen3 8B", Publisher: "Qwen", Description: "Compact reasoning-capable model with vLLM and SGLang model-card guidance.", UseCase: "chat, reasoning, and tools", Tasks: []string{"chat", "reasoning", "tools"}, Model: "Qwen/Qwen3-8B", Revision: "b968826d9c46dd6066d109eabc6255188de91218", Runtime: "vllm", Protocol: "chat", Capabilities: []string{"chat-completions", "streaming", "tool-calling", "structured-output"}, InputModalities: []string{"text"}, OutputModalities: []string{"text"}, License: "apache-2.0", LicenseURL: "https://huggingface.co/Qwen/Qwen3-8B/blob/main/LICENSE", EvidenceClass: configurationEvidence, EvidenceSummary: configurationScope, Source: "https://huggingface.co/Qwen/Qwen3-8B", ReviewedAt: "2026-08-13", Profiles: append(vllmGenerationProfiles("L40S"), elasticProfile("sglang"))},
	{Name: "llama-3.1-8b-instruct", DisplayName: "Llama 3.1 8B Instruct", Publisher: "Meta", Description: "Widely used gated instruction model; access and Llama 3.1 license acceptance are required.", UseCase: "chat and tool-oriented inference", Tasks: []string{"chat", "tools"}, Model: "meta-llama/Llama-3.1-8B-Instruct", Revision: "0e9e39f249a16976918f6564b8830bc894c89659", Runtime: "vllm", Protocol: "chat", Capabilities: []string{"chat-completions", "streaming", "tool-calling"}, InputModalities: []string{"text"}, OutputModalities: []string{"text"}, License: "llama3.1", LicenseURL: "https://llama.meta.com/llama3_1/license/", Gated: true, EvidenceClass: configurationEvidence, EvidenceSummary: configurationScope, Source: "https://huggingface.co/meta-llama/Llama-3.1-8B-Instruct", ReviewedAt: "2026-08-13", Profiles: vllmGenerationProfiles("L40S")},
	{Name: "bge-m3-embeddings", DisplayName: "BGE-M3", Publisher: "BAAI", Description: "Multilingual embedding model under the MIT license.", UseCase: "embeddings, retrieval, and RAG", Tasks: []string{"embeddings", "retrieval", "rag"}, Model: "BAAI/bge-m3", Revision: "5617a9f61b028005a4858fdac845db406aefb181", Runtime: "vllm", Protocol: "embeddings", Capabilities: []string{"embeddings"}, InputModalities: []string{"text"}, OutputModalities: []string{"embeddings"}, License: "mit", LicenseURL: "https://huggingface.co/BAAI/bge-m3/blob/main/LICENSE", EvidenceClass: configurationEvidence, EvidenceSummary: configurationScope, Source: "https://huggingface.co/BAAI/bge-m3", ReviewedAt: "2026-08-13", Profiles: []ServingProfile{elasticProfile("vllm")}},
	{Name: "qwen2.5-coder-7b-instruct", DisplayName: "Qwen2.5 Coder 7B Instruct", Publisher: "Qwen", Description: "Compact code-focused instruction model for completion, explanation, and code repair workloads.", UseCase: "coding assistants and code generation", Tasks: []string{"coding", "chat"}, Model: "Qwen/Qwen2.5-Coder-7B-Instruct", Revision: "c03e6d358207e414f1eca0bb1891e29f1db0e242", Runtime: "vllm", Protocol: "chat", Capabilities: []string{"chat-completions", "streaming"}, InputModalities: []string{"text"}, OutputModalities: []string{"text"}, License: "apache-2.0", LicenseURL: "https://huggingface.co/Qwen/Qwen2.5-Coder-7B-Instruct/blob/main/LICENSE", EvidenceClass: configurationEvidence, EvidenceSummary: configurationScope, Source: "https://huggingface.co/Qwen/Qwen2.5-Coder-7B-Instruct", ReviewedAt: "2026-08-19", Profiles: vllmGenerationProfiles("L40S")},
	{Name: "deepseek-r1-distill-qwen-7b", DisplayName: "DeepSeek R1 Distill Qwen 7B", Publisher: "DeepSeek", Description: "Compact reasoning-oriented distillation built from the Qwen2.5 family under the MIT license.", UseCase: "reasoning and analysis", Tasks: []string{"reasoning", "chat"}, Model: "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B", Revision: "916b56a44061fd5cd7d6a8fb632557ed4f724f60", Runtime: "vllm", Protocol: "chat", Capabilities: []string{"chat-completions", "streaming"}, InputModalities: []string{"text"}, OutputModalities: []string{"text"}, License: "mit", LicenseURL: "https://huggingface.co/deepseek-ai/DeepSeek-R1-Distill-Qwen-7B/blob/main/LICENSE", EvidenceClass: configurationEvidence, EvidenceSummary: configurationScope, Source: "https://huggingface.co/deepseek-ai/DeepSeek-R1-Distill-Qwen-7B", ReviewedAt: "2026-08-19", Profiles: vllmGenerationProfiles("L40S")},
	{Name: "gemma-3-4b-it", DisplayName: "Gemma 3 4B IT", Publisher: "Google", Description: "Compact multimodal instruction model for text-and-image application workflows; upstream terms and access acceptance are required.", UseCase: "compact multimodal chat and document understanding", Tasks: []string{"vision", "chat", "documents"}, Model: "google/gemma-3-4b-it", Revision: "093f9f388b31de276ce2de164bdc2081324b9767", Runtime: "vllm", Protocol: "chat", Capabilities: []string{"chat-completions", "streaming", "vision"}, InputModalities: []string{"text", "image"}, OutputModalities: []string{"text"}, License: "gemma", LicenseURL: "https://ai.google.dev/gemma/terms", Gated: true, EvidenceClass: configurationEvidence, EvidenceSummary: configurationScope, Source: "https://huggingface.co/google/gemma-3-4b-it", ReviewedAt: "2026-08-19", Profiles: []ServingProfile{elasticProfileWithLimitations("vllm", "A10G", "Multimodal memory use depends on image count, resolution, and prompt length; qualify the exact prompt shape.")}},
	{Name: "qwen2.5-vl-7b-instruct", DisplayName: "Qwen2.5 VL 7B Instruct", Publisher: "Qwen", Description: "Vision-language instruction model for document, screenshot, and image understanding.", UseCase: "document understanding and visual question answering", Tasks: []string{"vision", "documents", "chat"}, Model: "Qwen/Qwen2.5-VL-7B-Instruct", Revision: "cc594898137f460bfe9f0759e9844b3ce807cfb5", Runtime: "vllm", Protocol: "chat", Capabilities: []string{"chat-completions", "streaming", "vision"}, InputModalities: []string{"text", "image", "video"}, OutputModalities: []string{"text"}, License: "apache-2.0", LicenseURL: "https://huggingface.co/Qwen/Qwen2.5-VL-7B-Instruct/blob/main/LICENSE", EvidenceClass: configurationEvidence, EvidenceSummary: configurationScope, Source: "https://huggingface.co/Qwen/Qwen2.5-VL-7B-Instruct", ReviewedAt: "2026-08-19", Profiles: []ServingProfile{elasticProfileWithLimitations("vllm", "L40S", "Image resolution, item count, video duration, and media-fetch policy materially affect memory and latency; qualify bounded media inputs.")}},
	{Name: "granite-3.3-8b-instruct", DisplayName: "Granite 3.3 8B Instruct", Publisher: "IBM", Description: "Apache-2.0 instruction model for enterprise text generation, extraction, and summarization workloads.", UseCase: "enterprise assistants, extraction, and summarization", Tasks: []string{"chat", "extraction", "summarization"}, Model: "ibm-granite/granite-3.3-8b-instruct", Revision: "51dd4bc2ade4059a6bd87649d68aa11e4fb2529b", Runtime: "vllm", Protocol: "chat", Capabilities: []string{"chat-completions", "streaming"}, InputModalities: []string{"text"}, OutputModalities: []string{"text"}, License: "apache-2.0", LicenseURL: "https://huggingface.co/ibm-granite/granite-3.3-8b-instruct/blob/main/LICENSE", EvidenceClass: configurationEvidence, EvidenceSummary: configurationScope, Source: "https://huggingface.co/ibm-granite/granite-3.3-8b-instruct", ReviewedAt: "2026-08-19", Profiles: vllmGenerationProfiles("L40S")},
}

func cloneEntry(entry Entry) Entry {
	entry.Tasks = append([]string{}, entry.Tasks...)
	entry.RuntimeArgs = append([]string{}, entry.RuntimeArgs...)
	entry.Capabilities = append([]string{}, entry.Capabilities...)
	entry.InputModalities = append([]string{}, entry.InputModalities...)
	entry.OutputModalities = append([]string{}, entry.OutputModalities...)
	entry.Profiles = append([]ServingProfile{}, entry.Profiles...)
	for index := range entry.Profiles {
		entry.Profiles[index].RuntimeArgs = append([]string{}, entry.Profiles[index].RuntimeArgs...)
		entry.Profiles[index].Limitations = append([]string{}, entry.Profiles[index].Limitations...)
	}
	return entry
}

func All() []Entry {
	out := make([]Entry, 0, len(catalog))
	for _, entry := range catalog {
		out = append(out, cloneEntry(entry))
	}
	return out
}

func Search(query string) []Entry {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return All()
	}
	var out []Entry
	for _, entry := range catalog {
		haystack := strings.ToLower(entry.Name + " " + entry.DisplayName + " " + entry.Publisher + " " + entry.Model + " " + entry.UseCase + " " + strings.Join(entry.Tasks, " ") + " " + entry.Runtime + " " + entry.Protocol)
		if strings.Contains(haystack, query) {
			out = append(out, cloneEntry(entry))
		}
	}
	return out
}

func Get(name string) (Entry, bool) {
	for _, entry := range catalog {
		if entry.Name == name {
			return cloneEntry(entry), true
		}
	}
	return Entry{}, false
}
