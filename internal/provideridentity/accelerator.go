package provideridentity

import (
	"regexp"
	"strconv"
	"strings"
)

// acceleratorMemoryGiB is the device memory of accelerators InferCrane plans
// on, keyed by normalized name. An absent name has unknown memory.
var acceleratorMemoryGiB = map[string]int{
	"h100": 80, "h100sxm": 80, "h100pcie": 80, "h10080gb": 80, "h100nvl": 94, "h800": 80, "h20": 96, "h200": 141, "h200sxm": 141, "h200nvl": 141, "gh200": 96,
	"b200": 180, "b300": 288, "gb200": 186, "gb300": 288, "rtxpro6000": 96,
	"a100": 40, "a10040gb": 40, "a10080gb": 80, "a100sxm4": 40, "a800": 80,
	"l40s": 48, "l40": 48, "l4": 24, "a10g": 24, "a10": 24, "a40": 48, "a6000": 48, "rtxa6000": 48, "rtx6000ada": 48,
	"rtx4090": 24, "rtx5090": 32, "t4": 16, "v100": 16,
}

var memorySuffix = regexp.MustCompile(`([0-9]+)gb$`)

// acceleratorDriverMemoryMiB is the device memory the driver reports as
// total, in MiB, keyed by normalized name: the total nvidia-smi prints, and
// the capacity vLLM and SGLang take their share of memory from. It is below
// the nominal size above: GDDR GPUs keep ECC bits in device memory (each
// GDDR reading here has ECC on, the data-center default), and the driver
// reserves some. Every figure is read from the published output cited next
// to it. A GPU with no published reading is absent, so its usable memory
// stays unknown; a nominal size is never used in its place.
var acceleratorDriverMemoryMiB = map[string]int{
	// Tesla T4: "2MiB / 15360MiB", https://github.com/NVIDIA/nvidia-container-toolkit/issues/672
	"t4": 15360, "teslat4": 15360,
	// NVIDIA L4: "0MiB / 23034MiB", https://github.com/vllm-project/vllm/issues/17386
	"l4": 23034,
	// NVIDIA A10G: "21303MiB / 23028MiB",
	// https://forum.level1techs.com/t/nvidia-a10g-in-octominer-x12-or-putting-enterpise-gpus-where-they-dont-belong/253642
	"a10g": 23028,
	// NVIDIA A10 with ECC on, its default: "0MiB / 22731MiB",
	// https://forums.developer.nvidia.com/t/gpu-memory-less-than-promised/232129
	"a10": 22731,
	// NVIDIA GeForce RTX 4090: "2MiB / 24564MiB", https://github.com/vllm-project/vllm/issues/7553
	"rtx4090": 24564, "geforcertx4090": 24564,
	// NVIDIA A40: "0MiB / 46068MiB", https://github.com/NVIDIA/open-gpu-kernel-modules/issues/446
	"a40": 46068,
	// NVIDIA L40S: "1MiB / 46068MiB", https://github.com/NVIDIA/nvidia-container-toolkit/issues/1051
	"l40s": 46068,
	// NVIDIA A100-SXM4-40GB: "1MiB / 40960MiB", https://github.com/unslothai/unsloth/issues/3260;
	// NVIDIA A100-PCIE-40GB: memory 40Gi,
	// https://dra-driver-nvidia-gpu.sigs.k8s.io/docs/reference/resourceslice-attributes/
	"a100": 40960, "a10040gb": 40960, "a100sxm4": 40960, "a100sxm440gb": 40960, "a100pcie40gb": 40960,
	// NVIDIA A100-SXM4-80GB: "2MiB / 81920MiB", https://github.com/vllm-project/vllm/issues/2069;
	// NVIDIA A100 80GB PCIe: "0MiB / 81920MiB", https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/issues/32
	"a10080gb": 81920, "a100sxm480gb": 81920, "a10080gbpcie": 81920,
	// NVIDIA H100 80GB HBM3 (SXM): "4MiB / 81559MiB", https://github.com/NVIDIA/nccl/issues/1967;
	// NVIDIA H100 PCIe: "24MiB / 81559MiB", https://github.com/NVIDIA/nvtrust/issues/56
	"h100": 81559, "h100sxm": 81559, "h10080gb": 81559, "h10080gbhbm3": 81559, "h100pcie": 81559,
	// NVIDIA H200: "0MiB / 143771MiB", https://github.com/NVIDIA/cuda-samples/issues/311
	"h200": 143771, "h200sxm": 143771,
	// NVIDIA B200: "0MiB / 183359MiB", https://github.com/NVIDIA/nccl-tests/issues/333
	"b200": 183359,
	// AMD Instinct MI300X: torch.cuda.mem_get_info total 206141652992 bytes,
	// https://github.com/ROCm/rocm-systems/issues/15275
	"mi300x": 196592,
}

// NormalizedAccelerator lowercases an accelerator name and drops separators
// and a leading "nvidia", so "NVIDIA H100-80GB" and "h100 80gb" compare equal.
func NormalizedAccelerator(gpu string) string {
	normalized := strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(gpu)))
	return strings.TrimPrefix(normalized, "nvidia")
}

// AcceleratorMemoryGiB returns an accelerator's device memory. A reviewed
// accelerator (LookupAccelerator) reports its own, under every provider
// spelling; a name that ends in a size ("…80GB") is read from that size; any
// other unknown name reports false rather than a guess.
func AcceleratorMemoryGiB(gpu string) (int, bool) {
	if accelerator, reviewed := LookupAccelerator(gpu); reviewed {
		return accelerator.MemoryGiB, true
	}
	name := NormalizedAccelerator(gpu)
	if memory, known := acceleratorMemoryGiB[name]; known {
		return memory, true
	}
	if match := memorySuffix.FindStringSubmatch(name); match != nil {
		if parsed, err := strconv.Atoi(match[1]); err == nil && parsed > 0 {
			return parsed, true
		}
	}
	return 0, false
}

// AcceleratorUsableMemoryMiB returns the device memory an accelerator's
// driver reports as total, the figure a runtime sizes its memory from, or
// false when no published reading is recorded for it. A reviewed accelerator
// answers under every provider spelling; a size in the name is not read.
func AcceleratorUsableMemoryMiB(gpu string) (int, bool) {
	name := NormalizedAccelerator(gpu)
	if accelerator, reviewed := LookupAccelerator(gpu); reviewed {
		name = NormalizedAccelerator(accelerator.Name)
	}
	memory, known := acceleratorDriverMemoryMiB[name]
	return memory, known
}

// PlanningAccelerators are the GPU aliases the automatic model check sizes a
// model against, smallest device memory first. Each alias maps to an exact
// provider SKU through GPUTypeID, so a price observation can say whether the
// GPU is offered at all.
func PlanningAccelerators() []string {
	return []string{"T4", "L4", "A10G", "RTX 4090", "RTX 5090", "A100 40GB", "A40", "L40", "L40S", "RTX A6000", "A100 80GB", "H100", "H100 NVL", "RTX PRO 6000", "H200", "B200"}
}
