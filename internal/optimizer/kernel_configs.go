package optimizer

import (
	"fmt"

	"github.com/infercrane/infercrane/internal/kerneltuning"
)

// TechniqueTunedKernels serves the starting setup with the engine's own
// kernels tuned for the GPU and the workload's step sizes: a tuned set the
// request carries, offered by kernel tuning's technique (internal/kerneltuning)
// through the technique contract (technique_path.go). No diagnosis rule picks
// it; the request brings it, made where the engine's log showed it ran its
// default tiles for the model's shape on this GPU.
const TechniqueTunedKernels = kerneltuning.TechniqueID

// tunedKernelsPriority places the candidate below every change a diagnosis
// rule chose: on tuned engines kernel steps are reported at single digits to
// about 20%, and these settings were chosen on kernel timings alone.
const tunedKernelsPriority = 30

// validateKernelConfigs checks a request's tuned set: valid, and for the GPU
// the request plans on and an engine it may serve.
func validateKernelConfigs(request Request) error {
	if request.KernelConfigs == nil {
		return nil
	}
	set := *request.KernelConfigs
	if err := kerneltuning.Validate(set); err != nil {
		return fmt.Errorf("kernel_configs: %w", err)
	}
	if device := kerneltuning.DeviceForGPU(request.GPU); device != set.Device {
		return fmt.Errorf("kernel_configs were tuned on %s, not the requested GPU %s (%s)", set.Device, request.GPU, device)
	}
	if len(request.Runtimes) > 0 && !contains(request.Runtimes, set.Runtime) {
		return fmt.Errorf("kernel_configs are for %s, which the request's runtimes leave out", set.Runtime)
	}
	return nil
}
