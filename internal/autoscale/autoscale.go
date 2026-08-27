// Package autoscale implements bounded, explainable scaling decisions.
package autoscale

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const MinimumSLOSamples = 20

type Policy struct {
	Enabled                              bool
	MinReplicas, MaxReplicas             int
	QueueThreshold, LowLoadThreshold     float64
	ScaleUpIntervals, ScaleDownIntervals int
	Cooldown                             time.Duration
	MaxTTFTP95MS, MaxLatencyP95MS        *float64
}

type Signals struct {
	Waiting, Running float64
	ObservedAt       time.Time
}

// SLOEvidence is a content-free snapshot derived from persisted request and
// benchmark records. Capacity is usable only when Comparable is true and both
// sample floors are satisfied; unknown values never become zeroes.
type SLOEvidence struct {
	RequestSamples       int      `json:"request_samples"`
	RequestsPerSecond    *float64 `json:"requests_per_second,omitempty"`
	P95TTFTMS            *float64 `json:"p95_ttft_ms,omitempty"`
	P95LatencyMS         *float64 `json:"p95_latency_ms,omitempty"`
	BenchmarkID          string   `json:"benchmark_id,omitempty"`
	BenchmarkSamples     int      `json:"benchmark_samples,omitempty"`
	GoodputPerReplica    *float64 `json:"goodput_per_replica,omitempty"`
	Comparable           bool     `json:"comparable"`
	ComparisonBoundary   string   `json:"comparison_boundary,omitempty"`
	RequestWindowSeconds int      `json:"request_window_seconds"`
}

type EvidenceSnapshot struct {
	Queue Signals     `json:"queue"`
	SLO   SLOEvidence `json:"slo"`
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

// EvaluateWithSLO preserves the queue evaluator as the fail-safe path. An SLO
// target is considered only from fresh request attainment and comparable
// benchmark goodput with explicit sample floors. Scale-down retains the same
// hysteresis/cooldown path and the fleet remains responsible for route fencing.
func EvaluateWithSLO(policy Policy, state State, signals Signals, evidence SLOEvidence) (Decision, error) {
	fallback := func(reason string) (Decision, error) {
		decision, err := Evaluate(policy, state, signals)
		if err == nil {
			decision.Reason = strings.TrimSpace(reason + "; queue policy: " + decision.Reason)
		}
		return decision, err
	}
	if policy.MaxTTFTP95MS == nil && policy.MaxLatencyP95MS == nil {
		return fallback("SLO policy absent; capacity evidence absent")
	}
	if evidence.RequestSamples < MinimumSLOSamples || evidence.RequestsPerSecond == nil {
		return fallback(fmt.Sprintf("SLO attainment evidence insufficient: need %d persisted requests, have %d; capacity evidence absent", MinimumSLOSamples, evidence.RequestSamples))
	}
	if !evidence.Comparable || evidence.BenchmarkSamples < MinimumSLOSamples || evidence.GoodputPerReplica == nil || *evidence.GoodputPerReplica <= 0 {
		return fallback("capacity evidence absent for the comparable model/runtime/provider/accelerator tuple")
	}
	breaches := make([]string, 0, 2)
	if policy.MaxTTFTP95MS != nil {
		if evidence.P95TTFTMS == nil {
			return fallback("TTFT attainment is unknown; queue policy used")
		}
		if *evidence.P95TTFTMS > *policy.MaxTTFTP95MS {
			breaches = append(breaches, fmt.Sprintf("p95 TTFT %.2fms > %.2fms", *evidence.P95TTFTMS, *policy.MaxTTFTP95MS))
		}
	}
	if policy.MaxLatencyP95MS != nil {
		if evidence.P95LatencyMS == nil {
			return fallback("latency attainment is unknown; queue policy used")
		}
		if *evidence.P95LatencyMS > *policy.MaxLatencyP95MS {
			breaches = append(breaches, fmt.Sprintf("p95 latency %.2fms > %.2fms", *evidence.P95LatencyMS, *policy.MaxLatencyP95MS))
		}
	}
	target := int(math.Ceil(*evidence.RequestsPerSecond / *evidence.GoodputPerReplica))
	target = max(policy.MinReplicas, min(policy.MaxReplicas, target))
	if len(breaches) > 0 && target <= state.Replicas {
		target = min(policy.MaxReplicas, state.Replicas+1)
	}
	decision, err := evaluateEvidenceTarget(policy, state, signals.ObservedAt, target, len(breaches) > 0)
	if err != nil {
		return Decision{}, err
	}
	decision.Reason = fmt.Sprintf("SLO evidence: %s; target %d from %.4f requests/s / %.4f benchmark goodput/replica; benchmark=%s samples=%d", evidenceStatus(breaches), target, *evidence.RequestsPerSecond, *evidence.GoodputPerReplica, evidence.BenchmarkID, evidence.BenchmarkSamples)
	return decision, nil
}

func evaluateEvidenceTarget(policy Policy, state State, observedAt time.Time, target int, breached bool) (Decision, error) {
	if policy.MinReplicas < 1 || policy.MaxReplicas < policy.MinReplicas || target < policy.MinReplicas || target > policy.MaxReplicas {
		return Decision{}, errors.New("invalid replica bounds or evidence target")
	}
	d := Decision{Action: "hold", OldReplicas: state.Replicas, NewReplicas: state.Replicas}
	if !policy.Enabled {
		d.Reason = "policy disabled"
		return d, nil
	}
	if !state.LastScaledAt.IsZero() && observedAt.Sub(state.LastScaledAt) < policy.Cooldown {
		d.Reason = "cooldown active"
		return d, nil
	}
	if target > state.Replicas || breached {
		d.NextConsecutiveHigh = state.ConsecutiveHigh + 1
		if d.NextConsecutiveHigh >= policy.ScaleUpIntervals && state.Replicas < policy.MaxReplicas {
			d.Action, d.NewReplicas, d.NextConsecutiveHigh = "scale_up", min(target, state.Replicas+1), 0
		}
		return d, nil
	}
	if target < state.Replicas {
		d.NextConsecutiveLow = state.ConsecutiveLow + 1
		if d.NextConsecutiveLow >= policy.ScaleDownIntervals && state.Replicas > policy.MinReplicas {
			d.Action, d.NewReplicas, d.NextConsecutiveLow = "scale_down", max(target, state.Replicas-1), 0
		}
	}
	return d, nil
}

func evidenceStatus(breaches []string) string {
	if len(breaches) == 0 {
		return "attained"
	}
	return "breached (" + strings.Join(breaches, ", ") + ")"
}
