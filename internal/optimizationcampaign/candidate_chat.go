package optimizationcampaign

import (
	"context"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
)

var (
	// ErrCandidateUndecided means the run is still provisioning, measuring or
	// ranking this candidate. A prompt now could disturb the measurement.
	ErrCandidateUndecided = errors.New("candidate has not been decided yet")
	// ErrCandidateStopped means the candidate lost, failed or was cleaned up,
	// so nothing is running to answer.
	ErrCandidateStopped = errors.New("candidate is no longer running")
	// ErrCandidateUnreachable means the decided candidate has no healthy ready
	// replica, or no server-side credential, at this moment.
	ErrCandidateUnreachable = errors.New("candidate has no healthy replica to answer")
)

// ChatTarget is one exact revision's OpenAI-compatible server and the
// server-side credential for it. The credential never leaves the control plane.
type ChatTarget struct {
	Endpoint, APIKey, Model, DeploymentName, RevisionID string
}

type CandidateChatStore interface {
	ResolveForTenant(context.Context, string, string) (domain.ResolvedDeployment, error)
	ReplicasForDeployment(context.Context, string, string) ([]domain.Replica, error)
}

// CandidateChatTarget addresses a decided candidate for a single prompt the
// same way measurement and the built-in answer check address it: the
// lowest-ordinal healthy ready replica of the exact measured revision. Only a
// candidate the run has qualified, passed through Release Guard, or rolled out
// is addressable, so a prompt can never run beside a measurement.
func CandidateChatTarget(ctx context.Context, store CandidateChatStore, backends map[string]BenchmarkBackend, candidate domain.OptimizationCandidateRun) (ChatTarget, error) {
	switch candidate.State {
	case CandidateQualified, CandidateGuardPassed, CandidatePromoted, CandidateObserved:
	case CandidateRejected, CandidateInconclusive, CandidateFailed, CandidateCancelled, CandidateCleaned:
		return ChatTarget{}, ErrCandidateStopped
	default:
		return ChatTarget{}, ErrCandidateUndecided
	}
	if store == nil || candidate.DeploymentName == "" || candidate.RevisionID == "" {
		return ChatTarget{}, ErrCandidateUnreachable
	}
	draft, err := candidateDraft(candidate)
	if err != nil {
		return ChatTarget{}, err
	}
	resolved, err := store.ResolveForTenant(ctx, candidate.TenantID, candidate.DeploymentName)
	if errors.Is(err, domain.ErrNotFound) {
		return ChatTarget{}, ErrCandidateStopped
	}
	if err != nil {
		return ChatTarget{}, fmt.Errorf("resolve candidate deployment: %w", err)
	}
	target, _, err := directReplica(ctx, store, backends, candidate, resolved.Deployment, draft)
	if errors.Is(err, errNoDirectEndpoint) {
		return ChatTarget{}, ErrCandidateUnreachable
	}
	if err != nil {
		return ChatTarget{}, err
	}
	return ChatTarget{Endpoint: target.endpoint, APIKey: target.apiKey, Model: target.model, DeploymentName: candidate.DeploymentName, RevisionID: candidate.RevisionID}, nil
}
