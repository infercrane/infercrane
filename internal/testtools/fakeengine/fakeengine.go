// Package fakeengine serves deterministic stand-ins for local model servers
// (vLLM, SGLang, llama.cpp, Ollama, LM Studio) for agent tests. It is a
// development fake: production code never imports it.
package fakeengine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// Flavor is the server a fake imitates.
type Flavor string

const (
	VLLM     Flavor = "vllm"
	SGLang   Flavor = "sglang"
	LlamaCPP Flavor = "llama.cpp"
	Ollama   Flavor = "ollama"
	LMStudio Flavor = "lm-studio"
)

// Options configure a fake.
type Options struct {
	Flavor Flavor
	// Models lists what /v1/models returns; Loaded is the model an Ollama or
	// LM Studio fake reports as loaded.
	Models []string
	Loaded string
	// APIKey, when set, is required as a bearer token on every request.
	APIKey string
	// Metrics turns /metrics on for vLLM, SGLang, and llama.cpp.
	Metrics bool
	// ExtraMetrics is appended to /metrics, for unrelated families that the
	// agent must drop before relaying.
	ExtraMetrics string
}

// Engine is a running fake. Counters advance with every completion, so two
// scrapes around traffic yield real window values.
type Engine struct {
	*httptest.Server
	options Options

	mu          sync.Mutex
	completions int
	prompt      int
	generated   int
	// Prompts records every prompt received, so tests can prove the agent
	// relays none of them.
	Prompts []string
}

// Start serves a fake engine until Close.
func Start(options Options) *Engine {
	if len(options.Models) == 0 {
		options.Models = []string{"Qwen/Qwen3-8B"}
	}
	engine := &Engine{options: options}
	engine.Server = httptest.NewServer(engine.handler())
	return engine
}

// Completions reports how many completions the fake served.
func (e *Engine) Completions() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.completions
}

func (e *Engine) authorized(r *http.Request) bool {
	return e.options.APIKey == "" || r.Header.Get("Authorization") == "Bearer "+e.options.APIKey
}

func (e *Engine) handler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		data := []map[string]string{}
		for _, model := range e.options.Models {
			data = append(data, map[string]string{"id": model, "object": "model"})
		}
		write(w, map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	switch e.options.Flavor {
	case VLLM:
		mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) { write(w, map[string]string{"version": "0.11.2"}) })
	case SGLang:
		mux.HandleFunc("GET /server_info", func(w http.ResponseWriter, _ *http.Request) {
			write(w, map[string]any{"version": "0.5.4", "model_path": e.options.Models[0]})
		})
	case LlamaCPP:
		mux.HandleFunc("GET /props", func(w http.ResponseWriter, _ *http.Request) {
			write(w, map[string]any{"build_info": "b6900-abcdef1", "model_path": "/models/model.gguf"})
		})
	case Ollama:
		mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) { write(w, map[string]string{"version": "0.12.10"}) })
		mux.HandleFunc("GET /api/ps", func(w http.ResponseWriter, _ *http.Request) {
			models := []map[string]string{}
			if e.options.Loaded != "" {
				models = append(models, map[string]string{"name": e.options.Loaded, "model": e.options.Loaded})
			}
			write(w, map[string]any{"models": models})
		})
	case LMStudio:
		mux.HandleFunc("GET /api/v0/models", func(w http.ResponseWriter, _ *http.Request) {
			data := []map[string]string{}
			for _, model := range e.options.Models {
				state := "not-loaded"
				if model == e.options.Loaded {
					state = "loaded"
				}
				data = append(data, map[string]string{"id": model, "state": state, "type": "llm"})
			}
			write(w, map[string]any{"object": "list", "data": data})
		})
	}
	if e.options.Metrics {
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprint(w, e.metrics())
		})
	}
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		e.mu.Lock()
		e.completions++
		e.prompt += 12
		e.generated += 5
		for _, message := range body.Messages {
			e.Prompts = append(e.Prompts, message.Content)
		}
		e.mu.Unlock()
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for _, piece := range []string{"local", " answer"} {
				fmt.Fprintf(w, "data: {\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", body.Model, piece)
				if flusher != nil {
					flusher.Flush()
				}
			}
			fmt.Fprintf(w, "data: {\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":5}}\n\n", body.Model)
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		write(w, map[string]any{"id": "chatcmpl-fake", "object": "chat.completion", "model": body.Model,
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": "local answer"}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 12, "completion_tokens": 5, "total_tokens": 17}})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !e.authorized(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// metrics renders the flavor's exposition from the fake's counters. Each
// completion is booked as 40 ms queue, 120 ms to first token, and 20 ms per
// output token.
func (e *Engine) metrics() string {
	e.mu.Lock()
	completions, prompt, generated := e.completions, e.prompt, e.generated
	e.mu.Unlock()
	var b strings.Builder
	histogram := func(name string, seconds float64) {
		fmt.Fprintf(&b, "# HELP %s fake histogram\n# TYPE %s histogram\n", name, name)
		for _, bound := range []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1} {
			count := 0
			if seconds <= bound {
				count = completions
			}
			fmt.Fprintf(&b, "%s_bucket{le=\"%g\",model_name=\"fake\"} %d\n", name, bound, count)
		}
		fmt.Fprintf(&b, "%s_bucket{le=\"+Inf\",model_name=\"fake\"} %d\n%s_sum{model_name=\"fake\"} %g\n%s_count{model_name=\"fake\"} %d\n", name, completions, name, seconds*float64(completions), name, completions)
	}
	switch e.options.Flavor {
	case VLLM:
		fmt.Fprintf(&b, "vllm:num_requests_running{model_name=\"fake\"} 1\nvllm:num_requests_waiting{model_name=\"fake\"} 2\nvllm:kv_cache_usage_perc{model_name=\"fake\"} 0.42\n")
		fmt.Fprintf(&b, "vllm:prompt_tokens_total{model_name=\"fake\"} %d\nvllm:generation_tokens_total{model_name=\"fake\"} %d\nvllm:num_preemptions_total{model_name=\"fake\"} 0\n", prompt, generated)
		histogram("vllm:request_queue_time_seconds", 0.04)
		histogram("vllm:time_to_first_token_seconds", 0.12)
		histogram("vllm:inter_token_latency_seconds", 0.02)
		histogram("vllm:e2e_request_latency_seconds", 0.2)
	case SGLang:
		fmt.Fprintf(&b, "sglang:num_running_reqs 1\nsglang:num_queue_reqs 0\nsglang:token_usage 0.3\n")
		fmt.Fprintf(&b, "sglang:prompt_tokens_total %d\nsglang:generation_tokens_total %d\nsglang:cached_tokens_total 0\n", prompt, generated)
		histogram("sglang:queue_time_seconds", 0.04)
		histogram("sglang:time_to_first_token_seconds", 0.12)
	case LlamaCPP:
		fmt.Fprintf(&b, "llamacpp:requests_processing 1\nllamacpp:requests_deferred 0\nllamacpp:kv_cache_usage_ratio 0.2\n")
		fmt.Fprintf(&b, "llamacpp:prompt_tokens_total %d\nllamacpp:tokens_predicted_total %d\nllamacpp:tokens_predicted_seconds_total %g\n", prompt, generated, 0.02*float64(generated))
	}
	b.WriteString(e.options.ExtraMetrics)
	return b.String()
}
