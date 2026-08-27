package autoscale

import (
	"context"
	"encoding/json"
	"errors"
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
	AutoscalingSLOEvidence(context.Context, string, time.Time) (SLOEvidence, error)
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
	var failures []error
	for _, deployment := range deployments {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		signals, err := c.Signals.Signals(ctx, deployment.ID)
		if err != nil {
			failures = append(failures, fmt.Errorf("signals for %s: %w", deployment.ID, err))
			continue
		}
		signals.ObservedAt = now()
		evidence, evidenceErr := c.Repository.AutoscalingSLOEvidence(ctx, deployment.ID, signals.ObservedAt)
		if evidenceErr != nil {
			failures = append(failures, fmt.Errorf("SLO evidence for %s: %w", deployment.ID, evidenceErr))
			continue
		}
		decision, err := EvaluateWithSLO(deployment.Policy, deployment.State, signals, evidence)
		if err != nil {
			failures = append(failures, fmt.Errorf("evaluate %s: %w", deployment.ID, err))
			continue
		}
		encoded, _ := json.Marshal(EvidenceSnapshot{Queue: signals, SLO: evidence})
		if decision.Action != "hold" {
			if err := c.Fleet.ScaleTo(ctx, deployment.ID, decision.NewReplicas); err != nil {
				failures = append(failures, fmt.Errorf("scale %s: %w", deployment.ID, err))
				continue
			}
			deployment.State.Replicas = decision.NewReplicas
			deployment.State.LastScaledAt = signals.ObservedAt
		}
		deployment.State.ConsecutiveHigh, deployment.State.ConsecutiveLow = decision.NextConsecutiveHigh, decision.NextConsecutiveLow
		if err := c.Repository.RecordDecision(ctx, deployment.ID, decision, string(encoded)); err != nil {
			failures = append(failures, fmt.Errorf("record autoscaling decision for %s: %w", deployment.ID, err))
			continue
		}
		if err := c.Repository.SaveState(ctx, deployment.ID, deployment.State); err != nil {
			failures = append(failures, fmt.Errorf("save autoscaling state for %s: %w", deployment.ID, err))
		}
	}
	return errors.Join(failures...)
}
