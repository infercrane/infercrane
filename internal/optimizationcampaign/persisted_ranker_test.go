package optimizationcampaign

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
)

type rankingStoreFixture struct {
	campaign       domain.OptimizationCampaign
	benchmarks     []domain.BenchmarkResult
	recorded       map[string]domain.LabEvaluation
	benchmarkModel string
}

func (f *rankingStoreFixture) OptimizationCampaign(_ context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	if f.campaign.TenantID != tenant || f.campaign.ID != id {
		return domain.OptimizationCampaign{}, domain.ErrNotFound
	}
	return f.campaign, nil
}

func (f *rankingStoreFixture) BenchmarksForModel(_ context.Context, tenant, model string, _ int) ([]domain.BenchmarkResult, error) {
	expected := f.benchmarkModel
	if expected == "" {
		expected = f.campaign.ModelIdentity
	}
	if f.campaign.TenantID != tenant || expected != model {
		return nil, domain.ErrNotFound
	}
	return append([]domain.BenchmarkResult(nil), f.benchmarks...), nil
}

func TestPersistedRankerQueriesCanonicalIdentityForLegacyCampaign(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	var proposal optimizer.Proposal
	if err := json.Unmarshal([]byte(campaign.ProposalJSON), &proposal); err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("d", 40)
	proposal.Input.ModelRevision = revision
	inputJSON, _ := json.Marshal(proposal.Input)
	digest := sha256.Sum256(inputJSON)
	proposal.InputDigest = fmt.Sprintf("%x", digest)
	encoded, _ := json.Marshal(proposal)
	campaign.InputDigest = proposal.InputDigest
	campaign.ProposalJSON = string(encoded)
	pinned := campaign.ModelIdentity + "@" + revision
	for index := range benchmarks {
		benchmarks[index].ModelIdentity = pinned
	}
	store := &rankingStoreFixture{campaign: campaign, benchmarks: benchmarks, benchmarkModel: pinned}
	result, err := (PersistedRanker{Store: store}).Rank(t.Context(), campaign.Candidates[1])
	if err != nil || result.Decision != RankSelect {
		t.Fatalf("legacy campaign did not query pinned evidence identity: result=%+v err=%v", result, err)
	}
}

func (f *rankingStoreFixture) RecordLabEvaluation(_ context.Context, tenant string, value domain.LabEvaluation) (domain.LabEvaluation, error) {
	if tenant != f.campaign.TenantID {
		return domain.LabEvaluation{}, domain.ErrNotFound
	}
	if f.recorded == nil {
		f.recorded = map[string]domain.LabEvaluation{}
	}
	if existing, ok := f.recorded[value.ID]; ok {
		if existing.InputDigest != value.InputDigest || existing.ResultsJSON != value.ResultsJSON {
			return existing, domain.ErrConflict
		}
		return existing, nil
	}
	value.TenantID = tenant
	f.recorded[value.ID] = value
	return value, nil
}

func TestPersistedRankerIsContentAddressedAndReplaySafe(t *testing.T) {
	campaign, benchmarks := rankingFixture(t, "interactive", nil)
	store := &rankingStoreFixture{campaign: campaign, benchmarks: benchmarks}
	ranker := PersistedRanker{Store: store}
	first, err := ranker.Rank(t.Context(), campaign.Candidates[1])
	if err != nil {
		t.Fatal(err)
	}
	second, err := ranker.Rank(t.Context(), campaign.Candidates[1])
	if err != nil {
		t.Fatal(err)
	}
	if first.Decision != RankSelect || first.LabEvaluationID == "" || first != second || len(store.recorded) != 1 {
		t.Fatalf("ranking replay was not stable: first=%+v second=%+v rows=%d", first, second, len(store.recorded))
	}
}

func TestPersistedRankerRejectsMissingIdentity(t *testing.T) {
	_, err := (PersistedRanker{}).Rank(t.Context(), domain.OptimizationCandidateRun{})
	if err == nil || errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("invalid ranker identity was not rejected locally: %v", err)
	}
}
