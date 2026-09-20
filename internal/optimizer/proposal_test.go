package optimizer

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/curatedrecipe"
	"github.com/infercrane/infercrane/internal/integration"
	"github.com/infercrane/infercrane/internal/servingcontract"
	"github.com/infercrane/infercrane/internal/workloadprofile"
)

func TestCatalogProposalCoversDistinctModelAndWorkloadFamilies(t *testing.T) {
	tests := []struct {
		name       string
		model      string
		capability string
	}{
		{name: "general instruction", model: "llama-3.1-8b-instruct", capability: "chat-completions"},
		{name: "reasoning", model: "deepseek-r1-distill-qwen-7b", capability: "chat-completions"},
		{name: "coding", model: "qwen2.5-coder-7b-instruct", capability: "chat-completions"},
		{name: "embeddings", model: "bge-m3-embeddings", capability: "embeddings"},
		{name: "multimodal", model: "qwen2.5-vl-7b-instruct", capability: "vision"},
		{name: "enterprise text", model: "granite-3.3-8b-instruct", capability: "chat-completions"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: test.model, Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive"})
			if err != nil || len(proposal.Candidates) == 0 {
				t.Fatalf("model family has no safe starting candidate: candidates=%d missing=%v err=%v", len(proposal.Candidates), proposal.Missing, err)
			}
			entry, ok := curatedrecipe.Get(test.model)
			if !ok || !contains(entry.Capabilities, test.capability) {
				t.Fatalf("model capability boundary missing: entry=%+v", entry)
			}
			for _, candidate := range proposal.Candidates {
				if candidate.EvidenceState != "unmeasured" || candidate.Deployment.Model.Revision == "" {
					t.Fatalf("model-specific candidate overclaims evidence: %+v", candidate)
				}
			}
		})
	}
}

func catalogSource(t *testing.T) CatalogSource {
	t.Helper()
	registry, err := integration.V1Catalog()
	if err != nil {
		t.Fatal(err)
	}
	return NewCatalogSource(curatedrecipe.All(), registry.Snapshot())
}

func TestCatalogProposalIsDeterministicUnmeasuredAndDeployable(t *testing.T) {
	request := Request{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive"}
	first, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.InputDigest != second.InputDigest || !reflect.DeepEqual(first.Candidates, second.Candidates) {
		t.Fatal("proposal is not deterministic")
	}
	if first.Mutation != "none" || len(first.Candidates) != 3 {
		t.Fatalf("proposal=%#v", first)
	}
	selected := first.Candidates[0]
	if selected.Status != "proposed-unmeasured" || selected.BenchmarkProfile != "interactive" || selected.Deployment.Provider.Adapter != "aws-ec2" || selected.Deployment.Runtime.Engine != "vllm" || selected.Deployment.Runtime.Version == "" || selected.Deployment.Model.Revision == "" {
		t.Fatalf("candidate=%#v", selected)
	}
	if first.Candidates[1].ConfigurationProfile != "vllm-balanced" || first.Candidates[2].ConfigurationProfile != "vllm-throughput" {
		t.Fatalf("unexpected alternative ordering: %#v", first.Candidates)
	}
	if first.StartingPoint == nil || first.StartingPoint.CandidateID != selected.ID || first.StartingPoint.EvidenceState != EvidenceUnmeasured || len(first.PostDeployWorkflow) != 5 {
		t.Fatalf("proposal did not connect baseline to monitored improvement: %+v", first)
	}
	encoded, _ := json.Marshal(first)
	for _, forbidden := range []string{`"qualified":true`, `"recommended":true`, `"estimated_throughput"`, `"estimated_cost"`} {
		if containsString(string(encoded), forbidden) {
			t.Fatalf("proposal fabricated evidence %q: %s", forbidden, encoded)
		}
	}
}

func TestCatalogProposalCarriesGeneralImmutableMultiGPUProfile(t *testing.T) {
	request := Request{ModelIdentity: "glm-5.3-flash", Provider: "kubernetes", GPU: "H200", Runtimes: []string{"custom-oci"}, Objective: "throughput", IncludeSimulated: true, MaxCandidates: 1}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || len(proposal.Candidates) != 1 {
		t.Fatalf("proposal=%+v err=%v", proposal, err)
	}
	candidate := proposal.Candidates[0]
	if candidate.Deployment.Resources.GPUCount != 8 || candidate.Deployment.Runtime.Workload.Image == "" || candidate.Deployment.Runtime.Version == "" || !hasFeature(candidate.Features, "immutable_runtime_artifact", "pinned") {
		t.Fatalf("portable serving profile was flattened: %+v", candidate)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatalf("portable serving proposal failed validation: %v", err)
	}
	request.GPU = "L40S"
	wrongGPU, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || len(wrongGPU.Candidates) != 0 || !reflect.DeepEqual(wrongGPU.Missing, []string{"reviewed_runtime_profile"}) {
		t.Fatalf("hardware-specific profile crossed its reviewed GPU boundary: %+v err=%v", wrongGPU, err)
	}
}

func TestCatalogProposalFailsClosedForUnknownOrUnqualifiedBoundary(t *testing.T) {
	unknown, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "unknown/model", Provider: "gcp", Region: "europe-west4", GPU: "nvidia-l4", Objective: "latency"})
	if err != nil || !reflect.DeepEqual(unknown.Missing, []string{"immutable_model_revision"}) || len(unknown.Candidates) != 0 {
		t.Fatalf("unknown=%#v err=%v", unknown, err)
	}
	deferred, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "mistral-7b-instruct", Provider: "gcp-mig", Region: "europe-west4", GPU: "nvidia-l4", Objective: "latency"})
	if err != nil || !reflect.DeepEqual(deferred.Missing, []string{"qualified_provider_runtime_compatibility"}) || len(deferred.Candidates) != 0 {
		t.Fatalf("deferred=%#v err=%v", deferred, err)
	}
}

