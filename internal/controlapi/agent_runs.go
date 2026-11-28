package controlapi

import (
	"context"
	"errors"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizationcampaign"
	"github.com/infercrane/infercrane/internal/optimizationcapability"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/runtimecontract"
	"github.com/infercrane/infercrane/internal/support"
)

// agentEndpointStore finds the agent that serves an endpoint.
type agentEndpointStore interface {
	AgentForEndpoint(context.Context, string, string) (domain.Agent, error)
}

// agentRunTarget checks a run on an endpoint an InferCrane agent serves
// before it is stored, and returns that agent, or why the run is refused.
// The agent measures every setup in the customer's cluster, so the run must
// be one it can measure: setups it can start, the synthetic profile, and the
// built-in check with the default suite or an eval set kept beside the agent.
func (a API) agentRunTarget(ctx context.Context, tenant, endpoint string, proposal optimizer.Proposal) (string, string) {
	store, ok := a.Store.(agentEndpointStore)
	if !ok {
		return "", "this control plane cannot run an optimization on an agent's endpoint"
	}
	agent, err := store.AgentForEndpoint(ctx, tenant, endpoint)
	if errors.Is(err, domain.ErrNotFound) {
		return "", "endpoint " + endpoint + " was not found or has no server"
	}
	if errors.Is(err, domain.ErrConflict) {
		return "", err.Error()
	}
	if err != nil {
		return "", "the endpoint's agent could not be read"
	}
	input := proposal.Input
	switch {
	case proposal.StartingPoint == nil || proposal.StartingPoint.CandidateID == "":
		return "", "a run on an agent's endpoint compares every setup with the endpoint's own server: the proposal needs its starting point"
	case input.Migration != nil:
		return "", "a migration Proof measures new owned setups; it cannot evolve an agent's endpoint"
	case input.QualityCheck == optimizer.QualityCheckCustomer:
		return "", "customer-signed evidence for setups an agent measures is not supported yet; use the built-in check with an eval set kept beside the agent"
	case input.WorkloadSource == optimizer.WorkloadSourceCustomerObserved && input.WorkloadFingerprint != "":
		return "", "an agent measures the synthetic profile; a run bound to a replayed trace is not supported on an agent's endpoint yet"
	}
	for _, candidate := range proposal.Candidates {
		if candidate.ID == proposal.StartingPoint.CandidateID {
			continue
		}
		if _, setupErr := optimizationcampaign.AgentSetup(candidate.Deployment, a.Integrations.Providers); setupErr != nil {
			return "", "setup " + candidate.ID + ": " + setupErr.Error()
		}
	}
	return agent.ID, ""
}

// declaredAgentSetup records what an agent endpoint's operator declares its
// server runs as the proposal's starting setup. It is accepted only for an
// endpoint one active InferCrane agent serves and that has no deployment
// revision to read the setup from.
func (a API) declaredAgentSetup(ctx context.Context, tenant, endpoint string, declared optimizer.DeclaredSetup, revisioned bool) (optimizer.StartingSetup, string) {
	if revisioned {
		return optimizer.StartingSetup{}, "the endpoint's setup is read from its deployment's active revision; declared_setup is for an endpoint an InferCrane agent serves"
	}
	store, ok := a.Store.(agentEndpointStore)
	if !ok {
		return optimizer.StartingSetup{}, "this control plane cannot record a declared setup"
	}
	if _, err := store.AgentForEndpoint(ctx, tenant, endpoint); err != nil {
		return optimizer.StartingSetup{}, "declared_setup is for an endpoint one active InferCrane agent serves: " + err.Error()
	}
	setup, err := declared.StartingSetup(endpoint)
	if err != nil {
		return optimizer.StartingSetup{}, err.Error()
	}
	// A declaration is checked where it enters, as a deployment's setup is:
	// its engine arguments against the reviewed set, its environment against
	// the reviewed names. A secret never belongs in it: the declaration is
	// stored in the proposal and its digest.
	if err = optimizationcapability.ValidateEngineArgs(setup.Engine, setup.Args); err != nil {
		return optimizer.StartingSetup{}, err.Error()
	}
	for name := range setup.Env {
		upper := strings.ToUpper(name)
		for _, secret := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL"} {
			if strings.Contains(upper, secret) {
				return optimizer.StartingSetup{}, "declared_setup.env must not carry a secret (" + name + "); a private model's token goes in model_secret_reference_id"
			}
		}
	}
	if err = support.ValidateRuntimeEnv(setup.Engine, setup.Version, setup.GPU, runtimecontract.Workload{}, setup.Env); err != nil {
		return optimizer.StartingSetup{}, err.Error()
	}
	return setup, ""
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}
