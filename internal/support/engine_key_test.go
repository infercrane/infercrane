package support

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

func TestSGLangServersStartWithTheWorkerKeyInTheirConfig(t *testing.T) {
	built := SGLangWorkload()
	keyed := WithEngineKey("sglang", built)
	if !EngineKeyRequired(keyed) || !strings.HasSuffix(strings.Join(keyed.Command, " "), strings.Join(built.Command, " ")) {
		t.Fatalf("built-in SGLang command %v", keyed.Command)
	}
	if again := WithEngineKey("sglang", keyed); strings.Join(again.Command, "\x00") != strings.Join(keyed.Command, "\x00") {
		t.Fatal("the launcher was added twice")
	}
	if err := keyed.Validate(); err != nil {
		t.Fatalf("keyed workload is not a valid contract: %v", err)
	}
	// Recipe and recipe-search commands start SGLang through other
	// interpreters, and a tenant's or a candidate's command may put a prefix
	// such as numactl, taskset or env in front of it.
	for _, command := range [][]string{
		{"/opt/sglang/bin/python", "-m", "sglang.launch_server", "--model-path", "m"},
		{"sglang", "serve", "--model-path", "m"},
		{"numactl", "--interleave=all", "sglang", "serve", "--model-path", "m"},
		{"taskset", "-c", "0-7", "sglang", "serve", "--model-path", "m"},
		{"env", "FOO=1", "sglang", "serve", "--model-path", "m"},
	} {
		workload := built
		workload.Command = command
		keyed := WithEngineKey("sglang", workload)
		if !EngineKeyRequired(keyed) || strings.Join(keyed.Command[4:], "\x00") != strings.Join(command, "\x00") {
			t.Fatalf("%v was not keyed as is: %v", command, keyed.Command)
		}
	}
	// A command that only names sglang in an argument is not an SGLang
	// server and keeps its command.
	for _, command := range [][]string{
		{"/app/server", "--engine", "sglang", "--model-path", "/models/sglang.launch_server"},
		{"vllm", "serve", "m", "--served-model-name", "sglang"},
	} {
		workload := built
		workload.Command = command
		if EngineKeyRequired(WithEngineKey("sglang", workload)) {
			t.Fatalf("%v was keyed", command)
		}
	}
	// vLLM reads VLLM_API_KEY; a custom image keeps its command.
	for _, test := range []struct {
		runtime  string
		workload runtimecontract.Workload
	}{{"vllm", VLLMWorkload()}, {"custom-oci", built}, {"sglang", runtimecontract.Workload{}}} {
		if got := WithEngineKey(test.runtime, test.workload); EngineKeyRequired(got) {
			t.Fatalf("%s was keyed", test.runtime)
		}
	}
}

func TestEngineKeyLauncherWritesTheKeyOnlyToAPrivateConfigFile(t *testing.T) {
	run := func(key string) (string, error) {
		// The engine is replaced by a command that prints its arguments and
		// the config file it was given.
		command := exec.Command("/bin/sh", "-c", engineKeyScript, engineKeyLauncher, "/bin/sh", "-c", `echo "args:$*"; ls -l "$2" | cut -c1-10; cat "$2"`, "engine")
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "INFERCRANE_WORKER_API_KEY=" + key}
		output, err := command.CombinedOutput()
		return string(output), err
	}
	output, err := run("icw_Ab-9_x")
	if err != nil {
		t.Fatalf("launcher failed: %v %s", err, output)
	}
	if !strings.Contains(output, `api-key: "icw_Ab-9_x"`) || !strings.Contains(output, "-rw-------") {
		t.Fatalf("config file: %s", output)
	}
	if args := strings.SplitN(output, "\n", 2)[0]; strings.Contains(args, "icw_") || !strings.Contains(args, "--config") {
		t.Fatalf("the key reached the engine's arguments: %s", args)
	}
	for _, key := range []string{"", `a"b`, "a b", "a\nb"} {
		if output, err := run(key); err == nil || !strings.Contains(output, "needs a well-formed worker key") {
			t.Fatalf("key %q started the engine: %s", key, output)
		}
	}
}

// Behind a prefix, the launcher's trailing --config still reaches SGLang:
// numactl, taskset and env pass the rest of argv to the program they start,
// so SGLang, the last program, gets --config <file> as its last arguments.
func TestEngineKeyLauncherPassesTheConfigThroughAPrefix(t *testing.T) {
	dir := t.TempDir()
	// A prefix that, like numactl or taskset, skips its own options and
	// starts the rest of argv; and an engine that prints what it received.
	for name, body := range map[string]string{
		"numactl": "#!/bin/sh\nwhile [ \"${1#-}\" != \"$1\" ]; do shift; done\nexec \"$@\"\n",
		"sglang":  "#!/bin/sh\necho \"argv:$*\"\necho \"foo:${FOO:-}\"\nwhile [ $# -gt 2 ]; do shift; done\n[ \"$1\" = --config ] && cat \"$2\"\n",
	} {
		if err := os.WriteFile(dir+"/"+name, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, prefix := range [][]string{{dir + "/numactl", "--interleave=all"}, {"env", "FOO=1"}} {
		engine := append(append([]string{}, prefix...), dir+"/sglang", "serve", "--port", "30000")
		keyed := WithEngineKey("sglang", runtimecontract.Workload{Image: "lmsysorg/sglang@sha256:" + strings.Repeat("a", 64), Command: engine})
		if !EngineKeyRequired(keyed) {
			t.Fatalf("%v was not keyed", engine)
		}
		command := exec.Command(keyed.Command[0], keyed.Command[1:]...)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "INFERCRANE_WORKER_API_KEY=icw_prefixed"}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v %s", prefix, err, output)
		}
		text := string(output)
		argv := strings.SplitN(text, "\n", 2)[0]
		if !strings.HasPrefix(argv, "argv:serve --port 30000 --config ") || !strings.HasSuffix(argv, "/engine-key.yaml") || strings.Contains(argv, "icw_") {
			t.Fatalf("%v: SGLang received %q", prefix, argv)
		}
		if !strings.Contains(text, `api-key: "icw_prefixed"`) {
			t.Fatalf("%v: config file not read: %s", prefix, text)
		}
		if prefix[0] == "env" && !strings.Contains(text, "foo:1") {
			t.Fatalf("env prefix lost its variable: %s", text)
		}
	}
}
