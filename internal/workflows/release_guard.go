package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
)

const ReleaseGuardEvaluateKind = "release-guard.evaluate"

type ReleaseGuardStore interface {
	EvaluateReleaseGuard(context.Context, string, string, time.Duration) (domain.ReleaseGuardEvaluation, error)
	Audit(context.Context, domain.AuditEvent) error
}

func ReleaseGuardHandlers(store ReleaseGuardStore) map[string]operations.Handler {
	return map[string]operations.Handler{ReleaseGuardEvaluateKind: func(ctx context.Context, operation domain.Operation) (string, error) {
		request, err := decodeRollout(operation)
		if err != nil {
			return "", err
		}
		evaluation, err := store.EvaluateReleaseGuard(ctx, request.TenantID, request.Name, 5*time.Minute)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) {
				return "", operations.Permanent("release_guard_rejected", err)
			}
			return "", operations.Retryable("release_guard_evaluation_failed", err)
		}
		_ = store.Audit(context.WithoutCancel(ctx), domain.AuditEvent{TenantID: request.TenantID, Actor: request.Actor, Action: "release_guard.evaluate", ResourceType: "deployment", ResourceName: request.Name, Outcome: evaluation.Decision, Payload: operation.RequestJSON})
		result, _ := json.Marshal(map[string]any{"evaluation_id": evaluation.ID, "decision": evaluation.Decision, "reasons": json.RawMessage(evaluation.ReasonCodesJSON), "metrics": json.RawMessage(evaluation.MetricsJSON), "policy": json.RawMessage(evaluation.PolicyJSON)})
		return string(result), nil
	}}
}
