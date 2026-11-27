package controlapi

import (
	"encoding/json"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/kerneltuning"
	"github.com/infercrane/infercrane/internal/optimizationreport"
	"github.com/infercrane/infercrane/internal/optimizer"
	"github.com/infercrane/infercrane/internal/technique"
)

// A campaign's candidate made by a contract technique carries the technique
// block the dashboard reads; a candidate no technique made carries none.
func TestACampaignCandidateCarriesItsTechnique(t *testing.T) {
	proposal := optimizer.Proposal{Candidates: []optimizer.Candidate{{ID: "proposal-control"}, {ID: "proposal-kernels",
		Technique: &optimizer.CandidateTechnique{ID: kerneltuning.TechniqueID, Version: kerneltuning.TechniqueVersion}}}}
	encoded, _ := json.Marshal(proposal)
	campaign := domain.OptimizationCampaign{ID: "campaign-1", ProposalJSON: string(encoded), Candidates: []domain.OptimizationCandidateRun{
		{ID: "control", ProposalCandidateID: "proposal-control", Rank: 1, DeploymentSpecJSON: `{}`, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
		{ID: "kernels", ProposalCandidateID: "proposal-kernels", Rank: 2, DeploymentSpecJSON: `{}`, PredictedEvidenceJSON: `{}`, ActualEvidenceJSON: `{}`},
	}}
	response := optimizationCampaignResponse(campaign, campaignQualityInputs{})
	candidates := response["candidates"].([]map[string]any)
	if _, has := candidates[0]["technique"]; has {
		t.Fatal("the control shows a technique")
	}
	view, ok := candidates[1]["technique"].(*optimizationreport.TechniqueView)
	if !ok || view.ID != kerneltuning.TechniqueID || view.Claim != technique.ClaimNotShown || len(view.Unknown) == 0 {
		t.Fatalf("technique = %#v", candidates[1]["technique"])
	}
	body, _ := json.Marshal(candidates[1])
	var decoded struct {
		Technique struct {
			ID      string   `json:"id"`
			Claim   string   `json:"claim"`
			Unknown []string `json:"unknown"`
		} `json:"technique"`
	}
	if json.Unmarshal(body, &decoded) != nil || decoded.Technique.ID != kerneltuning.TechniqueID || decoded.Technique.Claim != "not shown" {
		t.Fatalf("encoded = %s", body)
	}
}
