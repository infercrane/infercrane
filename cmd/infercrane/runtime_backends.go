package main

import (
	"fmt"

	"github.com/infercrane/infercrane/internal/integration"
)

var inspectedRuntimeNames = []string{
	"vllm",
	"sglang",
	"custom-oci",
	"openai-compatible",
	"litellm",
}

// bindRuntimeBackends keeps the executable inspector registry aligned with
// the runtime profiles accepted by endpoint discovery. A runtime being present
// in the public catalog is not sufficient: reconciliation must also be able to
// select its inspector before a discovered endpoint can become routable.
func bindRuntimeBackends(registry *integration.Registry, inspector integration.RuntimeInspector) (integration.RuntimeBackends, error) {
	backends := make([]integration.RuntimeBackend, 0, len(inspectedRuntimeNames))
	for _, name := range inspectedRuntimeNames {
		profile, err := registry.Runtime(name)
		if err != nil {
			return integration.RuntimeBackends{}, fmt.Errorf("configure %s runtime contract: %w", name, err)
		}
		backends = append(backends, integration.RuntimeBackend{Profile: profile, Inspector: inspector})
	}
	return integration.NewRuntimeBackends(backends...)
}
