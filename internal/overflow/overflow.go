// Package overflow owns deterministic control-loop policy for explicit external capacity.
package overflow

import (
	"errors"
	"math"
	"time"
)

type Policy struct {
	Mode                                 string
	QueueThreshold                       float64
	BreachIntervals, RecoveryIntervals   int
	Cooldown, SignalMaxAge               time.Duration
	PrivacyAcknowledged, BudgetAvailable bool
}

type State struct {
	External                        bool
	ConsecutiveHigh, ConsecutiveLow int
	LastChangedAt                   time.Time
}

type Signal struct {
	PrimaryHealthy bool
	Waiting        *float64
	ObservedAt     time.Time
}

type Decision struct {
	Route           string `json:"route"`
	Action          string `json:"action"`
	Reason          string `json:"reason"`
	ConsecutiveHigh int    `json:"consecutive_high"`
	ConsecutiveLow  int    `json:"consecutive_low"`
}

func (p Policy) Validate() error {
	if p.Mode != "health" && p.Mode != "health_and_queue" {
		return errors.New("overflow mode must be health or health_and_queue")
	}
	if !p.PrivacyAcknowledged {
		return errors.New("overflow requires explicit privacy acknowledgement")
	}
	if p.BreachIntervals < 1 || p.RecoveryIntervals < 1 || p.BreachIntervals > 100 || p.RecoveryIntervals > 100 {
		return errors.New("overflow breach and recovery intervals must be between 1 and 100")
	}
	if p.Cooldown < 0 || p.Cooldown > 24*time.Hour {
		return errors.New("overflow cooldown must be between zero and 24 hours")
	}
	if p.Mode == "health_and_queue" && (p.QueueThreshold <= 0 || math.IsNaN(p.QueueThreshold) || math.IsInf(p.QueueThreshold, 0) || p.SignalMaxAge <= 0 || p.SignalMaxAge > 10*time.Minute) {
		return errors.New("queue overflow requires positive threshold and signal max age no greater than ten minutes")
	}
	return nil
}

func Evaluate(policy Policy, state State, signal Signal, now time.Time) (Decision, error) {
	if err := policy.Validate(); err != nil {
		return Decision{}, err
	}
	decision := Decision{Route: "primary", Action: "hold", Reason: "primary is healthy and no overflow threshold is active", ConsecutiveHigh: state.ConsecutiveHigh, ConsecutiveLow: state.ConsecutiveLow}
	if !policy.BudgetAvailable {
		decision.Reason = "external hard budget is unavailable; overflow is denied"
		if !signal.PrimaryHealthy {
			decision.Route = "unavailable"
			decision.Action = "deny"
		} else if state.External {
			decision.Action = "recover"
		}
		return decision, nil
	}
	if policy.Mode == "health" {
		cooldown := !state.LastChangedAt.IsZero() && now.Sub(state.LastChangedAt) < policy.Cooldown
		if !signal.PrimaryHealthy {
			decision.ConsecutiveHigh = min(policy.BreachIntervals, state.ConsecutiveHigh+1)
			decision.ConsecutiveLow = 0
			if state.External {
				decision.Route = "external"
				decision.Reason = "unhealthy capacity remains decayed"
			} else if decision.ConsecutiveHigh >= policy.BreachIntervals && !cooldown {
				decision.Route, decision.Action = "external", "overflow"
				decision.Reason = "capacity was unhealthy for the required consecutive observations"
			} else {
				decision.Reason = "waiting for unhealthy-capacity hysteresis"
			}
			return decision, nil
		}
		decision.ConsecutiveHigh = 0
		decision.ConsecutiveLow = min(policy.RecoveryIntervals, state.ConsecutiveLow+1)
		if state.External {
			decision.Route = "external"
			decision.Reason = "decayed capacity awaits bounded recovery"
			if decision.ConsecutiveLow >= policy.RecoveryIntervals && !cooldown {
				decision.Route, decision.Action = "primary", "recover"
				decision.Reason = "capacity recovered for the required consecutive observations"
			}
		}
		return decision, nil
	}
	if !signal.PrimaryHealthy {
		decision.Route = "external"
		decision.Action = "overflow"
		decision.Reason = "all primary capacity is unhealthy"
		decision.ConsecutiveHigh = 0
		decision.ConsecutiveLow = 0
		return decision, nil
	}
	if signal.Waiting == nil || math.IsNaN(*signal.Waiting) || math.IsInf(*signal.Waiting, 0) || signal.ObservedAt.IsZero() || now.Sub(signal.ObservedAt) < 0 || now.Sub(signal.ObservedAt) > policy.SignalMaxAge {
		decision.Reason = "queue evidence is missing or stale; preserve the current bounded route"
		if state.External {
			decision.Route = "external"
		}
		return decision, nil
	}
	high := *signal.Waiting >= policy.QueueThreshold
	if high {
		if decision.ConsecutiveHigh < policy.BreachIntervals {
			decision.ConsecutiveHigh++
		}
		decision.ConsecutiveLow = 0
	} else {
		if decision.ConsecutiveLow < policy.RecoveryIntervals {
			decision.ConsecutiveLow++
		}
		decision.ConsecutiveHigh = 0
	}
	cooldown := !state.LastChangedAt.IsZero() && now.Sub(state.LastChangedAt) < policy.Cooldown
	if state.External {
		decision.Route = "external"
		decision.Reason = "external overflow remains active until bounded recovery"
		if !high && decision.ConsecutiveLow >= policy.RecoveryIntervals && !cooldown {
			decision.Route = "primary"
			decision.Action = "recover"
			decision.Reason = "primary queue recovered for the required consecutive observations"
		}
		return decision, nil
	}
	if high && decision.ConsecutiveHigh >= policy.BreachIntervals && !cooldown {
		decision.Route = "external"
		decision.Action = "overflow"
		decision.Reason = "primary queue exceeded the threshold for the required consecutive observations"
	}
	return decision, nil
}
