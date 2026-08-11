// Package support owns the public qualification matrix without owning provider
// or runtime implementations. Lifecycle code remains provider-neutral; release
// policy decides which registered combinations are exposed to users.
package support

import (
	"fmt"
	"sort"
	"strings"

	"github.com/infercrane/infercrane/internal/runtimecontract"
)

const SGLangRuntimeVersion = "0.5.12"

func SGLangWorkload() runtimecontract.Workload {
	return runtimecontract.Workload{Image: "lmsysorg/sglang:v0.5.12@sha256:42194170546745092e74cd5f81ad32a7c6e944c7111fe7bf13588152277ff356", Command: []string{"python3", "-m", "sglang.launch_server", "--model-path", "${MODEL}", "--host", "0.0.0.0", "--port", "${PORT}", "--api-key", "${WORKER_API_KEY}"}, Protocol: "openai", Port: 8000, ReadinessPath: "/health", ModelsPath: "/v1/models", MetricsPath: "/metrics", Cancellation: "http-disconnect", Drain: "connection", ShutdownGraceSeconds: 30}
}

func NormalizeWorkload(runtime string, workload runtimecontract.Workload) runtimecontract.Workload {
	if workload.Empty() && runtime == "sglang" {
		return SGLangWorkload()
	}
	return workload
}

const (
	DefaultRuntime = "vllm"
	// DefaultRuntimeVersion is the default vLLM runtime qualified by the public
	// support matrix. Persist it in every revision so benchmarks and
	// explanations never depend on an implicit image default.
	DefaultRuntimeVersion = "0.8.5.post1"
	DefaultCloud          = "runpod"
	DefaultGPU            = "L40S"
	ElasticMode           = "elastic"
	ServerlessMode        = "serverless"
)

// Matrix is an immutable-by-convention set of qualified runtime and
// provider/compute-mode combinations. Adding an adapter does not silently make
// it public: qualification must be added here explicitly.
type Matrix struct {
	runtimes     map[string]struct{}
	clouds       map[string]map[string]struct{}
	combinations map[string]map[string]map[string]struct{}
}

// Qualified builds an exact runtime/provider/mode matrix. It avoids implying
// that every registered runtime works on every provider.
func Qualified(combinations map[string]map[string][]string) Matrix {
	m := Matrix{runtimes: map[string]struct{}{}, clouds: map[string]map[string]struct{}{}, combinations: map[string]map[string]map[string]struct{}{}}
	for cloud, modes := range combinations {
		m.clouds[cloud] = map[string]struct{}{}
		m.combinations[cloud] = map[string]map[string]struct{}{}
		for mode, runtimes := range modes {
			m.clouds[cloud][mode] = struct{}{}
			m.combinations[cloud][mode] = map[string]struct{}{}
			for _, runtime := range runtimes {
				m.runtimes[runtime] = struct{}{}
				m.combinations[cloud][mode][runtime] = struct{}{}
			}
		}
	}
	return m
}

func New(runtimes []string, clouds map[string][]string) Matrix {
	matrix := Matrix{runtimes: make(map[string]struct{}, len(runtimes)), clouds: make(map[string]map[string]struct{}, len(clouds))}
	for _, runtime := range runtimes {
		if runtime = strings.TrimSpace(runtime); runtime != "" {
			matrix.runtimes[runtime] = struct{}{}
		}
	}
	for cloud, modes := range clouds {
		cloud = strings.TrimSpace(cloud)
		if cloud == "" {
			continue
		}
		qualified := make(map[string]struct{}, len(modes))
		for _, mode := range modes {
			if mode = strings.TrimSpace(mode); mode != "" {
				qualified[mode] = struct{}{}
			}
		}
		matrix.clouds[cloud] = qualified
	}
	return matrix
}

func V01() Matrix {
	return New([]string{DefaultRuntime}, map[string][]string{DefaultCloud: {ElasticMode, ServerlessMode}})
}

// V03 is the public qualification policy for the v0.3 release. Provider
// adapters remain registered separately; adding one to the integration
// catalog does not expose it until this policy is intentionally expanded.
func V03() Matrix {
	return New([]string{DefaultRuntime}, map[string][]string{
		DefaultCloud: {ElasticMode, ServerlessMode},
		"aws":        {ElasticMode},
	})
}

// V06 is the local qualification policy for portable runtimes. SGLang and
// custom OCI are deliberately limited to the SkyPilot elastic path until each
// additional provider proves the workload contract independently.
func V06() Matrix {
	return Qualified(map[string]map[string][]string{
		DefaultCloud: {ElasticMode: {DefaultRuntime}, ServerlessMode: {DefaultRuntime}},
		"aws":        {ElasticMode: {DefaultRuntime, "sglang", "custom-oci"}},
	})
}

// V09 adds the Kubernetes elastic adapter to the executable pre-release
// matrix. Its runtime combinations are simulated/Kind-qualified; callers must
// still inspect the integration qualification state before production use.
func V09() Matrix {
	return Qualified(map[string]map[string][]string{
		DefaultCloud: {ElasticMode: {DefaultRuntime}, ServerlessMode: {DefaultRuntime}},
		"aws":        {ElasticMode: {DefaultRuntime, "sglang", "custom-oci"}},
		"kubernetes": {ElasticMode: {DefaultRuntime, "sglang", "custom-oci"}},
	})
}

// V1 is the public v1 runtime/provider/compute qualification policy. Keep the
// historical milestone functions so old qualification evidence remains
// reproducible while current composition has a version-appropriate owner.
func V1() Matrix { return V09() }

func (m Matrix) Validate(runtime, cloud, mode string) error {
	if err := m.ValidateRuntime(runtime); err != nil {
		return err
	}
	modes, ok := m.clouds[cloud]
	if !ok {
		return fmt.Errorf("provider cloud %q is not qualified; supported clouds: %s", cloud, strings.Join(sortedNestedKeys(m.clouds), ", "))
	}
	if _, ok = modes[mode]; !ok {
		return fmt.Errorf("compute mode %q is not qualified for cloud %q", mode, cloud)
	}
	if len(m.combinations) > 0 {
		if _, ok = m.combinations[cloud][mode][defaultRuntime(runtime)]; !ok {
			return fmt.Errorf("runtime %q is not qualified for cloud %q in %s mode", defaultRuntime(runtime), cloud, mode)
		}
	}
	return nil
}

func defaultRuntime(runtime string) string {
	if runtime == "" {
		return DefaultRuntime
	}
	return runtime
}

func (m Matrix) ValidateRuntime(runtime string) error {
	if runtime == "" {
		runtime = DefaultRuntime
	}
	if _, ok := m.runtimes[runtime]; !ok {
		return fmt.Errorf("runtime %q is not qualified; supported runtimes: %s", runtime, strings.Join(sortedKeys(m.runtimes), ", "))
	}
	return nil
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedNestedKeys(values map[string]map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