func TestCatalogProposalCreatesConservativeGenericOpenWeightBaseline(t *testing.T) {
	revision := strings.Repeat("a", 40)
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "acme/novel-model", ModelRevision: revision, Provider: "runpod-pods", GPU: "H100", GPUCount: 2, Runtimes: []string{"vllm", "sglang"}, Objective: "throughput", IncludeSimulated: true})
	if err != nil || len(proposal.Candidates) != 2 {
		t.Fatalf("proposal=%+v err=%v", proposal, err)
	}
	for _, candidate := range proposal.Candidates {
		if candidate.Source.Name != "infercrane-generic-open-weight" || candidate.EvidenceState != EvidenceUnmeasured || candidate.Deployment.Model.ID != "acme/novel-model" || candidate.Deployment.Model.Revision != revision || candidate.Deployment.Resources.GPUCount != 2 || len(candidate.Deployment.Runtime.Args) != 0 || !hasFeature(candidate.Features, "runtime_baseline", "untuned") {
			t.Fatalf("generic baseline crossed its proof boundary: %+v", candidate)
		}
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatalf("generic proposal failed validation: %v", err)
	}
}

func TestCatalogProposalAcceptsPinnedIdentityAndRejectsMutableRevision(t *testing.T) {
	revision := strings.Repeat("b", 64)
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "acme/novel-model@" + revision, Provider: "aws", Region: "eu-central-1", GPU: "H100", Objective: "interactive"})
	if err != nil || len(proposal.Candidates) != 1 || proposal.Input.ModelRevision != revision || proposal.Candidates[0].Deployment.Model.ID != "acme/novel-model" {
		t.Fatalf("pinned identity was not normalized: proposal=%+v err=%v", proposal, err)
	}
	if _, err = catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "acme/novel-model", ModelRevision: "main", Provider: "aws", Region: "eu-central-1", GPU: "H100"}); err == nil {
		t.Fatal("mutable model revision was accepted")
	}
}

