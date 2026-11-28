package optimizer

import (
	"context"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/integration"
)

// dynamoCatalogSource plans like a control plane configured to run Dynamo
// graphs with the given runtimes.
func dynamoCatalogSource(t *testing.T, runtimes ...string) CatalogSource {
	t.Helper()
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.ConfigurePrerequisite("kubernetes-dynamo", runtimes...); err != nil {
		t.Fatal(err)
	}
	return NewCatalogSource(curatedrecipe.All(), registry.Snapshot())
}

func adaptersOf(proposal Proposal) map[string][]string {
	out := map[string][]string{}
	for _, candidate := range proposal.Candidates {
		adapter := candidate.Deployment.Provider.Adapter
		out[adapter] = append(out[adapter], candidate.Deployment.Runtime.Engine)
	}
	return out
}

func dynamoWarning(proposal Proposal) string {
	for _, warning := range proposal.Warnings {
		if strings.Contains(warning, "kubernetes-dynamo") {
			return warning
		}
	}
	return ""
}

// On 27 Nov 2026 `optimize propose --provider kubernetes --include-simulated`
// proposed a kubernetes-dynamo setup for an EKS cluster with no NVIDIA Dynamo.
// Asking for the cloud says nothing about Dynamo, so the planner leaves those
// setups out and says why, for reviewed and generic models alike.
func TestProviderKubernetesLeavesOutDynamoSetupsTheClusterMayNotRun(t *testing.T) {
	for name, request := range map[string]Request{
		"reviewed": {ModelIdentity: "Qwen/Qwen3-8B", Provider: "kubernetes", GPU: "NVIDIA-L40S", Objective: "interactive", IncludeSimulated: true, MaxCandidates: 10},
		"generic":  {ModelIdentity: "acme/novel-open-model", ModelRevision: strings.Repeat("a", 40), Provider: "kubernetes", GPU: "NVIDIA-A10G", Objective: "interactive", IncludeSimulated: true, MaxCandidates: 10},
	} {
		t.Run(name, func(t *testing.T) {
			proposal, err := catalogSource(t).Propose(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			adapters := adaptersOf(proposal)
			if _, dynamo := adapters["kubernetes-dynamo"]; dynamo || len(adapters["kubernetes"]) == 0 {
				t.Fatalf("want plain Kubernetes setups only, got %v", adapters)
			}
			for _, candidate := range proposal.Candidates {
				if !candidate.Deployment.Serving.Empty() {
					t.Fatalf("a setup carries a serving graph nobody asked for: %+v", candidate.Deployment)
				}
			}
			warning := dynamoWarning(proposal)
			for _, want := range []string{"Setups on kubernetes-dynamo (sglang, vllm) were left out", "NVIDIA Dynamo", "Name provider kubernetes-dynamo"} {
				if !strings.Contains(warning, want) {
					t.Fatalf("warning %q lacks %q; warnings=%v", warning, want, proposal.Warnings)
				}
			}
			if err = ValidateProposal(proposal); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Without --include-simulated no Dynamo setup was a candidate, so nothing was
// left out and nothing is said.
func TestNoDynamoWarningWhenNoDynamoSetupWouldHaveMatched(t *testing.T) {
	for name, request := range map[string]Request{
		"qualified only": {ModelIdentity: "Qwen/Qwen3-0.6B", ModelRevision: "c1899de289a04d12100db370d81485cdf75e47ca", Provider: "kubernetes", GPU: "NVIDIA-A10G", Runtimes: []string{"vllm"}, Objective: "interactive", MaxCandidates: 4},
		"another cloud":  {ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive", IncludeSimulated: true},
	} {
		t.Run(name, func(t *testing.T) {
			proposal, err := catalogSource(t).Propose(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if warning := dynamoWarning(proposal); warning != "" {
				t.Fatalf("unexpected warning %q", warning)
			}
		})
	}
}

// Naming the adapter is the operator saying Dynamo runs there, so the CLI,
// which plans from the catalog alone, still plans Dynamo setups for it.
func TestANamedDynamoAdapterIsPlannedWithoutAConfiguredControlPlane(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "Qwen/Qwen3-8B", Provider: "kubernetes-dynamo", GPU: "NVIDIA-L40S", Objective: "interactive", IncludeSimulated: true, MaxCandidates: 10})
	if err != nil {
		t.Fatal(err)
	}
	if adapters := adaptersOf(proposal); len(adapters["kubernetes-dynamo"]) == 0 {
		t.Fatalf("named adapter has no setup: %v missing=%v", adapters, proposal.Missing)
	}
	if warning := dynamoWarning(proposal); warning != "" {
		t.Fatalf("a named adapter left nothing out, got %q", warning)
	}
}

// A control plane with only the vLLM Dynamo image configured offers Dynamo
// setups for vLLM and leaves out the SGLang ones, which have no backend there.
func TestAConfiguredControlPlaneOffersDynamoOnlyForItsRuntimes(t *testing.T) {
	request := Request{ModelIdentity: "acme/novel-open-model", ModelRevision: strings.Repeat("a", 40), Provider: "kubernetes", GPU: "H100", Objective: "throughput", IncludeSimulated: true, MaxCandidates: 10}
	proposal, err := dynamoCatalogSource(t, "vllm").Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	adapters := adaptersOf(proposal)
	if got := strings.Join(adapters["kubernetes-dynamo"], ","); got != "vllm" {
		t.Fatalf("Dynamo setups for runtimes %q, want vllm only; all=%v", got, adapters)
	}
	if first := proposal.Candidates[0].Deployment.Provider.Adapter; first != "kubernetes" {
		t.Fatalf("provider kubernetes must still start from the plain server, got %q", first)
	}
	if warning := dynamoWarning(proposal); !strings.Contains(warning, "Setups on kubernetes-dynamo (sglang) were left out") {
		t.Fatalf("warning=%q", warning)
	}

	proposal, err = dynamoCatalogSource(t, "vllm", "sglang").Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(adaptersOf(proposal)["kubernetes-dynamo"], ","); got != "sglang,vllm" && got != "vllm,sglang" {
		t.Fatalf("Dynamo setups for runtimes %q, want both", got)
	}
	if warning := dynamoWarning(proposal); warning != "" {
		t.Fatalf("nothing was left out, got %q", warning)
	}
}
