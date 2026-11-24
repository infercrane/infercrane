package optimizer

import (
	"context"
	"strings"
	"testing"
)

// The seven built-in load profiles. Before the adapter order, the content hash of
// the two generic Kubernetes candidates decided which came first, and the hash
// includes the profile: four of these seven started from the Dynamo serving
// graph, where the plan refuses every engine-flag technique.
var builtInLoadProfiles = []string{"interactive", "throughput", "long-context", "long-generation", "overload", "buffered", "balanced"}

// Two models outside the reviewed catalog, so both plan from generic setups. The
// second is the model and revision of the 2026-11-23 run that found the fault.
var startingSetupModels = map[string]string{
	"acme/novel-open-model": strings.Repeat("a", 40),
	"Qwen/Qwen3.8-27B-FP8":  "c3a462cc3d38cf351f3b26cbd763469eeb74ef81",
}

func kubernetesRequest(provider, profile string, diagnosis *Diagnosis) Request {
	return kubernetesModelRequest("acme/novel-open-model", provider, profile, diagnosis)
}

func kubernetesModelRequest(model, provider, profile string, diagnosis *Diagnosis) Request {
	return Request{ModelIdentity: model, ModelRevision: startingSetupModels[model], Provider: provider, GPU: "H100", Runtimes: []string{"vllm"},
		Objective: "throughput", WorkloadProfile: profile, IncludeSimulated: true, MaxCandidates: 6, Diagnosis: diagnosis}
}

func TestStartingSetupIsOnTheAdapterTheRequestNamesForEveryLoadProfile(t *testing.T) {
	for model := range startingSetupModels {
		for _, profile := range builtInLoadProfiles {
			t.Run(model+"/"+profile, func(t *testing.T) {
				proposal, err := catalogSource(t).Propose(context.Background(), kubernetesModelRequest(model, "kubernetes", profile, nil))
				if err != nil {
					t.Fatal(err)
				}
				adapters := map[string]bool{}
				for _, candidate := range proposal.Candidates {
					adapters[candidate.Deployment.Provider.Adapter] = true
				}
				if !adapters["kubernetes"] || !adapters["kubernetes-dynamo"] {
					t.Fatalf("the case needs both Kubernetes adapters as candidates, got %v", adapters)
				}
				first := proposal.Candidates[0]
				if first.Deployment.Provider.Adapter != "kubernetes" || !first.Deployment.Serving.Empty() {
					t.Fatalf("provider kubernetes must start from the plain server, got adapter %q (serving graph: %t)", first.Deployment.Provider.Adapter, !first.Deployment.Serving.Empty())
				}
			})
		}
	}
}

func TestDiagnosisBuildsSetupsFromThePlainServerForEveryLoadProfile(t *testing.T) {
	for _, profile := range builtInLoadProfiles {
		t.Run(profile, func(t *testing.T) {
			proposal, err := catalogSource(t).Propose(context.Background(), kubernetesRequest("kubernetes", profile, preemptionDiagnosis()))
			if err != nil {
				t.Fatal(err)
			}
			if proposal.Diagnosis == nil || len(proposal.Diagnosis.Techniques) == 0 {
				t.Fatalf("no diagnosis plan: %+v", proposal.Diagnosis)
			}
			for _, planned := range proposal.Diagnosis.Techniques {
				if strings.Contains(planned.Skipped, "Dynamo serving graph") {
					t.Fatalf("%s was refused for a serving graph nobody asked for: %s", planned.Technique, planned.Skipped)
				}
			}
			if got := strings.Join(reasonOrder(proposal), ","); !strings.HasPrefix(got, "control,fp8-kv-cache") {
				t.Fatalf("the plan must start from the plain server and add the FP8 KV cache setup first, got %s", got)
			}
			if adapter := proposal.Candidates[1].Deployment.Provider.Adapter; adapter != "kubernetes" {
				t.Fatalf("the derived setup must stay on the starting setup's adapter, got %q", adapter)
			}
		})
	}
}

func TestANamedDynamoAdapterStillStartsFromItsServingGraph(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), kubernetesRequest("kubernetes-dynamo", "overload", preemptionDiagnosis()))
	if err != nil {
		t.Fatal(err)
	}
	first := proposal.Candidates[0]
	if first.Deployment.Provider.Adapter != "kubernetes-dynamo" || first.Deployment.Serving.Empty() {
		t.Fatalf("provider kubernetes-dynamo must start from its serving graph, got %+v", first.Deployment.Provider)
	}
	if skipped := plannedTechnique(t, proposal, TechniqueFP8KVCache).Skipped; !strings.Contains(skipped, "Dynamo serving graph") {
		t.Fatalf("an engine flag is not planned on a serving graph; skipped=%q", skipped)
	}
}

// A provider that names only a cloud has no adapter to prefer. The starting
// setup must still be the same adapter for every load profile, not whichever
// hash sorts first.
func TestAProviderCloudStartsFromOneAdapterForEveryLoadProfile(t *testing.T) {
	for model := range startingSetupModels {
		t.Run(model, func(t *testing.T) {
			for _, profile := range builtInLoadProfiles {
				proposal, err := catalogSource(t).Propose(context.Background(), kubernetesModelRequest(model, "runpod", profile, nil))
				if err != nil {
					t.Fatal(err)
				}
				adapters := map[string]bool{}
				for _, candidate := range proposal.Candidates {
					adapters[candidate.Deployment.Provider.Adapter] = true
				}
				if len(adapters) < 2 {
					t.Fatalf("the case needs several RunPod adapters as candidates, got %v", adapters)
				}
				if first := proposal.Candidates[0].Deployment.Provider.Adapter; first != "runpod-pods" {
					t.Fatalf("%s: provider runpod must start from the first adapter in name order, got %q", profile, first)
				}
			}
		})
	}
}
