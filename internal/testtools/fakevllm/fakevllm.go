// Package fakevllm is a development fake of a vLLM OpenAI-compatible server
// for Compose and tests. It never runs in production.
package fakevllm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// ToolCall is the function call the fake makes when a request offers tools.
type ToolCall struct {
	Name, Arguments string
}

// DefaultToolCall is the call made when Options leaves ToolCall empty.
var DefaultToolCall = ToolCall{Name: "weather", Arguments: `{"city":"Berlin"}`}

// Options configures the fake.
type Options struct {
	Worker, Model, APIKey string
	ToolCall              ToolCall
	// Delay is added before every chat answer, so one fake can serve
	// slower than another of the same setup.
	Delay time.Duration
}

// Handler serves the fake's routes.
func Handler(options Options) http.Handler {
	call := options.ToolCall
	if call == (ToolCall{}) {
		call = DefaultToolCall
	}
	var running, waiting atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"object": "list", "data": []map[string]string{{"id": options.Model, "object": "model"}}})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "vllm:num_requests_running %d\nvllm:num_requests_waiting %d\nvllm:kv_cache_usage_perc 0\n", running.Load(), waiting.Load())
	})
	mux.HandleFunc("POST /test/metrics", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Running int64 `json:"running"`
			Waiting int64 `json:"waiting"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Running < 0 || body.Waiting < 0 {
			http.Error(w, "non-negative running and waiting are required", http.StatusBadRequest)
			return
		}
		running.Store(body.Running)
		waiting.Store(body.Waiting)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if options.Delay > 0 {
			select {
			case <-time.After(options.Delay):
			case <-r.Context().Done():
				return
			}
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if streaming, _ := body["stream"].(bool); streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-fake\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", body["model"], "response from "+options.Worker)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			// Load generators count tokens from the server's usage report.
			fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-fake\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":3,\"total_tokens\":%d}}\n\n", body["model"], promptTokens(body), promptTokens(body)+3)
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
			write(w, map[string]any{"id": "chatcmpl-fake", "object": "chat.completion", "model": body["model"], "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_fake", "type": "function", "function": map[string]string{"name": call.Name, "arguments": call.Arguments}}}}, "finish_reason": "tool_calls"}}})
			return
		}
		if format, ok := body["response_format"].(map[string]any); ok && format["type"] == "json_schema" {
			write(w, map[string]any{"id": "chatcmpl-fake", "object": "chat.completion", "model": body["model"], "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": `{"answer":"ok"}`}, "finish_reason": "stop"}}})
			return
		}
		write(w, map[string]any{"id": "chatcmpl-fake", "object": "chat.completion", "model": body["model"], "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": "response from " + options.Worker}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": promptTokens(body), "completion_tokens": 3, "total_tokens": promptTokens(body) + 3}})
	})
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		write(w, map[string]any{"id": "cmpl-fake", "object": "text_completion", "model": body["model"], "choices": []any{map[string]any{"index": 0, "text": "response from " + options.Worker, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 3, "total_tokens": 4}})
	})
	mux.HandleFunc("POST /v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		write(w, map[string]any{"object": "list", "model": body["model"], "data": []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}}}, "usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1}})
	})
	handler := http.Handler(mux)
	if options.APIKey != "" {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/health" && r.Header.Get("Authorization") != "Bearer "+options.APIKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			mux.ServeHTTP(w, r)
		})
	}
	return handler
}

func write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// promptTokens approximates a prompt's token count by its words, so a fake
// usage report scales with the request.
func promptTokens(body map[string]any) int {
	count := 0
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		if message, ok := raw.(map[string]any); ok {
			if text, ok := message["content"].(string); ok {
				count += len(strings.Fields(text))
			}
		}
	}
	if count == 0 {
		count = 1
	}
	return count
}
