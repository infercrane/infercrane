package provideridentity

// Platforms that are recognized but not qualified (AcceleratorNotQualified).
// Each names the software stack and the target its engine builds compile
// for, as the ROCm platforms do, so a measured run qualifies one generation
// without qualifying another.
const (
	// PlatformTPUV5E is Google Cloud TPU v5e: XLA through libtpu.
	PlatformTPUV5E = "tpu-v5e"
	// PlatformTPUV5P is Google Cloud TPU v5p.
	PlatformTPUV5P = "tpu-v5p"
	// PlatformTPUV6E is Google Cloud TPU v6e (Trillium).
	PlatformTPUV6E = "tpu-v6e"
	// PlatformTPUV7X is Google Cloud TPU7x (Ironwood).
	PlatformTPUV7X = "tpu-v7x"
	// PlatformHPUGaudi2 is Intel Gaudi 2 on the Intel Gaudi software stack
	// (vLLM's "hpu" device, through the vLLM Gaudi plugin).
	PlatformHPUGaudi2 = "hpu-gaudi2"
	// PlatformHPUGaudi3 is Intel Gaudi 3 on the Intel Gaudi software stack.
	PlatformHPUGaudi3 = "hpu-gaudi3"
	// PlatformXPUXe2 is Intel Xe2 (Battlemage) GPUs on vLLM's "xpu" device
	// (PyTorch XPU and oneAPI).
	PlatformXPUXe2 = "xpu-xe2"
)

// Kubernetes device-plugin resources of accelerators that are not qualified.
const (
	// DeviceResourceGoogleTPU is GKE's TPU resource; it counts chips.
	DeviceResourceGoogleTPU = "google.com/tpu"
	// DeviceResourceIntelGaudi is the Intel Gaudi device plugin's resource.
	DeviceResourceIntelGaudi = "habana.ai/gaudi"
	// DeviceResourceIntelGPUXe is Intel's GPU plugin resource for GPUs on the
	// xe kernel driver: discrete Battlemage and newer.
	DeviceResourceIntelGPUXe = "gpu.intel.com/xe"
)

const (
	// Chip memory, peak BF16 and FP8 compute, and topologies per generation.
	// Google lists FP8 peak equal to BF16 peak on v5p (459 TFLOPs) and v6e
	// (918), and twice BF16 only on TPU7x (4,614 against 2,307).
	sourceTPUV5E = "https://docs.cloud.google.com/tpu/docs/v5e"
	sourceTPUV5P = "https://docs.cloud.google.com/tpu/docs/v5p"
	sourceTPUV6E = "https://docs.cloud.google.com/tpu/docs/v6e"
	sourceTPU7X  = "https://docs.cloud.google.com/tpu/docs/tpu7x"
	// GKE machine types, the cloud.google.com/gke-tpu-accelerator label
	// values (tpu-v5-lite-podslice, tpu-v5p-slice, tpu-v6e-slice, tpu7x), and
	// the google.com/tpu resource.
	sourceGKETPUs = "https://docs.cloud.google.com/kubernetes-engine/docs/concepts/plan-tpus"
	// vLLM JAX (formerly tpu-inference) v0.34.0: v7x, v5e, and v6e
	// recommended; v5p experimental.
	sourceVLLMJAXSupport = "https://github.com/vllm-project/tpu-inference/blob/v0.34.0/README.md"
	// vLLM TorchTPU v0.33.0: v7x recommended and validated in CI, v6e
	// supported without CI, v5e and v5p experimental.
	sourceVLLMTorchTPUSupport = "https://github.com/vllm-project/vllm-torchtpu/blob/v0.33.0/README.md"
	// Gaudi 2: 96 GB HBM2E, FP8 (E4M3 and E5M2) in the MME; Gaudi 3: 128 GB
	// HBM2e, FP8 and BF16 compute.
	sourceGaudiArchitecture = "https://docs.habana.ai/en/latest/Gaudi_Overview/Gaudi_Architecture.html"
	// The Gaudi device plugin exposes habana.ai/gaudi.
	sourceGaudiKubernetes = "https://docs.habana.ai/en/latest/Installation_Guide/Additional_Installation/Kubernetes_Installation/Deamonsets_Kubernetes_Operator.html"
	// vLLM Gaudi plugin v0.30.0 features, and the models validated per
	// device (most only on Gaudi 3).
	sourceVLLMGaudiFeatures = "https://github.com/vllm-project/vllm-gaudi/blob/v0.30.0/docs/features/supported_features.md"
	sourceVLLMGaudiModels   = "https://github.com/vllm-project/vllm-gaudi/blob/v0.30.0/docs/getting_started/validated_models.md"
	// Arc Pro B60: Xe2 (Battlemage), 24 GB GDDR6; B70: Xe2, 32 GB GDDR6.
	sourceArcProB60 = "https://www.intel.com/content/www/us/en/products/sku/243916/intel-arc-pro-b60-graphics/specifications.html"
	sourceArcProB70 = "https://www.intel.com/content/www/us/en/products/sku/245797/intel-arc-pro-b70-graphics/specifications.html"
	// vLLM 0.34.0 validates Intel Arc Pro B-Series only on its XPU platform.
	sourceVLLMXPU = "https://github.com/vllm-project/vllm/blob/v0.34.0/docs/models/hardware_supported_models/xpu.md"
	// gpu.intel.com/xe is the xe kernel driver's resource, the driver for
	// discrete Battlemage GPUs.
	sourceIntelGPUPlugin = "https://github.com/intel/intel-device-plugins-for-kubernetes/blob/main/cmd/gpu_plugin/README.md"
)

