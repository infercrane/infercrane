package runtimecontract

import "path"

// CommandEngine names the built-in engine a command starts: "vllm" for
// `vllm serve`, "sglang" for `sglang serve` or `-m sglang.launch_server`,
// and "" for any other command. The engine is found anywhere in argv, so a
// prefix such as numactl, taskset, nice or env does not hide it; the first
// engine from the left wins. It is the one matcher for which reviewed
// arguments apply and which commands start through the engine key launcher.
func CommandEngine(command []string) string {
	for index, arg := range command {
		next := ""
		if index+1 < len(command) {
			next = command[index+1]
		}
		switch base := path.Base(arg); {
		case base == "vllm" && next == "serve":
			return "vllm"
		case base == "sglang" && next == "serve", arg == "-m" && next == "sglang.launch_server":
			return "sglang"
		}
	}
	return ""
}
