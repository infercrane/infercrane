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

// evidenceRowLimit is the newest rows read per candidate deployment. A record
// older than that is reported missing, which keeps the dependent field unknown.
const evidenceRowLimit = 100

// Store is the tenant-scoped, read-only evidence boundary of a report. Every
// method filters by tenant, so a report never reads another tenant's records.
type Store interface {
	OptimizationCampaign(context.Context, string, string) (domain.OptimizationCampaign, error)
	BenchmarksForDeployment(context.Context, string, string, int) ([]domain.BenchmarkResult, error)
	QualityEvidenceForDeployment(context.Context, string, string, int) ([]domain.QualityEvidence, error)
	ReleaseGuardEvaluations(context.Context, string, string, int) ([]domain.ReleaseGuardEvaluation, error)
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
	evidence := Evidence{Campaign: campaign, Benchmarks: map[string]domain.BenchmarkResult{}, Quality: map[string]domain.QualityEvidence{}, ReleaseGuard: map[string]domain.ReleaseGuardEvaluation{}}
	wanted := struct{ benchmarks, quality, guard map[string]bool }{map[string]bool{}, map[string]bool{}, map[string]bool{}}
	deployments := []string{}
	seen := map[string]bool{}
	for _, run := range campaign.Candidates {
		if run.BenchmarkID != "" {
			wanted.benchmarks[run.BenchmarkID] = true
		}
		if run.QualityEvidenceID != "" {
			wanted.quality[run.QualityEvidenceID] = true
		}
		if run.ReleaseGuardEvaluationID != "" {
			wanted.guard[run.ReleaseGuardEvaluationID] = true
		}
		if run.DeploymentName != "" && !seen[run.DeploymentName] {
			seen[run.DeploymentName] = true
			deployments = append(deployments, run.DeploymentName)
		}
	}
	for _, name := range deployments {
		if len(wanted.benchmarks) > len(evidence.Benchmarks) {
			rows, lookupErr := store.BenchmarksForDeployment(ctx, tenant, name, evidenceRowLimit)
			if lookupErr != nil && !errors.Is(lookupErr, domain.ErrNotFound) {
				return Report{}, fmt.Errorf("read campaign benchmarks: %w", lookupErr)
			}
			for _, row := range rows {
				if wanted.benchmarks[row.ID] {
					evidence.Benchmarks[row.ID] = row
				}
			}
		}
		if len(wanted.quality) > len(evidence.Quality) {
			rows, lookupErr := store.QualityEvidenceForDeployment(ctx, tenant, name, evidenceRowLimit)
			if lookupErr != nil && !errors.Is(lookupErr, domain.ErrNotFound) {
				return Report{}, fmt.Errorf("read campaign quality evidence: %w", lookupErr)
			}
			for _, row := range rows {
				if wanted.quality[row.ID] {
					evidence.Quality[row.ID] = row
				}
			}
		}
		if len(wanted.guard) > len(evidence.ReleaseGuard) {
			rows, lookupErr := store.ReleaseGuardEvaluations(ctx, tenant, name, evidenceRowLimit)
			if lookupErr != nil && !errors.Is(lookupErr, domain.ErrNotFound) {
				return Report{}, fmt.Errorf("read campaign Release Guard evidence: %w", lookupErr)
			}
			for _, row := range rows {
				if wanted.guard[row.ID] {
					evidence.ReleaseGuard[row.ID] = row
				}
			}
		}
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
