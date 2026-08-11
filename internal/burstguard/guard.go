// Package burstguard makes deterministic policy-bounded overflow decisions.
package burstguard

import "time"

type Policy struct {
	Enabled                                            bool
	QueueThreshold, BreachIntervals, RecoveryIntervals int
	SignalMaxAge                                       time.Duration
	MaxIncrementalCostMicrousdHour                     int64
}
type Signal struct {
	QueueDepth, ConsecutiveBreaches, ConsecutiveRecovery int
	IncrementalCostMicrousdHour                          int64
	ObservedAt                                           time.Time
	ExternalHealthy                                      bool
}
type Decision struct {
	Action, Reason string
	Cost           int64
}

func Evaluate(p Policy, s Signal, now time.Time) Decision {
	d := Decision{Action: "hold", Reason: "burst threshold not sustained", Cost: s.IncrementalCostMicrousdHour}
	if !p.Enabled {
		return d
	}
	if s.ObservedAt.IsZero() || now.Sub(s.ObservedAt) < 0 || now.Sub(s.ObservedAt) > p.SignalMaxAge {
		return Decision{Action: "unknown", Reason: "fresh queue and capacity evidence required"}
	}
	if !s.ExternalHealthy {
		return Decision{Action: "hold", Reason: "overflow capacity is not healthy"}
	}
	if s.IncrementalCostMicrousdHour < 0 || s.IncrementalCostMicrousdHour > p.MaxIncrementalCostMicrousdHour {
		return Decision{Action: "hold", Reason: "incremental hourly cost exceeds hard policy budget", Cost: s.IncrementalCostMicrousdHour}
	}
	if s.QueueDepth >= p.QueueThreshold && s.ConsecutiveBreaches >= p.BreachIntervals {
		return Decision{Action: "overflow", Reason: "fresh queue breach sustained within cost budget", Cost: s.IncrementalCostMicrousdHour}
	}
	if s.QueueDepth < p.QueueThreshold && s.ConsecutiveRecovery >= p.RecoveryIntervals {
		return Decision{Action: "recover", Reason: "queue recovered for the required intervals", Cost: s.IncrementalCostMicrousdHour}
	}
	return d
}
