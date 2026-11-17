package optimizationreport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/optimizer"
)

// Store is the tenant-scoped, read-only evidence boundary of a report. Every
// method filters by tenant, so a report never reads another tenant's records.
// Evidence is read by the IDs the candidate runs recorded, never by deployment
// name: a campaign deletes its candidate deployments at cleanup, and a deleted
// deployment's name can be deployed again by something else.
type Store interface {
	OptimizationCampaign(context.Context, string, string) (domain.OptimizationCampaign, error)
	BenchmarksByIDs(context.Context, string, []string) (map[string]domain.BenchmarkResult, error)
	QualityEvidenceByIDs(context.Context, string, []string) (map[string]domain.QualityEvidence, error)
	ReleaseGuardEvaluationsByIDs(context.Context, string, []string) (map[string]domain.ReleaseGuardEvaluation, error)
	ReplayTraceByDigest(context.Context, string, string) (domain.ReplayTrace, error)
}

// Load reads one campaign and the evidence its candidates reference, then
// assembles the report. domain.ErrNotFound means the campaign does not exist
// for this tenant. A missing evidence record is not an error: the field that
// needs it stays unknown.
func Load(ctx context.Context, store Store, tenant, campaignID string) (Report, error) {
	campaign, err := store.OptimizationCampaign(ctx, tenant, campaignID)
	if err != nil {
		return Report{}, err
	}
	var benchmarkIDs, qualityIDs, guardIDs []string
	for _, run := range campaign.Candidates {
		benchmarkIDs = append(benchmarkIDs, run.BenchmarkID)
		qualityIDs = append(qualityIDs, run.QualityEvidenceID)
		guardIDs = append(guardIDs, run.ReleaseGuardEvaluationID)
	}
	evidence := Evidence{Campaign: campaign}
	if evidence.Benchmarks, err = store.BenchmarksByIDs(ctx, tenant, benchmarkIDs); err != nil {
		return Report{}, fmt.Errorf("read campaign benchmarks: %w", err)
	}
	if evidence.Quality, err = store.QualityEvidenceByIDs(ctx, tenant, qualityIDs); err != nil {
		return Report{}, fmt.Errorf("read campaign quality evidence: %w", err)
	}
	if evidence.ReleaseGuard, err = store.ReleaseGuardEvaluationsByIDs(ctx, tenant, guardIDs); err != nil {
		return Report{}, fmt.Errorf("read campaign Release Guard evidence: %w", err)
	}
	if digest, bound := replayDigest(campaign); bound {
		if digest == "" {
			evidence.ReplayReason = "the campaign names no valid replayed-traffic digest"
		} else {
			trace, lookupErr := store.ReplayTraceByDigest(ctx, tenant, digest)
			switch {
			case errors.Is(lookupErr, domain.ErrNotFound):
				evidence.ReplayReason = "the replayed traffic record was not found"
			case lookupErr != nil:
				return Report{}, fmt.Errorf("read campaign replayed traffic: %w", lookupErr)
			default:
				evidence.Replay = &trace
			}
		}
	}
	return Assemble(evidence), nil
}

// replayDigest returns the Replay shape digest a customer-observed campaign
// is bound to. bound is false when the campaign replayed no customer traffic.
func replayDigest(campaign domain.OptimizationCampaign) (string, bool) {
	var proposal optimizer.Proposal
	if json.Unmarshal([]byte(campaign.ProposalJSON), &proposal) != nil || proposal.Input.WorkloadSource != optimizer.WorkloadSourceCustomerObserved {
		return "", false
	}
	digest := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(proposal.Input.WorkloadFingerprint), "sha256:"))
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", true
	}
	return digest, true
}
