package optimizer

import (
	"context"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

// Every candidate of a proposal serves what the customer serves: the private
// model through the same credential reference and the same pinned adapter.
func TestProposalCarriesModelCredentialAndAdapterToEveryCandidate(t *testing.T) {
	adapter := domain.ModelAdapter{Name: "support-bot", Repository: "acme/support-lora", Revision: strings.Repeat("b", 40), Rank: 16}
	request := Request{ModelIdentity: "acme/private-model", ModelRevision: strings.Repeat("a", 40), Provider: "runpod-pods", GPU: "H100", Runtimes: []string{"vllm", "sglang"}, Objective: "throughput", IncludeSimulated: true, ModelSecretReferenceID: "secret-1", ModelAdapters: []domain.ModelAdapter{adapter}}
	proposal, err := catalogSource(t).Propose(context.Background(), request)
	if err != nil || len(proposal.Candidates) == 0 {
		t.Fatalf("proposal=%+v err=%v", proposal, err)
	}
	for _, candidate := range proposal.Candidates {
		draft := candidate.Deployment
		if draft.ModelSecretReferenceID != "secret-1" || len(draft.Model.Adapters) != 1 || draft.Model.Adapters[0] != adapter || draft.ServedModel() != "support-bot" {
			t.Fatalf("candidate %s does not serve the customer's model: %+v", candidate.ID, draft)
		}
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatal(err)
	}
	plain, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "acme/private-model", ModelRevision: strings.Repeat("a", 40), Provider: "runpod-pods", GPU: "H100", Runtimes: []string{"vllm", "sglang"}, Objective: "throughput", IncludeSimulated: true})
	if err != nil {
		t.Fatal(err)
	}
	for index := range plain.Candidates {
		if plain.Candidates[index].ID == proposal.Candidates[index].ID {
			t.Fatal("an adapter candidate shares its identity with a base-model candidate")
		}
		if plain.Candidates[index].Deployment.ServedModel() != "acme/private-model" {
			t.Fatal("a base-model candidate names an adapter")
		}
	}
}

func TestProposalAcceptsImportedWeightsPinnedByManifest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "s3://acme-models/run-42", ModelRevision: digest, Provider: "runpod-pods", GPU: "H100", Runtimes: []string{"vllm"}, Objective: "throughput", IncludeSimulated: true})
	if err != nil || len(proposal.Candidates) == 0 || proposal.Candidates[0].Deployment.Model.Revision != digest {
		t.Fatalf("proposal=%+v err=%v", proposal, err)
	}
	if _, err = catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "s3://acme-models/run-42", ModelRevision: "main", Provider: "runpod-pods", GPU: "H100", Objective: "throughput"}); err == nil {
		t.Fatal("imported weights without their manifest digest were accepted")
	}
}
