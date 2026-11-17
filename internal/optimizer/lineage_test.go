package optimizer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// checkpointProposal returns a catalog proposal on B200 that holds the
// unmodified base model and at least one publisher checkpoint built from it.
func checkpointProposal(t *testing.T, model, revision string) Proposal {
	t.Helper()
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: model, ModelRevision: revision, Provider: "runpod-pods", GPU: "B200", Runtimes: []string{"vllm"}, Objective: "throughput", MaxCandidates: 8})
	if err != nil {
		t.Fatal(err)
	}
	seeded := false
	for _, candidate := range proposal.Candidates {
		seeded = seeded || candidate.ArtifactSeed != nil
	}
	if !seeded || proposal.StartingPoint == nil {
		t.Fatalf("catalog no longer proposes a publisher checkpoint for %s@%s: %+v", model, revision, proposal.Candidates)
	}
	return proposal
}

func seededIndex(t *testing.T, proposal Proposal) int {
	t.Helper()
	for index, candidate := range proposal.Candidates {
		if candidate.ArtifactSeed != nil {
			return index
		}
	}
	t.Fatal("proposal has no publisher checkpoint")
	return -1
}

func TestProposalLineageComparesCheckpointsWithTheirBaseModel(t *testing.T) {
	for _, test := range []struct {
		name, model, revision, lineage string
	}{
		{"exact", "deepseek-ai/DeepSeek-V4.1-Flash", "dba1be0a40aa45a94ad051997016db3960a90277", LineageExact},
		{"publisher declared", "Qwen/Qwen3.8-27B", strings.Repeat("a", 40), LineagePublisherDeclared},
	} {
		t.Run(test.name, func(t *testing.T) {
			proposal := checkpointProposal(t, test.model, test.revision)
			lineage, err := ProposalLineage(proposal)
			if err != nil {
				t.Fatal(err)
			}
			if lineage.Base != test.model+"@"+test.revision {
				t.Fatalf("base model = %q, want the requested model", lineage.Base)
			}
			var derived []string
			for _, candidate := range proposal.Candidates {
				artifact := lineage.Candidates[candidate.ID]
				identity := candidate.Deployment.Model.ID + "@" + candidate.Deployment.Model.Revision
				switch {
				case artifact.Identity != identity:
					t.Fatalf("candidate %s artifact = %q, want %q", candidate.ID, artifact.Identity, identity)
				case candidate.ArtifactSeed == nil && artifact.Lineage != LineageBase:
					t.Fatalf("unmodified candidate labelled %q", artifact.Lineage)
				case candidate.ArtifactSeed != nil && artifact.Lineage != test.lineage:
					t.Fatalf("checkpoint %s labelled %q, want %q", identity, artifact.Lineage, test.lineage)
				case candidate.ArtifactSeed != nil:
					derived = append(derived, identity)
				}
			}
			if got := lineage.Derived(); strings.Join(got, ",") != strings.Join(derived, ",") || len(got) == 0 {
				t.Fatalf("derived artifacts = %v, want %v", got, derived)
			}
		})
	}
}

func TestProposalLineageKeepsSingleArtifactCampaignsUnchanged(t *testing.T) {
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: "qwen3-8b", Provider: "aws", Region: "eu-central-1", GPU: "L40S", Objective: "interactive"})
	if err != nil {
		t.Fatal(err)
	}
	lineage, err := ProposalLineage(proposal)
	if err != nil {
		t.Fatal(err)
	}
	if len(lineage.Derived()) != 0 || lineage.Base != proposal.Candidates[0].Deployment.Model.ID+"@"+proposal.Candidates[0].Deployment.Model.Revision {
		t.Fatalf("a single-artifact proposal gained derived artifacts: %+v", lineage)
	}
}

func TestProposalLineageRefusesADifferentBaseModel(t *testing.T) {
	const model, revision = "deepseek-ai/DeepSeek-V4.1-Flash", "dba1be0a40aa45a94ad051997016db3960a90277"
	for _, test := range []struct {
		name string
		edit func(*Proposal, int)
		want error
	}{
		{"checkpoint of another repository", func(p *Proposal, i int) { p.Candidates[i].ArtifactSeed.BaseRepository = "Qwen/Qwen3.8-27B" }, ErrDifferentBaseModel},
		{"exact checkpoint of another revision", func(p *Proposal, i int) { p.Candidates[i].ArtifactSeed.BaseRevision = strings.Repeat("b", 40) }, ErrDifferentBaseModel},
		{"publisher-declared checkpoint that names another revision", func(p *Proposal, i int) {
			p.Candidates[i].ArtifactSeed.LineageState = LineagePublisherDeclared
			p.Candidates[i].ArtifactSeed.BaseRevision = strings.Repeat("b", 40)
		}, ErrDifferentBaseModel},
		{"unmodified candidate of another model", func(p *Proposal, i int) {
			p.Candidates[i].ArtifactSeed = nil
			p.Candidates[i].Deployment.Model.ID = "Qwen/Qwen3.8-27B"
		}, ErrDifferentBaseModel},
		{"no unmodified base model", func(p *Proposal, i int) {
			second, seed := p.Candidates[i], *p.Candidates[i].ArtifactSeed
			second.ID, second.ArtifactSeed = strings.Repeat("c", 64), &seed
			second.Deployment.Model.Revision = strings.Repeat("d", 40)
			seed.OutputRevision = second.Deployment.Model.Revision
			p.Candidates = []Candidate{p.Candidates[i], second}
			p.Candidates[0].Rank, p.Candidates[1].Rank = 1, 2
			p.StartingPoint.CandidateID = p.Candidates[0].ID
		}, ErrDifferentBaseModel},
		{"checkpoint as the control", func(p *Proposal, i int) { p.StartingPoint.CandidateID = p.Candidates[i].ID }, ErrDerivedControl},
	} {
		t.Run(test.name, func(t *testing.T) {
			proposal := checkpointProposal(t, model, revision)
			test.edit(&proposal, seededIndex(t, proposal))
			if _, err := ProposalLineage(proposal); !errors.Is(err, test.want) {
				t.Fatalf("lineage error = %v, want %v", err, test.want)
			}
			if err := ValidateProposal(proposal); !errors.Is(err, test.want) {
				t.Fatalf("a campaign could be created from it: %v", err)
			}
		})
	}
}

// A pinned base revision that differs from the reviewed recipe's is the base
// model of every candidate, including the baseline the checkpoint expansion
// adds; the proposal never mixes two revisions of one model.
func TestCatalogProposalKeepsThePinnedRevisionForCheckpointBaselines(t *testing.T) {
	const model, pinned = "zai-org/GLM-5.3-Flash", "46a050bec13002ad29ee795c16e61cf4fa3c4a9c"
	recipe, found := findRecipe(catalogSource(t).Recipes, model)
	if !found || recipe.Revision == pinned {
		t.Fatalf("fixture no longer pins a revision other than the recipe's: %+v", recipe)
	}
	proposal, err := catalogSource(t).Propose(context.Background(), Request{ModelIdentity: model, ModelRevision: pinned, Provider: "runpod-pods", GPU: "B200", Objective: "throughput", MaxCandidates: 8})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range proposal.Candidates {
		if candidate.ArtifactSeed == nil && candidate.Deployment.Model.Revision != pinned {
			t.Fatalf("baseline %s serves revision %s, not the pinned %s", candidate.ID, candidate.Deployment.Model.Revision, pinned)
		}
	}
	if err = ValidateProposal(proposal); err != nil {
		t.Fatal(err)
	}
}
