package provideridentity

import (
	"slices"
	"strings"
)

// Accelerator platforms name the software stack a runtime image is built
// for. Runtime images and provider qualification are keyed by platform, so
// lifecycle code chooses an image by looking up data, never by vendor name.
const (
	// PlatformCUDA is the default platform. A GPU name that is not a reviewed
	// accelerator below runs the CUDA images, as it always has.
	PlatformCUDA = "cuda"
	// PlatformROCmGFX942 is AMD CDNA 3 (MI300X, MI325X) on ROCm.
	PlatformROCmGFX942 = "rocm-gfx942"
	// PlatformROCmGFX950 is AMD CDNA 4 (MI350X, MI355X) on ROCm.
	PlatformROCmGFX950 = "rocm-gfx950"
)

// Kubernetes device-plugin resource names. The default platform's resource
// is the NVIDIA device plugin's.
const (
	DeviceResourceNVIDIA = "nvidia.com/gpu"
	DeviceResourceAMD    = "amd.com/gpu"
)

// Accelerator is one reviewed GPU outside the default CUDA platform. Every
// value is read from the vendor's published specifications or the runtime's
// source; Sources lists where.
type Accelerator struct {
	// Name is the canonical alias InferCrane uses in specs and proposals.
	Name         string `json:"name"`
	Vendor       string `json:"vendor"`
	Product      string `json:"product"`
	Architecture string `json:"architecture"`
	// Target is the compiler target the runtime images must be built for.
	Target   string `json:"target"`
	Platform string `json:"platform"`
	// DeviceResource is the Kubernetes device-plugin resource that exposes it.
	DeviceResource string `json:"device_resource"`
	MemoryGiB      int    `json:"memory_gib"`
	// NativeFP8 reports FP8 matrix math in hardware that the pinned runtimes
	// use for FP8 weights and FP8 KV cache.
	NativeFP8 bool     `json:"native_fp8"`
	Sources   []string `json:"sources"`
	// aliases are normalized spellings beyond the canonical name: provider
	// SKUs and node labels.
	aliases []string
}

const (
	// Architecture, compiler target, and memory per model.
	sourceROCmSpecs = "https://rocm.docs.amd.com/en/latest/reference/gpu-arch-specs.html"
	// Product pages list peak FP8 matrix performance.
	sourceMI300X = "https://www.amd.com/en/products/accelerators/instinct/mi300/mi300x.html"
	sourceMI325X = "https://www.amd.com/en/products/accelerators/instinct/mi300/mi325x.html"
	sourceMI350X = "https://www.amd.com/en/products/accelerators/instinct/mi350/mi350x.html"
	sourceMI355X = "https://www.amd.com/en/products/accelerators/instinct/mi350/mi355x.html"
	// vLLM 0.22.1 lists FP8 W8A8 as supported on AMD GPUs and an FP8 KV cache
	// (fp8 = fp8_e4m3) as supported on ROCm.
	sourceVLLMFP8   = "https://github.com/vllm-project/vllm/blob/v0.22.1/docs/features/quantization/README.md#supported-hardware"
	sourceVLLMFP8KV = "https://github.com/vllm-project/vllm/blob/v0.22.1/vllm/config/cache.py"
	// SGLang 0.5.12: "FP8 works via Aiter or Triton" on AMD GPUs.
	sourceSGLangFP8 = "https://github.com/sgl-project/sglang/blob/v0.5.12/docs/platforms/amd_gpu.md#quantization-on-amd-gpus"
)

