// Package curatedrecipe is a small reviewed configuration catalog. Entries
// are not benchmark claims; measured recipes remain control-plane evidence.
package curatedrecipe

import "strings"

type Entry struct {
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	UseCase       string   `json:"use_case"`
	Model         string   `json:"model"`
	Revision      string   `json:"revision"`
	Runtime       string   `json:"runtime"`
	RuntimeArgs   []string `json:"runtime_args"`
	Protocol      string   `json:"protocol"`
	License       string   `json:"license"`
	Gated         bool     `json:"gated"`
	EvidenceClass string   `json:"evidence_class"`
	Source        string   `json:"source"`
	ReviewedAt    string   `json:"reviewed_at"`
}

var catalog = []Entry{
	{Name: "mistral-7b-instruct", Description: "Compact instruction model with an Apache-2.0 model license.", UseCase: "chat and tool-oriented inference", Model: "mistralai/Mistral-7B-Instruct-v0.3", Revision: "c170c708c41dac9275d15a8fff4eca08d52bab71", Runtime: "vllm", Protocol: "chat", License: "apache-2.0", EvidenceClass: "configuration-only", Source: "https://huggingface.co/mistralai/Mistral-7B-Instruct-v0.3", ReviewedAt: "2026-08-13"},
	{Name: "qwen3-8b", Description: "Compact reasoning-capable model with vLLM and SGLang model-card guidance.", UseCase: "chat, reasoning, and tools", Model: "Qwen/Qwen3-8B", Revision: "b968826d9c46dd6066d109eabc6255188de91218", Runtime: "vllm", Protocol: "chat", License: "apache-2.0", EvidenceClass: "configuration-only", Source: "https://huggingface.co/Qwen/Qwen3-8B", ReviewedAt: "2026-08-13"},
	{Name: "llama-3.1-8b-instruct", Description: "Widely used gated instruction model; access and Llama 3.1 license acceptance are required.", UseCase: "chat and tool-oriented inference", Model: "meta-llama/Llama-3.1-8B-Instruct", Revision: "0e9e39f249a16976918f6564b8830bc894c89659", Runtime: "vllm", Protocol: "chat", License: "llama3.1", Gated: true, EvidenceClass: "configuration-only", Source: "https://huggingface.co/meta-llama/Llama-3.1-8B-Instruct", ReviewedAt: "2026-08-13"},
	{Name: "bge-m3-embeddings", Description: "Multilingual embedding model under the MIT license.", UseCase: "embeddings, retrieval, and RAG", Model: "BAAI/bge-m3", Revision: "5617a9f61b028005a4858fdac845db406aefb181", Runtime: "vllm", Protocol: "embeddings", License: "mit", EvidenceClass: "configuration-only", Source: "https://huggingface.co/BAAI/bge-m3", ReviewedAt: "2026-08-13"},
}

func All() []Entry { return append([]Entry(nil), catalog...) }

func Search(query string) []Entry {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return All()
	}
	var out []Entry
	for _, entry := range catalog {
		haystack := strings.ToLower(entry.Name + " " + entry.Model + " " + entry.UseCase + " " + entry.Runtime + " " + entry.Protocol)
		if strings.Contains(haystack, query) {
			out = append(out, entry)
		}
	}
	return out
}

func Get(name string) (Entry, bool) {
	for _, entry := range catalog {
		if entry.Name == name {
			return entry, true
		}
	}
	return Entry{}, false
}
