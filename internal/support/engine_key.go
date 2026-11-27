package support

import (
	"github.com/infercrane/infercrane/internal/runtimecontract"
)

// engineKeyLauncher is the name the launcher script runs under ($0).
const engineKeyLauncher = "infercrane-engine-key"

// engineKeyScript starts the engine with the worker key in a configuration
// file. The key comes from INFERCRANE_WORKER_API_KEY, which every adapter sets
// from its secret; it is written to a file only the container user can read,
// and only the file's path reaches the engine's arguments, so the key never
// appears in a process command line. Without a well-formed key the engine
// does not start.
const engineKeyScript = `umask 077
key="${INFERCRANE_WORKER_API_KEY:-}"
case "$key" in
  ""|*[!A-Za-z0-9._~+/=-]*) echo "infercrane: the engine needs a well-formed worker key" >&2; exit 64 ;;
esac
dir=$(mktemp -d)
printf 'api-key: "%s"\n' "$key" > "$dir/engine-key.yaml"
unset key
exec "$@" --config "$dir/engine-key.yaml"
`

// WithEngineKey makes an SGLang runtime's server require the worker key: the
// command starts through a launcher that passes the key in SGLang's --config
// file. SGLang reads its key only from --api-key or --config, and a workload
// may not carry the key in its arguments. Health and metrics paths stay open
// in SGLang, so probes and scrapes are unchanged; the router and the runtime
// inspector already send the key. It changes, once, a workload of the sglang
// runtime whose command starts the SGLang server anywhere in argv
// (runtimecontract.CommandEngine): the built-in contract, curated recipes,
// recipe search, a tenant's own command, and agent candidates, also behind a
// prefix such as numactl or env. The launcher appends --config to the end of
// the command, so it reaches SGLang, the last program. vLLM reads
// VLLM_API_KEY, which every adapter sets, and a custom OCI image brings its
// own command.
func WithEngineKey(runtime string, workload runtimecontract.Workload) runtimecontract.Workload {
	if runtime != "sglang" || workload.Empty() || EngineKeyRequired(workload) || runtimecontract.CommandEngine(workload.Command) != "sglang" {
		return workload
	}
	workload.Command = append([]string{"/bin/sh", "-c", engineKeyScript, engineKeyLauncher}, workload.Command...)
	return workload
}

// EngineKeyRequired reports whether a workload starts through the key
// launcher.
func EngineKeyRequired(workload runtimecontract.Workload) bool {
	return len(workload.Command) > 3 && workload.Command[0] == "/bin/sh" && workload.Command[2] == engineKeyScript && workload.Command[3] == engineKeyLauncher
}
