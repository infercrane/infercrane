package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
	"github.com/infercrane/infercrane/internal/workflows"
)

const (
	ExecuteKind  = "optimization.campaign.execute"
	CleanupKind  = "optimization.campaign.cleanup"
	ActivateKind = "optimization.campaign.activate"
)

type ExecuteRequest struct {
	TenantID   string   `json:"tenant_id"`
	CampaignID string   `json:"campaign_id"`
	Candidates []string `json:"candidates"`
}

type ActivateRequest struct {
	TenantID    string `json:"tenant_id"`
	CampaignID  string `json:"campaign_id"`
	CandidateID string `json:"candidate_id"`
	Actor       string `json:"actor"`
}

func (r ActivateRequest) Validate() error {
	if r.TenantID == "" || r.CampaignID == "" || r.CandidateID == "" || r.Actor == "" {
		return errors.New("optimization activation requires tenant, campaign, candidate, and actor")
	}
	return nil
}

type ActivationStore interface {
	Repository
	EnqueueOperation(context.Context, domain.Operation) (domain.Operation, bool, error)
	PublishDeploymentEndpoint(context.Context, string, string, string) (domain.ResolvedEndpoint, error)
}

// ActivationHandlers owns the explicit human boundary after qualification.
// New-endpoint candidates publish the requested stable alias without a fake
// rollout baseline; evolution candidates delegate traffic mutation to the
// existing guarded rollout operation. Both publish the request-path generation
// before persisting campaign promotion.
func ActivationHandlers(store ActivationStore, refresh func(context.Context) error, now func() time.Time) map[string]operations.Handler {
	return map[string]operations.Handler{ActivateKind: func(ctx context.Context, operation domain.Operation) (string, error) {
		var request ActivateRequest
		if err := json.Unmarshal([]byte(operation.RequestJSON), &request); err != nil {
			return "", operations.Permanent("invalid_request", fmt.Errorf("decode optimization activation: %w", err))
		}
		if err := request.Validate(); err != nil {
			return "", operations.Permanent("invalid_request", err)
		}
		campaign, err := store.OptimizationCampaign(ctx, request.TenantID, request.CampaignID)
		if err != nil {
			return "", executionFailure(err)
		}
		candidate, ok := candidateByID(campaign.Candidates, request.CandidateID)
		if !ok {
			return "", operations.Permanent("optimization_candidate_not_found", domain.ErrNotFound)
		}
		if candidate.State == CandidatePromoted || candidate.State == CandidateObserved {
			return activationResult(campaign, candidate, activationEndpoint(campaign, candidate)), nil
		}
		stamp := time.Now().UTC()
		if now != nil {
			stamp = now().UTC()
		}
		if campaign.ApprovalExpiresAt == nil || !campaign.ApprovalExpiresAt.After(stamp) {
			return "", operations.Permanent("optimization_approval_expired", ErrApprovalExpired)
		}
		expected := CandidateQualified
		if campaign.Intent == IntentEvolveEndpoint {
			expected = CandidateGuardPassed
		}
		if candidate.State != expected {
			return "", operations.Permanent("optimization_candidate_not_qualified", fmt.Errorf("candidate state %s requires %s", candidate.State, expected))
		}
		if campaign.Intent == IntentEvolveEndpoint {
			payload, _ := json.Marshal(workflows.RolloutRequest{Name: campaign.TargetDeployment, CandidateID: candidate.RevisionID, TenantID: request.TenantID, Actor: request.Actor})
			child, _, enqueueErr := store.EnqueueOperation(ctx, domain.Operation{TenantID: request.TenantID, Kind: workflows.RolloutPromoteKind, ResourceType: "deployment", ResourceName: campaign.TargetDeployment, IdempotencyKey: childKey(candidate.ID, "activate"), RequestJSON: string(payload), MaxAttempts: 120})
			if enqueueErr != nil {
				return "", operations.Retryable("optimization_activation_enqueue_failed", enqueueErr)
			}
			if childErr := childComplete(child, "guarded candidate promotion"); childErr != nil {
				return "", childErr
			}
		} else {
			draft, draftErr := candidateDraft(candidate)
			if draftErr != nil {
				return "", operations.Permanent("optimization_candidate_spec_invalid", draftErr)
			}
			published, publishErr := store.PublishDeploymentEndpoint(ctx, request.TenantID, draft.Name, candidate.DeploymentName)
			if errors.Is(publishErr, domain.ErrConflict) {
				return "", operations.Permanent("optimization_endpoint_alias_conflict", publishErr)
			}
			if publishErr != nil {
				return "", operations.Retryable("optimization_endpoint_publish_failed", publishErr)
			}
			if published.Endpoint.Name != draft.Name {
				return "", operations.Permanent("optimization_endpoint_identity_mismatch", errors.New("published endpoint does not match the qualified candidate request"))
			}
		}
		if refresh == nil {
			return "", operations.Permanent("optimization_route_publisher_unavailable", errors.New("endpoint route publisher is not configured"))
		}
		if refreshErr := refresh(ctx); refreshErr != nil {
			return "", operations.Retryable("optimization_route_publish_pending", refreshErr)
		}
		promoted, err := store.TransitionOptimizationCandidate(ctx, request.TenantID, campaign.ID, candidate.ID, candidate.State, CandidatePromoted, domain.OptimizationCandidateRun{})
		if errors.Is(err, domain.ErrConflict) {
			reloaded, reloadErr := store.OptimizationCampaign(ctx, request.TenantID, campaign.ID)
			if reloadErr == nil {
				if current, found := candidateByID(reloaded.Candidates, candidate.ID); found && (current.State == CandidatePromoted || current.State == CandidateObserved) {
					return activationResult(reloaded, current, activationEndpoint(reloaded, current)), nil
				}
			}
		}
		if err != nil {
			return "", executionFailure(err)
		}
		return activationResult(campaign, promoted, activationEndpoint(campaign, promoted)), nil
	}}
}

