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
	encoded, _ := json.Marshal(first)
	for _, forbidden := range []string{`"qualified":true`, `"recommended":true`, `"estimated_throughput"`, `"estimated_cost"`} {
		if containsString(string(encoded), forbidden) {
			t.Fatalf("proposal fabricated evidence %q: %s", forbidden, encoded)
		}
	}
}

func TestCatalogProposalFailsClosedForUnknownOrUnqualifiedBoundary(t *testing.T) {
	unknown, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "unknown/model", Provider: "gcp", Region: "europe-west4", GPU: "nvidia-l4", Objective: "latency"})
	if err != nil || !reflect.DeepEqual(unknown.Missing, []string{"reviewed_model_recipe"}) || len(unknown.Candidates) != 0 {
		t.Fatalf("unknown=%#v err=%v", unknown, err)
	}
	deferred, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "mistral-7b-instruct", Provider: "gcp-mig", Region: "europe-west4", GPU: "nvidia-l4", Objective: "latency"})
	if err != nil || !reflect.DeepEqual(deferred.Missing, []string{"qualified_provider_runtime_compatibility"}) || len(deferred.Candidates) != 0 {
		t.Fatalf("deferred=%#v err=%v", deferred, err)
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

func TestCatalogProposalLinksRuntimeArgumentsToExactCapabilityEvidence(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "Qwen/Qwen3-8B", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive"})
	if err != nil {
		t.Fatal(err)
	}
	foundArguments := false
	for _, candidate := range proposal.Candidates {
		foundArguments = foundArguments || len(candidate.Deployment.Runtime.Args) > 0
		for _, feature := range candidate.Features {
			if feature.Name != "continuous_batching" && !strings.Contains(feature.Source, "vllm-0.22.0-") {
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
