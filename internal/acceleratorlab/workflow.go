package acceleratorlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
)

const ExecuteKind = "optimization.accelerator-lab.execute"

type operationCheckpointer interface {
	CheckpointClaimedOperation(context.Context, string, string, int64, string, string, string, int, string) error
}

// Handlers turns Accelerator Lab into a leased, restart-safe product
// operation. The browser submits intent to InferCrane and only observes the
// normal operation resource; worker and Brezel credentials remain server-side.
func Handlers(engine Engine, checkpointer operationCheckpointer) map[string]operations.Handler {
	return map[string]operations.Handler{ExecuteKind: func(ctx context.Context, operation domain.Operation) (string, error) {
		var request Request
		decoder := json.NewDecoder(strings.NewReader(operation.RequestJSON))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			return "", operations.Permanent("accelerator_lab_request_invalid", fmt.Errorf("decode accelerator lab request: %w", err))
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return "", operations.Permanent("accelerator_lab_request_invalid", errors.New("accelerator lab request contains trailing JSON"))
		}
		request.TenantID = operation.TenantID
		validated, _, err := ValidateRequest(request)
		if err != nil {
			return "", operations.Permanent("accelerator_lab_request_invalid", err)
		}
		runner := engine
		upstreamProgress := runner.Progress
		runner.Progress = func(progressContext context.Context, progress Progress) error {
			if upstreamProgress != nil {
				if progressErr := upstreamProgress(progressContext, progress); progressErr != nil {
					return progressErr
				}
			}
			if checkpointer == nil || operation.ID == "" || operation.LeaseOwner == "" || operation.LeaseGeneration < 1 {
				return nil
			}
			checkpoint, _ := json.Marshal(map[string]any{
				"campaign_id": validated.CampaignID, "candidate_id": validated.CandidateID,
				"stage": progress.Stage, "cost_usd": progress.CostUSD, "max_cost_usd": validated.Policy.MaxCostUSD,
				"accelerator": validated.Hardware.Accelerator, "runtime": validated.Runtime.Name, "modality": validated.Workload.Modality,
			})
			return checkpointer.CheckpointClaimedOperation(progressContext, operation.ID, operation.LeaseOwner, operation.LeaseGeneration, progress.Stage, "running", string(checkpoint), progress.Progress, progress.Message)
		}
		result, err := runner.Run(ctx, validated)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", err
			}
			if errors.Is(err, ErrExecutionAuthorityExpired) {
				return "", operations.Permanent("accelerator_lab_authority_expired", err)
			}
			return "", operations.Retryable("accelerator_lab_execution_failed", err)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return "", operations.Permanent("accelerator_lab_result_invalid", err)
		}
		return string(encoded), nil
	}}
}