func activationEndpoint(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun) string {
	if campaign.Intent == IntentEvolveEndpoint {
		return campaign.TargetDeployment
	}
	draft, err := candidateDraft(candidate)
	if err != nil {
		return ""
	}
	return draft.Name
}

func activationResult(campaign domain.OptimizationCampaign, candidate domain.OptimizationCandidateRun, endpoint string) string {
	encoded, _ := json.Marshal(map[string]any{"campaign_id": campaign.ID, "candidate_id": candidate.ID, "endpoint": endpoint, "deployment": candidate.DeploymentName, "revision_id": candidate.RevisionID, "state": candidate.State, "automatic_promotion": false})
	return string(encoded)
}

func (r ExecuteRequest) Validate() error {
	if r.TenantID == "" || r.CampaignID == "" || len(r.Candidates) < 1 || len(r.Candidates) > 100 {
		return errors.New("optimization execution requires tenant, campaign, and 1..100 candidates")
	}
	seen := map[string]struct{}{}
	for _, candidate := range r.Candidates {
		if candidate == "" {
			return errors.New("optimization execution candidate IDs must not be empty")
		}
		if _, duplicate := seen[candidate]; duplicate {
			return errors.New("optimization execution candidate IDs must be unique")
		}
		seen[candidate] = struct{}{}
	}
	return nil
}

// Handlers turns the coordinator into a restart-safe durable operation. Each
// candidate advances through bounded persisted phases. A successful operation
// can finish with candidates waiting for an explicit human promotion.
func Handlers(coordinator Coordinator) map[string]operations.Handler {
	return map[string]operations.Handler{
		ExecuteKind: func(ctx context.Context, operation domain.Operation) (string, error) {
			var request ExecuteRequest
			if err := json.Unmarshal([]byte(operation.RequestJSON), &request); err != nil {
				return "", operations.Permanent("invalid_request", fmt.Errorf("decode optimization execution: %w", err))
			}
			if err := request.Validate(); err != nil {
				return "", operations.Permanent("invalid_request", err)
			}
			waiting := make([]string, 0, len(request.Candidates))
			// Phase one advances every candidate only to the measured ranking
			// barrier. This prevents the first proposal from winning before its
			// peers have produced comparable evidence.
			for _, candidateID := range request.Candidates {
				for boundary := 0; boundary < 8; boundary++ {
					result, err := coordinator.Step(ctx, request.TenantID, request.CampaignID, candidateID)
					if err != nil {
						return "", executionFailure(err)
					}
					if result.WaitingForHuman || result.To == CandidateRanked || result.To == CandidateCleaned {
						break
					}
					if !result.Progressed {
						break
					}
					if boundary == 7 {
						return "", operations.Permanent("optimization_state_loop", errors.New("candidate exceeded the bounded optimization transition count"))
					}
				}
			}
			// Phase two ranks the complete measured set, rejects non-selected
			// candidates, and evaluates Release Guard only for the winner.
			for _, candidateID := range request.Candidates {
				for boundary := 0; boundary < 3; boundary++ {
					result, err := coordinator.Step(ctx, request.TenantID, request.CampaignID, candidateID)
					if err != nil {
						return "", executionFailure(err)
					}
					if result.WaitingForHuman {
						waiting = append(waiting, candidateID)
						break
					}
					if !result.Progressed || result.To == CandidateCleaned {
						break
					}
					if boundary == 2 {
						return "", operations.Permanent("optimization_state_loop", errors.New("candidate exceeded the bounded ranking, guard, and cleanup transition count"))
					}
				}
			}
			encoded, _ := json.Marshal(map[string]any{
				"campaign_id":       request.CampaignID,
				"waiting_for_human": waiting,
				"promotion":         "not_performed",
			})
			if len(waiting) > 0 {
				return "", operations.Retryable("optimization_waiting_for_human", fmt.Errorf("campaign %s is qualified and waiting for explicit activation or promotion before authority expires", request.CampaignID))
			}
			return string(encoded), nil
		},
		ExecuteKind + ".cancel": cleanupHandler(coordinator),
		CleanupKind:             cleanupHandler(coordinator),
	}
}

func cleanupHandler(coordinator Coordinator) operations.Handler {
	return func(ctx context.Context, operation domain.Operation) (string, error) {
		var request ExecuteRequest
		if err := json.Unmarshal([]byte(operation.RequestJSON), &request); err != nil {
			return "", operations.Permanent("invalid_request", fmt.Errorf("decode optimization cancellation: %w", err))
		}
		if err := request.Validate(); err != nil {
			return "", operations.Permanent("invalid_request", err)
		}
		for _, candidateID := range request.Candidates {
			for boundary := 0; boundary < 2; boundary++ {
				result, err := coordinator.CancelCandidate(ctx, request.TenantID, request.CampaignID, candidateID)
				if err != nil {
					return "", executionFailure(err)
				}
				if !result.Progressed || result.To == CandidateCleaned {
					break
				}
			}
		}
		encoded, _ := json.Marshal(map[string]any{"campaign_id": request.CampaignID, "cleanup": "completed", "promotion": "not_performed"})
		return string(encoded), nil
	}
}

func executionFailure(err error) error {
	if errors.Is(err, domain.ErrConflict) {
		return operations.Retryable("optimization_fence_conflict", err)
	}
	var failure operations.Failure
	if errors.As(err, &failure) {
		return failure
	}
	if errors.Is(err, ErrExecutionAuthority) {
		return operations.Permanent("optimization_authority_unavailable", err)
	}
	return operations.Retryable("optimization_step_failed", err)
}
