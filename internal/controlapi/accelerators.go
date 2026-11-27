package controlapi

import (
	"net/http"

	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/support"
)

// acceleratorCatalog is the reviewed accelerator identity the dashboard and
// CLI use to name a GPU's vendor and platform without guessing from a model
// number. Pinning an image is not a measurement: Measured stays false until
// a recorded run on that platform exists. Accelerators, RuntimeImages, and
// Placement list only what a spec may select; NotQualified lists the
// accelerators InferCrane recognizes but refuses.
type acceleratorCatalog struct {
	DefaultPlatform string                          `json:"default_platform"`
	Accelerators    []acceleratorEntry              `json:"accelerators"`
	RuntimeImages   []support.RuntimeImage          `json:"runtime_images"`
	Placement       []support.PlatformQualification `json:"placement"`
	NotQualified    []notQualifiedEntry             `json:"not_qualified"`
}

type acceleratorEntry struct {
	provideridentity.Accelerator
	// ProviderIDs are the exact provider SKUs this accelerator is rented
	// under, where InferCrane maps one.
	ProviderIDs map[string]string `json:"provider_ids"`
	Measured    bool              `json:"measured"`
	Limitations []string          `json:"limitations"`
}

// notQualifiedEntry is an accelerator that resolves to its own platform, so
// it is never served a CUDA image, and that every spec, plan, and setup
// refuses until a measured run qualifies the platform.
type notQualifiedEntry struct {
	provideridentity.Accelerator
	// CandidateImages are the engine builds a first qualification run on the
	// platform would pin. None is selectable.
	CandidateImages []support.RuntimeImage `json:"candidate_images"`
	Limitations     []string               `json:"limitations"`
}

func (a API) accelerators(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": buildAcceleratorCatalog()})
}

func buildAcceleratorCatalog() acceleratorCatalog {
	out := acceleratorCatalog{DefaultPlatform: provideridentity.PlatformCUDA, RuntimeImages: support.RuntimeImages(), Placement: support.V1().AcceleratorPlatforms()}
	for _, accelerator := range provideridentity.Accelerators() {
		entry := acceleratorEntry{Accelerator: accelerator, ProviderIDs: map[string]string{}, Limitations: []string{
			"No model has been served or measured on this accelerator through InferCrane yet; speed, cost, and quality stay unknown until a recorded run.",
		}}
		if runpod := provideridentity.GPUTypeID("runpod", accelerator.Name); runpod != accelerator.Name {
			entry.ProviderIDs["runpod"] = runpod
		} else {
			entry.Limitations = append(entry.Limitations, "RunPod does not list this accelerator; run it on your own Kubernetes cluster.")
		}
		out.Accelerators = append(out.Accelerators, entry)
	}
	candidates := support.NotQualifiedRuntimeImages()
	for _, accelerator := range provideridentity.NotQualifiedAccelerators() {
		entry := notQualifiedEntry{Accelerator: accelerator, CandidateImages: []support.RuntimeImage{}, Limitations: []string{
			"Not qualified: InferCrane recognizes this accelerator by name and refuses every spec, plan, and setup that names it until a measured run on its platform qualifies it.",
			"Speed, cost, and quality on this accelerator are unknown.",
		}}
		for _, image := range candidates {
			if image.Platform == accelerator.Platform {
				entry.CandidateImages = append(entry.CandidateImages, image)
			}
		}
		out.NotQualified = append(out.NotQualified, entry)
	}
	return out
}
