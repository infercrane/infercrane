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
	request.Runtime.Name = "vllm-torchtpu"
	request.Hardware.Vendor = "google"
	request.Topology.Mode = "disaggregated"
	request.Topology.PrefillReplicas = 1
	request.Topology.DecodeReplicas = 1
	if err := DefaultCapabilityCatalog().ValidateRequest(request); err == nil || !strings.Contains(err.Error(), "disaggregated") {
		t.Fatalf("unregistered TPU disaggregation accepted: %v", err)
	}
}

func TestCapabilityCatalogNamesTheCurrentServingPluginsAndProfiler(t *testing.T) {
	catalog := DefaultCapabilityCatalog()
	// AWS retired NxD Inference in favor of the vLLM Neuron plugin, and the TPU
	// backends are now vLLM TorchTPU and vLLM JAX. A bare or retired name could
	// select the wrong plugin, so it is refused.
	for _, test := range []struct {
		vendor   string
		accepted []string
		refused  []string
	}{
		{vendor: "google", accepted: []string{"vllm-torchtpu", "vllm-jax"}, refused: []string{"vllm", "jax", "tpu-inference"}},
		{vendor: "aws", accepted: []string{"vllm-neuron"}, refused: []string{"vllm", "neuronx-distributed-inference"}},
	} {
		request := fixtureRequest(ModalityText)
		request.Hardware.Vendor = test.vendor
		for _, runtime := range test.accepted {
			request.Runtime.Name = runtime
			if err := catalog.ValidateRequest(request); err != nil {
				t.Fatalf("%s %s refused: %v", test.vendor, runtime, err)
			}
		}
		for _, runtime := range test.refused {
			request.Runtime.Name = runtime
			if err := catalog.ValidateRequest(request); err == nil || !strings.Contains(err.Error(), "not registered") {
				t.Fatalf("%s %s accepted: %v", test.vendor, runtime, err)
			}
		}
	}
	// The shipped NVIDIA adapter records PyTorch profiler traces, not Nsight.
	if nvidia, _ := catalog.For("nvidia"); nvidia.Profiler != "torch-profiler" {
		t.Fatalf("nvidia profiler=%s", nvidia.Profiler)
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
