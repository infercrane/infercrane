package main

import (
	"flag"
	"fmt"
	"net/http"

	"github.com/infercrane/infercrane/internal/testtools/fakevllm"
)

func main() {
	port := flag.Int("port", 8101, "port")
	worker := flag.String("worker", "gpu", "worker")
	model := flag.String("model", "Qwen/Qwen3-8B", "model")
	apiKey := flag.String("api-key", "", "optional bearer token")
	toolName := flag.String("tool-call-name", fakevllm.DefaultToolCall.Name, "function the fake calls when a request offers tools")
	toolArguments := flag.String("tool-call-arguments", fakevllm.DefaultToolCall.Arguments, "arguments of that call, as JSON text")
	flag.Parse()
	handler := fakevllm.Handler(fakevllm.Options{Worker: *worker, Model: *model, APIKey: *apiKey, ToolCall: fakevllm.ToolCall{Name: *toolName, Arguments: *toolArguments}})
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), handler); err != nil {
		panic(err)
	}
}
