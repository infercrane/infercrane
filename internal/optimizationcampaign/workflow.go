package optimizationcampaign

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
)

const ExecuteKind = "optimization.campaign.execute"

type ExecuteRequest struct {
	TenantID   string   `json:"tenant_id"`
	CampaignID string   `json:"campaign_id"`
	Candidates []string `json:"candidates"`
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
				for boundary := 0; boundary < 2; boundary++ {
					result, err := coordinator.Step(ctx, request.TenantID, request.CampaignID, candidateID)
					if err != nil {
						return "", executionFailure(err)
					}
					if result.WaitingForHuman {
						waiting = append(waiting, candidateID)
						break
					}
					if !result.Progressed || result.To == CandidateRejected || result.To == CandidateInconclusive || result.To == CandidateFailed || result.To == CandidateCancelled || result.To == CandidateCleaned {
						break
					}
					if boundary == 1 {
						return "", operations.Permanent("optimization_state_loop", errors.New("candidate exceeded the bounded ranking and guard transition count"))
					}
				}
			}
			encoded, _ := json.Marshal(map[string]any{
				"campaign_id":       request.CampaignID,
				"waiting_for_human": waiting,
				"promotion":         "not_performed",
			})
			return string(encoded), nil
		},
		ExecuteKind + ".cancel": func(ctx context.Context, operation domain.Operation) (string, error) {
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
		},
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
