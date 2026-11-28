package modelcheck

import (
	"maps"
	"strings"
)

// reviewedSize is what the memory estimate needs for one reviewed catalog
// model at its pinned commit, recorded so that a control plane that does not
// read Hugging Face can still size a deploy's context. Nothing here is
// estimated. Each entry was read once, without a token, from
//
//	https://huggingface.co/{Repository}/raw/{Commit}/config.json
//	https://huggingface.co/api/models/{Repository}/revision/{Commit}?expand[]=safetensors
//
// the same two documents the live check reads, and parsed by ParseConfig.
// ConfigDigest is the sha256 of that config.json, so each entry can be
// checked against the file.
type reviewedSize struct {
	Repository   string
	Commit       string
	ConfigDigest string
	// Architectures is config.json's list; it decides the task.
	Architectures []string
	// Config is config.json's declared context and the attention shape the
	// KV cache size per token is computed from.
	Config Config
	// Weights is the safetensors summary's parameter count by dtype, or nil
	// when Hugging Face publishes none for the commit.
	Weights map[string]int64
}

// reviewedSizes covers the reviewed catalog's models on the built-in
// runtimes. Two have no entry: meta-llama/Llama-3.1-8B-Instruct and
// google/gemma-3-4b-it are gated, and their config.json is not readable
// without a token. The catalog's custom-image models set their own context
// in their commands, and a deploy never sizes a custom image.
var reviewedSizes = []reviewedSize{
	{
		Repository: "mistralai/Mistral-7B-Instruct-v0.3", Commit: "c170c708c41dac9275d15a8fff4eca08d52bab71",
		ConfigDigest:  "sha256:affafc6478ec0fd07a32f0ca57aa2fc57743f4d17d6730f86a96ac24d1507f99",
		Architectures: []string{"MistralForCausalLM"},
		Config:        Config{Layers: 32, HiddenSize: 4096, AttentionHeads: 32, KVHeads: 8, ContextLength: 32768},
		Weights:       map[string]int64{"BF16": 7248023552},
	},
	{
		Repository: "Qwen/Qwen3-8B", Commit: "b968826d9c46dd6066d109eabc6255188de91218",
		ConfigDigest:  "sha256:f7c4eadfbbf522470667b797a3c89be2524832d2d599797248dc304fff447c30",
		Architectures: []string{"Qwen3ForCausalLM"},
		Config:        Config{Layers: 36, HiddenSize: 4096, AttentionHeads: 32, KVHeads: 8, HeadDim: 128, ContextLength: 40960},
		Weights:       map[string]int64{"BF16": 8190735360},
	},
	{
		// An embedding model: the task is all a deploy needs.
		Repository: "BAAI/bge-m3", Commit: "5617a9f61b028005a4858fdac845db406aefb181",
		ConfigDigest:  "sha256:26159e7ad065073448460117eb24b7a4572f6f4e78eadff65dc0a11c052449fa",
		Architectures: []string{"XLMRobertaModel"},
	},
	{
		Repository: "Qwen/Qwen2.5-Coder-7B-Instruct", Commit: "c03e6d358207e414f1eca0bb1891e29f1db0e242",
		ConfigDigest:  "sha256:c0242402ad6a13b331ea320feea8c7e3776ffb7a4eff0757b9cd667e116d9a28",
		Architectures: []string{"Qwen2ForCausalLM"},
		Config:        Config{Layers: 28, HiddenSize: 3584, AttentionHeads: 28, KVHeads: 4, ContextLength: 32768},
		Weights:       map[string]int64{"BF16": 7615616512},
	},
	{
		Repository: "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B", Commit: "916b56a44061fd5cd7d6a8fb632557ed4f724f60",
		ConfigDigest:  "sha256:8290d3c1e385e2616572eb6926f7e2745f901d1c13da95970326a8a73f6c379c",
		Architectures: []string{"Qwen2ForCausalLM"},
		Config:        Config{Layers: 28, HiddenSize: 3584, AttentionHeads: 28, KVHeads: 4, ContextLength: 131072},
		Weights:       map[string]int64{"BF16": 7615616512},
	},
	{
		Repository: "Qwen/Qwen2.5-VL-7B-Instruct", Commit: "cc594898137f460bfe9f0759e9844b3ce807cfb5",
		ConfigDigest:  "sha256:77d9ec7321cc572e3579e2c84799c9cadaded63c49ce93b101733349fc330c43",
		Architectures: []string{"Qwen2_5_VLForConditionalGeneration"},
		Config:        Config{Layers: 28, HiddenSize: 3584, AttentionHeads: 28, KVHeads: 4, ContextLength: 128000},
		Weights:       map[string]int64{"BF16": 8292166656},
	},
	{
		Repository: "ibm-granite/granite-3.3-8b-instruct", Commit: "51dd4bc2ade4059a6bd87649d68aa11e4fb2529b",
		ConfigDigest:  "sha256:1313edf0d39dcf7ed35a072d341ec11b516c12acc4267cfb6d248c6bdcdddcb7",
		Architectures: []string{"GraniteForCausalLM"},
		Config:        Config{Layers: 40, HiddenSize: 4096, AttentionHeads: 32, KVHeads: 8, ContextLength: 131072},
		Weights:       map[string]int64{"BF16": 8170864640},
	},
}

// ReviewedModel returns a reviewed catalog model's recorded sizes as the Model
// a check evaluates, when revision is the commit they were read at or empty.
// An empty revision deploys the repository's main branch, which is the
// pinned commit for every entry when it was recorded. Any other revision, and
// any model without an entry, reports false: its sizes are unknown.
func ReviewedModel(repository, revision string) (Model, bool) {
	for _, entry := range reviewedSizes {
		if !strings.EqualFold(entry.Repository, strings.TrimSpace(repository)) {
			continue
		}
		if revision = strings.TrimSpace(revision); revision != "" && !strings.EqualFold(revision, entry.Commit) {
			return Model{}, false
		}
		config := entry.Config
		config.Digest, config.Architectures = entry.ConfigDigest, append([]string(nil), entry.Architectures...)
		model := Model{
			Repository: entry.Repository, RequestedRevision: revision, Commit: entry.Commit,
			Architectures: append([]string(nil), entry.Architectures...), Config: &config, ConfigState: ConfigRead,
		}
		if len(entry.Weights) > 0 {
			model.Weights = &Weights{ByDType: maps.Clone(entry.Weights)}
			for _, count := range entry.Weights {
				model.Weights.Parameters += count
			}
		}
		return model, true
	}
	return Model{}, false
}
