package optimizer

import (
	"context"
	"slices"
	"testing"

	"github.com/infercrane/infercrane/internal/integration"
)

// The 27 Nov 2026 EKS run qualified vLLM 0.34.0 on one NVIDIA A10G with the
// Kubernetes adapter. A proposal for that GPU needs no --include-simulated;
// every other GPU still does.
func TestKubernetesProposalUsesTheQualifiedA10GTupleOnly(t *testing.T) {
	const revision = "c1899de289a04d12100db370d81485cdf75e47ca"
	request := Request{ModelIdentity: "Qwen/Qwen3-0.6B", ModelRevision: revision, Provider: "kubernetes", Runtimes: []string{"vllm"}, Objective: "interactive", MaxCandidates: 4}

	a10g := request
	a10g.GPU = "NVIDIA-A10G"
	proposal, err := catalogSource(t).Propose(context.Background(), a10g)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Candidates) != 1 || proposal.Candidates[0].Deployment.Provider.Adapter != "kubernetes" || proposal.Candidates[0].CompatibilityState != string(integration.QualificationReal) {
		t.Fatalf("A10G without simulated setups: missing=%v candidates=%+v", proposal.Missing, proposal.Candidates)
	}

	l4 := request
	l4.GPU = "NVIDIA-L4"
	proposal, err = catalogSource(t).Propose(context.Background(), l4)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Candidates) != 0 || !slices.Contains(proposal.Missing, "qualified_provider_runtime_compatibility") {
		t.Fatalf("L4 is not qualified on a real cluster: missing=%v candidates=%d", proposal.Missing, len(proposal.Candidates))
	}

	a10g.IncludeSimulated = true
	proposal, err = catalogSource(t).Propose(context.Background(), a10g)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, candidate := range proposal.Candidates {
		if _, twice := states[candidate.Deployment.Provider.Adapter]; twice {
			t.Fatalf("two setups for adapter %s: %+v", candidate.Deployment.Provider.Adapter, proposal.Candidates)
		}
		states[candidate.Deployment.Provider.Adapter] = candidate.CompatibilityState
	}
	// The cluster ran no NVIDIA Dynamo, and asking for the cloud does not say
	// it does, so no kubernetes-dynamo setup joins the simulated ones.
	if _, dynamo := states["kubernetes-dynamo"]; states["kubernetes"] != string(integration.QualificationReal) || dynamo {
		t.Fatalf("with simulated setups: %v", states)
	}
}
