package autoscale

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type Deployment struct {
	ID     string
	Policy Policy
	State  State
}
type Repository interface {
	AutoscalingDeployments(context.Context) ([]Deployment, error)
	RecordDecision(context.Context, string, Decision, string) error
	SaveState(context.Context, string, State) error
}
type SignalSource interface {
	Signals(context.Context, string) (Signals, error)
}
type Fleet interface {
	ScaleTo(context.Context, string, int) error
}

type Controller struct {
	Repository Repository
	Signals    SignalSource
	Fleet      Fleet
	Now        func() time.Time
}

func (c Controller) Once(ctx context.Context) error {
	if c.Repository == nil || c.Signals == nil || c.Fleet == nil {
		return fmt.Errorf("autoscaling repository, signals, and fleet are required")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	deployments, err := c.Repository.AutoscalingDeployments(ctx)
	if err != nil {
		return err
	}
	for _, deployment := range deployments {
		signals, err := c.Signals.Signals(ctx, deployment.ID)
		if err != nil {
			return fmt.Errorf("signals for %s: %w", deployment.ID, err)
		}
		signals.ObservedAt = now()
		decision, err := Evaluate(deployment.Policy, deployment.State, signals)
		if err != nil {
			return fmt.Errorf("evaluate %s: %w", deployment.ID, err)
		}
		encoded, _ := json.Marshal(signals)
		if decision.Action != "hold" {
			if err := c.Fleet.ScaleTo(ctx, deployment.ID, decision.NewReplicas); err != nil {
				return fmt.Errorf("scale %s: %w", deployment.ID, err)
			}
			deployment.State.Replicas = decision.NewReplicas
			deployment.State.LastScaledAt = signals.ObservedAt
		}
		deployment.State.ConsecutiveHigh, deployment.State.ConsecutiveLow = decision.NextConsecutiveHigh, decision.NextConsecutiveLow
		if err := c.Repository.RecordDecision(ctx, deployment.ID, decision, string(encoded)); err != nil {
			return err
		}
		if err := c.Repository.SaveState(ctx, deployment.ID, deployment.State); err != nil {
			return err
		}
	}
	return nil
}