var reviewedAccelerators = []Accelerator{
	{
		Name: "MI300X", Vendor: "amd", Product: "AMD Instinct MI300X", Architecture: "CDNA 3", Target: "gfx942",
		Platform: PlatformROCmGFX942, DeviceResource: DeviceResourceAMD, MemoryGiB: 192, NativeFP8: true,
		Sources: []string{sourceROCmSpecs, sourceMI300X, sourceVLLMFP8, sourceVLLMFP8KV, sourceSGLangFP8},
	},
	{
		Name: "MI325X", Vendor: "amd", Product: "AMD Instinct MI325X", Architecture: "CDNA 3", Target: "gfx942",
		Platform: PlatformROCmGFX942, DeviceResource: DeviceResourceAMD, MemoryGiB: 256, NativeFP8: true,
		Sources: []string{sourceROCmSpecs, sourceMI325X, sourceVLLMFP8, sourceVLLMFP8KV, sourceSGLangFP8},
	},
	{
		Name: "MI350X", Vendor: "amd", Product: "AMD Instinct MI350X", Architecture: "CDNA 4", Target: "gfx950",
		Platform: PlatformROCmGFX950, DeviceResource: DeviceResourceAMD, MemoryGiB: 288, NativeFP8: true,
		Sources: []string{sourceROCmSpecs, sourceMI350X, sourceVLLMFP8, sourceVLLMFP8KV, sourceSGLangFP8},
		// RunPod's GPU type for its MI350X offer is "AMD Instinct MI350 OAM".
		aliases: []string{"mi350"},
	},
	{
		Name: "MI355X", Vendor: "amd", Product: "AMD Instinct MI355X", Architecture: "CDNA 4", Target: "gfx950",
		Platform: PlatformROCmGFX950, DeviceResource: DeviceResourceAMD, MemoryGiB: 288, NativeFP8: true,
		Sources: []string{sourceROCmSpecs, sourceMI355X, sourceVLLMFP8, sourceVLLMFP8KV, sourceSGLangFP8},
	},
}

// Accelerators returns the reviewed accelerators outside the default CUDA
// platform, in catalog order.
func Accelerators() []Accelerator {
	out := make([]Accelerator, len(reviewedAccelerators))
	for index, accelerator := range reviewedAccelerators {
		accelerator.Sources = slices.Clone(accelerator.Sources)
		accelerator.aliases = nil
		out[index] = accelerator
	}
	return out
}

// LookupAccelerator resolves a GPU name to a reviewed accelerator. It accepts
// the canonical name and the spellings providers use for the same device:
// "MI300X", "AMD Instinct MI300X", RunPod's "AMD Instinct MI300X OAM", and the
// AMD node labeller's "AMD_Instinct_MI300X_OAM". Any other name is not
// reviewed and reports false; it is never guessed from a model number.
func LookupAccelerator(gpu string) (Accelerator, bool) {
	key := acceleratorKey(gpu)
	if key == "" {
		return Accelerator{}, false
	}
	for _, accelerator := range reviewedAccelerators {
		if key == strings.ToLower(accelerator.Name) || slices.Contains(accelerator.aliases, key) {
			accelerator.Sources = slices.Clone(accelerator.Sources)
			accelerator.aliases = nil
			return accelerator, true
		}
	}
	return Accelerator{}, false
}

// AcceleratorPlatform is the runtime platform a GPU needs: a reviewed
// accelerator's platform, or the default CUDA platform for every other name.
func AcceleratorPlatform(gpu string) string {
	if accelerator, ok := LookupAccelerator(gpu); ok {
		return accelerator.Platform
	}
	return PlatformCUDA
}

// ExpectedDeviceResource is the Kubernetes device-plugin resource that
// exposes a GPU: a reviewed accelerator's, or NVIDIA's for every other name.
func ExpectedDeviceResource(gpu string) string {
	if accelerator, ok := LookupAccelerator(gpu); ok {
		return accelerator.DeviceResource
	}
	return DeviceResourceNVIDIA
}

// KnownDeviceResource reports a device-plugin resource whose platform is
// known. Other resources, such as NVIDIA MIG slices, are not checked.
func KnownDeviceResource(resource string) bool {
	resource = strings.TrimSpace(resource)
	return resource == DeviceResourceNVIDIA || resource == DeviceResourceAMD
}

// acceleratorKey lowercases a name, drops separators, the vendor and brand
// words ("amd", "instinct"), and the OAM form-factor suffix, which do not
// change the device.
func acceleratorKey(gpu string) string {
	key := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(gpu)))
	key = strings.TrimPrefix(key, "amd")
	key = strings.TrimPrefix(key, "instinct")
	return strings.TrimSuffix(key, "oam")
}