func TestCatalogProposalRequiresExplicitCloudBoundaryAndBoundsInputs(t *testing.T) {
	for _, request := range []Request{
		{ModelIdentity: "mistral-7b-instruct", GPU: "L40S"},
		{ModelIdentity: "mistral-7b-instruct", Provider: "aws", GPU: "L40S"},
		{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "fastest"},
		{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", MaxCandidates: 101},
		{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", MaxHourlyCost: number(math.Inf(1))},
	} {
		if _, err := catalogSource(t).Propose(context.Background(), request); err == nil {
			t.Fatalf("invalid request accepted: %#v", request)
		}
	}
}

func number(value float64) *float64 { return &value }

func TestSimulatedRuntimeRequiresExplicitOptIn(t *testing.T) {
	request := Request{ModelIdentity: "qwen3-8b", Provider: "gcp", Region: "europe-west4", GPU: "nvidia-l4", Runtimes: []string{"sglang"}, Objective: "throughput"}
	without, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || len(without.Candidates) != 0 {
		t.Fatalf("simulated candidate leaked into default output: %#v err=%v", without, err)
	}
	request.IncludeSimulated = true
	with, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || len(with.Candidates) != 1 || with.Candidates[0].CompatibilityState != "simulated" {
		t.Fatalf("explicit simulated candidate missing: %#v err=%v", with, err)
	}
}

func TestCatalogSourceCompilesDynamoAsOneExplicitMutationOwner(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "Qwen/Qwen3-8B", Provider: "kubernetes-dynamo", GPU: "NVIDIA-L40S", Runtimes: []string{"vllm"}, Objective: "interactive", IncludeSimulated: true, MaxCandidates: 1})
	if err != nil || len(proposal.Candidates) != 1 {
		t.Fatalf("proposal=%+v err=%v", proposal, err)
	}
	candidate := proposal.Candidates[0]
	if candidate.Deployment.Provider.Adapter != "kubernetes-dynamo" || candidate.Deployment.Scaling.MinReplicas != 1 || candidate.Deployment.Scaling.MaxReplicas != 1 || candidate.Deployment.Serving.Backend != servingcontract.BackendDynamo || candidate.Deployment.Serving.Mode != servingcontract.ModeAggregated || candidate.Deployment.Serving.Worker.Replicas != 1 {
		t.Fatalf("Dynamo candidate has conflicting or implicit ownership: %+v", candidate.Deployment)
	}
	if candidate.Deployment.Resources.GPU != "NVIDIA-L40S" || !hasFeature(candidate.Features, "dynamo_graph", "single-mutation-owner") {
		t.Fatalf("Dynamo candidate lost explicit hardware or ownership evidence: %+v", candidate)
	}
}

func hasFeature(features []Feature, name, state string) bool {
	for _, feature := range features {
		if feature.Name == name && feature.State == state {
			return true
		}
	}
	return false
}

func TestCatalogProposalLinksRuntimeArgumentsToExactCapabilityEvidence(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "Qwen/Qwen3-8B", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive"})
	if err != nil {
		t.Fatal(err)
	}
	foundArguments := false
	for _, candidate := range proposal.Candidates {
		foundArguments = foundArguments || len(candidate.Deployment.Runtime.Args) > 0
		for _, feature := range candidate.Features {
			if feature.Name != "continuous_batching" && !strings.Contains(feature.Source, "vllm-0.22.1-") {
				t.Fatalf("runtime feature lacks exact capability descriptor: %+v", feature)
			}
		}
	}
	if !foundArguments {
		t.Fatal("expected a reviewed candidate with compiled runtime arguments")
	}
}

func TestValidateProposalRejectsTamperingAndQualifiedClaims(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "mistral-7b-instruct", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive", MaxCandidates: 3})
	if err != nil || ValidateProposal(proposal) != nil {
		t.Fatalf("valid proposal rejected: proposal=%+v err=%v validation=%v", proposal, err, ValidateProposal(proposal))
	}
	proposal.Input.GPU = "H100"
	if err = ValidateProposal(proposal); err == nil {
		t.Fatal("tampered input digest was accepted")
	}
	proposal.Input.GPU = "L40S"
	proposal.Candidates[0].EvidenceState = EvidenceQualified
	if err = ValidateProposal(proposal); err == nil {
		t.Fatal("unproven qualified candidate was accepted")
	}
}

