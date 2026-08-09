package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
)

func main() {
	port := flag.Int("port", 8101, "port")
	worker := flag.String("worker", "gpu", "worker")
	model := flag.String("model", "Qwen/Qwen3-8B", "model")
	flag.Parse()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		write(w, map[string]any{"object": "list", "data": []map[string]string{{"id": *model, "object": "model"}}})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "vllm:num_requests_running 0\nvllm:num_requests_waiting 0\nvllm:kv_cache_usage_perc 0\n")
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		write(w, map[string]any{"id": "chatcmpl-fake", "object": "chat.completion", "model": body["model"], "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": "response from " + *worker}, "finish_reason": "stop"}}})
	})
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), mux); err != nil {
		panic(err)
	}
}
func write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
