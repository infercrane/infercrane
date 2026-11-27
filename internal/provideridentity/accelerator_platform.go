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

// Accelerator statuses. Platform qualification is data: an accelerator's
// status decides whether any lookup that selects an image, a placement, or a
// planning input may return it.
const (
	// AcceleratorServing means runtime images are pinned for the
	// accelerator's platform and the support matrix places it, so a spec may
	// name it. Serving is not measured: speed, cost, and quality stay unknown
	// until a recorded run.
	AcceleratorServing = "serving"
	// AcceleratorNotQualified means InferCrane knows the accelerator by name,
	// so it resolves to its own platform and device resource instead of the
	// CUDA default, but nothing may select it: it has no selectable image, no
	// placement, and no planning or catalog entry. Every spec that names it is
	// refused until a measured run on its platform qualifies it.
	AcceleratorNotQualified = "not-qualified"
)

// Kubernetes device-plugin resource names. The default platform's resource
// is the NVIDIA device plugin's.
const (
	DeviceResourceNVIDIA = "nvidia.com/gpu"
	DeviceResourceAMD    = "amd.com/gpu"
)

// Accelerator is one reviewed accelerator outside the default CUDA platform.
// Every value is read from the vendor's published specifications or the
// runtime's source; Sources lists where.
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
	// use for FP8 weights and FP8 KV cache. It is false for an accelerator
	// that is not qualified, whatever the hardware does: no pinned runtime
	// uses it there yet.
	NativeFP8 bool `json:"native_fp8"`
	// Status is AcceleratorServing or AcceleratorNotQualified.
	Status  string   `json:"status"`
	Sources []string `json:"sources"`
	// aliases are normalized spellings beyond the canonical name: provider
	// SKUs and node labels.
	aliases []string
	// sliceSuffix admits an alias followed by a slice size in digits, as
	// SkyPilot names TPU slices ("tpu-v6e-8"). The size never changes the
	// accelerator.
	sliceSuffix bool
}

const (
	// Architecture, compiler target, and memory per model.
	sourceROCmSpecs = "https://rocm.docs.amd.com/en/latest/reference/gpu-arch-specs.html"
	// Product pages list peak FP8 matrix performance.
	sourceMI300X = "https://www.amd.com/en/products/accelerators/instinct/mi300/mi300x.html"
	sourceMI325X = "https://www.amd.com/en/products/accelerators/instinct/mi300/mi325x.html"
	sourceMI350X = "https://www.amd.com/en/products/accelerators/instinct/mi350/mi350x.html"
	sourceMI355X = "https://www.amd.com/en/products/accelerators/instinct/mi350/mi355x.html"
	// vLLM 0.34.0, as 0.22.1 did, lists FP8 W8A8 (llm-compressor) as supported
	// on AMD GPUs and an FP8 KV cache (fp8 = fp8_e4m3) as supported on ROCm.
	sourceVLLMFP8   = "https://github.com/vllm-project/vllm/blob/v0.34.0/docs/features/quantization/README.md#supported-hardware"
	sourceVLLMFP8KV = "https://github.com/vllm-project/vllm/blob/v0.34.0/vllm/config/cache.py"
	// SGLang 0.5.23 (as 0.5.12 did): "FP8 works via Aiter or Triton" on AMD GPUs.
	sourceSGLangFP8 = "https://github.com/sgl-project/sglang/blob/v0.5.23/docs/docs/hardware-platforms/amd_gpu.mdx#quantization-on-amd-gpus"
)

var reviewedAccelerators = []Accelerator{
	{
		Name: "MI300X", Vendor: "amd", Product: "AMD Instinct MI300X", Architecture: "CDNA 3", Target: "gfx942",
		Platform: PlatformROCmGFX942, DeviceResource: DeviceResourceAMD, MemoryGiB: 192, NativeFP8: true, Status: AcceleratorServing,
		Sources: []string{sourceROCmSpecs, sourceMI300X, sourceVLLMFP8, sourceVLLMFP8KV, sourceSGLangFP8},
	},
	{
		Name: "MI325X", Vendor: "amd", Product: "AMD Instinct MI325X", Architecture: "CDNA 3", Target: "gfx942",
		Platform: PlatformROCmGFX942, DeviceResource: DeviceResourceAMD, MemoryGiB: 256, NativeFP8: true, Status: AcceleratorServing,
		Sources: []string{sourceROCmSpecs, sourceMI325X, sourceVLLMFP8, sourceVLLMFP8KV, sourceSGLangFP8},
	},
	{
		Name: "MI350X", Vendor: "amd", Product: "AMD Instinct MI350X", Architecture: "CDNA 4", Target: "gfx950",
		Platform: PlatformROCmGFX950, DeviceResource: DeviceResourceAMD, MemoryGiB: 288, NativeFP8: true, Status: AcceleratorServing,
		Sources: []string{sourceROCmSpecs, sourceMI350X, sourceVLLMFP8, sourceVLLMFP8KV, sourceSGLangFP8},
		// RunPod's GPU type for its MI350X offer is "AMD Instinct MI350 OAM".
		aliases: []string{"mi350"},
	},
	{
		Name: "MI355X", Vendor: "amd", Product: "AMD Instinct MI355X", Architecture: "CDNA 4", Target: "gfx950",
		Platform: PlatformROCmGFX950, DeviceResource: DeviceResourceAMD, MemoryGiB: 288, NativeFP8: true, Status: AcceleratorServing,
		Sources: []string{sourceROCmSpecs, sourceMI355X, sourceVLLMFP8, sourceVLLMFP8KV, sourceSGLangFP8},
	},
}

