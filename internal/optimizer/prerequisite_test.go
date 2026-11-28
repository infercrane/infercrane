package optimizer

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/integration"
)

// dynamoSettings are the settings a control plane names for its Dynamo
// images, as composition records them.
var dynamoSettings = map[string][]string{
	"vllm":   {"INFERCRANE_DYNAMO_VLLM_IMAGE_DIGEST", "INFERCRANE_DYNAMO_VLLM_RUNTIME_VERSION"},
	"sglang": {"INFERCRANE_DYNAMO_SGLANG_IMAGE_DIGEST", "INFERCRANE_DYNAMO_SGLANG_RUNTIME_VERSION"},
}

// dynamoCatalogSource plans like a control plane configured to run Dynamo
// graphs with the given runtimes, none for a control plane without Dynamo.
func dynamoCatalogSource(t *testing.T, runtimes ...string) CatalogSource {
	t.Helper()
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	if err = registry.ConfigurePrerequisite("kubernetes-dynamo", dynamoSettings, runtimes...); err != nil {
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
	return strings.Join(dynamoWarnings(proposal), " ")
}

func dynamoWarnings(proposal Proposal) []string {
	var out []string
	for _, warning := range proposal.Warnings {
		if strings.Contains(warning, "kubernetes-dynamo") {
			out = append(out, warning)
		}
	}
	return out
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

// A control plane without a Dynamo image for a runtime knows a named
// kubernetes-dynamo setup for it cannot deploy there. It still proposes the
// setup, since the request names the adapter, and says which settings to set.
func TestANamedDynamoAdapterOnAControlPlaneWithoutItsImageSaysItCannotDeploy(t *testing.T) {
	request := Request{ModelIdentity: "Qwen/Qwen3-8B", Provider: "kubernetes-dynamo", GPU: "NVIDIA-L40S", Objective: "interactive", IncludeSimulated: true, MaxCandidates: 10}
	for name, test := range map[string]struct {
		configured []string
		want       []string
	}{
		"no Dynamo": {want: []string{"Setups on kubernetes-dynamo (sglang, vllm) will fail to deploy: this control plane has no NVIDIA Dynamo image configured for sglang or vllm (set INFERCRANE_DYNAMO_SGLANG_IMAGE_DIGEST, INFERCRANE_DYNAMO_SGLANG_RUNTIME_VERSION, INFERCRANE_DYNAMO_VLLM_IMAGE_DIGEST, and INFERCRANE_DYNAMO_VLLM_RUNTIME_VERSION)."}},
		"vLLM only": {configured: []string{"vllm"}, want: []string{"Setups on kubernetes-dynamo (sglang) will fail to deploy: this control plane has no NVIDIA Dynamo image configured for sglang (set INFERCRANE_DYNAMO_SGLANG_IMAGE_DIGEST and INFERCRANE_DYNAMO_SGLANG_RUNTIME_VERSION)."}},
		"both":      {configured: []string{"vllm", "sglang"}},
	} {
		t.Run(name, func(t *testing.T) {
			proposal, err := dynamoCatalogSource(t, test.configured...).Propose(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			adapters := adaptersOf(proposal)
			if len(adapters["kubernetes-dynamo"]) == 0 {
				t.Fatalf("the named adapter has no setup: %v", adapters)
			}
			if got := dynamoWarnings(proposal); !slices.Equal(got, test.want) {
				t.Fatalf("warnings=%q want %q", got, test.want)
			}
			if err = ValidateProposal(proposal); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// For provider kubernetes the control plane leaves out the Dynamo setups it
// cannot deploy and names the settings that would add them, instead of
// telling the caller to name an adapter it would refuse.
func TestAControlPlaneWithoutDynamoNamesTheSettingsForTheSetupsItLeftOut(t *testing.T) {
	request := Request{ModelIdentity: "acme/novel-open-model", ModelRevision: strings.Repeat("a", 40), Provider: "kubernetes", GPU: "H100", Objective: "throughput", IncludeSimulated: true, MaxCandidates: 10}
	proposal, err := dynamoCatalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if adapters := adaptersOf(proposal); len(adapters["kubernetes-dynamo"]) != 0 || len(adapters["kubernetes"]) == 0 {
		t.Fatalf("want plain Kubernetes setups only, got %v", adapters)
	}
	want := []string{"Setups on kubernetes-dynamo (sglang, vllm) were left out: they need NVIDIA Dynamo, and this control plane has no NVIDIA Dynamo image configured for sglang or vllm (set INFERCRANE_DYNAMO_SGLANG_IMAGE_DIGEST, INFERCRANE_DYNAMO_SGLANG_RUNTIME_VERSION, INFERCRANE_DYNAMO_VLLM_IMAGE_DIGEST, and INFERCRANE_DYNAMO_VLLM_RUNTIME_VERSION)."}
	if got := dynamoWarnings(proposal); !slices.Equal(got, want) {
		t.Fatalf("warnings=%q want %q", got, want)
	}
}

// A plan for an endpoint an InferCrane agent serves offers only setups the
// agent can start as they are named. It starts plain servers only, so Dynamo
// setups are left out with a warning, even on a control plane that runs
// Dynamo and when the request names the adapter; the same request for another
// endpoint may still use them.
func TestAnAgentEndpointsPlanLeavesOutSetupsThatAreNotPlainServers(t *testing.T) {
	body, err := os.ReadFile("testdata/diagnosis/kv-cache-pressure-overload.json")
	if err != nil {
		t.Fatal(err)
	}
	var diagnosis Diagnosis
	if err = json.Unmarshal(body, &diagnosis); err != nil {
		t.Fatal(err)
	}
	declared, err := DeclaredSetup{Engine: "vllm", Version: "0.34.0", GPU: "H100", GPUCount: 1}.StartingSetup("support-bot")
	if err != nil {
		t.Fatal(err)
	}
	request := Request{ModelIdentity: "Qwen/Qwen3.8-27B-FP8", ModelRevision: "c3a462cc3d38cf351f3b26cbd763469eeb74ef81", Provider: "kubernetes", GPU: "H100",
		Runtimes: []string{"vllm"}, Objective: "throughput", WorkloadProfile: "throughput", MaxCandidates: 6, IncludeSimulated: true,
		ModelFeatures: []string{FeatureNativeMTP}, ModelConfigDigest: recipeTestDigest, Diagnosis: &diagnosis}
	source := dynamoCatalogSource(t, "vllm")
	offers := func(request Request) bool {
		for _, entry := range matchingCompatibility(source.Integrations, request) {
			if entry.Adapter == "kubernetes-dynamo" {
				return true
			}
		}
		return false
	}
	if !offers(request) {
		t.Fatal("another endpoint's plan may use the Dynamo graph")
	}
	request.StartingSetup = &declared
	if offers(request) {
		t.Fatal("an agent endpoint's plan may use the Dynamo graph")
	}
	proposal, err := source.Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatal(err)
	}
	adapters := adaptersOf(proposal)
	if _, dynamo := adapters["kubernetes-dynamo"]; dynamo || len(adapters["kubernetes"]) < 2 {
		t.Fatalf("want plain servers only, got %v", adapters)
	}
	for _, candidate := range proposal.Candidates {
		if !candidate.Deployment.Serving.Empty() {
			t.Fatalf("a setup carries a serving graph: %+v", candidate.Deployment)
		}
	}
	const want = "Setups on kubernetes-dynamo (vllm) were left out: the endpoint's InferCrane agent starts plain servers only, and they need NVIDIA Dynamo."
	if warning := dynamoWarning(proposal); warning != want {
		t.Fatalf("warning %q; warnings=%v", warning, proposal.Warnings)
	}

	request.Provider = "kubernetes-dynamo"
	proposal, err = source.Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposal.Candidates) != 0 || dynamoWarning(proposal) != want {
		t.Fatalf("naming the adapter for an agent's endpoint: %v %v", adaptersOf(proposal), proposal.Warnings)
	}
}
