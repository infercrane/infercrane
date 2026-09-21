package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

type operationCheckpointer interface {
	CheckpointClaimedOperation(context.Context, string, string, int64, string, string, string, int, string) error
}

type executionCheckpoint struct {
	Name, Status, Message string
	Progress              int
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
			for candidateIndex, candidateID := range request.Candidates {
				for boundary := 0; boundary < 8; boundary++ {
					if err := checkpointCandidate(ctx, coordinator, operation, request, candidateID, candidateIndex); err != nil {
						return "", operations.Retryable("optimization_checkpoint_failed", err)
					}
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
			for candidateIndex, candidateID := range request.Candidates {
				for boundary := 0; boundary < 3; boundary++ {
					if err := checkpointCandidate(ctx, coordinator, operation, request, candidateID, candidateIndex); err != nil {
						return "", operations.Retryable("optimization_checkpoint_failed", err)
					}
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
			if len(waiting) > 0 {
				if err := checkpointOperation(ctx, coordinator.Repository, operation, executionCheckpoint{
					Name:     "awaiting-activation",
					Status:   "waiting",
					Progress: 95,
					Message:  "Evidence passed. Waiting for an operator to publish the measured winner.",
				}, map[string]any{"campaign_id": request.CampaignID, "candidate_ids": waiting}); err != nil {
					return "", operations.Retryable("optimization_checkpoint_failed", err)
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
		for candidateIndex, candidateID := range request.Candidates {
			for boundary := 0; boundary < 2; boundary++ {
				if err := checkpointOperation(ctx, coordinator.Repository, operation, executionCheckpoint{
					Name:     "cleanup",
					Status:   "running",
					Progress: 96,
					Message:  fmt.Sprintf("Cleaning candidate %d of %d.", candidateIndex+1, len(request.Candidates)),
				}, map[string]any{"campaign_id": request.CampaignID, "candidate_id": candidateID}); err != nil {
					return "", operations.Retryable("optimization_checkpoint_failed", err)
				}
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

func checkpointCandidate(ctx context.Context, coordinator Coordinator, operation domain.Operation, request ExecuteRequest, candidateID string, candidateIndex int) error {
	campaign, err := coordinator.Repository.OptimizationCampaign(ctx, request.TenantID, request.CampaignID)
	if err != nil {
		return err
	}
	candidate, found := candidateByID(campaign.Candidates, candidateID)
	if !found {
		return domain.ErrNotFound
	}
	checkpoint := checkpointForCandidate(candidate, candidateIndex, len(request.Candidates))
	return checkpointOperation(ctx, coordinator.Repository, operation, checkpoint, map[string]any{
		"campaign_id":  request.CampaignID,
		"candidate_id": candidateID,
		"candidate":    candidateIndex + 1,
		"candidates":   len(request.Candidates),
		"state":        candidate.State,
	})
}

func checkpointOperation(ctx context.Context, repository Repository, operation domain.Operation, checkpoint executionCheckpoint, payload map[string]any) error {
	checkpointer, ok := repository.(operationCheckpointer)
	if !ok || operation.ID == "" || operation.LeaseOwner == "" || operation.LeaseGeneration < 1 {
		return nil
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return checkpointer.CheckpointClaimedOperation(ctx, operation.ID, operation.LeaseOwner, operation.LeaseGeneration, checkpoint.Name, checkpoint.Status, string(encoded), checkpoint.Progress, checkpoint.Message)
}

func checkpointForCandidate(candidate domain.OptimizationCandidateRun, index, total int) executionCheckpoint {
	position := fmt.Sprintf("candidate %d of %d", index+1, total)
	switch candidate.State {
	case CandidateProposed:
		return executionCheckpoint{Name: "candidate-search", Status: "running", Progress: 15, Message: "Preparing " + position + "."}
	case CandidateProvisioning:
		return executionCheckpoint{Name: "candidate-build", Status: "running", Progress: 30, Message: buildCheckpointMessage(candidate, position)}
	case CandidateReady:
		return executionCheckpoint{Name: "benchmark-prepare", Status: "running", Progress: 45, Message: "Preparing the exact workload for " + position + "."}
	case CandidateMeasuring:
		return executionCheckpoint{Name: "benchmark-measure", Status: "running", Progress: 58, Message: "Measuring latency, throughput, errors, and cost for " + position + "."}
	case CandidateValidating:
		return executionCheckpoint{Name: "quality-gate", Status: "running", Progress: 72, Message: "Checking output quality and workload SLOs for " + position + "."}
	case CandidateRanked:
		return executionCheckpoint{Name: "measured-ranking", Status: "running", Progress: 84, Message: "Comparing measured candidates on the same workload."}
	case CandidateGuarding:
		return executionCheckpoint{Name: "release-guard", Status: "running", Progress: 91, Message: "Checking the measured winner against the active deployment."}
	case CandidateQualified, CandidateGuardPassed:
		return executionCheckpoint{Name: "awaiting-activation", Status: "waiting", Progress: 95, Message: "Evidence passed. Waiting for an operator to publish the measured winner."}
	case CandidateRejected, CandidateInconclusive, CandidateFailed, CandidateCancelled:
		return executionCheckpoint{Name: "cleanup", Status: "running", Progress: 96, Message: "Cleaning rejected resources for " + position + "."}
	case CandidateCleaned:
		return executionCheckpoint{Name: "cleanup", Status: "succeeded", Progress: 98, Message: "Rejected resources were cleaned."}
	case CandidatePromoted, CandidateObserved:
		return executionCheckpoint{Name: "deployed", Status: "succeeded", Progress: 99, Message: "The measured winner is serving behind the stable endpoint."}
	default:
		return executionCheckpoint{Name: "campaign", Status: "running", Progress: 10, Message: "Processing " + position + "."}
	}
}

func buildCheckpointMessage(candidate domain.OptimizationCandidateRun, position string) string {
	var predicted map[string]any
	_ = json.Unmarshal([]byte(candidate.PredictedEvidenceJSON), &predicted)
	kind := ""
	for _, key := range []string{"technique", "candidate_kind", "optimization_type"} {
		if value, ok := predicted[key].(string); ok {
			kind = value
			break
		}
	}
	kind = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(kind), "-", "_"))
	if kind == "custom_kernel" || kind == "kernel" || strings.Contains(kind, "kernel") {
		return "Building and checking a custom kernel for " + position + "."
	}
	if candidate.OptimizedArtifactID != "" || strings.Contains(kind, "quant") || strings.Contains(kind, "artifact") {
		return "Building an immutable optimized artifact for " + position + "."
	}
	return "Provisioning the serving recipe for " + position + "."
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
