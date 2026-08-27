package servingcontract

import (
	"errors"
	"fmt"
	"strconv"
)

var ErrDeferredMechanism = errors.New("serving mechanism is registered but deferred")

// DeferredMechanism is an inspectable argument translation, not an execution
// grant. It lets plans and adapters agree on exact intent while Compile fails
// closed until the mechanism receives version-pinned lifecycle qualification.
type DeferredMechanism struct {
	Name        string              `json:"name"`
	Runtime     string              `json:"runtime"`
	Components  map[string][]string `json:"components"`
	Environment map[string]string   `json:"environment,omitempty"`
	Executable  bool                `json:"executable"`
	Reason      string              `json:"reason"`
}

func (m DeferredMechanism) Compile() error {
	if !m.Executable {
		return fmt.Errorf("%w: %s: %s", ErrDeferredMechanism, m.Name, m.Reason)
	}
	return nil
}

func TranslateDeferredMechanism(topology Topology, runtime string) (DeferredMechanism, error) {
	topology = topology.Normalize()
	switch {
	case topology.Cache.Backend == CacheLMCache:
		if runtime != "vllm" || topology.Mode != ModeAggregated || topology.Cache.ConfigurationRef == "" {
			return DeferredMechanism{}, errors.New("LMCache translation requires aggregated vLLM and an explicit configuration_ref")
		}
		return DeferredMechanism{
			Name: "lmcache-vllm", Runtime: runtime, Executable: false,
			Components:  map[string][]string{"worker": {"--kv-transfer-config", `{"kv_connector":"LMCacheConnectorV1","kv_role":"kv_both"}`}},
			Environment: map[string]string{"LMCACHE_CONFIG_FILE": topology.Cache.ConfigurationRef},
			Reason:      "LMCache process lifecycle, configuration delivery, memory pressure, and failure semantics are not qualified",
		}, nil
	case topology.Mode == ModeDisaggregated:
		if runtime != "vllm" && runtime != "sglang" {
			return DeferredMechanism{}, errors.New("NIXL disaggregation translation requires vLLM or SGLang")
		}
		components := map[string][]string{}
		for _, component := range []string{"prefill", "decode"} {
			if runtime == "vllm" {
				components[component] = []string{"--disaggregation-mode", component, "--kv-transfer-config", `{"kv_connector":"NixlConnector","kv_role":"kv_both"}`}
			} else {
				components[component] = []string{"--disaggregation-mode", component, "--disaggregation-transfer-backend", "nixl", "--disaggregation-bootstrap-port", strconv.Itoa(12345)}
			}
		}
		return DeferredMechanism{
			Name: "dynamo-nixl-prefill-decode", Runtime: runtime, Components: components, Executable: false,
			Reason: "NIXL transport compatibility, failure recovery, routing, and GPU behavior require a version-pinned real cluster qualification",
		}, nil
	default:
		return DeferredMechanism{}, errors.New("topology does not select a deferred serving mechanism")
	}
}
