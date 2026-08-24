package optimizationcampaign

import (
	"context"
	"errors"
	"testing"

	"github.com/infercrane/infercrane/internal/domain"
)

type rankingStoreFixture struct {
	campaign   domain.OptimizationCampaign
	benchmarks []domain.BenchmarkResult
	recorded   map[string]domain.LabEvaluation
}

func (f *rankingStoreFixture) OptimizationCampaign(_ context.Context, tenant, id string) (domain.OptimizationCampaign, error) {
	if f.campaign.TenantID != tenant || f.campaign.ID != id {
		return domain.OptimizationCampaign{}, domain.ErrNotFound
	}
	return f.campaign, nil
}

func (f *rankingStoreFixture) BenchmarksForModel(_ context.Context, tenant, model string, _ int) ([]domain.BenchmarkResult, error) {
	if f.campaign.TenantID != tenant || f.campaign.ModelIdentity != model {
		return nil, domain.ErrNotFound
	}
	return append([]domain.BenchmarkResult(nil), f.benchmarks...), nil
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
