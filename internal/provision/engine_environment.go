package provision

import (
	"fmt"
	"slices"
	"sort"

	"github.com/infercrane/infercrane/internal/support"
)

// engineEnvironment returns a replica's reviewed engine variables as name
// order. It re-checks the allowlist in the adapter, so no adapter passes an
// unreviewed variable whoever built the spec, and it requires the pinned
// runtime workload the variables were reviewed for: an adapter's own default
// image is not that image.
func engineEnvironment(spec ReplicaSpec) ([]string, error) {
	if len(spec.RuntimeEnv) == 0 {
		return nil, nil
	}
	if spec.Workload.Empty() {
		return nil, fmt.Errorf("%w: runtime env needs the pinned runtime workload it was reviewed for", ErrInvalidReplicaSpec)
	}
	if err := support.ValidateRuntimeEnv(spec.Runtime, spec.RuntimeVersion, spec.GPU, spec.Workload, spec.RuntimeEnv); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidReplicaSpec, err)
	}
	names := make([]string, 0, len(spec.RuntimeEnv))
	for name := range spec.RuntimeEnv {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// containerEngineEnvironment renders reviewed engine variables as quoted
// docker run -e flags with a trailing space, or "" when none are set. Callers
// validate the spec first; an invalid map renders nothing.
func containerEngineEnvironment(spec ReplicaSpec) string {
	names, err := engineEnvironment(spec)
	if err != nil {
		return ""
	}
	out := ""
	for _, name := range names {
		out += "-e " + shellQuote(name+"="+spec.RuntimeEnv[name]) + " "
	}
	return out
}

// refuseEngineEnvironment is for adapters that cannot pass engine variables:
// starting the replica without them would serve a different revision.
func refuseEngineEnvironment(adapter string, spec ReplicaSpec) error {
	if len(spec.RuntimeEnv) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s does not pass engine environment variables to the runtime", ErrInvalidReplicaSpec, adapter)
}

// engineEnvironmentMatches reports whether an adopted resource's environment
// carries exactly the revision's reviewed engine variables: each requested
// value, and no reviewed variable the revision leaves unset.
func engineEnvironmentMatches(actual, requested map[string]string) bool {
	for name, value := range requested {
		if actual[name] != value {
			return false
		}
	}
	var reviewed []string
	for _, variable := range support.EngineEnvironment() {
		reviewed = append(reviewed, variable.Name)
	}
	for name := range actual {
		if _, set := requested[name]; !set && slices.Contains(reviewed, name) {
			return false
		}
	}
	return true
}
