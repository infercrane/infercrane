package optimizationcampaign

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
)

type RankingStore interface {
	OptimizationCampaign(context.Context, string, string) (domain.OptimizationCampaign, error)
	BenchmarksForModel(context.Context, string, string, int) ([]domain.BenchmarkResult, error)
	RecordLabEvaluation(context.Context, string, domain.LabEvaluation) (domain.LabEvaluation, error)
}

// PersistedRanker creates one content-addressed Lab evaluation for the current
// bounded campaign evidence. It is safe to call again after a lost response.
type PersistedRanker struct {
	Store RankingStore
}

func (r PersistedRanker) Rank(ctx context.Context, candidate domain.OptimizationCandidateRun) (RankingResult, error) {
	if r.Store == nil || candidate.TenantID == "" || candidate.CampaignID == "" || candidate.ID == "" {
		return RankingResult{}, errors.New("persisted ranker requires store and candidate identity")
	}
	campaign, err := r.Store.OptimizationCampaign(ctx, candidate.TenantID, candidate.CampaignID)
	if err != nil {
		return RankingResult{}, err
	}
	var proposal optimizer.Proposal
	if err = json.Unmarshal([]byte(campaign.ProposalJSON), &proposal); err != nil {
		return RankingResult{}, fmt.Errorf("decode immutable campaign proposal: %w", err)
	}
	lineage, err := optimizer.ProposalLineage(proposal)
	if err != nil {
		return RankingResult{}, err
	}
	// Benchmarks are stored by the artifact each candidate served, so a
	// campaign with a derived artifact reads the base model's rows and each
	// derived artifact's rows.
	var benchmarks []domain.BenchmarkResult
	seen := map[string]struct{}{}
	for _, identity := range append([]string{lineage.Base}, lineage.Derived()...) {
		rows, readErr := r.Store.BenchmarksForModel(ctx, candidate.TenantID, identity, 500)
		if readErr != nil {
			return RankingResult{}, readErr
		}
		for _, row := range rows {
			if _, duplicate := seen[row.ID]; !duplicate {
				seen[row.ID] = struct{}{}
				benchmarks = append(benchmarks, row)
			}
		}
	}
	ranking, err := RankMeasuredCampaign(campaign, benchmarks)
	if err != nil {
		return RankingResult{}, err
	}
	digest := sha256.Sum256([]byte(candidate.TenantID + "\x00" + campaign.ID + "\x00" + ranking.Evaluation.InputDigest + "\x00" + ranking.Evaluation.ResultsJSON))
	ranking.Evaluation.ID = "optlab-" + hex.EncodeToString(digest[:])
	persisted, err := r.Store.RecordLabEvaluation(ctx, candidate.TenantID, ranking.Evaluation)
	if err != nil {
		return RankingResult{}, err
	}
	decision := ranking.Decisions[candidate.ID]
	if decision == "" {
		return RankingResult{}, errors.New("persisted Lab evaluation omitted the requested candidate")
	}
	failureCode := ""
	switch decision {
	case RankSupersede:
		failureCode = "measured_candidate_superseded"
		if override := ranking.FailureCodes[candidate.ID]; override != "" {
			failureCode = override
		}
	case RankReject:
		failureCode = "measured_candidate_outside_constraints"
	case RankInconclusive:
		failureCode = "measured_ranking_inconclusive"
	}
	return RankingResult{LabEvaluationID: persisted.ID, Decision: decision, FailureCode: failureCode}, nil
}
