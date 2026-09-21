package acceleratorlab

import (
	"strings"
	"testing"
)

func TestCapabilityCatalogRegistersAllTargetVendorsAndModalities(t *testing.T) {
	catalog := DefaultCapabilityCatalog()
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(catalog.Vendors(), ","); got != "amd,aws,google,nvidia" {
		t.Fatalf("vendors=%s", got)
	}
	for _, vendor := range catalog.Vendors() {
		capability, _ := catalog.For(vendor)
		if len(capability.Modalities) != 4 || capability.Profiler == "" || capability.AdapterState != "worker-contract-ready" {
			t.Fatalf("capability=%+v", capability)
		}
	}
}

func TestCapabilityCatalogSupportsTensorRTLLMMultiNodeMoEDisaggregationOnNVIDIA(t *testing.T) {
	request := fixtureRequest(ModalityText)
	request.Runtime.Name = "tensorrt-llm"
	request.Topology = Topology{Mode: "disaggregated", Nodes: 2, Accelerators: 8, TensorParallel: 4, PipelineParallel: 1, ExpertParallel: 2, PrefillReplicas: 1, DecodeReplicas: 1}
	if err := DefaultCapabilityCatalog().ValidateRequest(request); err != nil {
		t.Fatal(err)
	}
}

func TestCapabilityCatalogRejectsRuntimeAndTopologyWithoutAnExecutor(t *testing.T) {
	request := fixtureRequest(ModalityText)
	request.Hardware.Vendor = "amd"
	request.Runtime.Name = "tensorrt-llm"
	if err := DefaultCapabilityCatalog().ValidateRequest(request); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("TensorRT-LLM on AMD accepted: %v", err)
	}
	request.Runtime.Name = "vllm"
	request.Hardware.Vendor = "google"
	request.Topology.Mode = "disaggregated"
	request.Topology.PrefillReplicas = 1
	request.Topology.DecodeReplicas = 1
	if err := DefaultCapabilityCatalog().ValidateRequest(request); err == nil || !strings.Contains(err.Error(), "disaggregated") {
		t.Fatalf("unregistered TPU disaggregation accepted: %v", err)
	}
}

func TestCapabilityCatalogRejectsGeneratedKernelWhenWorkerDoesNotDeclareIt(t *testing.T) {
	request := fixtureRequest(ModalityText)
	request.Policy.AllowGeneratedKernels = true
	catalog := DefaultCapabilityCatalog()
	catalog.Capabilities[0].GeneratedKernels = false
	if err := catalog.ValidateRequest(request); err == nil || !strings.Contains(err.Error(), "kernel generation") {
		t.Fatalf("undeclared kernel generation accepted: %v", err)
	}
}
