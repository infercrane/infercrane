package controlapi

import (
	"fmt"

	"github.com/infercrane/infercrane/internal/optimizer"
)

// prerequisiteRefusal says why this control plane cannot deploy a setup on
// the adapter with the runtime: the adapter's provider profile declares a
// prerequisite, such as NVIDIA Dynamo, and composition recorded that this
// control plane does not run the runtime through it. It is empty when nothing
// is known to be missing. The check reads the profile, never the adapter's
// name.
func (a API) prerequisiteRefusal(adapter, runtime string) string {
	for _, profile := range a.Integrations.Providers {
		if profile.Adapter != adapter {
			continue
		}
		if gap := profile.PrerequisiteGap(runtime); gap != "" {
			return fmt.Sprintf("provider adapter %s cannot deploy %s: %s", adapter, runtime, gap)
		}
		return ""
	}
	return ""
}

// proposalPrerequisiteRefusal is the first setup in a proposal that this
// control plane cannot deploy, and why. A run that starts it would fail at its
// first deploy, after the operator approved its cost.
func (a API) proposalPrerequisiteRefusal(proposal optimizer.Proposal) string {
	for _, candidate := range proposal.Candidates {
		if refused := a.prerequisiteRefusal(candidate.Deployment.Provider.Adapter, candidate.Deployment.Runtime.Engine); refused != "" {
			return "setup " + candidate.ID + ": " + refused
		}
	}
	return ""
}
