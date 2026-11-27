package provision

import (
	"fmt"
	"path"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/support"
)

// kernelConfigVariable prefixes the environment variables that carry a tuned
// set's files into the runtime container, one per file.
const kernelConfigVariable = "INFERCRANE_KERNEL_CONFIG_"

// kernelConfigs re-checks a replica's tuned kernel set in the adapter, as
// engineEnvironment does for engine switches: the set must be valid, for the
// replica's engine release and GPU, and the replica must serve that release's
// pinned image, the one the set was reviewed for. Images are pinned per
// release, so a revision that records an older release is checked against
// that release's image, not the current one. It returns nil without a set.
func kernelConfigs(spec ReplicaSpec) (*domain.KernelConfigSet, error) {
	if spec.KernelConfigs == nil {
		return nil, nil
	}
	runtime := spec.Runtime
	if runtime == "" {
		runtime = support.DefaultRuntime
	}
	version := support.NormalizeRuntimeVersion(spec.RuntimeVersion)
	if version == "" {
		version = support.CurrentRuntimeVersion(runtime)
	}
	if err := kerneltuning.ValidateForRevision(*spec.KernelConfigs, runtime, version, spec.GPU); err != nil {
		return nil, fmt.Errorf("%w: kernel configs: %v", ErrInvalidReplicaSpec, err)
	}
	pinned, ok := support.RuntimeWorkloadFor(runtime, version, provideridentity.AcceleratorPlatform(spec.GPU))
	if !ok || support.ServedWorkloadFor(runtime, version, spec.GPU, spec.Workload).Image != pinned.Image {
		return nil, fmt.Errorf("%w: kernel configs are reviewed only for the pinned %s %s image", ErrInvalidReplicaSpec, runtime, version)
	}
	return spec.KernelConfigs, nil
}

// refuseKernelConfigs is for adapters that cannot write a set's files before
// the engine starts: serving without them would serve another revision.
func refuseKernelConfigs(adapter string, spec ReplicaSpec) error {
	if spec.KernelConfigs == nil {
		return nil
	}
	return fmt.Errorf("%w: %s does not deliver tuned kernel configs", ErrInvalidReplicaSpec, adapter)
}

// kernelConfigScript writes each file of a set into the engine's tuned-config
// folder from its environment variable, stops the container when a file's
// sha256 differs, and names the folder for the engine. It runs before the
// server through wrapWithFetch. File names are in the engine's own form
// (kerneltuning.ParseFileName), so they need no more than single quotes.
func kernelConfigScript(set domain.KernelConfigSet) string {
	var script strings.Builder
	script.WriteString("set -eu\n")
	fmt.Fprintf(&script, "mkdir -p %s\n", shellQuote(kerneltuning.MountPath))
	for index, file := range set.Files {
		target := path.Join(kerneltuning.MountPath, file.Name)
		fmt.Fprintf(&script, "printf '%%s' \"$%s%d\" > %s\n", kernelConfigVariable, index, shellQuote(target))
		fmt.Fprintf(&script, "printf '%%s  %%s\\n' %s %s | sha256sum -c - >/dev/null\n", shellQuote(file.SHA256), shellQuote(target))
	}
	fmt.Fprintf(&script, "export %s=%s\n", kerneltuning.EngineConfigFolderVariable, shellQuote(kerneltuning.MountPath))
	return script.String()
}

// kernelConfigEnvironment is the container environment that carries a set's
// files, in file order.
func kernelConfigEnvironment(set domain.KernelConfigSet) []any {
	environment := make([]any, 0, len(set.Files))
	for index, file := range set.Files {
		environment = append(environment, map[string]any{"name": fmt.Sprintf("%s%d", kernelConfigVariable, index), "value": file.Content})
	}
	return environment
}
