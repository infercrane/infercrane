package controlapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/continualoptimizer"
	"github.com/infercrane/infercrane/internal/domain"
)

// A run Stay optimized proposes for an endpoint that serves a private model
// and a LoRA adapter keeps both on every setup.
func TestStayOptimizedProposalKeepsModelCredentialAndAdapter(t *testing.T) {
	store := newStayOptimizedFake(t, continualoptimizer.DefaultPolicy())
	adapter := domain.ModelAdapter{Name: "support-bot", Repository: "acme/support-lora", Revision: strings.Repeat("c", 40), Rank: 16}
	spec, err := json.Marshal(domain.DeploymentRevisionSpec{Model: "qwen3-8b", ModelSecretReferenceID: "secret-1", ModelAdapters: []domain.ModelAdapter{adapter}, Runtime: "vllm", RuntimeVersion: "0.21.0", Cloud: "aws", Region: "eu-central-1", GPU: "L40S", GPUCount: 1, MinReplicas: 1, MaxReplicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	store.revisions[0].SpecJSON = string(spec)
	loop := StayOptimized{API: stayOptimizedAPI(t, store), Owner: "instance-a"}
	if _, err = loop.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var proposal struct {
		Candidates []struct {
			Deployment struct {
				ModelSecretReferenceID string `json:"model_secret_reference_id"`
				Model                  struct {
					Adapters []domain.ModelAdapter `json:"adapters"`
				} `json:"model"`
			} `json:"deployment"`
		} `json:"candidates"`
	}
	if err = json.Unmarshal([]byte(store.campaign.ProposalJSON), &proposal); err != nil || len(proposal.Candidates) == 0 {
		t.Fatalf("no proposal: %v %s", err, store.campaign.ProposalJSON)
	}
	for _, candidate := range proposal.Candidates {
		if candidate.Deployment.ModelSecretReferenceID != "secret-1" || len(candidate.Deployment.Model.Adapters) != 1 || candidate.Deployment.Model.Adapters[0] != adapter {
			t.Fatalf("a proposed setup does not serve the endpoint's model: %+v", candidate.Deployment)
		}
	}
}
