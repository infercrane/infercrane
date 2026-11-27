package runtimecontract

import "testing"

func TestCommandEngineFindsTheEngineBehindAPrefix(t *testing.T) {
	for _, test := range []struct {
		command []string
		want    string
	}{
		{[]string{"sglang", "serve", "--model-path", "m"}, "sglang"},
		{[]string{"/opt/sglang/bin/python", "-m", "sglang.launch_server", "--model-path", "m"}, "sglang"},
		{[]string{"python3.12", "-m", "sglang.launch_server"}, "sglang"},
		{[]string{"numactl", "--interleave=all", "sglang", "serve", "--port", "30000"}, "sglang"},
		{[]string{"numactl", "-m", "0", "/usr/local/bin/sglang", "serve"}, "sglang"},
		{[]string{"taskset", "-c", "0-7", "sglang", "serve"}, "sglang"},
		{[]string{"env", "FOO=1", "sglang", "serve"}, "sglang"},
		{[]string{"vllm", "serve", "Qwen/Qwen3-8B"}, "vllm"},
		{[]string{"nice", "-n", "5", "/usr/bin/vllm", "serve", "m"}, "vllm"},
		// The word sglang in an argument is not an SGLang server.
		{[]string{"vllm", "serve", "m", "--served-model-name", "sglang"}, "vllm"},
		{[]string{"/app/server", "--engine", "sglang", "--model-path", "/models/sglang.launch_server"}, ""},
		{[]string{"/app/llama-server", "--alias", "sglang", "--model", "/models/sglang-serve.gguf"}, ""},
		{[]string{"python3", "-m", "myapp", "--name", "sglang.launch_server"}, ""},
		{nil, ""},
	} {
		if got := CommandEngine(test.command); got != test.want {
			t.Errorf("%q: engine %q, want %q", test.command, got, test.want)
		}
	}
}
