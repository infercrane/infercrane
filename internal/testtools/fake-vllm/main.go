package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/infercrane/infercrane/internal/testtools/fakevllm"
)

func main() {
	port := flag.Int("port", 8101, "port")
	worker := flag.String("worker", "gpu", "worker")
	model := flag.String("model", "Qwen/Qwen3-8B", "model")
	apiKey := flag.String("api-key", "", "optional bearer token")
	apiKeyEnv := flag.String("api-key-env", "", "read the bearer token from this environment variable, as vLLM reads VLLM_API_KEY; the fake does not start if it is empty")
	toolName := flag.String("tool-call-name", fakevllm.DefaultToolCall.Name, "function the fake calls when a request offers tools")
	toolArguments := flag.String("tool-call-arguments", fakevllm.DefaultToolCall.Arguments, "arguments of that call, as JSON text")
	delay := flag.Duration("delay", 0, "added before every chat answer, so one fake can serve slower than another")
	flag.Parse()
	if *apiKeyEnv != "" {
		if *apiKey = os.Getenv(*apiKeyEnv); *apiKey == "" {
			fmt.Fprintf(os.Stderr, "fake-vllm: %s is empty\n", *apiKeyEnv)
			os.Exit(64)
		}
	}
	handler := fakevllm.Handler(fakevllm.Options{Worker: *worker, Model: *model, APIKey: *apiKey, ToolCall: fakevllm.ToolCall{Name: *toolName, Arguments: *toolArguments}, Delay: *delay})
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), handler); err != nil {
		panic(err)
	}
}
