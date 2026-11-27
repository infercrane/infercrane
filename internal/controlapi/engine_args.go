package controlapi

import (
	"fmt"

	"github.com/infercrane/infercrane/internal/optimizationcapability"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// validateCandidateEngineArgs checks a submitted proposal's setups the way a
// deployment is checked: a built-in runtime's extra arguments must be in its
// reviewed set, and a few flags are refused in any image's arguments.
func validateCandidateEngineArgs(candidates []optimizer.Candidate) error {
	for _, candidate := range candidates {
		runtime := candidate.Deployment.Runtime
		var err error
		if runtime.Workload.Empty() {
			err = optimizationcapability.ValidateEngineArgs(runtime.Engine, runtime.Args)
		} else if err = optimizationcapability.RefuseUnsafeEngineArgs(runtime.Args); err == nil {
			err = optimizationcapability.RefuseUnsafeEngineArgs(runtime.Workload.Command)
		}
		if err != nil {
			return fmt.Errorf("setup %s: %w", candidate.ID, err)
		}
	}
	return nil
}
