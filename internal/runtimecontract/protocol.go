package runtimecontract

// ProtocolCapabilities describes independently qualified OpenAI-compatible
// request surfaces. It is intentionally not a universal request schema: the
// gateway forwards each qualified protocol body and response without lossy
// translation.
type ProtocolCapabilities struct {
	ChatCompletions bool `json:"chat_completions"`
	Responses       bool `json:"responses"`
	Embeddings      bool `json:"embeddings"`
	Batch           bool `json:"batch"`
	Completions     bool `json:"completions"`
	Streaming       bool `json:"streaming"`
	ToolCalling     bool `json:"tool_calling"`
}

func (c ProtocolCapabilities) Supports(operation string) bool {
	switch operation {
	case "chat":
		return c.ChatCompletions
	case "responses":
		return c.Responses
	case "embeddings":
		return c.Embeddings
	case "completions":
		return c.Completions
	case "batch":
		return c.Batch
	default:
		return false
	}
}