func TestCatalogProposalCarriesFullMeasuredSLOBoundary(t *testing.T) {
	errorRate, goodput := 0.01, 4.0
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "qwen3-8b", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive", MaxErrorRate: &errorRate, MinGoodput: &goodput})
	if err != nil || len(proposal.Candidates) == 0 {
		t.Fatalf("proposal=%+v err=%v", proposal, err)
	}
	if !contains(proposal.Candidates[0].RequiredEvidence, "measured SLO metrics") {
		t.Fatalf("candidate lost measured SLO boundary: %+v", proposal.Candidates[0].RequiredEvidence)
	}
	invalid := 1.01
	if _, err = catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "qwen3-8b", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive", MaxErrorRate: &invalid}); err == nil {
		t.Fatal("invalid error-rate boundary was accepted")
	}
}

func TestPublicWorkloadPriorIsBoundAndCannotQualifyProduction(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), Request{
		ModelIdentity: "qwen3-8b", Provider: "aws", Region: "eu-central-1", GPU: "L40S",
		Objective: "interactive", WorkloadProfile: "public-interactive", MaxCandidates: 1,
	})
	if err != nil || len(proposal.Candidates) != 1 {
		t.Fatalf("proposal=%+v err=%v", proposal, err)
	}
	if proposal.Input.WorkloadSource != WorkloadSourcePublicPrior || proposal.Input.WorkloadFingerprint != workloadprofile.PublicPriorDigest {
		t.Fatalf("public prior identity was not bound: %+v", proposal.Input)
	}
	if len(proposal.Warnings) == 0 || !strings.Contains(proposal.Warnings[0], "screening evidence") || !strings.Contains(proposal.SelectionBoundary, "cannot qualify") {
		t.Fatalf("public prior boundary missing: warnings=%v boundary=%q", proposal.Warnings, proposal.SelectionBoundary)
	}
	candidate := proposal.Candidates[0]
	if !contains(candidate.RequiredEvidence, "representative customer replay replacing the public workload prior") || !strings.Contains(strings.Join(candidate.Limitations, " "), "cannot qualify") {
		t.Fatalf("customer replay gate missing: %+v", candidate)
	}
	if len(proposal.PostDeployWorkflow) != 5 || !strings.Contains(proposal.PostDeployWorkflow[0], "replace the public prior") {
		t.Fatalf("upgrade workflow missing: %v", proposal.PostDeployWorkflow)
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatalf("valid public-prior proposal rejected: %v", err)
	}
}

func TestPublicWorkloadPriorRejectsMismatchedIdentity(t *testing.T) {
	_, err := catalogSource(t).Propose(context.Background(), Request{
		ModelIdentity: "qwen3-8b", Provider: "aws", Region: "eu-central-1", GPU: "L40S",
		WorkloadProfile: "public-interactive", WorkloadSource: WorkloadSourcePublicPrior,
		WorkloadFingerprint: "sha256:" + strings.Repeat("0", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("mismatched public prior identity accepted: %v", err)
	}
	_, err = catalogSource(t).Propose(context.Background(), Request{
		ModelIdentity: "qwen3-8b", Provider: "aws", Region: "eu-central-1", GPU: "L40S",
		WorkloadProfile: "public-interactive", WorkloadSource: WorkloadSourceCustomerObserved,
		WorkloadFingerprint: "sha256:" + strings.Repeat("1", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "cannot use a public workload profile") {
		t.Fatalf("public workload profile was laundered as observed evidence: %v", err)
	}
}

func containsString(value, target string) bool {
	return len(target) == 0 || len(value) >= len(target) && stringIndex(value, target) >= 0
}

func stringIndex(value, target string) int {
	for i := 0; i+len(target) <= len(value); i++ {
		if value[i:i+len(target)] == target {
			return i
		}
	}
	return -1
}
