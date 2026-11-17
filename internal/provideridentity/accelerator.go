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

// PlanningAccelerators are the GPU aliases the automatic model check sizes a
// model against, smallest device memory first. Each alias maps to an exact
// provider SKU through GPUTypeID, so a price observation can say whether the
// GPU is offered at all.
func PlanningAccelerators() []string {
	return []string{"T4", "L4", "A10G", "RTX 4090", "RTX 5090", "A100 40GB", "A40", "L40", "L40S", "RTX A6000", "A100 80GB", "H100", "H100 NVL", "RTX PRO 6000", "H200", "B200"}
}
