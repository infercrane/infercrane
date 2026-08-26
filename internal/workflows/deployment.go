// Package workflows contains resumable operation handlers for control-plane mutations.
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/infercrane/infercrane/internal/domain"
	"github.com/infercrane/infercrane/internal/operations"
)

const ApplyExistingKind = "deployment.apply-existing"

type DeploymentStore interface {
	ApplyDeploymentForTenant(context.Context, string, domain.Deployment, []string) (domain.Deployment, error)
	PublishDeploymentEndpoint(context.Context, string, string, string) (domain.ResolvedEndpoint, error)
	Audit(context.Context, domain.AuditEvent) error
}
type ApplyExistingRequest struct {
	Name               string   `json:"name"`
	EndpointName       string   `json:"endpoint_name,omitempty"`
	Model              string   `json:"model"`
	Targets            []string `json:"targets"`
	RoutingStrategy    string   `json:"routing_strategy,omitempty"`
	MinReplicas        int      `json:"min_replicas,omitempty"`
	MaxReplicas        int      `json:"max_replicas,omitempty"`
	AutoscalingEnabled bool     `json:"autoscaling_enabled,omitempty"`
	Actor              string   `json:"actor,omitempty"`
	TenantID           string   `json:"tenant_id,omitempty"`
}

func (r *ApplyExistingRequest) Validate() error {
	if r.Name == "" || r.Model == "" || len(r.Targets) == 0 {
		return errors.New("name, model, and at least one target are required")
	}
	if r.EndpointName == "" {
		r.EndpointName = r.Name
	}
	if !endpointNamePattern.MatchString(r.EndpointName) {
		return errors.New("endpoint_name must contain 1 to 64 lowercase letters, numbers, dots, underscores, or dashes and must start and end with a letter or number")
	}
	return nil
}

func DeploymentHandlers(store DeploymentStore) map[string]operations.Handler {
	return map[string]operations.Handler{ApplyExistingKind: func(ctx context.Context, operation domain.Operation) (string, error) {
		var request ApplyExistingRequest
		if err := json.Unmarshal([]byte(operation.RequestJSON), &request); err != nil {
			return "", operations.Permanent("invalid_request", fmt.Errorf("decode apply request: %w", err))
		}
		if err := request.Validate(); err != nil {
			return "", operations.Permanent("invalid_request", err)
		}
		deployment, err := store.ApplyDeploymentForTenant(ctx, request.TenantID, domain.Deployment{Name: request.Name, Model: request.Model, RoutingStrategy: request.RoutingStrategy, MinReplicas: request.MinReplicas, MaxReplicas: request.MaxReplicas, AutoscalingEnabled: request.AutoscalingEnabled}, request.Targets)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) {
				return "", operations.Permanent("apply_rejected", err)
			}
			return "", operations.Retryable("apply_failed", err)
		}
		published, err := store.PublishDeploymentEndpoint(ctx, request.TenantID, request.EndpointName, deployment.Name)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrConflict) {
				return "", operations.Permanent("endpoint_publish_rejected", err)
			}
			return "", operations.Retryable("endpoint_publish_failed", err)
		}
		result, _ := json.Marshal(map[string]string{"deployment_id": deployment.ID, "name": deployment.Name, "endpoint_name": published.Endpoint.Name})
		_ = store.Audit(context.WithoutCancel(ctx), domain.AuditEvent{TenantID: request.TenantID, Actor: request.Actor, Action: "deployment.apply", ResourceType: "deployment", ResourceName: request.Name, Outcome: "succeeded", Payload: operation.RequestJSON})
		return string(result), nil
	}}
}
