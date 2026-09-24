package autoscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// CapacityDeployment binds an autoscaling policy to capacity measured on the
// exact production tuple. A modeled or cross-provider envelope is rejected by
// EvaluateCapacityEnvelope before any mutation can occur.
type CapacityDeployment struct {
	ID      string
	Policy  CapacityPolicy
	Replica ReplicaEnvelope
	Fleet   FleetState
}

type CapacityRepository interface {
	CapacityDeployments(context.Context) ([]CapacityDeployment, error)
	CapacityDemand(context.Context, string) (DemandEnvelope, error)
	RecordCapacityDecision(context.Context, string, CapacityDecision, string) error
}

type CapacityFleet interface {
	ScaleCapacityTo(context.Context, string, int) error
}

// CapacityAdmission applies the safe envelope at the edge. It is deliberately
// called before provider mutations so a slow GPU launch cannot turn into an
// unbounded queue or an SLO collapse.
type CapacityAdmission interface {
	ApplyCapacityDecision(context.Context, string, CapacityDecision) error
}

// CapacityController is the medium-speed loop between request admission and
// recipe optimization. Admission reacts per request, this controller changes
// fleet size, and the continual optimizer changes the qualified recipe.
type CapacityController struct {
	Repository CapacityRepository
	Admission  CapacityAdmission
	Fleet      CapacityFleet
}

func (c CapacityController) Once(ctx context.Context) error {
	if c.Repository == nil || c.Admission == nil || c.Fleet == nil {
		return errors.New("capacity repository, admission, and fleet are required")
	}
	deployments, err := c.Repository.CapacityDeployments(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, deployment := range deployments {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		demand, err := c.Repository.CapacityDemand(ctx, deployment.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("capacity demand for %s: %w", deployment.ID, err))
			continue
		}
		decision, err := EvaluateCapacityEnvelope(deployment.Policy, demand, deployment.Replica, deployment.Fleet)
		if err != nil {
			failures = append(failures, fmt.Errorf("evaluate capacity for %s: %w", deployment.ID, err))
			continue
		}
		evidence, _ := json.Marshal(struct {
			Demand  DemandEnvelope  `json:"demand"`
			Replica ReplicaEnvelope `json:"replica"`
			Fleet   FleetState      `json:"fleet"`
		}{demand, deployment.Replica, deployment.Fleet})

		if err := c.Admission.ApplyCapacityDecision(ctx, deployment.ID, decision); err != nil {
			failures = append(failures, fmt.Errorf("apply admission envelope for %s: %w", deployment.ID, err))
			continue
		}
		if decision.Action == "scale_up" || decision.Action == "scale_down" {
			if err := c.Fleet.ScaleCapacityTo(ctx, deployment.ID, decision.TargetReplicas); err != nil {
				decision.Action = "scale_failed"
				decision.Reasons = append(decision.Reasons, "provider mutation failed; admission remains bounded by ready capacity")
				_ = c.Repository.RecordCapacityDecision(ctx, deployment.ID, decision, string(evidence))
				failures = append(failures, fmt.Errorf("scale capacity for %s: %w", deployment.ID, err))
				continue
			}
		}
		if err := c.Repository.RecordCapacityDecision(ctx, deployment.ID, decision, string(evidence)); err != nil {
			failures = append(failures, fmt.Errorf("record capacity decision for %s: %w", deployment.ID, err))
		}
	}
	return errors.Join(failures...)
}