// Accelerators returns the serving accelerators outside the default CUDA
// platform, in catalog order. Accelerators that are not qualified are listed
// only by NotQualifiedAccelerators.
func Accelerators() []Accelerator {
	return acceleratorsWithStatus(AcceleratorServing)
}

// NotQualifiedAccelerators returns the accelerators InferCrane recognizes by
// name but refuses to select, in catalog order.
func NotQualifiedAccelerators() []Accelerator {
	return acceleratorsWithStatus(AcceleratorNotQualified)
}

func acceleratorsWithStatus(status string) []Accelerator {
	var out []Accelerator
	for _, accelerator := range catalogAccelerators() {
		if accelerator.Status == status {
			out = append(out, exported(accelerator))
		}
	}
	return out
}

// LookupAccelerator resolves a GPU name to a serving accelerator. It accepts
// the canonical name and the spellings providers use for the same device:
// "MI300X", "AMD Instinct MI300X", RunPod's "AMD Instinct MI300X OAM", and the
// AMD node labeller's "AMD_Instinct_MI300X_OAM". Any other name, including an
// accelerator that is not qualified, reports false; it is never guessed from
// a model number.
func LookupAccelerator(gpu string) (Accelerator, bool) {
	accelerator, ok := IdentifyAccelerator(gpu)
	if !ok || accelerator.Status != AcceleratorServing {
		return Accelerator{}, false
	}
	return accelerator, true
}

// IdentifyAccelerator resolves a GPU name to any reviewed accelerator,
// serving or not qualified. Use it only to name a device's platform and
// device resource; anything that selects an image, a placement, or a
// planning input must use LookupAccelerator.
func IdentifyAccelerator(gpu string) (Accelerator, bool) {
	key := acceleratorKey(gpu)
	if key == "" {
		return Accelerator{}, false
	}
	for _, accelerator := range catalogAccelerators() {
		if accelerator.matches(key) {
			return exported(accelerator), true
		}
	}
	return Accelerator{}, false
}

func (a Accelerator) matches(key string) bool {
	if key == acceleratorKey(a.Name) || slices.Contains(a.aliases, key) {
		return true
	}
	if !a.sliceSuffix {
		return false
	}
	for _, alias := range append([]string{acceleratorKey(a.Name)}, a.aliases...) {
		size, found := strings.CutPrefix(key, alias)
		if found && size != "" && strings.Trim(size, "0123456789") == "" {
			return true
		}
	}
	return false
}

// AcceleratorPlatform is the runtime platform a GPU needs: a reviewed
// accelerator's platform, serving or not qualified, or the default CUDA
// platform for every other name. A platform that is not qualified has no
// selectable image or placement, so a spec on it is refused.
func AcceleratorPlatform(gpu string) string {
	if accelerator, ok := IdentifyAccelerator(gpu); ok {
		return accelerator.Platform
	}
	return PlatformCUDA
}

// ExpectedDeviceResource is the Kubernetes device-plugin resource that
// exposes a GPU: a reviewed accelerator's, serving or not qualified, or
// NVIDIA's for every other name.
func ExpectedDeviceResource(gpu string) string {
	if accelerator, ok := IdentifyAccelerator(gpu); ok {
		return accelerator.DeviceResource
	}
	return DeviceResourceNVIDIA
}

// KnownDeviceResource reports a device-plugin resource whose platform is
// known: NVIDIA's or a reviewed accelerator's. Other resources, such as
// NVIDIA MIG slices, are not checked.
func KnownDeviceResource(resource string) bool {
	resource = strings.TrimSpace(resource)
	if resource == DeviceResourceNVIDIA {
		return true
	}
	return slices.ContainsFunc(catalogAccelerators(), func(accelerator Accelerator) bool {
		return accelerator.DeviceResource == resource
	})
}

func catalogAccelerators() []Accelerator {
	return slices.Concat(reviewedAccelerators, notQualifiedAccelerators)
}

func exported(accelerator Accelerator) Accelerator {
	accelerator.Sources = slices.Clone(accelerator.Sources)
	accelerator.aliases = nil
	accelerator.sliceSuffix = false
	return accelerator
}

// acceleratorKey lowercases a name, drops separators, the vendor and brand
// words ("amd", "instinct", "google", "cloud", "intel", "habana"), and the
// OAM form-factor suffix, which do not change the device.
func acceleratorKey(gpu string) string {
	key := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(gpu)))
	for _, word := range []string{"amd", "instinct", "google", "cloud", "intel", "habana"} {
		key = strings.TrimPrefix(key, word)
	}
	return strings.TrimSuffix(key, "oam")
}
