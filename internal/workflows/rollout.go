package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
)

const (
	RolloutCreateKind   = "rollout.create-candidate"
	RolloutPromoteKind  = "rollout.promote"
	RolloutRejectKind   = "rollout.reject"
	RolloutRollbackKind = "rollout.rollback"
)

type RolloutRequest struct {
	Name        string          `json:"name"`
	CandidateID string          `json:"candidate_id,omitempty"`
	RevisionID  string          `json:"revision_id,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	Spec        json.RawMessage `json:"spec,omitempty"`
	Actor       string          `json:"actor,omitempty"`
	TenantID    string          `json:"tenant_id,omitempty"`
}

type RolloutStore interface {
	EnsureCandidateRevision(context.Context, string, string, string, string) (domain.DeploymentRevision, error)
	PromoteCandidateRevision(context.Context, string, string, string) error
	RejectCandidateRevision(context.Context, string, string, string, string) error
	RollbackRevision(context.Context, string, string, string, string) error
	Audit(context.Context, domain.AuditEvent) error
}

func RolloutHandlers(store RolloutStore) map[string]operations.Handler {
	transition := func(action string) operations.Handler {
		return func(ctx context.Context, operation domain.Operation) (string, error) {
			request, err := decodeRollout(operation)
			if err != nil {
				return "", err
			}
			switch action {
			case "promote":
				if request.CandidateID == "" {
					return "", operations.Permanent("invalid_request", errors.New("candidate_id is required"))
				}
				err = store.PromoteCandidateRevision(ctx, request.TenantID, request.Name, request.CandidateID)
			case "reject":
				if request.CandidateID == "" || request.Reason == "" {
					return "", operations.Permanent("invalid_request", errors.New("candidate_id and reason are required"))
				}
				err = store.RejectCandidateRevision(ctx, request.TenantID, request.Name, request.CandidateID, request.Reason)
			case "rollback":
				if request.RevisionID == "" || request.Reason == "" {
					return "", operations.Permanent("invalid_request", errors.New("revision_id and reason are required"))
				}
				err = store.RollbackRevision(ctx, request.TenantID, request.Name, request.RevisionID, request.Reason)
			}
			if err != nil {
				if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) {
					return "", operations.Permanent("rollout_rejected", err)
				}
				return "", operations.Retryable("rollout_transition_failed", err)
			}
			_ = store.Audit(context.WithoutCancel(ctx), domain.AuditEvent{TenantID: request.TenantID, Actor: request.Actor, Action: "rollout." + action, ResourceType: "deployment", ResourceName: request.Name, Outcome: "succeeded", Payload: operation.RequestJSON})
			result, _ := json.Marshal(map[string]string{"deployment": request.Name, "action": action})
			return string(result), nil
		}
	}
	return map[string]operations.Handler{
		RolloutCreateKind: func(ctx context.Context, operation domain.Operation) (string, error) {
			request, err := decodeRollout(operation)
			if err != nil {
				return "", err
			}
			if len(request.Spec) == 0 {
				return "", operations.Permanent("invalid_request", errors.New("spec is required"))
			}
			revision, err := store.EnsureCandidateRevision(ctx, request.TenantID, request.Name, string(request.Spec), operation.ID)
			if err != nil {
				if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) {
					return "", operations.Permanent("candidate_rejected", err)
				}
				return "", operations.Retryable("candidate_create_failed", err)
			}
			_ = store.Audit(context.WithoutCancel(ctx), domain.AuditEvent{TenantID: request.TenantID, Actor: request.Actor, Action: "rollout.create", ResourceType: "deployment", ResourceName: request.Name, Outcome: "succeeded", Payload: operation.RequestJSON})
			result, _ := json.Marshal(map[string]any{"deployment": request.Name, "candidate_id": revision.ID, "revision_number": revision.Number})
			return string(result), nil
		},
		RolloutPromoteKind:  transition("promote"),
		RolloutRejectKind:   transition("reject"),
		RolloutRollbackKind: transition("rollback"),
	}
}

func decodeRollout(operation domain.Operation) (RolloutRequest, error) {
	var request RolloutRequest
	if err := json.Unmarshal([]byte(operation.RequestJSON), &request); err != nil {
		return request, operations.Permanent("invalid_request", fmt.Errorf("decode rollout request: %w", err))
	}
	if request.Name == "" || request.TenantID == "" {
		return request, operations.Permanent("invalid_request", errors.New("name and tenant_id are required"))
	}
	return request, nil
}
