package controlapi

import (
	"net/http"

	"github.com/infercrane/infercrane/internal/provideridentity"
	"github.com/infercrane/infercrane/internal/support"
)

// acceleratorCatalog is the reviewed accelerator identity the dashboard and
// CLI use to name a GPU's vendor and platform without guessing from a model
// number. Pinning an image is not a measurement: Measured stays false until
// a recorded run on that platform exists.
type acceleratorCatalog struct {
	DefaultPlatform string                          `json:"default_platform"`
	Accelerators    []acceleratorEntry              `json:"accelerators"`
	RuntimeImages   []support.RuntimeImage          `json:"runtime_images"`
	Placement       []support.PlatformQualification `json:"placement"`
}

type acceleratorEntry struct {
	provideridentity.Accelerator
	// ProviderIDs are the exact provider SKUs this accelerator is rented
	// under, where InferCrane maps one.
	ProviderIDs map[string]string `json:"provider_ids"`
	Measured    bool              `json:"measured"`
	Limitations []string          `json:"limitations"`
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
	return out
}
