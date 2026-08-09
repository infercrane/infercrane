// Package autoscale implements bounded, explainable scaling decisions.
package autoscale

import (
	"errors"
	"fmt"
	"time"
)

type Policy struct {
	Enabled                              bool
	MinReplicas, MaxReplicas             int
	QueueThreshold, LowLoadThreshold     float64
	ScaleUpIntervals, ScaleDownIntervals int
	Cooldown                             time.Duration
}

type Signals struct {
	Waiting, Running float64
	ObservedAt       time.Time
}

type State struct {
	Replicas, ConsecutiveHigh, ConsecutiveLow int
	LastScaledAt                              time.Time
}

type Decision struct {
	Action, Reason                          string
	OldReplicas, NewReplicas                int
	NextConsecutiveHigh, NextConsecutiveLow int
}

func Evaluate(policy Policy, state State, signals Signals) (Decision, error) {
	if policy.MinReplicas < 1 || policy.MaxReplicas < policy.MinReplicas {
		return Decision{}, errors.New("invalid replica bounds")
	}
	if policy.ScaleUpIntervals < 1 || policy.ScaleDownIntervals < 1 || policy.Cooldown < 0 {
		return Decision{}, errors.New("invalid stability or cooldown settings")
	}
	if state.Replicas < policy.MinReplicas || state.Replicas > policy.MaxReplicas {
		return Decision{}, errors.New("current replicas are outside policy bounds")
	}
	d := Decision{Action: "hold", OldReplicas: state.Replicas, NewReplicas: state.Replicas}
	if !policy.Enabled {
		d.Reason = "policy disabled"
		return d, nil
	}
	if !state.LastScaledAt.IsZero() && signals.ObservedAt.Sub(state.LastScaledAt) < policy.Cooldown {
		d.Reason = "cooldown active"
		return d, nil
	}
	if signals.Waiting >= policy.QueueThreshold {
		d.NextConsecutiveHigh = state.ConsecutiveHigh + 1
		if d.NextConsecutiveHigh >= policy.ScaleUpIntervals && state.Replicas < policy.MaxReplicas {
			d.Action, d.NewReplicas = "scale_up", state.Replicas+1
			d.NextConsecutiveHigh = 0
			d.Reason = fmt.Sprintf("waiting %.2f >= threshold %.2f for %d intervals", signals.Waiting, policy.QueueThreshold, policy.ScaleUpIntervals)
		} else if state.Replicas == policy.MaxReplicas {
			d.Reason = "maximum replicas reached"
		} else {
			d.Reason = "waiting for scale-up stability window"
		}
		return d, nil
	}
	load := signals.Running + signals.Waiting
	if load <= policy.LowLoadThreshold {
		d.NextConsecutiveLow = state.ConsecutiveLow + 1
		if d.NextConsecutiveLow >= policy.ScaleDownIntervals && state.Replicas > policy.MinReplicas {
			d.Action, d.NewReplicas = "scale_down", state.Replicas-1
			d.NextConsecutiveLow = 0
			d.Reason = fmt.Sprintf("load %.2f <= threshold %.2f for %d intervals", load, policy.LowLoadThreshold, policy.ScaleDownIntervals)
		} else if state.Replicas == policy.MinReplicas {
			d.Reason = "minimum replicas reached"
		} else {
			d.Reason = "waiting for scale-down stability window"
		}
		return d, nil
	}
	d.Reason = "signals are within policy thresholds"
	return d, nil
}

type Scaler interface{ ScaleTo(replicas int) error }

func Apply(decision Decision, scaler Scaler) error {
	if decision.Action == "hold" {
		return nil
	}
	if scaler == nil {
		return errors.New("scaler is required for a scaling action")
	}
	return scaler.ScaleTo(decision.NewReplicas)
}
