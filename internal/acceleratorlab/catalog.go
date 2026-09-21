package acceleratorlab

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const CapabilityCatalogVersion = "infercrane-accelerator-capabilities-v1"

// Capability is an executable adapter contract, not a performance or capacity
// claim. A target remains unqualified until the worker returns evidence bound
// to the exact hardware, runtime image, topology, workload, and build artifact.
type Capability struct {
	Vendor               string     `json:"vendor"`
	Profiler             string     `json:"profiler"`
	RuntimeAllowlist     []string   `json:"runtime_allowlist"`
	Modalities           []Modality `json:"modalities"`
	MultiNode            bool       `json:"multi_node"`
	DisaggregatedServing bool       `json:"disaggregated_serving"`
	ExpertParallel       bool       `json:"expert_parallel"`
	GeneratedKernels     bool       `json:"generated_kernels"`
	AdapterState         string     `json:"adapter_state"`
	Qualification        string     `json:"qualification"`
}

type CapabilityCatalog struct {
	Version      string       `json:"version"`
	Capabilities []Capability `json:"capabilities"`
}

func DefaultCapabilityCatalog() CapabilityCatalog {
	modalities := []Modality{ModalityText, ModalityImage, ModalityVideo, ModalityAudio}
	return CapabilityCatalog{Version: CapabilityCatalogVersion, Capabilities: []Capability{
		{Vendor: "nvidia", Profiler: "nsight-systems+nsight-compute", RuntimeAllowlist: []string{"vllm", "sglang", "tensorrt-llm"}, Modalities: modalities, MultiNode: true, DisaggregatedServing: true, ExpertParallel: true, GeneratedKernels: true, AdapterState: "worker-contract-ready", Qualification: "exact target required"},
		{Vendor: "amd", Profiler: "rocprofv3+rocprofiler-compute", RuntimeAllowlist: []string{"vllm", "sglang"}, Modalities: modalities, MultiNode: true, DisaggregatedServing: true, ExpertParallel: true, GeneratedKernels: true, AdapterState: "worker-contract-ready", Qualification: "exact target required"},
		{Vendor: "google", Profiler: "xprof", RuntimeAllowlist: []string{"vllm", "jax"}, Modalities: modalities, MultiNode: true, DisaggregatedServing: false, ExpertParallel: true, GeneratedKernels: true, AdapterState: "worker-contract-ready", Qualification: "exact target required"},
		{Vendor: "aws", Profiler: "neuron-explorer", RuntimeAllowlist: []string{"vllm", "neuronx-distributed-inference"}, Modalities: modalities, MultiNode: true, DisaggregatedServing: false, ExpertParallel: true, GeneratedKernels: true, AdapterState: "worker-contract-ready", Qualification: "exact target required"},
	}}
}

func (catalog CapabilityCatalog) Validate() error {
	if catalog.Version == "" || len(catalog.Capabilities) == 0 {
		return errors.New("accelerator capability catalog requires a version and entries")
	}
	seen := map[string]struct{}{}
	for _, capability := range catalog.Capabilities {
		vendor := strings.ToLower(strings.TrimSpace(capability.Vendor))
		if vendor == "" || capability.Profiler == "" || len(capability.RuntimeAllowlist) == 0 || len(capability.Modalities) == 0 || capability.AdapterState == "" || capability.Qualification == "" {
			return fmt.Errorf("accelerator capability %q is incomplete", capability.Vendor)
		}
		if _, duplicate := seen[vendor]; duplicate {
			return fmt.Errorf("duplicate accelerator capability %q", vendor)
		}
		seen[vendor] = struct{}{}
	}
	return nil
}

func (catalog CapabilityCatalog) For(vendor string) (Capability, bool) {
	vendor = strings.ToLower(strings.TrimSpace(vendor))
	for _, capability := range catalog.Capabilities {
		if strings.EqualFold(capability.Vendor, vendor) {
			return capability, true
		}
	}
	return Capability{}, false
}

func (catalog CapabilityCatalog) ValidateRequest(request Request) error {
	if err := catalog.Validate(); err != nil {
		return err
	}
	capability, found := catalog.For(request.Hardware.Vendor)
	if !found {
		return fmt.Errorf("no accelerator worker adapter is registered for %q", request.Hardware.Vendor)
	}
	if !containsFold(capability.RuntimeAllowlist, request.Runtime.Name) {
		return fmt.Errorf("runtime %q is not registered for %s workers", request.Runtime.Name, request.Hardware.Vendor)
	}
	if !containsModality(capability.Modalities, request.Workload.Modality) {
		return fmt.Errorf("%s workers do not register the %s benchmark suite", request.Hardware.Vendor, request.Workload.Modality)
	}
	if request.Topology.Nodes > 1 && !capability.MultiNode {
		return fmt.Errorf("%s worker adapter does not support multi-node qualification", request.Hardware.Vendor)
	}
	if request.Topology.Mode == "disaggregated" && !capability.DisaggregatedServing {
		return fmt.Errorf("%s worker adapter does not support disaggregated serving qualification", request.Hardware.Vendor)
	}
	if request.Topology.ExpertParallel > 1 && !capability.ExpertParallel {
		return fmt.Errorf("%s worker adapter does not support expert-parallel qualification", request.Hardware.Vendor)
	}
	if request.Policy.AllowGeneratedKernels && !capability.GeneratedKernels {
		return fmt.Errorf("%s worker adapter does not support profile-bound kernel generation", request.Hardware.Vendor)
	}
	return nil
}

func (catalog CapabilityCatalog) Vendors() []string {
	result := make([]string, 0, len(catalog.Capabilities))
	for _, capability := range catalog.Capabilities {
		result = append(result, strings.ToLower(capability.Vendor))
	}
	sort.Strings(result)
	return result
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(wanted)) {
			return true
		}
	}
	return false
}

func containsModality(values []Modality, wanted Modality) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
