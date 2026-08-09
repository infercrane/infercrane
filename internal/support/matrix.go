// Package support owns the public qualification matrix without owning provider
// or runtime implementations. Lifecycle code remains provider-neutral; release
// policy decides which registered combinations are exposed to users.
package support

import (
	"fmt"
	"sort"
	"strings"
)

const (
	DefaultRuntime = "vllm"
	DefaultCloud   = "runpod"
	DefaultGPU     = "L40S"
	ElasticMode    = "elastic"
	ServerlessMode = "serverless"
)

// Matrix is an immutable-by-convention set of qualified runtime and
// provider/compute-mode combinations. Adding an adapter does not silently make
// it public: qualification must be added here explicitly.
type Matrix struct {
	runtimes map[string]struct{}
	clouds   map[string]map[string]struct{}
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
	return nil
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