// notQualifiedAccelerators are recognized by name and refused (see
// AcceleratorNotQualified). Every value was read on 2026-11-26 from the
// sources listed. NativeFP8 stays false: no pinned runtime uses FP8 there.
var notQualifiedAccelerators = []Accelerator{
	{
		Name: "TPU v5e", Vendor: "google", Product: "Google Cloud TPU v5e", Architecture: "TPU v5e", Target: "v5e",
		Platform: PlatformTPUV5E, DeviceResource: DeviceResourceGoogleTPU, MemoryGiB: 16, Status: AcceleratorNotQualified,
		Sources: []string{sourceTPUV5E, sourceGKETPUs, sourceVLLMJAXSupport, sourceVLLMTorchTPUSupport},
		// "v5litepod-8" is the Cloud TPU accelerator type, SkyPilot writes
		// "tpu-v5litepod-8", and GKE labels the node "tpu-v5-lite-podslice".
		aliases: []string{"v5e", "v5litepod", "tpuv5litepod", "tpuv5litepodslice"}, sliceSuffix: true,
	},
	{
		Name: "TPU v5p", Vendor: "google", Product: "Google Cloud TPU v5p", Architecture: "TPU v5p", Target: "v5p",
		Platform: PlatformTPUV5P, DeviceResource: DeviceResourceGoogleTPU, MemoryGiB: 95, Status: AcceleratorNotQualified,
		Sources: []string{sourceTPUV5P, sourceGKETPUs, sourceVLLMJAXSupport, sourceVLLMTorchTPUSupport},
		aliases: []string{"v5p", "tpuv5pslice"}, sliceSuffix: true,
	},
	{
		Name: "TPU v6e", Vendor: "google", Product: "Google Cloud TPU v6e (Trillium)", Architecture: "Trillium", Target: "v6e",
		Platform: PlatformTPUV6E, DeviceResource: DeviceResourceGoogleTPU, MemoryGiB: 32, Status: AcceleratorNotQualified,
		Sources: []string{sourceTPUV6E, sourceGKETPUs, sourceVLLMJAXSupport, sourceVLLMTorchTPUSupport},
		aliases: []string{"v6e", "trillium", "tpuv6eslice", "tputrillium"}, sliceSuffix: true,
	},
	{
		Name: "TPU7x", Vendor: "google", Product: "Google Cloud TPU7x (Ironwood)", Architecture: "Ironwood", Target: "v7x",
		Platform: PlatformTPUV7X, DeviceResource: DeviceResourceGoogleTPU, MemoryGiB: 192, Status: AcceleratorNotQualified,
		Sources: []string{sourceTPU7X, sourceGKETPUs, sourceVLLMJAXSupport, sourceVLLMTorchTPUSupport},
		// GKE's label value is "tpu7x"; the vLLM plugins write "v7x".
		aliases: []string{"v7x", "tpuv7x", "ironwood", "tpuironwood"}, sliceSuffix: true,
	},
	{
		Name: "Gaudi 2", Vendor: "intel", Product: "Intel Gaudi 2 AI Accelerator", Architecture: "Gaudi 2", Target: "gaudi2",
		Platform: PlatformHPUGaudi2, DeviceResource: DeviceResourceIntelGaudi, MemoryGiB: 96, Status: AcceleratorNotQualified,
		Sources: []string{sourceGaudiArchitecture, sourceGaudiKubernetes, sourceVLLMGaudiFeatures, sourceVLLMGaudiModels},
		aliases: []string{"gaudi2aiaccelerator"},
	},
	{
		Name: "Gaudi 3", Vendor: "intel", Product: "Intel Gaudi 3 AI Accelerator", Architecture: "Gaudi 3", Target: "gaudi3",
		Platform: PlatformHPUGaudi3, DeviceResource: DeviceResourceIntelGaudi, MemoryGiB: 128, Status: AcceleratorNotQualified,
		Sources: []string{sourceGaudiArchitecture, sourceGaudiKubernetes, sourceVLLMGaudiFeatures, sourceVLLMGaudiModels},
		aliases: []string{"gaudi3aiaccelerator"},
	},
	{
		Name: "Arc Pro B60", Vendor: "intel", Product: "Intel Arc Pro B60 Graphics", Architecture: "Xe2", Target: "bmg",
		Platform: PlatformXPUXe2, DeviceResource: DeviceResourceIntelGPUXe, MemoryGiB: 24, Status: AcceleratorNotQualified,
		Sources: []string{sourceArcProB60, sourceVLLMXPU, sourceIntelGPUPlugin},
		aliases: []string{"arcprob60graphics"},
	},
	{
		Name: "Arc Pro B70", Vendor: "intel", Product: "Intel Arc Pro B70 Graphics", Architecture: "Xe2", Target: "bmg",
		Platform: PlatformXPUXe2, DeviceResource: DeviceResourceIntelGPUXe, MemoryGiB: 32, Status: AcceleratorNotQualified,
		Sources: []string{sourceArcProB70, sourceVLLMXPU, sourceIntelGPUPlugin},
		aliases: []string{"arcprob70graphics"},
	},
}
