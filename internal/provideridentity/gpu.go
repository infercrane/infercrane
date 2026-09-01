// Package provideridentity owns reviewed mappings between InferCrane's GPU
// aliases and provider resource identifiers. Price discovery, planning, and
// provisioning must all use the same mapping so they refer to the same SKU.
package provideridentity

import "strings"

// GPUTypeID returns the exact provider resource identifier for a reviewed
// alias. Unknown values remain unchanged instead of being guessed.
func GPUTypeID(provider, gpu string) string {
	provider = strings.ToLower(strings.TrimSpace(provider))
	normalized := strings.ToUpper(strings.TrimSpace(gpu))
	if provider == "aws" {
		switch normalized {
		case "T4", "NVIDIA T4":
			return "NVIDIA T4"
		case "L4", "NVIDIA L4":
			return "NVIDIA L4"
		case "L40S", "NVIDIA L40S":
			return "NVIDIA L40S"
		case "A100", "A100 80GB", "NVIDIA A100-SXM4-80GB":
			return "NVIDIA A100-SXM4-80GB"
		case "A100 40GB", "NVIDIA A100-SXM4-40GB":
			return "NVIDIA A100-SXM4-40GB"
		case "H100", "H100 SXM", "NVIDIA H100 80GB HBM3":
			return "NVIDIA H100 80GB HBM3"
		case "H200", "NVIDIA H200":
			return "NVIDIA H200"
		case "B200", "NVIDIA B200":
			return "NVIDIA B200"
		default:
			return gpu
		}
	}
	if provider == "azure" {
		switch normalized {
		case "T4":
			return "T4"
		case "V100", "V100 16GB", "V100-16GB":
			return "V100-16GB"
		case "A10", "A10 24GB", "A10-24GB":
			return "A10-24GB"
		case "A100", "A100 80GB", "A100-80GB":
			return "A100-80GB"
		case "A100 40GB", "A100-40GB":
			return "A100-40GB"
		case "H100", "H100 80GB", "H100-80GB":
			return "H100-80GB"
		case "H100 NVL", "H100-NVL", "H100-NVL-94GB":
			return "H100-NVL-94GB"
		case "H200", "H200 141GB", "H200-141GB":
			return "H200-141GB"
		default:
			return gpu
		}
	}
	if provider == "vast" {
		switch normalized {
		case "H100", "H100 SXM":
			return "H100 SXM"
		case "H100 PCIE":
			return "H100 PCIE"
		case "H100 NVL":
			return "H100 NVL"
		case "H200", "H200 SXM":
			return "H200"
		case "H200 NVL":
			return "H200 NVL"
		case "A100", "A100 80GB", "A100 SXM4":
			return "A100 SXM4"
		case "A100 PCIE", "A100 80GB PCIE":
			return "A100 PCIE"
		case "T4", "TESLA T4":
			return "Tesla T4"
		case "V100", "TESLA V100":
			return "Tesla V100"
		default:
			return gpu
		}
	}
	if provider != "runpod" {
		return gpu
	}
	switch normalized {
	case "L40S", "NVIDIA L40S":
		return "NVIDIA L40S"
	case "L40", "NVIDIA L40":
		return "NVIDIA L40"
	case "L4", "NVIDIA L4":
		return "NVIDIA L4"
	case "H100", "H100 SXM", "NVIDIA H100 80GB HBM3":
		return "NVIDIA H100 80GB HBM3"
	case "H100 PCIE", "NVIDIA H100 PCIE":
		return "NVIDIA H100 PCIe"
	case "H100 NVL", "NVIDIA H100 NVL":
		return "NVIDIA H100 NVL"
	case "H200", "H200 SXM", "NVIDIA H200":
		return "NVIDIA H200"
	case "H200 NVL", "NVIDIA H200 NVL":
		return "NVIDIA H200 NVL"
	case "B200", "NVIDIA B200":
		return "NVIDIA B200"
	case "RTXPRO6000", "RTX PRO 6000", "NVIDIA RTX PRO 6000 BLACKWELL SERVER EDITION":
		return "NVIDIA RTX PRO 6000 Blackwell Server Edition"
	case "A40", "NVIDIA A40":
		return "NVIDIA A40"
	case "A100-80GB", "A100 80GB", "NVIDIA A100-SXM4-80GB":
		return "NVIDIA A100-SXM4-80GB"
	case "A100 PCIE", "A100 80GB PCIE", "NVIDIA A100 80GB PCIE":
		return "NVIDIA A100 80GB PCIe"
	default:
		return gpu
	}
}

// PriceRegion returns the scope used by a provider's price feed. RunPod's
// secure-cloud gpuTypes quote is global; launch capacity is checked separately
// for the requested data center.
func PriceRegion(provider, region string) string {
	if strings.EqualFold(strings.TrimSpace(provider), "runpod") || strings.EqualFold(strings.TrimSpace(provider), "vast") {
		return "global"
	}
	return region
}

// MatchesGPU lets public filters accept either InferCrane's reviewed alias or
// the exact provider SKU without fuzzy variant matching.
func MatchesGPU(provider, catalogGPU, requestedGPU string) bool {
	return strings.EqualFold(strings.TrimSpace(catalogGPU), strings.TrimSpace(GPUTypeID(provider, requestedGPU)))
}
